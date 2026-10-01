package assistant_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/teb-ooo/playground-go/assistant"
	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/surface"
)

// A tool call dispatched by the assistant reads as surface "assistant" in the
// operation, even though the chat request itself is a browser request ("ui")
// and even if the client sends the marker header.
func TestToolCallsRunAsAssistantSurface(t *testing.T) {
	fa := newFakeAnthropic(t, func(n int, req map[string]any) reply {
		if n == 0 {
			return reply{blocks: []block{{tool: "which-surface", input: `{}`}}}
		}
		return reply{blocks: []block{{text: "done"}}}
	})
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("notes", "1.0.0"))
	huma.Register(api, huma.Operation{OperationID: "which-surface", Method: "GET", Path: "/api/surface",
		Summary: "Surface", Description: "Reports the surface."},
		func(ctx context.Context, in *struct{}) (*struct{ Body map[string]string }, error) {
			return &struct{ Body map[string]string }{map[string]string{"surface": surface.From(ctx).String()}}, nil
		})
	a, err := assistant.New(api, surface.Middleware(mux), assistant.Options{APIKey: "k", Store: assistant.NewMemoryStore(),
		ClientOptions: []option.RequestOption{option.WithBaseURL(fa.URL), option.WithMaxRetries(0)}})
	if err != nil {
		t.Fatal(err)
	}
	h := surface.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithUser(r.Context(), auth.User{Subject: "u1"}))
		a.ServeHTTP(w, r)
	}))
	do := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set(surface.Header, "ui")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := do("/api/assistant/conversations", `{}`)
	id := strings.Split(strings.Split(w.Body.String(), `"id":"`)[1], `"`)[0]
	w = do("/api/assistant/conversations/"+id+"/messages", `{"content":"go"}`)
	if !strings.Contains(w.Body.String(), `surface\":\"assistant\"`) {
		t.Fatalf("tool result did not report assistant surface:\n%s", w.Body)
	}
}
