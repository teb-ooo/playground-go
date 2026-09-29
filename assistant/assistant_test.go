package assistant_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/teb-ooo/factory-go/assistant"
	"github.com/teb-ooo/factory-go/auth"
)

// ---- fake Anthropic server ----

type block struct {
	text  string
	tool  string // tool name; makes it a tool_use block
	input string // raw JSON input for tool_use
}

type reply struct {
	blocks []block
	stop   string // default end_turn, or tool_use when any block is a tool
	status int    // non-200 makes an error response
}

type fakeAnthropic struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
	script   func(n int, req map[string]any) reply
}

func newFakeAnthropic(t *testing.T, script func(n int, req map[string]any) reply) *fakeAnthropic {
	t.Helper()
	f := &fakeAnthropic{script: script}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(b, &req); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		f.mu.Lock()
		n := len(f.requests)
		f.requests = append(f.requests, req)
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()

		rep := f.script(n, req)
		if rep.status != 0 && rep.status != 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rep.status)
			fmt.Fprintf(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		ev := func(name string, data any) {
			d, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, d)
			fl.Flush()
		}
		ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": fmt.Sprintf("msg_%d", n), "type": "message", "role": "assistant", "model": req["model"],
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 1}}})
		stop := rep.stop
		for i, bl := range rep.blocks {
			if bl.tool != "" {
				stop = "tool_use"
				ev("content_block_start", map[string]any{"type": "content_block_start", "index": i,
					"content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d_%d", n, i), "name": bl.tool, "input": map[string]any{}}})
				in := bl.input
				if in == "" {
					in = "{}"
				}
				half := len(in) / 2
				for _, part := range []string{in[:half], in[half:]} {
					ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": part}})
				}
			} else {
				ev("content_block_start", map[string]any{"type": "content_block_start", "index": i,
					"content_block": map[string]any{"type": "text", "text": ""}})
				half := len(bl.text) / 2
				for _, part := range []string{bl.text[:half], bl.text[half:]} {
					if part == "" {
						continue
					}
					ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
						"delta": map[string]any{"type": "text_delta", "text": part}})
				}
			}
			ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		}
		if stop == "" {
			stop = "end_turn"
		}
		ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 5}})
		ev("message_stop", map[string]any{"type": "message_stop"})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAnthropic) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.requests) }

// ---- app under test ----

type item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func testAPI() (huma.API, *http.ServeMux) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("notes", "1.0.0"))
	huma.Register(api, huma.Operation{OperationID: "get-item", Method: "GET", Path: "/api/items/{id}",
		Summary: "Get item", Description: "Returns one item."},
		func(ctx context.Context, in *struct {
			ID string `path:"id"`
		}) (*struct{ Body item }, error) {
			if _, err := auth.Require(ctx); err != nil {
				return nil, err
			}
			if in.ID == "missing" {
				return nil, huma.Error404NotFound("no such item")
			}
			return &struct{ Body item }{item{ID: in.ID, Name: "milk"}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "whoami.v1", Method: "GET", Path: "/api/whoami",
		Summary: "Who am I", Description: "Reports the user and credentials the operation ran with."},
		func(ctx context.Context, in *struct {
			Cookie string `header:"Cookie"`
			Auth   string `header:"Authorization"`
		}) (*struct{ Body map[string]string }, error) {
			u, err := auth.Require(ctx)
			if err != nil {
				return nil, err
			}
			return &struct{ Body map[string]string }{map[string]string{"user": u.Subject, "cookie": in.Cookie, "authorization": in.Auth}}, nil
		})
	return api, mux
}

type harness struct {
	fa    *fakeAnthropic
	store assistant.Store
	h     http.Handler
	a     *assistant.Assistant
}

func newHarness(t *testing.T, script func(n int, req map[string]any) reply, mut func(*assistant.Options)) *harness {
	t.Helper()
	fa := newFakeAnthropic(t, script)
	api, mux := testAPI()
	var store assistant.Store = assistant.NewMemoryStore()
	opts := assistant.Options{
		AppName: "notes", APIKey: "test-key", Store: store,
		Spec:          "# Notes\n\nNotes keeps short notes for a household.\nEveryone can read them.\n\nSecond paragraph is ignored.",
		ClientOptions: []option.RequestOption{option.WithBaseURL(fa.URL), option.WithMaxRetries(0)},
	}
	if mut != nil {
		mut(&opts)
	}
	a, err := assistant.New(api, mux, opts)
	if err != nil {
		t.Fatal(err)
	}
	store = opts.Store
	// Stand-in for auth.Middleware: X-Test-User is the subject.
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sub := r.Header.Get("X-Test-User"); sub != "" {
			r = r.WithContext(auth.WithUser(r.Context(), auth.User{Subject: sub}))
		}
		a.ServeHTTP(w, r)
	})
	return &harness{fa: fa, store: store, h: outer, a: a}
}

type sseEvent struct {
	Name string
	Data map[string]any
}

func parseSSE(t *testing.T, body io.Reader) []sseEvent {
	t.Helper()
	var evs []sseEvent
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur = sseEvent{Name: strings.TrimPrefix(line, "event: ")}
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data); err != nil {
				t.Fatalf("bad SSE data %q: %v", line, err)
			}
		case line == "":
			if cur.Name != "" {
				evs = append(evs, cur)
			}
			cur = sseEvent{}
		}
	}
	return evs
}

func names(evs []sseEvent) string {
	var n []string
	for _, e := range evs {
		n = append(n, e.Name)
	}
	return strings.Join(n, ",")
}

func (h *harness) do(t *testing.T, method, path, user, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != "" {
		r.Header.Set("X-Test-User", user)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *harness) newConv(t *testing.T, user string) string {
	t.Helper()
	w := h.do(t, "POST", "/api/assistant/conversations", user, `{"title":"t"}`, nil)
	if w.Code != 201 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var c map[string]any
	json.Unmarshal(w.Body.Bytes(), &c)
	return c["id"].(string)
}

func (h *harness) send(t *testing.T, user, conv, text string, hdr map[string]string) (*httptest.ResponseRecorder, []sseEvent) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"content": text})
	w := h.do(t, "POST", "/api/assistant/conversations/"+conv+"/messages", user, string(b), hdr)
	if w.Code != 200 {
		return w, nil
	}
	return w, parseSSE(t, w.Body)
}

// ---- tests ----

func TestPlainReplyAndRequestShape(t *testing.T) {
	h := newHarness(t, func(n int, req map[string]any) reply {
		return reply{blocks: []block{{text: "Hello there, friend."}}}
	}, nil)
	conv := h.newConv(t, "u1")
	w, evs := h.send(t, "u1", conv, "hi", nil)
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	if names(evs) != "text,text,done" {
		t.Fatalf("events = %s", names(evs))
	}
	if got := evs[0].Data["text"].(string) + evs[1].Data["text"].(string); got != "Hello there, friend." {
		t.Errorf("text = %q", got)
	}
	if len(evs[2].Data) != 1 || evs[2].Data["stop_reason"] != "end_turn" {
		t.Errorf("done = %v", evs[2].Data)
	}

	req := h.fa.requests[0]
	if req["model"] != "claude-sonnet-5-5" || req["max_tokens"] != 4096.0 || req["stream"] != true {
		t.Errorf("model/max_tokens/stream = %v/%v/%v", req["model"], req["max_tokens"], req["stream"])
	}
	if oc, _ := req["output_config"].(map[string]any); oc["effort"] != "low" {
		t.Errorf("output_config = %v", req["output_config"])
	}
	if h.fa.headers[0].Get("X-Api-Key") != "test-key" {
		t.Errorf("api key header = %q", h.fa.headers[0].Get("X-Api-Key"))
	}
	sys := req["system"].([]any)[0].(map[string]any)
	prompt := sys["text"].(string)
	for _, want := range []string{"notes", "Notes keeps short notes for a household. Everyone can read them.", "- get-item: Get item", "- whoami.v1: Who am I"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Second paragraph") {
		t.Error("system prompt includes more than the first paragraph")
	}
	if sys["cache_control"] == nil {
		t.Error("system prompt is not marked cacheable")
	}
	tools := req["tools"].([]any)
	var toolNames []string
	for _, x := range tools {
		toolNames = append(toolNames, x.(map[string]any)["name"].(string))
	}
	if strings.Join(toolNames, ",") != "get-item,whoami_v1" {
		t.Errorf("tools = %v", toolNames)
	}
	first := tools[0].(map[string]any)
	if first["description"] != "Get item\n\nReturns one item." || first["input_schema"].(map[string]any)["type"] != "object" {
		t.Errorf("tool = %v", first)
	}

	msgs, _ := h.store.ListMessages(context.Background(), "u1", conv)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("stored = %+v", msgs)
	}
	c, _ := h.store.GetConversation(context.Background(), "u1", conv)
	if c.Title != "t" {
		t.Errorf("title = %q", c.Title)
	}
}

func TestToolLoopRunsAsSignedInUser(t *testing.T) { testToolLoop(t, nil) }

// TestToolLoopWithPgxStore replays the same scenario through Postgres, so
// stored tool_use/tool_result blocks survive a jsonb round trip and are accepted
// again on the next turn.
func TestToolLoopWithPgxStore(t *testing.T) {
	if os.Getenv("FACTORY_TEST_DATABASE_URL") == "" {
		t.Skip("FACTORY_TEST_DATABASE_URL not set")
	}
	testToolLoop(t, func(o *assistant.Options) { o.Store = newPgxStore(t) })
}

func testToolLoop(t *testing.T, mut func(*assistant.Options)) {
	h := newHarness(t, func(n int, req map[string]any) reply {
		switch n {
		case 0:
			return reply{blocks: []block{{text: "Let me look."}, {tool: "get-item", input: `{"id":"42"}`}, {tool: "whoami_v1", input: `{}`}}}
		default:
			return reply{blocks: []block{{text: "It is milk."}}}
		}
	}, mut)
	conv := h.newConv(t, "u1")
	_, evs := h.send(t, "u1", conv, "what is item 42?", map[string]string{"Cookie": "factory_session=abc", "Authorization": "Bearer xyz"})
	// tool_call is emitted right before each execution, tool_result right after.
	if want := "text,text,tool_call,tool_result,tool_call,tool_result,text,text,done"; names(evs) != want {
		t.Fatalf("events = %s, want %s", names(evs), want)
	}
	var results []sseEvent
	for _, e := range evs {
		if e.Name == "tool_result" {
			results = append(results, e)
		}
	}
	if _, has := results[0].Data["is_error"]; has || !strings.Contains(results[0].Data["content"].(string), `"name":"milk"`) || results[0].Data["id"] == "" || len(results[0].Data) != 2 {
		t.Errorf("result 0 = %v", results[0].Data)
	}
	who := results[1].Data["content"].(string)
	for _, want := range []string{`"user":"u1"`, "factory_session=abc", "Bearer xyz"} {
		if !strings.Contains(who, want) {
			t.Errorf("whoami result %q missing %q (the caller's identity must be forwarded)", who, want)
		}
	}
	var call map[string]any
	for _, e := range evs {
		if e.Name == "tool_call" {
			call = e.Data
			break
		}
	}
	if call["name"] != "get-item" || call["input"].(map[string]any)["id"] != "42" || call["id"] == "" {
		t.Errorf("tool_call = %v", call)
	}

	// The second model request carries assistant tool_use blocks and one user message with both tool_results.
	req2 := h.fa.requests[1]
	msgs := req2["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second request has %d messages", len(msgs))
	}
	last := msgs[2].(map[string]any)
	blocks := last["content"].([]any)
	if last["role"] != "user" || len(blocks) != 2 || blocks[0].(map[string]any)["type"] != "tool_result" {
		t.Errorf("tool results message = %v", last)
	}
	ids := map[string]bool{}
	for _, b := range msgs[1].(map[string]any)["content"].([]any) {
		if m := b.(map[string]any); m["type"] == "tool_use" {
			ids[m["id"].(string)] = true
		}
	}
	for _, b := range blocks {
		if !ids[b.(map[string]any)["tool_use_id"].(string)] {
			t.Errorf("tool_result %v does not match a tool_use", b)
		}
	}

	stored, _ := h.store.ListMessages(context.Background(), "u1", conv)
	var roles []string
	for _, m := range stored {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,user,assistant" {
		t.Fatalf("stored roles = %v", roles)
	}

	// A follow-up message replays the whole exchange, tool blocks included.
	h.fa.script = func(n int, req map[string]any) reply { return reply{blocks: []block{{text: "ok"}}} }
	_, evs = h.send(t, "u1", conv, "thanks", nil)
	if names(evs) != "text,text,done" && names(evs) != "text,done" {
		t.Fatalf("events = %s", names(evs))
	}
	req3 := h.fa.requests[len(h.fa.requests)-1]
	if n := len(req3["messages"].([]any)); n != 5 {
		t.Errorf("follow-up request has %d messages, want 5", n)
	}
}

func TestToolErrorsAreReportedToTheModel(t *testing.T) {
	h := newHarness(t, func(n int, req map[string]any) reply {
		if n == 0 {
			return reply{blocks: []block{{tool: "get-item", input: `{"id":"missing"}`}, {tool: "nope", input: `{}`}, {tool: "get-item", input: `{}`}}}
		}
		return reply{blocks: []block{{text: "That does not exist."}}}
	}, nil)
	conv := h.newConv(t, "u1")
	_, evs := h.send(t, "u1", conv, "find it", nil)
	var results []sseEvent
	for _, e := range evs {
		if e.Name == "tool_result" {
			results = append(results, e)
		}
	}
	if len(results) != 3 {
		t.Fatalf("results = %d (%s)", len(results), names(evs))
	}
	wants := []string{"HTTP 404", "unknown tool", "missing required path parameter"}
	for i, w := range wants {
		if results[i].Data["is_error"] != true || !strings.Contains(results[i].Data["content"].(string), w) {
			t.Errorf("result %d = %v, want error containing %q", i, results[i].Data, w)
		}
	}
	msgs := h.fa.requests[1]["messages"].([]any)
	for _, b := range msgs[len(msgs)-1].(map[string]any)["content"].([]any) {
		if b.(map[string]any)["is_error"] != true {
			t.Errorf("tool_result not flagged is_error: %v", b)
		}
	}
	if evs[len(evs)-1].Name != "done" {
		t.Errorf("last event = %s", evs[len(evs)-1].Name)
	}
}

func TestIterationLimit(t *testing.T) {
	h := newHarness(t, func(n int, req map[string]any) reply {
		return reply{blocks: []block{{tool: "get-item", input: `{"id":"1"}`}}}
	}, func(o *assistant.Options) { o.MaxIterations = 3 })
	conv := h.newConv(t, "u1")
	_, evs := h.send(t, "u1", conv, "loop forever", nil)
	if h.fa.count() != 3 {
		t.Errorf("model calls = %d, want 3", h.fa.count())
	}
	last := evs[len(evs)-1]
	if last.Name != "done" || last.Data["stop_reason"] != "max_iterations" {
		t.Errorf("last = %+v", last)
	}
	stored, _ := h.store.ListMessages(context.Background(), "u1", conv)
	if stored[len(stored)-1].Role != "assistant" {
		t.Errorf("history must end on an assistant turn, got %s", stored[len(stored)-1].Role)
	}
	// history stays valid: a following message goes through
	h.fa.script = func(n int, req map[string]any) reply { return reply{blocks: []block{{text: "resumed"}}} }
	_, evs = h.send(t, "u1", conv, "continue", nil)
	if evs[len(evs)-1].Name != "done" {
		t.Errorf("follow-up = %s", names(evs))
	}
}

func TestDefaultLimitsAreSpec(t *testing.T) {
	h := newHarness(t, func(n int, req map[string]any) reply {
		return reply{blocks: []block{{tool: "get-item", input: `{"id":"1"}`}}}
	}, nil)
	conv := h.newConv(t, "u1")
	h.send(t, "u1", conv, "loop", nil)
	if h.fa.count() != 12 {
		t.Errorf("default iteration cap = %d, want 12", h.fa.count())
	}
}

func TestModelOverride(t *testing.T) {
	h := newHarness(t, func(int, map[string]any) reply { return reply{blocks: []block{{text: "x"}}} },
		func(o *assistant.Options) { o.Model = "claude-opus-5-5"; o.Effort = "none" })
	h.send(t, "u1", h.newConv(t, "u1"), "hi", nil)
	req := h.fa.requests[0]
	if req["model"] != "claude-opus-5-5" {
		t.Errorf("model = %v", req["model"])
	}
	if _, has := req["output_config"]; has {
		t.Error("effort none should omit output_config")
	}
}

func TestRefusalAndAPIErrors(t *testing.T) {
	tests := []struct {
		name       string
		rep        reply
		wantEvents string
		wantStored int
		wantDetail string
	}{
		{"refusal", reply{stop: "refusal"}, "text,done", 2, ""},
		{"rate limited", reply{status: 429}, "error", 1, "busy"},
		{"unauthorized", reply{status: 401}, "error", 1, "not configured"},
		{"server error", reply{status: 500}, "error", 1, "status 500"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(int, map[string]any) reply { return tc.rep }, nil)
			conv := h.newConv(t, "u1")
			_, evs := h.send(t, "u1", conv, "hi", nil)
			if names(evs) != tc.wantEvents {
				t.Fatalf("events = %s, want %s", names(evs), tc.wantEvents)
			}
			if tc.wantDetail != "" && !strings.Contains(evs[0].Data["detail"].(string), tc.wantDetail) {
				t.Errorf("detail = %v", evs[0].Data["detail"])
			}
			stored, _ := h.store.ListMessages(context.Background(), "u1", conv)
			if len(stored) != tc.wantStored {
				t.Errorf("stored = %d, want %d", len(stored), tc.wantStored)
			}
		})
	}
}

func TestAccessControlAndValidation(t *testing.T) {
	h := newHarness(t, func(int, map[string]any) reply { return reply{blocks: []block{{text: "x"}}} }, nil)
	conv := h.newConv(t, "u1")
	tests := []struct {
		name         string
		method, path string
		user, body   string
		want         int
	}{
		{"list anonymous", "GET", "/api/assistant/conversations", "", "", 401},
		{"create anonymous", "POST", "/api/assistant/conversations", "", "{}", 401},
		{"send anonymous", "POST", "/api/assistant/conversations/" + conv + "/messages", "", `{"content":"hi"}`, 401},
		{"read anonymous", "GET", "/api/assistant/conversations/" + conv + "/messages", "", "", 401},
		{"other user cannot send", "POST", "/api/assistant/conversations/" + conv + "/messages", "u2", `{"content":"hi"}`, 404},
		{"other user cannot read", "GET", "/api/assistant/conversations/" + conv + "/messages", "u2", "", 404},
		{"unknown conversation", "POST", "/api/assistant/conversations/0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b/messages", "u1", `{"content":"hi"}`, 404},
		{"malformed id", "GET", "/api/assistant/conversations/not-a-uuid/messages", "u1", "", 200}, // memory store: empty owned check fails -> 404 below
		{"empty content", "POST", "/api/assistant/conversations/" + conv + "/messages", "u1", `{"content":"  "}`, 422},
		{"bad json", "POST", "/api/assistant/conversations/" + conv + "/messages", "u1", `nope`, 400},
		{"too long", "POST", "/api/assistant/conversations/" + conv + "/messages", "u1", `{"content":"` + strings.Repeat("a", 20000) + `"}`, 400},
		{"create with bad json", "POST", "/api/assistant/conversations", "u1", `[`, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := h.do(t, tc.method, tc.path, tc.user, tc.body, nil)
			want := tc.want
			if tc.name == "malformed id" {
				want = 404
			}
			if w.Code != want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, want, w.Body)
			}
			if w.Code >= 400 && w.Header().Get("Content-Type") != "application/problem+json" {
				t.Errorf("error content-type = %q", w.Header().Get("Content-Type"))
			}
		})
	}
	if h.fa.count() != 0 {
		t.Errorf("rejected requests reached the model: %d", h.fa.count())
	}
}

func TestConversationsAreScopedToUser(t *testing.T) {
	h := newHarness(t, func(int, map[string]any) reply { return reply{blocks: []block{{text: "x"}}} }, nil)
	a1 := h.newConv(t, "u1")
	h.newConv(t, "u1")
	b1 := h.newConv(t, "u2")

	list := func(user string) []map[string]any {
		w := h.do(t, "GET", "/api/assistant/conversations", user, "", nil)
		if w.Code != 200 {
			t.Fatalf("list = %d", w.Code)
		}
		var out []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if l := list("u1"); len(l) != 2 {
		t.Errorf("u1 sees %d conversations", len(l))
	}
	l2 := list("u2")
	if len(l2) != 1 || l2[0]["id"] != b1 {
		t.Errorf("u2 sees %v", l2)
	}
	c := list("u1")[0]
	for _, k := range []string{"id", "title", "created_at", "updated_at"} {
		if _, ok := c[k]; !ok {
			t.Errorf("conversation missing %s", k)
		}
	}
	if _, leaked := c["user_id"]; leaked {
		t.Error("user_id must not be exposed")
	}
	if _, err := time.Parse(time.RFC3339, c["created_at"].(string)); err != nil {
		t.Errorf("created_at not RFC 3339: %v", err)
	}
	if !strings.HasSuffix(c["created_at"].(string), "Z") {
		t.Errorf("created_at not UTC: %v", c["created_at"])
	}
	_ = a1
}

func TestTitleFromFirstMessage(t *testing.T) {
	h := newHarness(t, func(int, map[string]any) reply { return reply{blocks: []block{{text: "x"}}} }, nil)
	w := h.do(t, "POST", "/api/assistant/conversations", "u1", "", nil) // no body, no title
	var c map[string]any
	json.Unmarshal(w.Body.Bytes(), &c)
	id := c["id"].(string)
	h.send(t, "u1", id, "  Buy   milk\nand eggs  ", nil)
	got, _ := h.store.GetConversation(context.Background(), "u1", id)
	if got.Title != "Buy milk and eggs" {
		t.Errorf("title = %q", got.Title)
	}
}

func TestGetMessagesHidesThinking(t *testing.T) {
	h := newHarness(t, func(int, map[string]any) reply { return reply{} }, nil)
	conv := h.newConv(t, "u1")
	content := `[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"visible"}]`
	err := h.store.AppendMessages(context.Background(), "u1", conv, []assistant.Message{
		{ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", Role: "assistant", Content: json.RawMessage(content), CreatedAt: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	w := h.do(t, "GET", "/api/assistant/conversations/"+conv+"/messages", "u1", "", nil)
	if strings.Contains(w.Body.String(), "thinking") || !strings.Contains(w.Body.String(), "visible") {
		t.Errorf("body = %s", w.Body)
	}
}

func TestNewValidation(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "must-not-be-read")
	api, mux := testAPI()
	if _, err := assistant.New(api, mux, assistant.Options{Store: assistant.NewMemoryStore()}); err == nil {
		t.Error("New must not fall back to ANTHROPIC_API_KEY from the environment")
	}
	if _, err := assistant.New(api, mux, assistant.Options{APIKey: "k"}); err == nil {
		t.Error("New must require a store")
	}
	if o := assistant.OptionsFromEnv(); o.APIKey != "must-not-be-read" {
		t.Errorf("OptionsFromEnv should read ANTHROPIC_API_KEY, got %q", o.APIKey)
	}
}

func TestFirstParagraph(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"skips heading", "# Title\n\nFirst para line one.\nline two.\n\nSecond.", "First para line one. line two."},
		{"front matter", "---\ntitle: x\n---\n# H\nOnly para.", "Only para."},
		{"no heading", "Just text.\n\nMore.", "Just text."},
		{"heading ends para", "Intro\n## Next\nbody", "Intro"},
		{"empty", "", ""},
		{"only headings", "# A\n## B", ""},
		{"crlf", "# T\r\n\r\nWindows text.\r\n", "Windows text."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := assistant.FirstParagraph(tc.in); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestToolNamesAreWireSafe(t *testing.T) {
	h := newHarness(t, func(int, map[string]any) reply { return reply{} }, nil)
	got := strings.Join(h.a.ToolNames(), ",")
	if got != "get-item,whoami_v1" {
		t.Errorf("ToolNames = %s", got)
	}
}

func TestSystemPromptOverride(t *testing.T) {
	h := newHarness(t, func(int, map[string]any) reply { return reply{blocks: []block{{text: "x"}}} },
		func(o *assistant.Options) { o.SystemPrompt = "Custom prompt." })
	h.send(t, "u1", h.newConv(t, "u1"), "hi", nil)
	sys := h.fa.requests[0]["system"].([]any)[0].(map[string]any)["text"]
	if sys != "Custom prompt." {
		t.Errorf("system = %v", sys)
	}
}

func TestEventPayloadShapes(t *testing.T) {
	h := newHarness(t, func(n int, req map[string]any) reply {
		if n == 0 {
			return reply{blocks: []block{{text: "Checking."}, {tool: "get-item", input: `{"id":"missing"}`}}}
		}
		return reply{blocks: []block{{text: "Done."}}}
	}, nil)
	_, evs := h.send(t, "u1", h.newConv(t, "u1"), "go", nil)
	want := map[string][]string{ // event -> exact key set
		"text":        {"text"},
		"tool_call":   {"id", "input", "name"},
		"tool_result": {"content", "id", "is_error"}, // is_error only because this call failed
		"done":        {"stop_reason"},
	}
	seen := map[string]bool{}
	for _, e := range evs {
		w, ok := want[e.Name]
		if !ok {
			t.Errorf("unexpected event %q", e.Name)
			continue
		}
		seen[e.Name] = true
		var keys []string
		for k := range e.Data {
			keys = append(keys, k)
		}
		if !sameKeys(keys, w) {
			t.Errorf("%s payload keys = %v, want %v", e.Name, keys, w)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("event %s never sent", name)
		}
	}
	// error events carry {detail}
	h2 := newHarness(t, func(int, map[string]any) reply { return reply{status: 429} }, nil)
	_, evs = h2.send(t, "u1", h2.newConv(t, "u1"), "go", nil)
	if len(evs) != 1 || evs[0].Name != "error" || len(evs[0].Data) != 1 || evs[0].Data["detail"] == nil {
		t.Errorf("error event = %+v", evs)
	}
}

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, k := range a {
		m[k] = true
	}
	for _, k := range b {
		if !m[k] {
			return false
		}
	}
	return true
}
