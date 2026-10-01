package apitoken_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/teb-ooo/playground-go/apitoken"
	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/internal/uuidv7"
	"github.com/teb-ooo/playground-go/openapimcp"
	"github.com/teb-ooo/playground-go/ratelimit"
	"github.com/teb-ooo/playground-go/testkit"
)

var key = []byte("0123456789abcdef0123456789abcdef")

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// countingStore counts Touch calls and can swap the record Lookup returns.
type countingStore struct {
	apitoken.Store
	touches atomic.Int32
	lookups atomic.Int32
	rewrite func(apitoken.Record) apitoken.Record
}

func (s *countingStore) Touch(ctx context.Context, id string, at time.Time) error {
	s.touches.Add(1)
	return s.Store.Touch(ctx, id, at)
}

func (s *countingStore) Lookup(ctx context.Context, h []byte) (apitoken.Record, error) {
	s.lookups.Add(1)
	r, err := s.Store.Lookup(ctx, h)
	if err == nil && s.rewrite != nil {
		r = s.rewrite(r)
	}
	return r, err
}

func mint(t *testing.T, s apitoken.Store, user string, at time.Time, exp *time.Time) (string, apitoken.Record) {
	t.Helper()
	tok, err := apitoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	r := apitoken.Record{ID: uuidv7.New(), UserID: user, UserEmail: user + "@x.org", UserUsername: user, Name: "n",
		Hash: apitoken.Hash(tok), Prefix: apitoken.DisplayPrefix(tok), CreatedAt: at, ExpiresAt: exp}
	if err := s.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return tok, r
}

func req(ip string) *http.Request {
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = ip + ":1234"
	return r
}

func TestTokenFormat(t *testing.T) {
	a, _ := apitoken.Generate()
	b, _ := apitoken.Generate()
	if a == b || !strings.HasPrefix(a, "pat_") || len(a) != 47 {
		t.Fatalf("token %q", a)
	}
	if apitoken.DisplayPrefix(a) != a[:8] || len(apitoken.Hash(a)) != 32 {
		t.Fatal("prefix or hash")
	}
	if apitoken.Prefix != auth.PATPrefix {
		t.Fatal("prefix mismatch with auth")
	}
}

func TestVerify(t *testing.T) {
	ck := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	ms := apitoken.NewMemoryStore()
	cs := &countingStore{Store: ms}
	v := apitoken.NewVerifier(cs, apitoken.VerifierOptions{Now: ck.now})

	exp := ck.now().Add(time.Hour)
	good, rec := mint(t, ms, "u1", ck.now(), nil)
	expiring, _ := mint(t, ms, "u1", ck.now(), &exp)
	revoked, rrec := mint(t, ms, "u1", ck.now(), nil)
	if err := ms.Revoke(context.Background(), "u1", rrec.ID, ck.now()); err != nil {
		t.Fatal(err)
	}

	t.Run("valid token yields the snapshot, no groups", func(t *testing.T) {
		u, ok, err := v.Verify(req("10.0.0.1"), good)
		if err != nil || !ok || u.Subject != "u1" || u.Email != "u1@x.org" || u.Username != "u1" || len(u.Groups) != 0 || u.IsAdmin() {
			t.Fatalf("%+v %v %v", u, ok, err)
		}
	})
	t.Run("groups only through the option", func(t *testing.T) {
		vg := apitoken.NewVerifier(ms, apitoken.VerifierOptions{Now: ck.now,
			Groups: func(_ context.Context, r apitoken.Record) []string { return []string{"admin"} }})
		u, ok, _ := vg.Verify(req("10.0.0.1"), good)
		if !ok || !u.IsAdmin() {
			t.Fatalf("%+v", u)
		}
	})
	t.Run("unknown, malformed and wrong-prefix are refused", func(t *testing.T) {
		other, _ := apitoken.Generate()
		for _, tok := range []string{other, "pat_short", "pat_" + strings.Repeat("!", 43), "xat_" + good[4:], ""} {
			if _, ok, err := v.Verify(req("10.0.0.2"), tok); ok || err != nil {
				t.Errorf("%q: ok=%v err=%v", tok[:min(len(tok), 8)], ok, err)
			}
		}
	})
	t.Run("revoked is refused at once", func(t *testing.T) {
		if _, ok, err := v.Verify(req("10.0.0.3"), revoked); ok || err != nil {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		if _, ok, _ := v.Verify(req("10.0.0.3"), expiring); !ok {
			t.Fatal("refused before expiry")
		}
		ck.advance(time.Hour)
		if _, ok, _ := v.Verify(req("10.0.0.3"), expiring); ok {
			t.Fatal("accepted at expiry")
		}
		if _, ok, _ := v.Verify(req("10.0.0.3"), good); !ok {
			t.Fatal("non-expiring token refused")
		}
	})
	t.Run("revoking after use takes effect on the next request", func(t *testing.T) {
		tok, r := mint(t, ms, "u9", ck.now(), nil)
		if _, ok, _ := v.Verify(req("10.0.0.4"), tok); !ok {
			t.Fatal("refused")
		}
		ms.Revoke(context.Background(), "u9", r.ID, ck.now())
		if _, ok, _ := v.Verify(req("10.0.0.4"), tok); ok {
			t.Fatal("revoked token still accepted")
		}
	})
	_ = rec
}

func TestLastUsedThrottle(t *testing.T) {
	ck := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	ms := apitoken.NewMemoryStore()
	cs := &countingStore{Store: ms}
	v := apitoken.NewVerifier(cs, apitoken.VerifierOptions{Now: ck.now})
	tok, rec := mint(t, ms, "u1", ck.now(), nil)

	use := func() { v.Verify(req("10.0.0.1"), tok) }
	use()
	use()
	ck.advance(59 * time.Second)
	use()
	if n := cs.touches.Load(); n != 1 {
		t.Fatalf("touches = %d within a minute, want 1", n)
	}
	ck.advance(2 * time.Second)
	use()
	if n := cs.touches.Load(); n != 2 {
		t.Fatalf("touches = %d after a minute, want 2", n)
	}
	got, _ := ms.Lookup(context.Background(), rec.Hash)
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(ck.now()) {
		t.Fatalf("last_used_at = %v, want %v", got.LastUsedAt, ck.now())
	}
	// A fresh process (new verifier) honours the stored last_used_at.
	v2 := apitoken.NewVerifier(cs, apitoken.VerifierOptions{Now: ck.now})
	v2.Verify(req("10.0.0.1"), tok)
	if n := cs.touches.Load(); n != 2 {
		t.Fatalf("restarted verifier touched again within the minute: %d", n)
	}
}

func TestConstantTimeComparePathRejectsAMismatchingStore(t *testing.T) {
	ms := apitoken.NewMemoryStore()
	cs := &countingStore{Store: ms}
	v := apitoken.NewVerifier(cs, apitoken.VerifierOptions{})
	tok, _ := mint(t, ms, "u1", time.Now(), nil)
	cs.rewrite = func(r apitoken.Record) apitoken.Record { r.Hash = apitoken.Hash("pat_something-else"); return r }
	if _, ok, err := v.Verify(req("10.0.0.1"), tok); ok || err != nil {
		t.Fatalf("a record whose hash differs authenticated: ok=%v err=%v", ok, err)
	}
}

func TestBruteForceLimiter(t *testing.T) {
	ck := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	ms := apitoken.NewMemoryStore()
	cs := &countingStore{Store: ms}
	v := apitoken.NewVerifier(cs, apitoken.VerifierOptions{Now: ck.now,
		FailLimit: ratelimit.Options{Burst: 3, Requests: 3, Per: time.Minute}})
	good, _ := mint(t, ms, "u1", ck.now(), nil)
	revoked, rr := mint(t, ms, "u1", ck.now(), nil)
	ms.Revoke(context.Background(), "u1", rr.ID, ck.now())

	// Revoked tokens are not charged.
	for i := 0; i < 10; i++ {
		if _, ok, err := v.Verify(req("1.1.1.1"), revoked); ok || err != nil {
			t.Fatalf("revoked: ok=%v err=%v", ok, err)
		}
	}
	var rl *auth.RateLimitedError
	for i := 0; i < 3; i++ {
		tok, _ := apitoken.Generate()
		if _, ok, err := v.Verify(req("1.1.1.1"), tok); ok || err != nil {
			t.Fatalf("guess %d: ok=%v err=%v", i, ok, err)
		}
	}
	// Out of failures: even the right token is refused before any lookup.
	before := cs.lookups.Load()
	_, ok, err := v.Verify(req("1.1.1.1"), good)
	if ok || !errors.As(err, &rl) || rl.RetryAfter <= 0 {
		t.Fatalf("over limit: ok=%v err=%v", ok, err)
	}
	if cs.lookups.Load() != before {
		t.Fatal("store was consulted for a blocked client")
	}
	// Another client is unaffected.
	if _, ok, _ := v.Verify(req("2.2.2.2"), good); !ok {
		t.Fatal("other IP blocked")
	}
	// The limit lifts.
	ck.advance(30 * time.Second)
	if _, ok, err := v.Verify(req("1.1.1.1"), good); !ok || err != nil {
		t.Fatalf("after wait: ok=%v err=%v", ok, err)
	}
	// X-Forwarded-For from a trusted proxy (loopback) keys on the last hop.
	r := req("127.0.0.1")
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 3.3.3.3")
	for i := 0; i < 3; i++ {
		tok, _ := apitoken.Generate()
		v.Verify(r, tok)
	}
	if _, _, err := v.Verify(r, good); !errors.As(err, &rl) {
		t.Fatalf("forwarded client not limited: %v", err)
	}
	if _, ok, _ := v.Verify(req("9.9.9.9"), good); !ok {
		t.Fatal("first XFF hop was charged")
	}
}

func TestStoreFailureIsNotAnAuthFailure(t *testing.T) {
	v := apitoken.NewVerifier(failingStore{}, apitoken.VerifierOptions{})
	tok, _ := apitoken.Generate()
	_, ok, err := v.Verify(req("1.1.1.1"), tok)
	if ok || err == nil || strings.Contains(err.Error(), tok) {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

type failingStore struct{ apitoken.Store }

func (failingStore) Lookup(context.Context, []byte) (apitoken.Record, error) {
	return apitoken.Record{}, errors.New("connection refused")
}

// ---- HTTP: the operations behind the real auth middleware ----

type app struct {
	h     http.Handler
	mcpH  http.Handler
	api   huma.API
	store *apitoken.MemoryStore
	ck    *clock
}

func newApp(t *testing.T) *app {
	t.Helper()
	ck := &clock{t: time.Now().UTC()}
	ms := apitoken.NewMemoryStore()
	v := apitoken.NewVerifier(ms, apitoken.VerifierOptions{Now: ck.now})
	a, err := auth.New(auth.OIDCConfig{Issuer: "https://issuer.invalid", ClientID: "app", PublicURL: "https://app.example"}, key,
		auth.WithTokenVerifier(v.Verify))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	auth.AddSecuritySchemes(api.OpenAPI())
	a.Register(api, mux)
	apitoken.Register(api, ms, apitoken.Options{Now: ck.now})
	huma.Register(api, huma.Operation{OperationID: "get-caller", Method: "GET", Path: "/api/whoami", Summary: "Who am I", Description: "Returns the caller.",
		Tags: []string{"me"}, Security: []map[string][]string{{"session": {}}, {"bearer": {}}}},
		func(ctx context.Context, _ *struct{}) (*struct {
			Body struct {
				Subject string `json:"subject"`
				Via     string `json:"via"`
			}
		}, error) {
			u, err := auth.Require(ctx)
			if err != nil {
				return nil, err
			}
			out := &struct {
				Body struct {
					Subject string `json:"subject"`
					Via     string `json:"via"`
				}
			}{}
			out.Body.Subject, out.Body.Via = u.Subject, string(auth.CredentialFromContext(ctx))
			return out, nil
		})
	mcpH := openapimcp.Handler(api, mux, openapimcp.Options{Name: "t", Auth: a.BearerOrSession})
	mux.Handle("/mcp", mcpH)
	return &app{h: a.BearerOrSession(mux), mcpH: mcpH, api: api, store: ms, ck: ck}
}

func (a *app) do(t *testing.T, method, path, body string, hdr map[string]string, user string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if user != "" {
		c, err := testkit.MintSession(key, auth.User{Subject: user, Email: user + "@x.org", Username: user, Groups: []string{"admin"}})
		if err != nil {
			t.Fatal(err)
		}
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func (a *app) create(t *testing.T, user, body string) (id, token string) {
	t.Helper()
	code, b := a.do(t, "POST", "/api/tokens", body, nil, user)
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	var out struct{ ID, Token, Prefix, Name string }
	json.Unmarshal(b, &out)
	if !strings.HasPrefix(out.Token, "pat_") || out.Prefix != out.Token[:8] {
		t.Fatalf("created %s", b)
	}
	return out.ID, out.Token
}

func TestOperationsLifecycle(t *testing.T) {
	a := newApp(t)
	id, tok := a.create(t, "alice", `{"name":"claude code","expires_in_days":30}`)

	// The token authenticates as alice, with no groups, on the API.
	code, b := a.do(t, "GET", "/api/whoami", "", map[string]string{"Authorization": "Bearer " + tok}, "")
	if code != 200 || !strings.Contains(string(b), `"subject":"alice"`) || !strings.Contains(string(b), `"via":"token"`) {
		t.Fatalf("whoami: %d %s", code, b)
	}

	// List: own tokens only, never the secret.
	a.create(t, "bob", `{"name":"bobs"}`)
	code, b = a.do(t, "GET", "/api/tokens", "", nil, "alice")
	if code != 200 || strings.Contains(string(b), tok) || strings.Contains(string(b), `"token"`) || strings.Contains(string(b), "bobs") ||
		!strings.Contains(string(b), `"name":"claude code"`) || !strings.Contains(string(b), `"expires_at"`) {
		t.Fatalf("list: %d %s", code, b)
	}

	// Revoke: bob cannot, alice can, then the token is dead.
	if code, _ := a.do(t, "DELETE", "/api/tokens/"+id, "", nil, "bob"); code != 404 {
		t.Fatalf("bob revoking alice's token: %d", code)
	}
	if code, _ := a.do(t, "GET", "/api/whoami", "", map[string]string{"Authorization": "Bearer " + tok}, ""); code != 200 {
		t.Fatalf("token died after a stranger's revoke: %d", code)
	}
	if code, _ := a.do(t, "DELETE", "/api/tokens/"+id, "", nil, "alice"); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := a.do(t, "DELETE", "/api/tokens/"+id, "", nil, "alice"); code != 204 {
		t.Fatalf("second revoke: %d", code)
	}
	if code, _ := a.do(t, "GET", "/api/whoami", "", map[string]string{"Authorization": "Bearer " + tok}, ""); code != 401 {
		t.Fatalf("revoked token: %d", code)
	}
	code, b = a.do(t, "GET", "/api/tokens", "", nil, "alice")
	if !strings.Contains(string(b), `"revoked_at"`) {
		t.Fatalf("revoked_at missing: %s", b)
	}

	// Validation and anonymous access.
	for _, body := range []string{`{}`, `{"name":""}`, `{"name":"   "}`, `{"name":"x","expires_in_days":0}`, `{"name":"x","expires_in_days":99999}`, `{"name":"` + strings.Repeat("x", 81) + `"}`} {
		if code, _ := a.do(t, "POST", "/api/tokens", body, nil, "alice"); code != 422 {
			t.Errorf("create %s: %d, want 422", body, code)
		}
	}
	if code, _ := a.do(t, "GET", "/api/tokens", "", nil, ""); code != 401 {
		t.Errorf("anonymous list: %d", code)
	}
	if code, _ := a.do(t, "DELETE", "/api/tokens/not-a-uuid", "", nil, "alice"); code != 422 {
		t.Errorf("bad id: %d", code)
	}
}

func TestExpiryOverHTTP(t *testing.T) {
	a := newApp(t)
	_, tok := a.create(t, "alice", `{"name":"short","expires_in_days":1}`)
	h := map[string]string{"Authorization": "Bearer " + tok}
	if code, _ := a.do(t, "GET", "/api/whoami", "", h, ""); code != 200 {
		t.Fatalf("before expiry: %d", code)
	}
	a.ck.advance(25 * time.Hour)
	if code, _ := a.do(t, "GET", "/api/whoami", "", h, ""); code != 401 {
		t.Fatalf("after expiry: %d", code)
	}
}

func TestListPagination(t *testing.T) {
	a := newApp(t)
	for i := 0; i < 5; i++ {
		a.create(t, "alice", `{"name":"t"}`)
	}
	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		path := "/api/tokens?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		code, b := a.do(t, "GET", path, "", nil, "alice")
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		var out struct {
			Items      []struct{ ID string }
			NextCursor string `json:"next_cursor"`
		}
		json.Unmarshal(b, &out)
		for _, it := range out.Items {
			seen = append(seen, it.ID)
		}
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paged %d tokens, want 5", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] <= seen[i] {
			t.Fatalf("not newest first: %v", seen)
		}
	}
	for _, p := range []string{"?limit=0", "?limit=201"} {
		if code, _ := a.do(t, "GET", "/api/tokens"+p, "", nil, "alice"); code != 422 {
			t.Errorf("%s: %d, want 422", p, code)
		}
	}
	if code, _ := a.do(t, "GET", "/api/tokens?cursor=garbage", "", nil, "alice"); code != 400 {
		t.Errorf("malformed cursor: %d, want 400", code)
	}
}

func TestATokenCannotManageTokens(t *testing.T) {
	a := newApp(t)
	id, tok := a.create(t, "alice", `{"name":"t"}`)
	h := map[string]string{"Authorization": "Bearer " + tok}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/tokens", `{"name":"minted by a token"}`},
		{"GET", "/api/tokens", ""},
		{"DELETE", "/api/tokens/" + id, ""},
	} {
		if code, b := a.do(t, c.method, c.path, c.body, h, ""); code != 403 {
			t.Errorf("%s %s with a PAT: %d %s, want 403", c.method, c.path, code, b)
		}
	}
	// Nothing was minted or revoked.
	l, _ := a.store.ListByUser(context.Background(), "alice", 10, "")
	if len(l) != 1 || l[0].RevokedAt != nil {
		t.Fatalf("store changed: %+v", l)
	}
	// A bearer token wins over a cookie sent beside it (auth's existing rule), so it cannot borrow the session either.
	if code, _ := a.do(t, "GET", "/api/tokens", "", h, "alice"); code != 403 {
		t.Errorf("PAT beside a session cookie: %d, want 403", code)
	}
}

func TestMCPWithAToken(t *testing.T) {
	a := newApp(t)
	// Hidden operations are neither in the OpenAPI document nor tools, so ParityCheck passes.
	_, tok := a.create(t, "alice", `{"name":"claude desktop"}`)
	openapimcp.ParityCheck(t, a.api, a.mcpH, openapimcp.WithHeader("Authorization", "Bearer "+tok))
	for _, op := range []string{"create-api-token", "list-api-tokens", "revoke-api-token"} {
		for _, item := range a.api.OpenAPI().Paths {
			for _, o := range []*huma.Operation{item.Get, item.Post, item.Delete} {
				if o != nil && o.OperationID == op {
					t.Errorf("%s is in the OpenAPI document", op)
				}
			}
		}
	}

	ts := httptest.NewServer(a.mcpH)
	defer ts.Close()
	call := func(token string) (string, error) {
		hc := &http.Client{Transport: bearerTransport{token}}
		c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: hc}, nil)
		if err != nil {
			return "", err
		}
		defer s.Close()
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "get-caller"})
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(res)
		return string(b), nil
	}
	out, err := call(tok)
	if err != nil || !strings.Contains(out, "alice") {
		t.Fatalf("tools/call get-caller with a PAT: %v %s", err, out)
	}
	if out, _ := call("pat_" + strings.Repeat("A", 43)); strings.Contains(out, "alice") {
		t.Fatalf("a bogus token worked: %s", out)
	}
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestNothingLeaks(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	a := newApp(t)
	id, tok := a.create(t, "alice", `{"name":"t"}`)
	h := map[string]string{"Authorization": "Bearer " + tok}
	a.do(t, "GET", "/api/whoami", "", h, "")
	a.do(t, "GET", "/api/tokens", "", h, "") // refused
	wrong := tok[:len(tok)-1] + "A"
	if wrong == tok {
		wrong = tok[:len(tok)-1] + "B"
	}
	a.do(t, "GET", "/api/whoami", "", map[string]string{"Authorization": "Bearer " + wrong}, "")
	a.do(t, "DELETE", "/api/tokens/"+id, "", nil, "alice")
	a.do(t, "GET", "/api/whoami", "", h, "")

	// A store that fails: the error is logged by auth, never with the token.
	fs := failingStore{}
	v := apitoken.NewVerifier(fs, apitoken.VerifierOptions{})
	au, _ := auth.New(auth.OIDCConfig{Issuer: "https://issuer.invalid", ClientID: "a", PublicURL: "https://a.example"}, key, auth.WithTokenVerifier(v.Verify))
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	au.BearerOrSession(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), tok) {
		t.Fatalf("store failure: %d %s", w.Code, w.Body)
	}

	hash := hex.EncodeToString(apitoken.Hash(tok))
	for _, secret := range []string{tok, tok[4:], wrong, hash, string(apitoken.Hash(tok))} {
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("log contains a token or hash: %s", buf.String())
		}
	}
	if buf.Len() == 0 {
		t.Log("(nothing was logged at all)")
	}
}

func TestMigrationFile(t *testing.T) {
	b, err := apitoken.MigrationsFS.ReadFile("migrations/00001_api_tokens.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	up, down := strings.Index(s, "-- +goose Up"), strings.Index(s, "-- +goose Down")
	if up != 0 || down < up {
		t.Fatal("goose markers missing or misordered")
	}
	for _, want := range []string{"id            uuid PRIMARY KEY", "created_at    timestamptz NOT NULL DEFAULT now()",
		"updated_at    timestamptz NOT NULL DEFAULT now()", "EXECUTE FUNCTION set_updated_at()", "token_hash"} {
		if !strings.Contains(s, want) {
			t.Errorf("migration lacks %q", want)
		}
	}
}
