package openapimcp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/teb-ooo/playground-go/openapimcp"
	"github.com/teb-ooo/playground-go/surface"
)

func TestResourcesAndPrompts(t *testing.T) {
	var sawSurface surface.Surface
	authRan := 0
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authRan++
			if r.Header.Get("Authorization") != "Bearer good" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, "alice")))
		})
	}
	opts := openapimcp.Options{
		Auth: auth, Instructions: "Use drafts.",
		Resources: []openapimcp.Resource{{URI: "lore://about", Name: "about", MIMEType: "text/markdown",
			Read: func(ctx context.Context, uri string) (openapimcp.ResourceContent, error) {
				sawSurface = surface.From(ctx)
				return openapimcp.ResourceContent{Text: "hello " + ctx.Value(userKey{}).(string)}, nil
			}}},
		ResourceTemplates: []openapimcp.ResourceTemplate{{URITemplate: "lore://worlds/{id}/context-tray", Name: "tray",
			Read: func(ctx context.Context, uri string) (openapimcp.ResourceContent, error) {
				vars, ok := openapimcp.MatchTemplate("lore://worlds/{id}/context-tray", uri)
				if !ok || vars["id"] == "nope" {
					return openapimcp.ResourceContent{}, openapimcp.ErrResourceNotFound
				}
				return openapimcp.ResourceContent{Text: "tray of " + vars["id"]}, nil
			}}},
		Prompts: []openapimcp.Prompt{{Name: "load-world-context", Description: "Load a world",
			Arguments: []openapimcp.PromptArgument{{Name: "world", Required: true}},
			Get: func(ctx context.Context, args map[string]string) ([]openapimcp.PromptMessage, error) {
				return []openapimcp.PromptMessage{{Text: "Load " + args["world"] + " for " + ctx.Value(userKey{}).(string)}}, nil
			}}},
	}
	api, mux := newAPI(t)
	h := openapimcp.Handler(api, mux, opts)
	ctx := context.Background()
	s := connect(t, h, http.Header{"Authorization": {"Bearer good"}})

	if got := s.InitializeResult().Instructions; got != "Use drafts." {
		t.Errorf("instructions = %q", got)
	}
	rl, err := s.ListResources(ctx, nil)
	if err != nil || len(rl.Resources) != 1 || rl.Resources[0].URI != "lore://about" || rl.Resources[0].MIMEType != "text/markdown" {
		t.Fatalf("ListResources: %+v %v", rl, err)
	}
	tl, err := s.ListResourceTemplates(ctx, nil)
	if err != nil || len(tl.ResourceTemplates) != 1 || tl.ResourceTemplates[0].URITemplate != "lore://worlds/{id}/context-tray" {
		t.Fatalf("ListResourceTemplates: %+v %v", tl, err)
	}
	rr, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: "lore://about"})
	if err != nil || rr.Contents[0].Text != "hello alice" || sawSurface != surface.MCP {
		t.Fatalf("read static: %+v %v surface=%s", rr, err, sawSurface)
	}
	rr, err = s.ReadResource(ctx, &mcp.ReadResourceParams{URI: "lore://worlds/w1/context-tray"})
	if err != nil || rr.Contents[0].Text != "tray of w1" || rr.Contents[0].MIMEType != "text/plain" {
		t.Fatalf("read template: %+v %v", rr, err)
	}
	if _, err = s.ReadResource(ctx, &mcp.ReadResourceParams{URI: "lore://worlds/nope/context-tray"}); err == nil {
		t.Fatal("expected not found")
	}
	pl, err := s.ListPrompts(ctx, nil)
	if err != nil || len(pl.Prompts) != 1 || !pl.Prompts[0].Arguments[0].Required {
		t.Fatalf("ListPrompts: %+v %v", pl, err)
	}
	pg, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "load-world-context", Arguments: map[string]string{"world": "w1"}})
	if err != nil || len(pg.Messages) != 1 || pg.Messages[0].Role != "user" ||
		pg.Messages[0].Content.(*mcp.TextContent).Text != "Load w1 for alice" {
		t.Fatalf("GetPrompt: %+v %v", pg, err)
	}
	if _, err = s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "load-world-context"}); err == nil {
		t.Fatal("missing required argument should fail")
	}

	// Same auth as tools: a caller Auth rejects is turned away at the door.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Authorization", "Bearer bad")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("rejected caller: status %d", rec.Code)
	}
	if authRan == 0 {
		t.Fatal("Auth never ran")
	}
}

type userKey struct{}

func TestExtrasAreOptionalAndNotOperations(t *testing.T) {
	api, mux := newAPI(t)
	h := openapimcp.Handler(api, mux, openapimcp.Options{
		Resources: []openapimcp.Resource{{URI: "x://a", Name: "a", Read: func(context.Context, string) (openapimcp.ResourceContent, error) {
			return openapimcp.ResourceContent{}, errors.New("boom")
		}}},
		Prompts: []openapimcp.Prompt{{Name: "p", Get: func(context.Context, map[string]string) ([]openapimcp.PromptMessage, error) { return nil, nil }}},
	})
	s := connect(t, h, nil)
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 5 {
		t.Fatalf("tools = %d, %v (resources and prompts must not be tools)", len(tools.Tools), err)
	}
	openapimcp.ParityCheck(t, api, h, openapimcp.WithExempt("get-item", "list-items", "create-item", "rename-item", "whoami"))

	if _, err := openapimcp.New(api, mux, openapimcp.Options{Prompts: []openapimcp.Prompt{{Name: "p"}}}); err == nil {
		t.Error("prompt without Get must be rejected")
	}
}

func TestMatchTemplate(t *testing.T) {
	v, ok := openapimcp.MatchTemplate("lore://worlds/{id}/notes/{n}", "lore://worlds/w1/notes/7")
	if !ok || v["id"] != "w1" || v["n"] != "7" {
		t.Fatalf("%v %v", v, ok)
	}
	for _, u := range []string{"lore://worlds//notes/7", "lore://worlds/a/b/notes/7", "lore://worlds/w1/notes/7/x"} {
		if _, ok := openapimcp.MatchTemplate("lore://worlds/{id}/notes/{n}", u); ok {
			t.Errorf("%s should not match", u)
		}
	}
}
