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
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/teb-ooo/playground-go/openapimcp"
)

type Item struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags,omitempty"`
}

type NewItem struct {
	Name string   `json:"name" minLength:"1" doc:"Item name"`
	Tags []string `json:"tags,omitempty"`
}

func newAPI(t testing.TB) (huma.API, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "1.0.0"))

	huma.Register(api, huma.Operation{
		OperationID: "get-item", Method: http.MethodGet, Path: "/api/items/{id}",
		Summary: "Get item", Description: "Returns one item.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" doc:"Item id"`
	}) (*struct{ Body Item }, error) {
		if in.ID == "missing" {
			return nil, huma.Error404NotFound("no such item")
		}
		return &struct{ Body Item }{Item{ID: in.ID, Name: "thing"}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-items", Method: http.MethodGet, Path: "/api/items",
		Summary: "List items", Description: "Lists items.",
	}, func(ctx context.Context, in *struct {
		Limit int      `query:"limit" default:"10" doc:"Max items"`
		Tag   []string `query:"tag" doc:"Filter by tag"`
	}) (*struct{ Body []Item }, error) {
		items := []Item{{ID: "1", Name: "limit=" + itoa(in.Limit), Tags: in.Tag}}
		return &struct{ Body []Item }{items}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "create-item", Method: http.MethodPost, Path: "/api/items",
		Summary: "Create item", Description: "Creates an item.", DefaultStatus: 201,
	}, func(ctx context.Context, in *struct{ Body NewItem }) (*struct{ Body Item }, error) {
		return &struct{ Body Item }{Item{ID: "new", Name: in.Body.Name, Tags: in.Body.Tags}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "rename-item", Method: http.MethodPatch, Path: "/api/items/{id}",
		Summary: "Rename item", Description: "Renames an item.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body NewItem
	}) (*struct{ Body Item }, error) {
		return &struct{ Body Item }{Item{ID: in.ID, Name: in.Body.Name}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "whoami", Method: http.MethodGet, Path: "/api/whoami",
		Summary: "Who am I", Description: "Echoes credentials seen.",
	}, func(ctx context.Context, in *struct {
		Authorization string `header:"Authorization"`
		Cookie        string `header:"Cookie"`
	}) (*struct{ Body map[string]string }, error) {
		return &struct{ Body map[string]string }{map[string]string{"authorization": in.Authorization, "cookie": in.Cookie}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "secret-thing", Method: http.MethodGet, Path: "/api/secret",
		Summary: "Hidden", Description: "Never a tool.", Hidden: true,
	}, func(ctx context.Context, in *struct{}) (*struct{ Body string }, error) {
		return &struct{ Body string }{"x"}, nil
	})
	return api, mux
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func connect(t *testing.T, h http.Handler, hdr http.Header) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		for k, v := range hdr {
			r.Header[k] = v
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: rt}}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("want 1 content, got %d", len(res.Content))
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestToolsListing(t *testing.T) {
	api, mux := newAPI(t)
	h := openapimcp.Handler(api, mux, openapimcp.Options{})
	tl, ok := h.(interface{ Tools() []string })
	if !ok {
		t.Fatal("handler has no Tools method")
	}
	want := []string{"create-item", "get-item", "list-items", "rename-item", "whoami"}
	if got := strings.Join(tl.Tools(), ","); got != strings.Join(want, ",") {
		t.Fatalf("Tools() = %s, want %v", got, want)
	}
	openapimcp.ParityCheck(t, api, h)
}

func TestInputSchemaMergeRule(t *testing.T) {
	api, _ := newAPI(t)
	ts, err := openapimcp.NewToolset(api)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		tool     string
		wantKeys []string // top level properties
		required []string
	}{
		{"get-item", []string{"id"}, []string{"id"}},
		{"list-items", []string{"limit", "tag"}, nil},
		{"create-item", []string{"name", "tags"}, []string{"name"}}, // body only: flattened
		{"rename-item", []string{"body", "id"}, []string{"body", "id"}},
	}
	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			tool, _ := ts.Lookup(tc.tool)
			props := tool.InputSchema["properties"].(map[string]any)
			var keys []string
			for k := range props {
				keys = append(keys, k)
			}
			if !sameSet(keys, tc.wantKeys) {
				t.Errorf("properties = %v, want %v", keys, tc.wantKeys)
			}
			var req []string
			if r, ok := tool.InputSchema["required"].([]string); ok {
				req = r
			}
			if !sameSet(req, tc.required) {
				t.Errorf("required = %v, want %v", req, tc.required)
			}
			if _, has := props["$schema"]; has {
				t.Error("$schema leaked into tool input")
			}
		})
	}
	tool, _ := ts.Lookup("get-item")
	if !strings.HasPrefix(tool.Description, "Get item\n\nReturns one item.") {
		t.Errorf("description = %q", tool.Description)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestCalls(t *testing.T) {
	api, mux := newAPI(t)
	marker := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Auth-Ran", "1")
			next.ServeHTTP(w, r)
		})
	}
	h := openapimcp.Handler(api, mux, openapimcp.Options{Auth: marker})
	s := connect(t, h, http.Header{"Authorization": {"Bearer abc"}, "Cookie": {"playground_session=xyz"}})

	tests := []struct {
		name    string
		tool    string
		args    map[string]any
		wantErr bool
		contain []string
	}{
		{"path param", "get-item", map[string]any{"id": "a b/c"}, false, []string{`"id":"a b/c"`}},
		{"query and array", "list-items", map[string]any{"limit": 3, "tag": []string{"x", "y"}}, false, []string{"limit=3", `"tags":["x","y"]`}},
		{"flattened body", "create-item", map[string]any{"name": "milk", "tags": []string{"a"}}, false, []string{`"name":"milk"`}},
		{"nested body plus path", "rename-item", map[string]any{"id": "42", "body": map[string]any{"name": "eggs"}}, false, []string{`"id":"42"`, `"name":"eggs"`}},
		{"forwards credentials", "whoami", nil, false, []string{"Bearer abc", "playground_session=xyz"}},
		{"non-2xx is error result", "get-item", map[string]any{"id": "missing"}, true, []string{"HTTP 404", "no such item"}},
		{"validation is error result", "create-item", map[string]any{"name": ""}, true, []string{"HTTP 422"}},
		{"missing path param", "get-item", map[string]any{}, true, []string{"missing required path parameter"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if res.IsError != tc.wantErr {
				t.Fatalf("IsError = %v, want %v (%s)", res.IsError, tc.wantErr, text(t, res))
			}
			out := text(t, res)
			for _, c := range tc.contain {
				if !strings.Contains(out, c) {
					t.Errorf("output %q missing %q", out, c)
				}
			}
		})
	}
}

func TestNewRejectsBodyParamCollision(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	huma.Register(api, huma.Operation{OperationID: "x", Method: "GET", Path: "/x", Summary: "s", Description: "d"},
		func(ctx context.Context, in *struct {
			B string `query:"body"`
		}) (*struct{}, error) {
			return nil, nil
		})
	if _, err := openapimcp.New(api, mux, openapimcp.Options{}); err == nil {
		t.Fatal("expected error for parameter named body")
	}
}

func TestParityCheckDetectsMissingDescription(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	huma.Register(api, huma.Operation{OperationID: "x", Method: "GET", Path: "/x", Summary: "s"},
		func(ctx context.Context, in *struct{}) (*struct{}, error) { return nil, nil })
	ft := &fakeTB{}
	openapimcp.ParityCheck(ft, api, openapimcp.Handler(api, mux, openapimcp.Options{}))
	if !ft.failed {
		t.Fatal("ParityCheck should fail for an operation without a description")
	}
}

type fakeTB struct {
	testing.TB
	failed bool
}

func (f *fakeTB) Helper()               {}
func (f *fakeTB) Errorf(string, ...any) { f.failed = true }
func (f *fakeTB) Fatalf(string, ...any) { f.failed = true }
