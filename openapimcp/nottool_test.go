package openapimcp_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/teb-ooo/playground-go/openapimcp"
)

// x-mcp: false keeps the operation in the OpenAPI document but out of the tool set, and ParityCheck agrees.
func TestNoToolExtensionKeepsTheOperationOutOfTheTools(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "1.0.0"))
	noop := func(ctx context.Context, in *struct{}) (*struct{ Body []string }, error) {
		return &struct{ Body []string }{}, nil
	}
	huma.Register(api, huma.Operation{OperationID: "list-things", Method: http.MethodGet, Path: "/api/things", Summary: "List", Description: "Lists things."}, noop)
	huma.Register(api, huma.Operation{
		OperationID: "list-browser-things", Method: http.MethodGet, Path: "/api/browser-things", Summary: "Browser", Description: "Browser only.",
		Extensions: map[string]any{openapimcp.NoToolExtension: false},
	}, noop)
	ts, err := openapimcp.NewToolset(api)
	if err != nil {
		t.Fatal(err)
	}
	if got := ts.Names(); len(got) != 1 || got[0] != "list-things" {
		t.Fatalf("tools = %v, want only list-things", got)
	}
	if _, ok := api.OpenAPI().Paths["/api/browser-things"]; !ok {
		t.Fatal("the operation must stay in the OpenAPI document")
	}
	// ParityCheck compares the operations that are tools with the tools the MCP handler lists
	openapimcp.ParityCheck(t, api, openapimcp.Handler(api, mux, openapimcp.Options{}), openapimcp.WithExempt("list-things", "list-browser-things"))
	// the extension set to true (or any non-false value) changes nothing
	huma.Register(api, huma.Operation{
		OperationID: "list-other-things", Method: http.MethodGet, Path: "/api/other-things", Summary: "Other", Description: "Other.",
		Extensions: map[string]any{openapimcp.NoToolExtension: true},
	}, noop)
	ts, _ = openapimcp.NewToolset(api)
	if len(ts.Names()) != 2 {
		t.Fatalf("x-mcp: true must stay a tool: %v", ts.Names())
	}
}
