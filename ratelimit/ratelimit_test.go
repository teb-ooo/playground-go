package ratelimit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/teb-ooo/playground-go/ratelimit"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func TestTokenBucket(t *testing.T) {
	type step struct {
		advance   time.Duration
		key       string
		wantOK    bool
		wantRetry time.Duration // checked when !wantOK
	}
	tests := []struct {
		name  string
		opts  ratelimit.Options
		steps []step
	}{
		{"defaults: burst of 5 then refused, one token per 6s", ratelimit.Options{}, []step{
			{0, "a", true, 0}, {0, "a", true, 0}, {0, "a", true, 0}, {0, "a", true, 0}, {0, "a", true, 0},
			{0, "a", false, 6 * time.Second},
			{3 * time.Second, "a", false, 3 * time.Second},
			{3 * time.Second, "a", true, 0},
			{0, "a", false, 6 * time.Second},
			{6 * time.Second, "a", true, 0},
		}},
		{"keys are independent", ratelimit.Options{Burst: 1}, []step{
			{0, "a", true, 0}, {0, "a", false, 6 * time.Second}, {0, "b", true, 0}, {0, "b", false, 6 * time.Second},
		}},
		{"refill is capped at burst", ratelimit.Options{Burst: 2}, []step{
			{0, "a", true, 0}, {0, "a", true, 0}, {0, "a", false, 6 * time.Second},
			{time.Hour, "a", true, 0}, {0, "a", true, 0}, {0, "a", false, 6 * time.Second},
		}},
		{"custom rate 60 per minute", ratelimit.Options{Requests: 60, Per: time.Minute, Burst: 1}, []step{
			{0, "a", true, 0}, {0, "a", false, time.Second}, {time.Second, "a", true, 0},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newClock()
			tc.opts.Now = c.now
			l := ratelimit.New(tc.opts)
			for i, s := range tc.steps {
				c.advance(s.advance)
				ok, wait := l.Allow(s.key)
				if ok != s.wantOK {
					t.Fatalf("step %d: ok = %v, want %v", i, ok, s.wantOK)
				}
				if !ok && wait != s.wantRetry {
					t.Fatalf("step %d: retry = %v, want %v", i, wait, s.wantRetry)
				}
			}
		})
	}
}

func TestAllowIsAtomicAcrossKeys(t *testing.T) {
	c := newClock()
	l := ratelimit.New(ratelimit.Options{Burst: 1, Now: c.now})
	l.Allow("user:x") // empty the user bucket
	if ok, _ := l.Allow("ip:1", "user:x"); ok {
		t.Fatal("should be refused because user:x is empty")
	}
	// The refused request must not have consumed ip:1's token.
	if ok, _ := l.Allow("ip:1"); !ok {
		t.Error("a refused request consumed a token from another key")
	}
}

func TestClientIP(t *testing.T) {
	l := ratelimit.New(ratelimit.Options{})
	tests := []struct {
		name, remote, xff, want string
	}{
		{"direct client, no header", "203.0.113.9:5555", "", "203.0.113.9"},
		{"direct client cannot spoof XFF", "203.0.113.9:5555", "198.51.100.1", "203.0.113.9"},
		{"docker proxy, single hop", "172.18.0.5:41000", "198.51.100.7", "198.51.100.7"},
		{"docker proxy, last hop wins over client-supplied", "172.18.0.5:41000", "6.6.6.6, 198.51.100.7", "198.51.100.7"},
		{"loopback proxy", "127.0.0.1:1", "198.51.100.7", "198.51.100.7"},
		{"proxy without header", "172.18.0.5:41000", "", "172.18.0.5"},
		{"garbage header falls back to the proxy address", "172.18.0.5:41000", "not-an-ip", "172.18.0.5"},
		{"whitespace trimmed", "10.0.0.2:1", "1.1.1.1 ,  198.51.100.7 ", "198.51.100.7"},
		{"ipv6 client keyed by /64", "[2001:db8:1:2:3:4:5:6]:99", "", "2001:db8:1:2::/64"},
		{"ipv6 via proxy", "172.18.0.5:1", "2001:db8:1:2:aaaa::1", "2001:db8:1:2::/64"},
		{"ipv4-mapped is unmapped", "[::ffff:203.0.113.9]:1", "", "203.0.113.9"},
		{"remote without port", "203.0.113.9", "", "203.0.113.9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := l.ClientIP(tc.remote, tc.xff); got != tc.want {
				t.Errorf("ClientIP(%q, %q) = %q, want %q", tc.remote, tc.xff, got, tc.want)
			}
		})
	}
}

func TestCustomTrustedProxy(t *testing.T) {
	l := ratelimit.New(ratelimit.Options{TrustedProxy: func(a netip.Addr) bool { return a == netip.MustParseAddr("203.0.113.1") }})
	if got := l.ClientIP("203.0.113.1:1", "198.51.100.7"); got != "198.51.100.7" {
		t.Errorf("trusted custom proxy: %s", got)
	}
	if got := l.ClientIP("172.18.0.5:1", "198.51.100.7"); got != "172.18.0.5" {
		t.Errorf("untrusted private proxy: %s", got)
	}
}

func req(ip, xff string) *http.Request {
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = ip + ":1234"
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestMiddleware(t *testing.T) {
	c := newClock()
	l := ratelimit.New(ratelimit.Options{Burst: 2, Now: c.now})
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	do := func(ip, xff string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req(ip, xff))
		return w
	}
	for i := 0; i < 2; i++ {
		if w := do("172.18.0.5", "198.51.100.7"); w.Code != 204 {
			t.Fatalf("request %d = %d", i, w.Code)
		}
	}
	w := do("172.18.0.5", "198.51.100.7")
	if w.Code != 429 || w.Header().Get("Retry-After") != "6" || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("limited response: %d retry=%q ct=%q", w.Code, w.Header().Get("Retry-After"), w.Header().Get("Content-Type"))
	}
	var p map[string]any
	json.Unmarshal(w.Body.Bytes(), &p)
	if p["status"] != 429.0 || p["title"] != "Too Many Requests" || p["detail"] == nil {
		t.Errorf("problem = %v", p)
	}
	// A different client behind the same proxy is unaffected.
	if w := do("172.18.0.5", "198.51.100.8"); w.Code != 204 {
		t.Errorf("other client = %d", w.Code)
	}
	// Refill.
	c.advance(6 * time.Second)
	if w := do("172.18.0.5", "198.51.100.7"); w.Code != 204 {
		t.Errorf("after refill = %d", w.Code)
	}
}

func TestMiddlewareExtraKey(t *testing.T) {
	c := newClock()
	l := ratelimit.New(ratelimit.Options{Burst: 1, Now: c.now, Key: func(r *http.Request) string { return r.URL.Query().Get("u") }})
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	do := func(ip, user string) int {
		r := httptest.NewRequest("GET", "/x?u="+user, nil)
		r.RemoteAddr = ip + ":1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	tests := []struct {
		ip, user string
		want     int
	}{
		{"203.0.113.1", "ada", 200},
		{"203.0.113.2", "ada", 429}, // same user from another IP
		{"203.0.113.1", "bob", 429}, // same IP, other user
		{"203.0.113.3", "bob", 200},
		{"203.0.113.4", "", 200}, // no extra key
	}
	for i, s := range tests {
		if got := do(s.ip, s.user); got != s.want {
			t.Errorf("step %d (%s,%s) = %d, want %d", i, s.ip, s.user, got, s.want)
		}
	}
}

func TestMaxKeysBoundsMemoryAndKeepsWorking(t *testing.T) {
	c := newClock()
	l := ratelimit.New(ratelimit.Options{MaxKeys: 3, Burst: 1, Now: c.now})
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if ok, _ := l.Allow(k); !ok {
			t.Fatalf("first request for %s refused", k)
		}
	}
}

func TestHumaMiddlewareAndCheck(t *testing.T) {
	c := newClock()
	l := ratelimit.New(ratelimit.Options{Burst: 1, Now: c.now})
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	huma.Register(api, huma.Operation{
		OperationID: "login", Method: "POST", Path: "/login", Summary: "s", Description: "d",
		Middlewares: huma.Middlewares{l.HumaMiddleware(api)},
	}, func(ctx context.Context, in *struct{}) (*struct{}, error) { return nil, nil })
	userLimiter := ratelimit.New(ratelimit.Options{Burst: 1, Now: c.now})
	huma.Register(api, huma.Operation{
		OperationID: "by-user", Method: "POST", Path: "/by-user", Summary: "s", Description: "d",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name string `json:"name"`
		}
	}) (*struct{}, error) {
		return nil, userLimiter.Check("user:" + in.Body.Name)
	})

	post := func(path, body, ip, xff string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, nil)
		if body != "" {
			r = httptest.NewRequest("POST", path, stringsReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.RemoteAddr = ip + ":1"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	if w := post("/login", "", "172.18.0.2", "198.51.100.7"); w.Code != 204 {
		t.Fatalf("first login = %d %s", w.Code, w.Body)
	}
	w := post("/login", "", "172.18.0.2", "198.51.100.7")
	if w.Code != 429 || w.Header().Get("Retry-After") != "6" {
		t.Fatalf("limited login = %d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
	var p map[string]any
	json.Unmarshal(w.Body.Bytes(), &p)
	if p["status"] != 429.0 || p["title"] != "Too Many Requests" {
		t.Errorf("huma problem = %v", p)
	}
	if w := post("/login", "", "172.18.0.2", "198.51.100.9"); w.Code != 204 {
		t.Errorf("other client = %d", w.Code)
	}

	if w := post("/by-user", `{"name":"ada"}`, "1.1.1.1", ""); w.Code != 204 {
		t.Fatalf("first by-user = %d %s", w.Code, w.Body)
	}
	w = post("/by-user", `{"name":"ada"}`, "2.2.2.2", "")
	if w.Code != 429 || w.Header().Get("Retry-After") != "6" {
		t.Errorf("Check limited = %d retry=%q %s", w.Code, w.Header().Get("Retry-After"), w.Body)
	}
	if w := post("/by-user", `{"name":"bob"}`, "1.1.1.1", ""); w.Code != 204 {
		t.Errorf("other user = %d", w.Code)
	}
}

func TestPeekTakesNothing(t *testing.T) {
	c := newClock()
	l := ratelimit.New(ratelimit.Options{Burst: 2, Now: c.now})
	if blocked, _ := l.Peek("a"); blocked {
		t.Fatal("unseen key is blocked")
	}
	l.Allow("a")
	if blocked, _ := l.Peek("a"); blocked {
		t.Fatal("blocked with a token left")
	}
	l.Allow("a")
	blocked, wait := l.Peek("a")
	if !blocked || wait != 6*time.Second {
		t.Fatalf("Peek = %v %v, want blocked for 6s", blocked, wait)
	}
	// Peeking repeatedly must not consume or refill anything by itself.
	l.Peek("a")
	l.Peek("a")
	c.advance(6 * time.Second)
	if blocked, _ := l.Peek("a"); blocked {
		t.Fatal("still blocked after the refill interval")
	}
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("Allow refused after Peek said a token is available")
	}
}
