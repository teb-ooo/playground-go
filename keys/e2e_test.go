package keys

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/openapimcp"
	"github.com/teb-ooo/playground-go/surface"
)

var sessionKey = []byte("0123456789abcdef0123456789abcdef")

type app struct {
	*env
	top http.Handler // surface + BearerOrSession + mux, like the template
	mcp http.Handler // the app's /mcp, with the metadata
	pub string
}

type who struct {
	Subject string   `json:"subject"`
	Admin   bool     `json:"admin"`
	Cred    string   `json:"cred"`
	Surface string   `json:"surface"`
	Groups  []string `json:"groups"`
}

func newApp(t *testing.T) *app {
	e := newEnv(t)
	a, err := auth.New(auth.OIDCConfig{Issuer: "https://oidc.invalid", ClientID: "notes", PublicURL: "https://notes.example"}, sessionKey,
		auth.WithAppName("notes"), auth.WithKeyVerifier(e.v.Verify), auth.WithoutRateLimit())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("notes", "1"))
	a.Register(api, mux) // before the operations: Huma captures middleware at registration
	sec := []map[string][]string{{"session": {}}, {"bearer": {}}}
	type out struct{ Body string }
	huma.Register(api, huma.Operation{OperationID: "list-notes", Method: "GET", Path: "/api/notes", Summary: "List", Description: "d", Security: sec},
		func(ctx context.Context, _ *struct{}) (*out, error) { return &out{"notes"}, nil })
	huma.Register(api, huma.Operation{OperationID: "create-note", Method: "POST", Path: "/api/notes", Summary: "Create", Description: "d", Security: sec},
		func(ctx context.Context, _ *struct{}) (*out, error) { return &out{"created"}, nil })
	huma.Register(api, huma.Operation{OperationID: "purge-notes", Method: "DELETE", Path: "/api/purge", Summary: "Purge", Description: "d", Security: sec,
		Extensions: map[string]any{"x-scope": "admin"}},
		func(ctx context.Context, _ *struct{}) (*out, error) {
			if _, err := auth.RequireAdmin(ctx); err != nil {
				return nil, err
			}
			return &out{"purged"}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "export-notes", Method: "GET", Path: "/api/export", Summary: "Export", Description: "d", Security: sec,
		Extensions: map[string]any{"x-scope": "write"}},
		func(ctx context.Context, _ *struct{}) (*out, error) { return &out{"export"}, nil })
	huma.Register(api, huma.Operation{OperationID: "handler-scope", Method: "GET", Path: "/api/hs", Summary: "HS", Description: "d", Security: sec},
		func(ctx context.Context, _ *struct{}) (*out, error) {
			if err := auth.RequireScope(ctx, "admin"); err != nil {
				return nil, err
			}
			return &out{"ok"}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "public-info", Method: "GET", Path: "/api/public", Summary: "Public", Description: "d"},
		func(ctx context.Context, _ *struct{}) (*out, error) { return &out{"public"}, nil })
	huma.Register(api, huma.Operation{OperationID: "whoami", Method: "GET", Path: "/api/whoami", Summary: "Who", Description: "d", Security: sec},
		func(ctx context.Context, _ *struct{}) (*struct{ Body who }, error) {
			u, _ := auth.FromContext(ctx)
			return &struct{ Body who }{who{u.Subject, u.IsAdmin(), string(auth.CredentialFromContext(ctx)), surface.From(ctx).String(), u.Groups}}, nil
		})

	s, err := openapimcp.New(api, mux, openapimcp.Options{Auth: a.BearerOrSession, PublicURL: "https://notes.example", App: "notes",
		AuthorizationServer: "https://oidc.teb.ooo"})
	if err != nil {
		t.Fatal(err)
	}
	mmux := http.NewServeMux()
	mmux.Handle("/mcp", s)
	s.RegisterMetadata(mmux)
	return &app{env: e, top: surface.Middleware(a.BearerOrSession(mux)), mcp: surface.Middleware(mmux), pub: "https://notes.example"}
}

func (a *app) key(scp string, groups ...string) string {
	c := a.claims()
	c["scp"] = scp
	if len(groups) > 0 {
		c["groups"] = groups
	}
	return a.sign("k1", c)
}

func (a *app) call(method, path, tok string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "10.0.0.1:1"
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	a.top.ServeHTTP(w, r)
	return w
}

func (a *app) session(t *testing.T, groups ...string) *http.Cookie {
	c, err := auth.NewSessionCookie(sessionKey, auth.User{Subject: "u-1", Groups: groups}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestScopeMatrix(t *testing.T) {
	a := newApp(t)
	ro, rw := a.key("notes:read"), a.key("notes:read notes:write")
	adminKey := a.key("notes:read notes:write notes:admin", "admin")
	scopeAdminNotOwner := a.key("notes:admin") // admin scope, but the owner is no admin
	otherApp := a.key("bd:read bd:write")
	none := a.key("")

	tests := []struct {
		name, method, path, tok string
		want                    int
	}{
		{"read key GET", "GET", "/api/notes", ro, 200},
		{"read key POST", "POST", "/api/notes", ro, 403},
		{"write key POST", "POST", "/api/notes", rw, 200},
		{"write only key GET is refused", "GET", "/api/notes", a.key("notes:write"), 403},
		{"x-scope write on GET", "GET", "/api/export", ro, 403},
		{"x-scope write on GET ok", "GET", "/api/export", rw, 200},
		{"x-scope admin without it", "DELETE", "/api/purge", rw, 403},
		{"admin does not bypass scope", "DELETE", "/api/purge", a.key("notes:read notes:write", "admin"), 403},
		{"admin scope and admin group", "DELETE", "/api/purge", adminKey, 200},
		{"admin scope but no admin group", "DELETE", "/api/purge", scopeAdminNotOwner, 403},
		{"other app's scopes", "GET", "/api/notes", otherApp, 403},
		{"no scopes", "GET", "/api/notes", none, 403},
		{"public op needs no scope", "GET", "/api/public", none, 200},
		{"handler RequireScope denied", "GET", "/api/hs", ro, 403},
		{"handler RequireScope allowed", "GET", "/api/hs", a.key("notes:read notes:admin"), 200},
		{"anonymous reaches handler", "GET", "/api/notes", "", 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := a.call(tc.method, tc.path, tc.tok)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}

	// The 403 names the missing scope, as problem JSON.
	w := a.call("POST", "/api/notes", ro)
	if !strings.Contains(w.Body.String(), "missing scope notes:write") || !strings.Contains(w.Header().Get("Content-Type"), "problem+json") {
		t.Fatalf("403 = %s %v", w.Body, w.Header())
	}
	w = a.call("DELETE", "/api/purge", rw)
	if !strings.Contains(w.Body.String(), "notes:admin") {
		t.Fatalf("403 = %s", w.Body)
	}
}

func TestSessionIsUnrestricted(t *testing.T) {
	a := newApp(t)
	for _, tc := range []struct{ method, path string }{{"GET", "/api/notes"}, {"POST", "/api/notes"}, {"GET", "/api/export"}, {"GET", "/api/hs"}} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.AddCookie(a.session(t))
		w := httptest.NewRecorder()
		a.top.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	// Session admin may purge without any scope.
	r := httptest.NewRequest("DELETE", "/api/purge", nil)
	r.AddCookie(a.session(t, "admin"))
	w := httptest.NewRecorder()
	a.top.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("admin session purge: %d", w.Code)
	}
}

func TestKeyIdentityAndSurface(t *testing.T) {
	a := newApp(t)
	w := a.call("GET", "/api/whoami", a.key("notes:read", "admin"))
	var got who
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got.Subject != "u-1" || !got.Admin || got.Cred != "key" || got.Surface != "key" || !surface.Surface(got.Surface).IsAI() {
		t.Fatalf("%+v", got)
	}
	// A session is not an AI caller.
	r := httptest.NewRequest("GET", "/api/whoami", nil)
	r.AddCookie(a.session(t))
	rec := httptest.NewRecorder()
	a.top.ServeHTTP(rec, r)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Surface != "ui" || got.Cred != "session" {
		t.Fatalf("%+v", got)
	}
}

func TestKeyRejections(t *testing.T) {
	a := newApp(t)
	c := a.claims()
	c["aud"] = []string{"bd"}
	w := a.call("GET", "/api/notes", a.sign("k1", c))
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("wrong aud: %d %v", w.Code, w.Header())
	}
	// 503 when the revocation list is down on first use.
	b := newApp(t)
	b.v.o.RevokedURL = b.f.srv.URL + "/missing"
	w = b.call("GET", "/api/notes", b.key("notes:read"))
	if w.Code != 503 || strings.Contains(w.Body.String(), "pk_") {
		t.Fatalf("revocation down: %d %s", w.Code, w.Body)
	}
}

// ---- MCP ----

func (a *app) mcpClient(t *testing.T, hdr http.Header) *mcp.ClientSession {
	srv := httptest.NewServer(a.mcp)
	t.Cleanup(srv.Close)
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		for k, v := range hdr {
			r.Header[k] = v
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: rt}}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func toolNames(t *testing.T, s *mcp.ClientSession) []string {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func TestMCPFiltering(t *testing.T) {
	a := newApp(t)
	all := []string{"create-note", "export-notes", "handler-scope", "list-notes", "public-info", "purge-notes", "whoami"}

	if got := toolNames(t, a.mcpClient(t, nil)); !slices.Equal(got, all) {
		t.Errorf("anonymous sees %v (listing stays public)", got)
	}
	cookie := http.Header{"Cookie": {a.session(t).String()}}
	if got := toolNames(t, a.mcpClient(t, cookie)); !slices.Equal(got, all) {
		t.Errorf("session sees %v", got)
	}
	ro := http.Header{"Authorization": {"Bearer " + a.key("notes:read")}}
	if got, want := toolNames(t, a.mcpClient(t, ro)), []string{"handler-scope", "list-notes", "public-info", "whoami"}; !slices.Equal(got, want) {
		t.Errorf("read key sees %v, want %v", got, want)
	}
	rw := http.Header{"Authorization": {"Bearer " + a.key("notes:read notes:write")}}
	if got, want := toolNames(t, a.mcpClient(t, rw)), []string{"create-note", "export-notes", "handler-scope", "list-notes", "public-info", "whoami"}; !slices.Equal(got, want) {
		t.Errorf("read+write key sees %v, want %v", got, want)
	}
	adm := http.Header{"Authorization": {"Bearer " + a.key("notes:read notes:write notes:admin", "admin")}}
	if got := toolNames(t, a.mcpClient(t, adm)); !slices.Equal(got, all) {
		t.Errorf("admin key sees %v", got)
	}
}

func TestMCPCall(t *testing.T) {
	a := newApp(t)
	ro := a.mcpClient(t, http.Header{"Authorization": {"Bearer " + a.key("notes:read")}})
	res, err := ro.CallTool(context.Background(), &mcp.CallToolParams{Name: "create-note", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	txt := res.Content[0].(*mcp.TextContent).Text
	if !res.IsError || !strings.Contains(txt, "notes:write") || !strings.Contains(txt, "create-note") {
		t.Fatalf("filtered tool: %v %q", res.IsError, txt)
	}
	res, err = ro.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("allowed tool: %+v %v", res, err)
	}
	var w who
	_ = json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &w)
	if w.Cred != "key" || w.Surface != "mcp" {
		t.Fatalf("whoami = %+v", w)
	}
	rw := a.mcpClient(t, http.Header{"Authorization": {"Bearer " + a.key("notes:read notes:write")}})
	res, _ = rw.CallTool(context.Background(), &mcp.CallToolParams{Name: "create-note", Arguments: map[string]any{}})
	if res.IsError {
		t.Fatalf("write key create: %+v", res)
	}
}

func TestMCPMetadataAndChallenge(t *testing.T) {
	a := newApp(t)
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		r := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		a.mcp.ServeHTTP(w, r)
		var m struct {
			Resource string   `json:"resource"`
			AS       []string `json:"authorization_servers"`
			Scopes   []string `json:"scopes_supported"`
			Bearer   []string `json:"bearer_methods_supported"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &m) != nil {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
		if m.Resource != "https://notes.example/mcp" || !slices.Equal(m.AS, []string{"https://oidc.teb.ooo"}) ||
			!slices.Equal(m.Scopes, []string{"notes:read", "notes:write"}) || !slices.Equal(m.Bearer, []string{"header"}) {
			t.Fatalf("metadata = %+v", m)
		}
	}
	// A refused credential: 401 with the challenge naming the metadata.
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	r.Header.Set("Authorization", "Bearer pk_garbage")
	w := httptest.NewRecorder()
	a.mcp.ServeHTTP(w, r)
	want := `Bearer resource_metadata="https://notes.example/.well-known/oauth-protected-resource", error="invalid_token"`
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") != want {
		t.Fatalf("%d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
}

func TestMCPRequireAuth(t *testing.T) {
	a := newApp(t)
	au, _ := auth.New(auth.OIDCConfig{Issuer: "https://oidc.invalid", ClientID: "notes", PublicURL: "https://notes.example"}, sessionKey,
		auth.WithAppName("notes"), auth.WithKeyVerifier(a.v.Verify))
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("notes", "1"))
	au.Register(api, mux)
	s, err := openapimcp.New(api, mux, openapimcp.Options{Auth: au.BearerOrSession, PublicURL: "https://notes.example", App: "notes",
		AuthorizationServer: "https://oidc.teb.ooo", RequireAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") != `Bearer resource_metadata="https://notes.example/.well-known/oauth-protected-resource"` {
		t.Fatalf("%d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
}
