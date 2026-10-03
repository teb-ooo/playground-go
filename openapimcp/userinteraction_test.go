package openapimcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/teb-ooo/playground-go/openapimcp"
)

func TestRequireUserInteractionExtensionMarksTheTool(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "1.0.0"))
	huma.Register(api, huma.Operation{
		OperationID: "apply-thing", Method: http.MethodPost, Path: "/api/things/apply", Summary: "Apply", Description: "Applies a thing.",
		Extensions: map[string]any{openapimcp.RequireUserInteractionExtension: true},
	}, func(ctx context.Context, in *struct{}) (*struct{ Body struct{} }, error) {
		return &struct{ Body struct{} }{}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "list-things", Method: http.MethodGet, Path: "/api/things", Summary: "List", Description: "Lists things.",
	}, func(ctx context.Context, in *struct{}) (*struct{ Body []string }, error) {
		return &struct{ Body []string }{}, nil
	})
	h := openapimcp.Handler(api, mux, openapimcp.Options{})

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		Result struct {
			Tools []struct {
				Name string         `json:"name"`
				Meta map[string]any `json:"_meta"`
			} `json:"tools"`
		} `json:"result"`
	}
	body := rec.Body.String()
	if i := strings.Index(body, "{"); i >= 0 {
		body = body[i:]
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	got := map[string]bool{}
	for _, tl := range out.Result.Tools {
		v, _ := tl.Meta["anthropic/requiresUserInteraction"].(bool)
		got[tl.Name] = v
	}
	if !got["apply-thing"] || got["list-things"] {
		t.Fatalf("only apply-thing must carry _meta anthropic/requiresUserInteraction: %v (%s)", got, rec.Body.String())
	}
}
