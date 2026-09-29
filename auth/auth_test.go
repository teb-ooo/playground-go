package auth_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/teb-ooo/factory-go/auth"
	"github.com/teb-ooo/factory-go/ratelimit"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestParseKey(t *testing.T) {
	raw := string(testKey)
	tests := []struct {
		name, in string
		ok       bool
	}{
		{"raw 32", raw, true},
		{"hex 64", "3031323334353637383961626364656630313233343536373839616263646566", true},
		{"base64 std", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", true},
		{"base64 raw url", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY", true},
		{"too short", "short", false},
		{"empty", "", false},
		{"33 bytes", raw + "x", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := auth.ParseKey(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if tc.ok && string(b) != raw {
				t.Fatalf("decoded key mismatch")
			}
		})
	}
}

func TestSessionCookie(t *testing.T) {
	now := time.Now()
	u := auth.User{Subject: "s1", Email: "a@b.c", Username: "al", Picture: "https://x/p.png", Groups: []string{"admin"}}
	c, err := auth.NewSessionCookie(testKey, u, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.Path != "/" || c.Name != auth.CookieName {
		t.Errorf("bad cookie attributes: %+v", c)
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || bytes.Contains(raw, []byte("a@b.c")) || bytes.Contains(raw, []byte(`"sub"`)) {
		t.Error("cookie value is not encrypted")
	}

	tests := []struct {
		name  string
		key   []byte
		value string
		at    time.Time
		ok    bool
	}{
		{"valid", testKey, c.Value, now, true},
		{"expired", testKey, c.Value, now.Add(2 * time.Hour), false},
		{"wrong key", []byte("ffffffffffffffffffffffffffffffff"), c.Value, now, false},
		{"tampered", testKey, c.Value[:len(c.Value)-2] + "AA", now, false},
		{"garbage", testKey, "!!!", now, false},
		{"empty", testKey, "", now, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := auth.DecodeSession(tc.key, tc.value, tc.at)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if tc.ok && (got.Subject != "s1" || got.Email != "a@b.c" || !got.IsAdmin() || got.Username != "al" || got.Picture != u.Picture) {
				t.Errorf("user = %+v", got)
			}
		})
	}
	if _, err := auth.NewSessionCookie(testKey, auth.User{}, 0, now); err == nil {
		t.Error("expected error for user without subject")
	}
}

func TestNewValidates(t *testing.T) {
	ok := auth.OIDCConfig{Issuer: "https://i", ClientID: "c", PublicURL: "https://app.example"}
	tests := []struct {
		name string
		cfg  auth.OIDCConfig
		key  []byte
		ok   bool
	}{
		{"ok", ok, testKey, true},
		{"no issuer", auth.OIDCConfig{ClientID: "c", PublicURL: "https://a"}, testKey, false},
		{"no client", auth.OIDCConfig{Issuer: "i", PublicURL: "https://a"}, testKey, false},
		{"relative public url", auth.OIDCConfig{Issuer: "i", ClientID: "c", PublicURL: "app"}, testKey, false},
		{"short key", ok, []byte("short"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.New(tc.cfg, tc.key)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

type env struct {
	idp  *fakeIDP
	auth *auth.Auth
	h    http.Handler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	idp := newFakeIDP(t)
	idp.claims["user-1"] = map[string]any{"email": "ada@teb.ooo", "preferred_username": "ada",
		"picture": "https://id.teb.ooo/avatar/user-1", "groups": []string{"admin"}}
	idp.claims["user-2"] = map[string]any{"email": "bob@teb.ooo", "preferred_username": "bob"}
	a, err := auth.New(auth.OIDCConfig{Issuer: idp.srv.URL, ClientID: "app", ClientSecret: "shh",
		PublicURL: "https://app.example"}, testKey, auth.WithRateLimit(ratelimit.Options{Burst: 1000, Requests: 1000}))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	a.Register(api, mux)
	huma.Register(api, huma.Operation{OperationID: "needs-user", Method: "GET", Path: "/api/x", Summary: "s", Description: "d"},
		func(ctx context.Context, _ *struct{}) (*struct{ Body string }, error) {
			u, err := auth.Require(ctx)
			if err != nil {
				return nil, err
			}
			return &struct{ Body string }{u.Subject}, nil
		})
	return &env{idp: idp, auth: a, h: a.Middleware(mux)}
}

func (e *env) do(method, target string, cookies []*http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// login runs /auth/login and returns the login cookie, the authorize URL query.
func (e *env) login(t *testing.T, next string) (*http.Cookie, url.Values) {
	t.Helper()
	target := "/auth/login"
	if next != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	w := e.do("GET", target, nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("login status = %d body=%s", w.Code, w.Body)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if !strings.HasPrefix(loc.String(), e.idp.srv.URL+"/authorize") {
		t.Fatalf("redirect to %s", loc)
	}
	lc := cookieNamed(w, auth.LoginCookieName)
	if lc == nil || !lc.HttpOnly || !lc.Secure || lc.Domain != "" {
		t.Fatalf("login cookie = %+v", lc)
	}
	return lc, loc.Query()
}

func TestLoginRedirectParams(t *testing.T) {
	e := newEnv(t)
	_, q := e.login(t, "/items")
	tests := map[string]string{
		"response_type": "code", "client_id": "app", "redirect_uri": "https://app.example/auth/callback",
		"code_challenge_method": "S256",
	}
	for k, want := range tests {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if q.Get(k) == "" {
			t.Errorf("missing %s", k)
		}
	}
	for _, s := range []string{"openid", "email", "profile", "groups"} {
		if !strings.Contains(q.Get("scope"), s) {
			t.Errorf("scope %q missing %s", q.Get("scope"), s)
		}
	}
}

func (e *env) issueCode(q url.Values, sub string) string {
	code := "code-" + q.Get("state")
	e.idp.mu.Lock()
	e.idp.codes[code] = codeInfo{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), sub: sub, clientID: "app"}
	e.idp.mu.Unlock()
	return code
}

func TestFullFlow(t *testing.T) {
	e := newEnv(t)
	lc, q := e.login(t, "/items?x=1")
	code := e.issueCode(q, "user-1")

	w := e.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), []*http.Cookie{lc}, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/items?x=1" {
		t.Fatalf("callback = %d loc=%q body=%s", w.Code, w.Header().Get("Location"), w.Body)
	}
	sc := cookieNamed(w, auth.CookieName)
	if sc == nil {
		t.Fatal("no session cookie")
	}
	if !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteLaxMode || sc.Domain != "" {
		t.Errorf("session cookie attrs: %+v", sc)
	}
	if lcc := cookieNamed(w, auth.LoginCookieName); lcc == nil || lcc.MaxAge >= 0 {
		t.Error("login cookie not cleared")
	}

	w = e.do("GET", "/auth/me", []*http.Cookie{sc}, nil)
	if w.Code != 200 {
		t.Fatalf("me = %d %s", w.Code, w.Body)
	}
	var me map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"subject": "user-1", "email": "ada@teb.ooo", "username": "ada",
		"picture": "https://id.teb.ooo/avatar/user-1", "is_admin": true}
	for k, v := range want {
		if me[k] != v {
			t.Errorf("me[%s] = %v, want %v", k, me[k], v)
		}
	}
	if g, _ := me["groups"].([]any); len(g) != 1 || g[0] != "admin" {
		t.Errorf("groups = %v", me["groups"])
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("me should not be cacheable")
	}

	// Code cannot be replayed.
	w = e.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), []*http.Cookie{lc}, nil)
	if w.Code < 400 {
		t.Errorf("replayed code accepted: %d", w.Code)
	}
}

func TestNonAdminGroupsEmptyArray(t *testing.T) {
	e := newEnv(t)
	lc, q := e.login(t, "")
	code := e.issueCode(q, "user-2")
	w := e.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), []*http.Cookie{lc}, nil)
	if w.Header().Get("Location") != "/" {
		t.Errorf("default next = %q", w.Header().Get("Location"))
	}
	w = e.do("GET", "/auth/me", []*http.Cookie{cookieNamed(w, auth.CookieName)}, nil)
	if !strings.Contains(w.Body.String(), `"groups":[]`) || !strings.Contains(w.Body.String(), `"is_admin":false`) {
		t.Errorf("body = %s", w.Body)
	}
}

func TestCallbackRejections(t *testing.T) {
	e := newEnv(t)
	lc, q := e.login(t, "")
	code := e.issueCode(q, "user-1")
	state := url.QueryEscape(q.Get("state"))

	tests := []struct {
		name    string
		target  string
		cookies []*http.Cookie
	}{
		{"no login cookie", "/auth/callback?code=" + code + "&state=" + state, nil},
		{"wrong state", "/auth/callback?code=" + code + "&state=nope", []*http.Cookie{lc}},
		{"provider error", "/auth/callback?error=access_denied&state=" + state, []*http.Cookie{lc}},
		{"no code", "/auth/callback?state=" + state, []*http.Cookie{lc}},
		{"bad code", "/auth/callback?code=bogus&state=" + state, []*http.Cookie{lc}},
		{"tampered login cookie", "/auth/callback?code=" + code + "&state=" + state, []*http.Cookie{{Name: auth.LoginCookieName, Value: lc.Value + "x"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := e.do("GET", tc.target, tc.cookies, nil)
			if w.Code < 400 || cookieNamed(w, auth.CookieName) != nil {
				t.Fatalf("status %d, session cookie issued=%v", w.Code, cookieNamed(w, auth.CookieName) != nil)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("content-type = %q", ct)
			}
		})
	}
}

func TestPKCEIsEnforced(t *testing.T) {
	// A code minted against a different challenge must not exchange.
	e := newEnv(t)
	lc, q := e.login(t, "")
	q.Set("code_challenge", "AAAA")
	code := e.issueCode(q, "user-1")
	w := e.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), []*http.Cookie{lc}, nil)
	if w.Code < 400 {
		t.Fatalf("callback succeeded with wrong PKCE challenge: %d", w.Code)
	}
}

func TestNextSanitized(t *testing.T) {
	e := newEnv(t)
	tests := []struct{ next, want string }{
		{"/ok/path?a=b", "/ok/path?a=b"},
		{"//evil.example", "/"},
		{"https://evil.example", "/"},
		{`/\evil.example`, "/"},
		{"", "/"},
	}
	for _, tc := range tests {
		t.Run(tc.next, func(t *testing.T) {
			lc, q := e.login(t, tc.next)
			code := e.issueCode(q, "user-2")
			w := e.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), []*http.Cookie{lc}, nil)
			if got := w.Header().Get("Location"); got != tc.want {
				t.Errorf("redirect = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMeAndRequireUnauthenticated(t *testing.T) {
	e := newEnv(t)
	for _, target := range []string{"/auth/me", "/api/x"} {
		w := e.do("GET", target, nil, nil)
		if w.Code != 401 {
			t.Fatalf("%s = %d", target, w.Code)
		}
		var p map[string]any
		json.Unmarshal(w.Body.Bytes(), &p)
		if p["status"] != float64(401) || p["title"] == nil {
			t.Errorf("%s: not problem+json: %s", target, w.Body)
		}
	}
	// hidden from the OpenAPI document (and thus from MCP)
	w := e.do("GET", "/openapi.json", nil, nil)
	if strings.Contains(w.Body.String(), "get-current-user") {
		t.Error("get-current-user must not be in the OpenAPI document")
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	for _, m := range []string{"GET", "POST"} {
		w := e.do(m, "/auth/logout?next=/bye", nil, nil)
		c := cookieNamed(w, auth.CookieName)
		if w.Code != http.StatusSeeOther || c == nil || c.MaxAge >= 0 || w.Header().Get("Location") != "/bye" {
			t.Errorf("%s logout: %d %+v loc=%s", m, w.Code, c, w.Header().Get("Location"))
		}
	}
}

func TestBearer(t *testing.T) {
	e := newEnv(t)
	good := e.idp.accessToken("user-1", time.Now().Add(time.Hour))
	expired := e.idp.accessToken("user-1", time.Now().Add(-time.Hour))
	forged := good[:len(good)-4] + "AAAA"
	otherIssuer := func() string {
		other := newFakeIDP(t)
		other.srv.URL = e.idp.srv.URL // sign with a foreign key but claim our issuer
		return other.accessToken("user-1", time.Now().Add(time.Hour))
	}()

	tests := []struct {
		name          string
		token         string
		wantSubject   string
		wantAdmin     bool
		wantRejection bool
	}{
		{"valid jwt is enriched from userinfo", good, "user-1", true, false},
		{"expired", expired, "", false, true},
		{"forged signature", forged, "", false, true},
		{"foreign key", otherIssuer, "", false, true},
		{"opaque unknown", "opaque-token", "", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{"Authorization": "Bearer " + tc.token}
			// Middleware: anonymous on failure.
			w := e.do("GET", "/api/x", nil, hdr)
			if tc.wantRejection && w.Code != 401 {
				t.Errorf("middleware: code = %d", w.Code)
			}
			if !tc.wantRejection && (w.Code != 200 || !strings.Contains(w.Body.String(), tc.wantSubject)) {
				t.Errorf("middleware: %d %s", w.Code, w.Body)
			}
			// BearerOrSession: explicit 401 + WWW-Authenticate.
			bw := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/x", nil)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			var seen auth.User
			var ok bool
			e.auth.BearerOrSession(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen, ok = auth.FromContext(r.Context())
			})).ServeHTTP(bw, r)
			if tc.wantRejection {
				if bw.Code != 401 || bw.Header().Get("WWW-Authenticate") == "" {
					t.Errorf("BearerOrSession: %d %v", bw.Code, bw.Header())
				}
			} else if !ok || seen.Subject != tc.wantSubject || seen.IsAdmin() != tc.wantAdmin || seen.Email != "ada@teb.ooo" {
				t.Errorf("BearerOrSession user = %+v ok=%v", seen, ok)
			}
		})
	}
}

func TestBearerCachesUserinfo(t *testing.T) {
	e := newEnv(t)
	tok := e.idp.accessToken("user-1", time.Now().Add(time.Hour))
	for i := 0; i < 3; i++ {
		if w := e.do("GET", "/api/x", nil, map[string]string{"Authorization": "Bearer " + tok}); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if e.idp.userinfoCalls != 1 {
		t.Errorf("userinfo calls = %d, want 1", e.idp.userinfoCalls)
	}
}

func TestBearerUserinfoFailureIsNotAdmin(t *testing.T) {
	e := newEnv(t)
	e.idp.failUserinfo = true
	tok := e.idp.accessToken("user-1", time.Now().Add(time.Hour))
	var u auth.User
	var ok bool
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	e.auth.BearerOrSession(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		u, ok = auth.FromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), r)
	if !ok || u.Subject != "user-1" || u.IsAdmin() {
		t.Errorf("user = %+v ok=%v; want subject only, not admin", u, ok)
	}
}

func TestCookieBeatsInvalidBearerInBearerOrSession(t *testing.T) {
	e := newEnv(t)
	c, err := auth.NewSessionCookie(testKey, auth.User{Subject: "s"}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/x", nil)
	r.AddCookie(c)
	r.Header.Set("Authorization", "Bearer junk")
	var ok bool
	e.auth.BearerOrSession(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, ok = auth.FromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), r)
	if !ok {
		t.Error("valid session cookie should authenticate despite a junk bearer token")
	}
}

func TestRequireAdmin(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want int
	}{
		{"anonymous", context.Background(), 401},
		{"member", auth.WithUser(context.Background(), auth.User{Subject: "a"}), 403},
		{"admin", auth.WithUser(context.Background(), auth.User{Subject: "a", Groups: []string{"admin"}}), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.RequireAdmin(tc.ctx)
			got := 0
			if err != nil {
				se, ok := err.(huma.StatusError)
				if !ok {
					t.Fatalf("error %T is not a huma.StatusError", err)
				}
				got = se.GetStatus()
			}
			if got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDiscoveryFailureIsBadGateway(t *testing.T) {
	a, err := auth.New(auth.OIDCConfig{Issuer: "http://127.0.0.1:1", ClientID: "c", PublicURL: "https://a.example"}, testKey,
		auth.WithHTTPClient(&http.Client{Timeout: time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(humago.New(mux, huma.DefaultConfig("t", "1")), mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/login", nil))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d", w.Code)
	}
}

func TestRateLimitOnLoginAndCallback(t *testing.T) {
	idp := newFakeIDP(t)
	clk := time.Now()
	a, err := auth.New(auth.OIDCConfig{Issuer: idp.srv.URL, ClientID: "app", ClientSecret: "shh", PublicURL: "https://app.example"},
		testKey, auth.WithClock(func() time.Time { return clk }))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(humago.New(mux, huma.DefaultConfig("t", "1")), mux)
	do := func(path, remote, xff string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	// Default burst of 5 per client on login...
	for i := 0; i < 5; i++ {
		if w := do("/auth/login", "172.18.0.9:1", "198.51.100.7"); w.Code != http.StatusFound {
			t.Fatalf("login %d = %d", i, w.Code)
		}
	}
	w := do("/auth/login", "172.18.0.9:1", "198.51.100.7")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "6" || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("6th login = %d retry=%q ct=%q", w.Code, w.Header().Get("Retry-After"), w.Header().Get("Content-Type"))
	}
	// ...keyed by the last X-Forwarded-For hop, so another client is fine,
	if w := do("/auth/login", "172.18.0.9:1", "198.51.100.8"); w.Code != http.StatusFound {
		t.Errorf("other client behind proxy = %d", w.Code)
	}
	// and a client-supplied first hop does not reset the bucket.
	if w := do("/auth/login", "172.18.0.9:1", "9.9.9.9, 198.51.100.7"); w.Code != http.StatusTooManyRequests {
		t.Errorf("spoofed first hop = %d", w.Code)
	}
	// The callback has its own bucket.
	for i := 0; i < 5; i++ {
		if w := do("/auth/callback?state=x", "172.18.0.9:1", "198.51.100.7"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("callback %d limited too early", i)
		}
	}
	if w := do("/auth/callback?state=x", "172.18.0.9:1", "198.51.100.7"); w.Code != http.StatusTooManyRequests {
		t.Errorf("6th callback = %d", w.Code)
	}
	// Logout is not limited.
	for i := 0; i < 8; i++ {
		if w := do("/auth/logout", "172.18.0.9:1", "198.51.100.7"); w.Code != http.StatusSeeOther {
			t.Fatalf("logout %d = %d", i, w.Code)
		}
	}
	// Tokens come back with time.
	clk = clk.Add(6 * time.Second)
	if w := do("/auth/login", "172.18.0.9:1", "198.51.100.7"); w.Code != http.StatusFound {
		t.Errorf("login after refill = %d", w.Code)
	}
}

func TestRateLimitConfigurableAndDisableable(t *testing.T) {
	idp := newFakeIDP(t)
	cfg := auth.OIDCConfig{Issuer: idp.srv.URL, ClientID: "app", PublicURL: "https://app.example"}
	tests := []struct {
		name string
		opt  auth.Option
		want int // status of the 3rd login
	}{
		{"burst 2", auth.WithRateLimit(ratelimit.Options{Burst: 2}), http.StatusTooManyRequests},
		{"burst 3", auth.WithRateLimit(ratelimit.Options{Burst: 3}), http.StatusFound},
		{"disabled", auth.WithoutRateLimit(), http.StatusFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := auth.New(cfg, testKey, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			a.Register(humago.New(mux, huma.DefaultConfig("t", "1")), mux)
			var got int
			for i := 0; i < 3; i++ {
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/login", nil))
				got = w.Code
			}
			if got != tc.want {
				t.Errorf("3rd login = %d, want %d", got, tc.want)
			}
		})
	}
}

// Every cookie the package sets must be Secure, HttpOnly, SameSite=Lax and
// host-only, whether it sets a value or clears one.
func TestAllCookiesHaveSafeAttributes(t *testing.T) {
	e := newEnv(t)
	var cookies []*http.Cookie

	lc, q := e.login(t, "/x")
	w := e.do("GET", "/auth/login", nil, nil)
	cookies = append(cookies, w.Result().Cookies()...)

	code := e.issueCode(q, "user-1")
	w = e.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), []*http.Cookie{lc}, nil)
	cookies = append(cookies, w.Result().Cookies()...) // session cookie + cleared login cookie

	cookies = append(cookies, e.do("GET", "/auth/logout", nil, nil).Result().Cookies()...) // cleared session cookie

	sess, err := auth.NewSessionCookie(testKey, auth.User{Subject: "s"}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cookies = append(cookies, sess)

	if len(cookies) < 5 {
		t.Fatalf("expected at least 5 cookies to check, got %d", len(cookies))
	}
	for _, c := range cookies {
		if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.Path == "" {
			t.Errorf("cookie %s has unsafe attributes: secure=%v httponly=%v samesite=%v domain=%q path=%q",
				c.Name, c.Secure, c.HttpOnly, c.SameSite, c.Domain, c.Path)
		}
		// And on the wire.
		line := c.String()
		for _, want := range []string{"Secure", "HttpOnly", "SameSite=Lax"} {
			if !strings.Contains(line, want) {
				t.Errorf("Set-Cookie %q lacks %s", line, want)
			}
		}
		if strings.Contains(line, "Domain=") {
			t.Errorf("Set-Cookie %q must be host-only", line)
		}
	}
}
