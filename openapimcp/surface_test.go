package openapimcp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/teb-ooo/playground-go/openapimcp"
	"github.com/teb-ooo/playground-go/surface"
)

// surfaceApp is an app whose "which-surface" operation reports surface.From(ctx),
// with surface.Middleware mounted outermost and /mcp beside the API.
func surfaceApp(t *testing.T, opts openapimcp.Options) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "1.0.0"))
	huma.Register(api, huma.Operation{
		OperationID: "which-surface", Method: http.MethodGet, Path: "/api/surface",
		Summary: "Surface", Description: "Reports the surface the request came through.",
	}, func(ctx context.Context, in *struct{}) (*struct{ Body map[string]string }, error) {
		return &struct{ Body map[string]string }{map[string]string{"surface": surface.From(ctx).String()}}, nil
	})
	mux.Handle("/mcp", openapimcp.Handler(api, mux, opts))
	return surface.Middleware(mux)
}

func TestSurfaceCannotBeForgedThroughMCP(t *testing.T) {
	h := surfaceApp(t, openapimcp.Options{})

	// A plain browser request is ui; with a bearer token it is api.
	for _, tc := range []struct{ auth, want string }{{"", "ui"}, {"Bearer x", "api"}} {
		r := httptest.NewRequest("GET", "/api/surface", nil)
		if tc.auth != "" {
			r.Header.Set("Authorization", tc.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if !strings.Contains(w.Body.String(), `"surface":"`+tc.want+`"`) {
			t.Errorf("plain request (auth %q): %s, want %s", tc.auth, w.Body.String(), tc.want)
		}
	}

	// A client that claims to be human (header, and a browser-like cookie)
	// while talking MCP is still mcp.
	s := connectAt(t, h, "/mcp", http.Header{
		surface.Header: {"ui"}, "Cookie": {"playground_session=xyz"},
		"X-Surface": {"ui"},
	})
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "which-surface"})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); !strings.Contains(got, `"surface":"mcp"`) {
		t.Fatalf("through MCP: %s, want mcp", got)
	}
}
