package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tei is a stand-in for text-embeddings-inference's POST /embed.
type tei struct {
	t       *testing.T
	dims    int
	reqs    atomic.Int32
	seen    [][]string
	failing func(n int) (status int, body string) // n counts requests from 1
}

func (s *tei) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.WriteHeader(200)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/embed" {
		http.NotFound(w, r)
		return
	}
	n := int(s.reqs.Add(1))
	if s.failing != nil {
		if code, body := s.failing(n); code != 0 {
			w.WriteHeader(code)
			fmt.Fprint(w, body)
			return
		}
	}
	var in struct {
		Inputs    []string `json:"inputs"`
		Normalize bool     `json:"normalize"`
		Truncate  bool     `json:"truncate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		w.WriteHeader(422)
		return
	}
	if !in.Normalize || !in.Truncate {
		s.t.Errorf("normalize=%v truncate=%v, want both true", in.Normalize, in.Truncate)
	}
	s.seen = append(s.seen, in.Inputs)
	out := make([][]float32, len(in.Inputs))
	for i, text := range in.Inputs {
		v := make([]float32, s.dims)
		v[0] = float32(len(text)) // lets a test see which text became which vector
		out[i] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func newTEI(t *testing.T, dims int) (*tei, *Client) {
	s := &tei{t: t, dims: dims}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c := New(srv.URL, WithDimensions(dims), WithBackoff(time.Millisecond, 4*time.Millisecond))
	return s, c
}

func TestEmbedShapeOrderAndPrefixes(t *testing.T) {
	s, c := newTEI(t, 4)
	ctx := context.Background()
	got, err := c.Embed(ctx, []string{"aa", "bbbb"}, Passage)
	if err != nil || len(got) != 2 || got[0][0] != 2 || got[1][0] != 4 {
		t.Fatalf("%v %v", got, err)
	}
	if strings.Join(s.seen[0], "|") != "aa|bbbb" {
		t.Fatalf("a passage must carry no bge prefix: %v", s.seen[0])
	}
	if _, err := c.Embed(ctx, []string{"q"}, Query); err != nil {
		t.Fatal(err)
	}
	if want := "Represent this sentence for searching relevant passages: q"; s.seen[1][0] != want {
		t.Fatalf("query sent as %q", s.seen[1][0])
	}
	if c.Dimensions() != 4 {
		t.Fatal(c.Dimensions())
	}
}

func TestPrefixesPerModelFamily(t *testing.T) {
	for _, tc := range []struct{ model, passage, query string }{
		{"BAAI/bge-base-en-v1.5", "", "Represent this sentence for searching relevant passages: "},
		{"BAAI/bge-small-en", "", "Represent this sentence for searching relevant passages: "},
		{"intfloat/e5-base-v2", "passage: ", "query: "},
		{"intfloat/multilingual-e5-small", "passage: ", "query: "},
		{"BAAI/bge-m3", "", ""},
		{"sentence-transformers/all-MiniLM-L6-v2", "", ""},
	} {
		p, q := Prefixes(tc.model)
		if p != tc.passage || q != tc.query {
			t.Errorf("%s: %q %q", tc.model, p, q)
		}
	}
	s, _ := newTEI(t, 2)
	srv := httptest.NewServer(s)
	defer srv.Close()
	c := New(srv.URL, WithDimensions(2), WithPrefixes("P:", ""))
	if _, err := c.Embed(context.Background(), []string{"x"}, Query); err != nil || s.seen[0][0] != "x" {
		t.Fatalf("%v %v", s.seen, err)
	}
	if _, err := c.Embed(context.Background(), []string{"x"}, Passage); err != nil || s.seen[1][0] != "P:x" {
		t.Fatalf("%v %v", s.seen, err)
	}
}

func TestBatching(t *testing.T) {
	s, c := newTEI(t, 3)
	c.batch = 32
	texts := make([]string, 70)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}
	got, err := c.Embed(context.Background(), texts, Passage)
	if err != nil || len(got) != 70 {
		t.Fatalf("%d %v", len(got), err)
	}
	if len(s.seen) != 3 || len(s.seen[0]) != 32 || len(s.seen[1]) != 32 || len(s.seen[2]) != 6 {
		t.Fatalf("batches: %d", len(s.seen))
	}
	for i, v := range got {
		if int(v[0]) != i+1 {
			t.Fatalf("order broken at %d", i)
		}
	}
	if out, err := c.Embed(context.Background(), nil, Passage); err != nil || len(out) != 0 || len(s.seen) != 3 {
		t.Fatalf("empty input must not call the service: %v %v", out, err)
	}
}

func TestRetriesOverloadThenSucceeds(t *testing.T) {
	s, c := newTEI(t, 2)
	s.failing = func(n int) (int, string) {
		if n < 3 {
			return 429, `{"error":"Model is overloaded","error_type":"overloaded"}`
		}
		return 0, ""
	}
	if _, err := c.Embed(context.Background(), []string{"a"}, Passage); err != nil {
		t.Fatal(err)
	}
	if s.reqs.Load() != 3 {
		t.Fatalf("requests: %d", s.reqs.Load())
	}
}

func TestGivesUpAfterTheRetries(t *testing.T) {
	s, c := newTEI(t, 2)
	s.failing = func(int) (int, string) { return 503, "down" }
	_, err := c.Embed(context.Background(), []string{"a"}, Passage)
	var se *StatusError
	if err == nil || !errors.As(err, &se) || se.Status != 503 || s.reqs.Load() != 4 {
		t.Fatalf("%v, requests %d", err, s.reqs.Load())
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	for _, code := range []int{400, 413, 422} {
		s, c := newTEI(t, 2)
		s.failing = func(int) (int, string) { return code, `{"error":"input too long","error_type":"validation"}` }
		_, err := c.Embed(context.Background(), []string{"a"}, Passage)
		var se *StatusError
		if !errors.As(err, &se) || se.Status != code || se.Message != "input too long" || s.reqs.Load() != 1 {
			t.Fatalf("%d: %v, requests %d", code, err, s.reqs.Load())
		}
	}
}

func TestDimensionAndCountMismatchAreErrors(t *testing.T) {
	_, c := newTEI(t, 4)
	c.dims = 8 // the service answers 4
	if _, err := c.Embed(context.Background(), []string{"a"}, Passage); err == nil || !strings.Contains(err.Error(), "4 dimensions, want 8") {
		t.Fatalf("%v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[[0,0]]`) }))
	defer srv.Close()
	c2 := New(srv.URL, WithDimensions(2))
	if _, err := c2.Embed(context.Background(), []string{"a", "b"}, Passage); err == nil || !strings.Contains(err.Error(), "got 1") {
		t.Fatalf("%v", err)
	}
}

func TestContextCancelStopsRetrying(t *testing.T) {
	s, c := newTEI(t, 2)
	s.failing = func(int) (int, string) { return 503, "" }
	c.backoff, c.maxBack = time.Hour, time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c.sleep = sleepCtx
	start := time.Now()
	if _, err := c.Embed(ctx, []string{"a"}, Passage); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
}

func TestTimeoutIsRetriedAsANetworkError(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			time.Sleep(300 * time.Millisecond)
		}
		fmt.Fprint(w, `[[1,2]]`)
	}))
	defer srv.Close()
	c := New(srv.URL, WithDimensions(2), WithTimeout(100*time.Millisecond), WithBackoff(time.Millisecond, time.Millisecond))
	if _, err := c.Embed(context.Background(), []string{"a"}, Passage); err != nil || n.Load() != 2 {
		t.Fatalf("%v %d", err, n.Load())
	}
}

func TestHealth(t *testing.T) {
	_, c := newTEI(t, 2)
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	dead := New("http://127.0.0.1:1")
	if err := dead.Health(context.Background()); err == nil {
		t.Fatal("want an error")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("EMBED_URL", "")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "playground embeddings") {
		t.Fatalf("%v", err)
	}
	t.Setenv("EMBED_URL", "http://embed:8080/")
	t.Setenv("EMBED_DIMENSIONS", "384")
	t.Setenv("EMBED_MODEL", "intfloat/e5-small-v2")
	c, err := FromEnv()
	if err != nil || c.Dimensions() != 384 || c.url != "http://embed:8080" || c.queryPfx != "query: " {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("EMBED_DIMENSIONS", "many")
	if _, err := FromEnv(); err == nil {
		t.Fatal("bad dimensions must fail")
	}
	t.Setenv("EMBED_DIMENSIONS", "")
	t.Setenv("EMBED_MODEL", "")
	c, err = FromEnv()
	if err != nil || c.Dimensions() != 768 || c.Model() != DefaultModel {
		t.Fatalf("%+v %v", c, err)
	}
}

func cosine(a, b []float32) float64 {
	var d float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
	}
	return d
}

func TestFake(t *testing.T) {
	f := Fake{}
	if f.Dimensions() != 768 || (Fake{Dims: 16}).Dimensions() != 16 {
		t.Fatal("dimensions")
	}
	ctx := context.Background()
	v, err := f.Embed(ctx, []string{"The cat sat", "the CAT sat!", "quantum chromodynamics", ""}, Passage)
	if err != nil || len(v) != 4 || len(v[0]) != 768 {
		t.Fatal(err)
	}
	again, _ := f.Embed(ctx, []string{"The cat sat"}, Query)
	if cosine(v[0], again[0]) < 0.9999 {
		t.Fatal("not deterministic across calls and kinds")
	}
	if cosine(v[0], v[1]) < 0.9999 {
		t.Fatal("case and punctuation must not matter")
	}
	if c := cosine(v[0], v[2]); math.Abs(c) > 0.2 {
		t.Fatalf("unrelated texts too close: %f", c)
	}
	share, _ := f.Embed(ctx, []string{"the cat sat on the mat"}, Passage)
	if cosine(v[0], share[0]) <= cosine(v[2], share[0]) {
		t.Fatal("shared words must be closer")
	}
	for _, x := range v {
		var n float64
		for _, e := range x {
			n += float64(e) * float64(e)
		}
		if math.Abs(n-1) > 1e-4 {
			t.Fatalf("not unit length: %f", n)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Embed(cctx, []string{"x"}, Passage); err == nil {
		t.Fatal("cancelled context")
	}
}
