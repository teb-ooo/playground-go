package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	nmail "net/mail"
	"os"
	"strings"
	"time"
)

// Provider names.
const (
	ProviderResend   = "resend"
	ProviderPostmark = "postmark"
)

// Message is one outgoing email.
type Message struct {
	To      []string
	Subject string
	Text    string
	HTML    string
	ReplyTo string
}

// Config configures a Mailer.
type Config struct {
	// Provider is "resend" or "postmark" (MAIL_PROVIDER).
	Provider string
	// APIKey is the provider credential (MAIL_API_KEY).
	APIKey string
	// AppName is the sending app (APP_NAME); it names the default sender and
	// the staging subject prefix.
	AppName string
	// PlaygroundDomain is PLAYGROUND_DOMAIN; the default sender is AppName@PlaygroundDomain.
	PlaygroundDomain string
	// From overrides the default sender address.
	From string
	// Env is APP_ENV. Anything other than "production" is treated as staging.
	Env string
	// StagingSink is the inbox every staging message is redirected to
	// (MAIL_STAGING_SINK). Required outside production.
	StagingSink string
	// BaseURL overrides the provider endpoint (tests).
	BaseURL string
	// HTTPClient overrides the client (default: 15s timeout).
	HTTPClient *http.Client
}

// ConfigFromEnv reads MAIL_PROVIDER, MAIL_API_KEY, MAIL_STAGING_SINK,
// APP_NAME, APP_ENV and PLAYGROUND_DOMAIN.
func ConfigFromEnv() Config {
	return Config{
		Provider: os.Getenv("MAIL_PROVIDER"), APIKey: os.Getenv("MAIL_API_KEY"),
		StagingSink: os.Getenv("MAIL_STAGING_SINK"), AppName: os.Getenv("APP_NAME"),
		Env: os.Getenv("APP_ENV"), PlaygroundDomain: os.Getenv("PLAYGROUND_DOMAIN"),
	}
}

// Mailer sends messages through one provider.
type Mailer struct {
	cfg  Config
	from string
	prov provider
}

// New validates cfg and returns a Mailer.
func New(cfg Config) (*Mailer, error) {
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.APIKey == "" {
		return nil, errors.New("mail: MAIL_API_KEY is required")
	}
	if cfg.AppName == "" {
		return nil, errors.New("mail: app name is required")
	}
	from := cfg.From
	if from == "" {
		if cfg.PlaygroundDomain == "" {
			return nil, errors.New("mail: PLAYGROUND_DOMAIN (or an explicit From) is required")
		}
		from = cfg.AppName + "@" + cfg.PlaygroundDomain
	}
	if _, err := nmail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("mail: sender %q: %w", from, err)
	}
	if cfg.Env != "production" {
		if cfg.StagingSink == "" {
			return nil, errors.New("mail: MAIL_STAGING_SINK is required outside production")
		}
		if _, err := nmail.ParseAddress(cfg.StagingSink); err != nil {
			return nil, fmt.Errorf("mail: staging sink %q: %w", cfg.StagingSink, err)
		}
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	var prov provider
	switch cfg.Provider {
	case ProviderResend:
		prov = &resend{key: cfg.APIKey, base: orDefault(cfg.BaseURL, "https://api.resend.com"), client: client}
	case ProviderPostmark:
		prov = &postmark{key: cfg.APIKey, base: orDefault(cfg.BaseURL, "https://api.postmarkapp.com"), client: client}
	default:
		return nil, fmt.Errorf("mail: unknown MAIL_PROVIDER %q (want resend or postmark)", cfg.Provider)
	}
	return &Mailer{cfg: cfg, from: from, prov: prov}, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return strings.TrimRight(d, "/")
	}
	return strings.TrimRight(s, "/")
}

// outgoing is the provider-neutral wire message after validation and staging
// rewriting.
type outgoing struct {
	From, Subject, Text, HTML, ReplyTo string
	To                                 []string
}

type provider interface {
	name() string
	send(ctx context.Context, m outgoing) error
}

// Send validates m, applies the staging rewrite, and sends it. From defaults
// to APP_NAME@PLAYGROUND_DOMAIN. Bodies are never logged.
func (m *Mailer) Send(ctx context.Context, msg Message) error {
	out, err := m.prepare(msg)
	if err != nil {
		return err
	}
	if err := m.prov.send(ctx, out); err != nil {
		return fmt.Errorf("mail: sending via %s: %w", m.prov.name(), err)
	}
	slog.InfoContext(ctx, "mail sent", "provider", m.prov.name(), "recipients", len(out.To), "staging_rewrite", m.cfg.Env != "production")
	return nil
}

func (m *Mailer) prepare(msg Message) (outgoing, error) {
	if len(msg.To) == 0 {
		return outgoing{}, errors.New("mail: no recipients")
	}
	if strings.TrimSpace(msg.Subject) == "" {
		return outgoing{}, errors.New("mail: empty subject")
	}
	if msg.Text == "" && msg.HTML == "" {
		return outgoing{}, errors.New("mail: message has neither text nor HTML body")
	}
	for _, s := range append([]string{msg.Subject, msg.ReplyTo}, msg.To...) {
		if strings.ContainsAny(s, "\r\n") {
			return outgoing{}, errors.New("mail: line breaks are not allowed in headers")
		}
	}
	for _, a := range msg.To {
		if _, err := nmail.ParseAddress(a); err != nil {
			return outgoing{}, fmt.Errorf("mail: recipient %q: %w", a, err)
		}
	}
	if msg.ReplyTo != "" {
		if _, err := nmail.ParseAddress(msg.ReplyTo); err != nil {
			return outgoing{}, fmt.Errorf("mail: reply-to %q: %w", msg.ReplyTo, err)
		}
	}
	out := outgoing{From: m.from, To: append([]string(nil), msg.To...), Subject: msg.Subject,
		Text: msg.Text, HTML: msg.HTML, ReplyTo: msg.ReplyTo}
	if m.cfg.Env != "production" {
		out.To = []string{m.cfg.StagingSink}
		out.Subject = "[staging " + m.cfg.AppName + "] " + msg.Subject
	}
	return out, nil
}

// ---- providers ----

func doJSON(ctx context.Context, client *http.Client, url string, hdr map[string]string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response: %w", err)
	}
	return resp.StatusCode, rb, nil
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

type resend struct {
	key, base string
	client    *http.Client
}

func (*resend) name() string { return ProviderResend }

func (p *resend) send(ctx context.Context, m outgoing) error {
	payload := map[string]any{"from": m.From, "to": m.To, "subject": m.Subject}
	if m.Text != "" {
		payload["text"] = m.Text
	}
	if m.HTML != "" {
		payload["html"] = m.HTML
	}
	if m.ReplyTo != "" {
		payload["reply_to"] = m.ReplyTo
	}
	status, body, err := doJSON(ctx, p.client, p.base+"/emails", map[string]string{"Authorization": "Bearer " + p.key}, payload)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("status %d: %s", status, snippet(body))
	}
	return nil
}

type postmark struct {
	key, base string
	client    *http.Client
}

func (*postmark) name() string { return ProviderPostmark }

func (p *postmark) send(ctx context.Context, m outgoing) error {
	payload := map[string]any{"From": m.From, "To": strings.Join(m.To, ","), "Subject": m.Subject, "MessageStream": "outbound"}
	if m.Text != "" {
		payload["TextBody"] = m.Text
	}
	if m.HTML != "" {
		payload["HtmlBody"] = m.HTML
	}
	if m.ReplyTo != "" {
		payload["ReplyTo"] = m.ReplyTo
	}
	status, body, err := doJSON(ctx, p.client, p.base+"/email", map[string]string{"X-Postmark-Server-Token": p.key}, payload)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("status %d: %s", status, snippet(body))
	}
	var r struct {
		ErrorCode int
		Message   string
	}
	if json.Unmarshal(body, &r) == nil && r.ErrorCode != 0 {
		return fmt.Errorf("error code %d: %s", r.ErrorCode, r.Message)
	}
	return nil
}
