package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/ratelimit"
	"github.com/teb-ooo/playground-go/surface"
)

func TestHasScope(t *testing.T) {
	u := auth.User{Scopes: []string{"notes:read"}}
	if !u.HasScope("notes:read") || u.HasScope("notes:write") || (auth.User{}).HasScope("notes:read") {
		t.Fatal("HasScope")
	}
}

func TestOperationScope(t *testing.T) {
	sec := []map[string][]string{{"bearer": {}}}
	cases := []struct {
		method string
		op     *huma.Operation
		scope  string
		secure bool
	}{
		{"GET", &huma.Operation{Security: sec}, "read", true},
		{"HEAD", &huma.Operation{Security: sec}, "read", true},
		{"POST", &huma.Operation{Security: sec}, "write", true},
		{"PUT", &huma.Operation{Security: sec}, "write", true},
		{"PATCH", &huma.Operation{Security: sec}, "write", true},
		{"DELETE", &huma.Operation{Security: sec}, "write", true},
		{"GET", &huma.Operation{Security: sec, Extensions: map[string]any{"x-scope": "admin"}}, "admin", true},
		{"POST", &huma.Operation{Security: sec, Extensions: map[string]any{"x-scope": "read"}}, "read", true},
		{"GET", &huma.Operation{}, "", false},
		{"GET", nil, "", false},
	}
	for _, c := range cases {
		if s, ok := auth.OperationScope(c.method, c.op); s != c.scope || ok != c.secure {
			t.Errorf("%s %+v = %q %v", c.method, c.op, s, ok)
		}
	}
}

func TestScopesEnforcedRules(t *testing.T) {
	ctx := func(c auth.Credential, scopes []string) context.Context {
		return auth.WithApp(auth.WithCredential(auth.WithUser(context.Background(), auth.User{Subject: "u", Scopes: scopes}), c), "notes")
	}
	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"nobody", context.Background(), false},
		{"session", ctx(auth.CredentialSession, nil), false},
		{"session even with scopes", ctx(auth.CredentialSession, []string{"x:read"}), false},
		{"key", ctx(auth.CredentialKey, []string{"notes:read"}), true},
		{"key without scopes", ctx(auth.CredentialKey, nil), true},
		{"bearer without app scopes", ctx(auth.CredentialBearer, nil), false},
		{"bearer with scopes", ctx(auth.CredentialBearer, []string{"notes:read"}), true},
	}
	for _, c := range cases {
		if got := auth.ScopesEnforced(c.ctx); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	// ScopeAllowed / RequireScope
	k := ctx(auth.CredentialKey, []string{"notes:read"})
	if !auth.ScopeAllowed(k, "read") || auth.ScopeAllowed(k, "write") || auth.RequireScope(k, "write") == nil || auth.RequireScope(k, "read") != nil {
		t.Error("ScopeAllowed/RequireScope")
	}
	if auth.RequireScope(ctx(auth.CredentialSession, nil), "admin") != nil || auth.RequireScope(context.Background(), "admin") != nil {
		t.Error("unrestricted callers must pass")
	}
	// A restricted caller in a context without an app name fails closed.
	noApp := auth.WithCredential(auth.WithUser(context.Background(), auth.User{Scopes: []string{"notes:admin"}}), auth.CredentialKey)
	if auth.ScopeAllowed(noApp, "admin") {
		t.Error("no app name must fail closed")
	}
}

func TestKeyVerifierRouting(t *testing.T) {
	e := newEnv(t)
	var gotKey []string
	kv := func(_ *http.Request, tok string) (auth.User, bool, error) {
		gotKey = append(gotKey, tok)
		switch tok {
		case "pk_good":
			return auth.User{Subject: "k", Scopes: []string{"app:read"}}, true, nil
		case "pk_unavailable":
			return auth.User{}, false, &auth.UnavailableError{Err: errors.New("keys: revocation list unavailable")}
		}
		return auth.User{}, false, nil
	}
	a, err := auth.New(auth.OIDCConfig{Issuer: e.idp.srv.URL, ClientID: "app", PublicURL: "https://app.example"}, testKey,
		auth.WithRateLimit(ratelimit.Options{Burst: 1000, Requests: 1000}),
		auth.WithAppName("app"), auth.WithKeyVerifier(kv))
	if err != nil {
		t.Fatal(err)
	}
	h, s := probe(a.Middleware)
	do := func(tok string) {
		*s = seen{}
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	do("pk_good")
	if s.cred != auth.CredentialKey || s.user.Subject != "k" || !s.user.HasScope("app:read") {
		t.Fatalf("key: %+v", s)
	}
	if len(gotKey) != 1 {
		t.Fatalf("routing: key=%v", gotKey)
	}
	do("pk_bad")
	if s.ok {
		t.Fatal("bad key accepted")
	}

	// BearerOrSession answers: 401 bad, 503 unavailable.
	for tok, want := range map[string]int{"pk_bad": 401, "pk_unavailable": 503} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		a.BearerOrSession(http.NotFoundHandler()).ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: %d", tok, w.Code)
		}
	}

	// A key is an AI caller on every path.
	var sf surface.Surface
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer pk_good")
	surface.Middleware(a.BearerOrSession(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sf = surface.From(r.Context())
	}))).ServeHTTP(httptest.NewRecorder(), r)
	if !sf.IsAI() || sf != surface.Key {
		t.Fatalf("surface = %s", sf)
	}
	// A key presented through MCP stays MCP.
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer pk_good")
	r = r.WithContext(surface.With(r.Context(), surface.MCP))
	a.BearerOrSession(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sf = surface.From(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), r)
	if sf != surface.MCP {
		t.Fatalf("surface = %s", sf)
	}
}

func TestKeyVerifierNeedsAppName(t *testing.T) {
	_, err := auth.New(auth.OIDCConfig{Issuer: "https://x", ClientID: "a", PublicURL: "https://a.example"}, testKey,
		auth.WithKeyVerifier(func(*http.Request, string) (auth.User, bool, error) { return auth.User{}, false, nil }))
	if err == nil {
		t.Fatal("want an error without WithAppName")
	}
}

// Hydra JWT access tokens: scp (array or string) restricts only when it holds app scopes.
func TestBearerScopes(t *testing.T) {
	e := newEnv(t)
	a, err := auth.New(auth.OIDCConfig{Issuer: e.idp.srv.URL, ClientID: "app", PublicURL: "https://app.example"}, testKey,
		auth.WithAppName("app"), auth.WithoutRateLimit())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	a.Register(api, mux)
	sec := []map[string][]string{{"bearer": {}}}
	for _, m := range []string{"GET", "POST"} {
		huma.Register(api, huma.Operation{OperationID: "op-" + m, Method: m, Path: "/api/s", Summary: "s", Description: "d", Security: sec},
			func(ctx context.Context, _ *struct{}) (*struct{ Body string }, error) {
				return &struct{ Body string }{"ok"}, nil
			})
	}
	h := a.BearerOrSession(mux)
	tokenWith := func(scp any, key string) string {
		c := map[string]any{"iss": e.idp.srv.URL, "sub": "user-2", "aud": []string{"x"},
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
		if scp != nil {
			c[key] = scp
		}
		return e.idp.sign(c)
	}
	do := func(method, tok string) int {
		r := httptest.NewRequest(method, "/api/s", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	cases := []struct {
		name      string
		tok       string
		get, post int
	}{
		{"no scp: refused (no scope for this app)", tokenWith(nil, "scp"), 401, 401},
		{"openid only: refused", tokenWith([]string{"openid", "email"}, "scp"), 401, 401},
		{"aud names this app, no scopes: accepted", e.idp.accessTokenFor("user-2", []string{"openid"}, []string{"app"}), 200, 200},
		{"array with read", tokenWith([]string{"openid", "app:read"}, "scp"), 200, 403},
		{"string scp with read+write", tokenWith("openid app:read app:write", "scp"), 200, 200},
		{"scope claim string", tokenWith("app:read", "scope"), 200, 403},
		{"another app's scopes only: refused", tokenWith([]string{"other:read", "other:write"}, "scp"), 401, 401},
		{"another app's name as prefix: refused", tokenWith([]string{"app-two:read"}, "scp"), 401, 401},
	}
	for _, c := range cases {
		if got := do("GET", c.tok); got != c.get {
			t.Errorf("%s GET = %d, want %d", c.name, got, c.get)
		}
		if got := do("POST", c.tok); got != c.post {
			t.Errorf("%s POST = %d, want %d", c.name, got, c.post)
		}
	}
}

// B1: a bearer token must be meant for this app, at an operation, through the
// middleware, and for opaque tokens (whose scopes can only come from userinfo).
func TestBearerMustBeForThisApp(t *testing.T) {
	e := newEnv(t)
	e.idp.opaque = map[string]map[string]any{
		"opaque-none":  {"sub": "user-2", "email": "bob@teb.ooo"},
		"opaque-other": {"sub": "user-2", "scope": "other:read"},
		"opaque-read":  {"sub": "user-2", "email": "bob@teb.ooo", "scope": "openid app:read"},
		"opaque-aud":   {"sub": "user-2", "aud": "app"},
	}
	a, err := auth.New(auth.OIDCConfig{Issuer: e.idp.srv.URL, ClientID: "app", PublicURL: "https://app.example"}, testKey,
		auth.WithAppName("app"), auth.WithoutRateLimit())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	a.Register(api, mux)
	huma.Register(api, huma.Operation{OperationID: "op-get", Method: "GET", Path: "/api/s", Summary: "s", Description: "d",
		Security: []map[string][]string{{"bearer": {}}}},
		func(ctx context.Context, _ *struct{}) (*struct{ Body string }, error) {
			return &struct{ Body string }{"ok"}, nil
		})
	op := a.BearerOrSession(mux)
	jwt := func(scp, aud []string) string { return e.idp.accessTokenFor("user-2", scp, aud) }
	cases := []struct {
		name string
		tok  string
		want int
	}{
		{"jwt openid email", jwt([]string{"openid", "email"}, []string{"some-api"}), 401},
		{"jwt app:read", jwt([]string{"openid", "app:read"}, []string{"some-api"}), 200},
		{"jwt aud only", jwt([]string{"openid"}, []string{"x", "app"}), 200},
		{"opaque no scopes", "opaque-none", 401},
		{"opaque other app", "opaque-other", 401},
		{"opaque app:read", "opaque-read", 200},
		{"opaque aud", "opaque-aud", 200},
	}
	for _, c := range cases {
		// An operation.
		r := httptest.NewRequest("GET", "/api/s", nil)
		r.Header.Set("Authorization", "Bearer "+c.tok)
		w := httptest.NewRecorder()
		op.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s operation = %d, want %d", c.name, w.Code, c.want)
		}
		if c.want == 401 && w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate", c.name)
		}
		// The middleware never rejects but must not identify the caller.
		var ok bool
		r = httptest.NewRequest("GET", "/x", nil)
		r.Header.Set("Authorization", "Bearer "+c.tok)
		a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, ok = auth.FromContext(r.Context())
		})).ServeHTTP(httptest.NewRecorder(), r)
		if ok != (c.want == 200) {
			t.Errorf("%s middleware identified = %v", c.name, ok)
		}
	}
	// Without an app name nothing can be checked: as before, a token with
	// only openid is accepted.
	plain, err := auth.New(auth.OIDCConfig{Issuer: e.idp.srv.URL, ClientID: "app", PublicURL: "https://app.example"}, testKey, auth.WithoutRateLimit())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+jwt([]string{"openid"}, nil))
	w := httptest.NewRecorder()
	plain.BearerOrSession(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("no app name: %d", w.Code)
	}
}
