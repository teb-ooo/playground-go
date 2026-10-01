package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Kind says what a text is, because bge and e5 models embed a search query differently from a stored passage.
type Kind int

const (
	// Passage is text you store and search through (a note, a document chunk).
	Passage Kind = iota
	// Query is text a person searches with.
	Query
)

func (k Kind) String() string {
	if k == Query {
		return "query"
	}
	return "passage"
}

// Defaults of the platform service (compose service `embed`).
const (
	DefaultModel      = "BAAI/bge-base-en-v1.5"
	DefaultDimensions = 768
	// DefaultBatchSize equals the service's --max-client-batch-size.
	DefaultBatchSize = 32
)

// Embedder is what an app depends on, so its tests can hold a Fake instead of a Client.
type Embedder interface {
	Embed(ctx context.Context, texts []string, kind Kind) ([][]float32, error)
	Dimensions() int
}

var (
	_ Embedder = (*Client)(nil)
	_ Embedder = (*Fake)(nil)
)

// Client talks to the embeddings service. It is safe for concurrent use.
type Client struct {
	url       string
	model     string
	dims      int
	batch     int
	retries   int // retries after the first attempt
	backoff   time.Duration
	maxBack   time.Duration
	hc        *http.Client
	queryPfx  string
	passagePf string
	pfxSet    bool
	sleep     func(ctx context.Context, d time.Duration) error // replaced in tests
}

// Option configures a Client.
type Option func(*Client)

// WithModel sets the model name; it only selects the query/passage prefix convention (the service serves one model).
func WithModel(m string) Option { return func(c *Client) { c.model = m } }

// WithDimensions sets the expected vector size; a response of another size is an error.
func WithDimensions(n int) Option { return func(c *Client) { c.dims = n } }

// WithHTTPClient replaces the HTTP client (its Timeout is the per-request timeout).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.hc = h } }

// WithTimeout sets the per-request timeout (default 30s; a CPU batch of 32 passages takes a few seconds).
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.hc = &http.Client{Timeout: d} } }

// WithBatchSize sets how many texts go in one request (default 32, the service limit).
func WithBatchSize(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.batch = n
		}
	}
}

// WithRetries sets the number of retries after the first attempt (default 3; 0 disables retrying).
func WithRetries(n int) Option {
	return func(c *Client) {
		if n >= 0 {
			c.retries = n
		}
	}
}

// WithBackoff sets the first retry delay and its cap (default 200ms, 5s); the delay doubles with jitter.
func WithBackoff(first, max time.Duration) Option {
	return func(c *Client) { c.backoff, c.maxBack = first, max }
}

// WithPrefixes overrides the text prepended to passages and queries (the convention of the model is the default).
func WithPrefixes(passage, query string) Option {
	return func(c *Client) { c.passagePf, c.queryPfx, c.pfxSet = passage, query, true }
}

// New returns a client of the service at url (for example http://embed:8080).
func New(url string, opts ...Option) *Client {
	c := &Client{
		url: strings.TrimRight(url, "/"), model: DefaultModel, dims: DefaultDimensions, batch: DefaultBatchSize,
		retries: 3, backoff: 200 * time.Millisecond, maxBack: 5 * time.Second, sleep: sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	if c.hc == nil {
		c.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if !c.pfxSet { // the model's convention, unless WithPrefixes was given
		c.passagePf, c.queryPfx = Prefixes(c.model)
	}
	return c
}

// FromEnv builds a client from EMBED_URL (required), EMBED_MODEL (default bge-base-en-v1.5) and EMBED_DIMENSIONS (default 768).
// It fails when EMBED_URL is empty, so an app that was not given the service says so at start instead of at first search.
func FromEnv(opts ...Option) (*Client, error) {
	u := os.Getenv("EMBED_URL")
	if u == "" {
		return nil, errors.New("embed: EMBED_URL is not set (ask the platform to turn embeddings on for this app: `playground embeddings <app> on`, then restart it)")
	}
	all := []Option{}
	if m := os.Getenv("EMBED_MODEL"); m != "" {
		all = append(all, WithModel(m))
	}
	if d := os.Getenv("EMBED_DIMENSIONS"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("embed: EMBED_DIMENSIONS %q is not a positive integer", d)
		}
		all = append(all, WithDimensions(n))
	}
	return New(u, append(all, opts...)...), nil
}

// Prefixes returns the passage and query prefixes the model family was trained with.
//   - bge English v1.5 and v1 (bge-*-en*): queries get "Represent this sentence for searching relevant passages: ", passages none.
//   - e5 (e5-*, multilingual-e5-*): "query: " and "passage: ".
//   - anything else: none.
func Prefixes(model string) (passage, query string) {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "e5-") || strings.HasSuffix(m, "/e5"):
		return "passage: ", "query: "
	case strings.Contains(m, "bge-") && strings.Contains(m, "-en") && !strings.Contains(m, "bge-m3"):
		return "", "Represent this sentence for searching relevant passages: "
	}
	return "", ""
}

// Dimensions is the size of the vectors Embed returns (and the size of the vector column to create).
func (c *Client) Dimensions() int { return c.dims }

// Model is the configured model name.
func (c *Client) Model() string { return c.model }

// Embed returns one normalised vector per text, in order. Texts go to the service in batches of at most the batch size;
// each batch is retried with backoff on overload and transient failures. An empty slice returns an empty result without a request.
func (c *Client) Embed(ctx context.Context, texts []string, kind Kind) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	pfx := c.passagePf
	if kind == Query {
		pfx = c.queryPfx
	}
	for start := 0; start < len(texts); start += c.batch {
		end := min(start+c.batch, len(texts))
		inputs := make([]string, 0, end-start)
		for _, t := range texts[start:end] {
			inputs = append(inputs, pfx+t)
		}
		vecs, err := c.embedBatch(ctx, inputs)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// Health reports whether the service is up (GET /health) and, when it is not, why.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/health", nil)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("embed: health: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("embed: health: status %d", resp.StatusCode)
	}
	return nil
}

type embedRequest struct {
	Inputs    []string `json:"inputs"`
	Normalize bool     `json:"normalize"`
	Truncate  bool     `json:"truncate"`
}

// StatusError is a refusal by the service.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("embed: service answered %d", e.Status)
	}
	return fmt.Sprintf("embed: service answered %d: %s", e.Status, e.Message)
}

func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusFailedDependency:
		return true
	}
	return status >= 500 && status != http.StatusNotImplemented && status != http.StatusHTTPVersionNotSupported
}

func (c *Client) embedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Inputs: inputs, Normalize: true, Truncate: true})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	delay := c.backoff
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			// full jitter on the upper half: delay/2 .. delay
			d := delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))
			if err := c.sleep(ctx, d); err != nil {
				return nil, fmt.Errorf("embed: %w (last error: %v)", err, lastErr)
			}
			delay = min(delay*2, c.maxBack)
		}
		vecs, retry, err := c.once(ctx, body, len(inputs))
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		if !retry || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("embed: giving up after %d attempts: %w", c.retries+1, lastErr)
}

// once makes one request and says whether a failure is worth retrying.
func (c *Client) once(ctx context.Context, body []byte, want int) (vecs [][]float32, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("embed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("embed: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, retryable(resp.StatusCode), &StatusError{Status: resp.StatusCode, Message: msg}
	}
	// 64 MiB bounds a runaway answer (32 x 768 floats is well under 1 MiB).
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&vecs); err != nil {
		return nil, true, fmt.Errorf("embed: decoding the answer: %w", err)
	}
	if len(vecs) != want {
		return nil, false, fmt.Errorf("embed: asked for %d vectors, got %d", want, len(vecs))
	}
	for i, v := range vecs {
		if len(v) != c.dims {
			return nil, false, fmt.Errorf("embed: vector %d has %d dimensions, want %d (check EMBED_DIMENSIONS and the model)", i, len(v), c.dims)
		}
	}
	return vecs, false, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
