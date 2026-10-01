package playground_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/teb-ooo/playground-go/assistant"
	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/health"
	playgroundlog "github.com/teb-ooo/playground-go/log"
	"github.com/teb-ooo/playground-go/openapimcp"
	"github.com/teb-ooo/playground-go/spa"
	"github.com/teb-ooo/playground-go/testkit"

	playground "github.com/teb-ooo/playground-go"
)

// TestWholeStack wires every package together the way the README shows and
// checks the seams: a minted session cookie signs in over HTTP and through MCP,
// hidden operations stay out of the tool set, and parity holds.
func TestWholeStack(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	cfg, err := playground.FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	authn, err := cfg.NewAuth()
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig(cfg.AppName, "test"))
	auth.AddSecuritySchemes(api.OpenAPI())
	authn.Register(api, mux)
	health.Register(api, nil, "test")
	huma.Register(api, huma.Operation{
		OperationID: "whoami", Method: "GET", Path: "/api/whoami", Summary: "Who am I", Description: "Returns the caller.",
		Security: []map[string][]string{{"session": {}}, {"bearer": {}}},
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body map[string]any }, error) {
		u, err := auth.RequireAdmin(ctx)
		if err != nil {
			return nil, err
		}
		return &struct{ Body map[string]any }{map[string]any{"subject": u.Subject}}, nil
	})

	mcpH := openapimcp.Handler(api, mux, openapimcp.Options{Name: cfg.AppName, Auth: authn.BearerOrSession})
	mux.Handle("/mcp", mcpH)
	mux.Handle("/api/assistant/", assistant.Handler(api, mux, assistant.Options{
		AppName: cfg.AppName, APIKey: "unused-in-this-test", Store: assistant.NewMemoryStore()}))
	mux.Handle("/", spa.Handler(fstest.MapFS{"index.html": {Data: []byte("<html><head></head><body></body></html>")}}, spa.Config{AppName: cfg.AppName, Env: cfg.Env, SessionURLFile: "/nonexistent"}))
	handler := playgroundlog.Middleware(authn.Middleware(mux))

	// Parity: only whoami is a tool; /auth/me and /healthz are hidden.
	openapimcp.ParityCheck(t, api, mcpH, openapimcp.WithExempt("whoami")) // fixture op predates the contract
	if got := mcpH.(interface{ Tools() []string }).Tools(); strings.Join(got, ",") != "whoami" {
		t.Fatalf("tools = %v", got)
	}

	admin, err := testkit.MintSession(cfg.SessionKey, auth.User{Subject: "u-admin", Groups: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	member, _ := testkit.MintSession(cfg.SessionKey, auth.User{Subject: "u-member"})

	do := func(method, path string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	tests := []struct {
		name   string
		path   string
		cookie *http.Cookie
		want   int
		body   string
	}{
		{"anonymous", "/api/whoami", nil, 401, ""},
		{"member forbidden", "/api/whoami", member, 403, ""},
		{"admin ok", "/api/whoami", admin, 200, `"subject":"u-admin"`},
		{"me anonymous", "/auth/me", nil, 401, ""},
		{"me admin", "/auth/me", admin, 200, `"is_admin":true`},
		{"healthz", "/healthz", nil, 200, `"db":"ok"`},
		{"spa fallback", "/some/route", nil, 200, `<script src="/playground.js">`},
		{"playground.js", "/playground.js", nil, 200, "window.__PLAYGROUND__"},
		{"assistant needs login", "/api/assistant/conversations", nil, 401, ""},
		{"assistant list", "/api/assistant/conversations", admin, 200, "[]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do("GET", tc.path, tc.cookie)
			if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("%s = %d %s", tc.path, w.Code, w.Body)
			}
			if w.Header().Get("X-Request-Id") == "" {
				t.Error("no X-Request-Id")
			}
		})
	}

	// MCP: the same cookie, forwarded, makes the tool run as that user.
	srv := httptest.NewServer(handler)
	defer srv.Close()
	call := func(c *http.Cookie) (*mcp.CallToolResult, error) {
		client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		hc := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
			if c != nil {
				r.AddCookie(c)
			}
			return http.DefaultTransport.RoundTrip(r)
		})}
		s, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: hc}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		return s.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami"})
	}
	res, err := call(admin)
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "u-admin") {
		t.Errorf("admin via MCP: %+v err=%v", res, err)
	}
	res, err = call(member)
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "HTTP 403") {
		t.Errorf("member via MCP: %+v err=%v", res, err)
	}
	res, err = call(nil)
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "HTTP 401") {
		t.Errorf("anonymous via MCP: %+v err=%v", res, err)
	}

	// /auth/me is a real operation but must not be in the published spec.
	spec := do("GET", "/openapi.json", nil).Body.String()
	var doc struct{ Paths map[string]any }
	if err := json.Unmarshal([]byte(spec), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Paths["/auth/me"]; ok {
		t.Error("/auth/me should be hidden from the OpenAPI document")
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
