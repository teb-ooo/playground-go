package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/danielgtaylor/huma/v2"

	"github.com/teb-ooo/factory-go/auth"
	"github.com/teb-ooo/factory-go/internal/problem"
	"github.com/teb-ooo/factory-go/internal/uuidv7"
	"github.com/teb-ooo/factory-go/openapimcp"
)

// Defaults.
const (
	// DefaultModel is the factory-wide assistant model (BOOTSTRAP section 6b).
	DefaultModel = "claude-sonnet-5-5"
	// DefaultMaxTokens is max_tokens per model call.
	DefaultMaxTokens = 4096
	// DefaultMaxIterations is the most model calls made for one user message.
	DefaultMaxIterations = 12

	maxUserMessageBytes = 16 << 10
	maxToolResultBytes  = 64 << 10
)

// Options configures the assistant.
type Options struct {
	// AppName names the app in the system prompt.
	AppName string
	// Model defaults to DefaultModel (ASSISTANT_MODEL via OptionsFromEnv).
	Model string
	// APIKey is the Anthropic API key (ANTHROPIC_API_KEY via OptionsFromEnv).
	// The package never reads the environment itself.
	APIKey string
	// Store persists conversations. Required.
	Store Store
	// MaxTokens per model call (default 4096).
	MaxTokens int64
	// MaxIterations is the most model calls per user message (default 12).
	MaxIterations int
	// Effort is the output_config.effort sent with each call: low, medium,
	// high, xhigh, max, or "none" to omit it. Default "low": it keeps the
	// model's reasoning small so the 4096 max_tokens are spent on the answer.
	Effort string
	// Spec is the text of SPEC.md; its first paragraph goes in the system
	// prompt. If empty, SpecFile is read if it exists.
	Spec string
	// SpecFile is the path of SPEC.md (default "SPEC.md"); a missing file is fine.
	SpecFile string
	// SystemPrompt replaces the generated system prompt entirely.
	SystemPrompt string
	// Auth wraps the in-process dispatch of tool calls (see openapimcp.Options.Auth).
	// Usually auth.BearerOrSession. Not needed when the request context already
	// carries the user, which is the case behind auth.Middleware.
	Auth func(http.Handler) http.Handler
	// ClientOptions are extra anthropic-sdk-go request options (tests use
	// option.WithBaseURL to point at a fake server).
	ClientOptions []option.RequestOption
	// Now replaces time.Now, for tests.
	Now func() time.Time
}

// OptionsFromEnv fills AppName (APP_NAME), Model (ASSISTANT_MODEL) and APIKey
// (ANTHROPIC_API_KEY) from the environment.
func OptionsFromEnv() Options {
	return Options{AppName: os.Getenv("APP_NAME"), Model: os.Getenv("ASSISTANT_MODEL"), APIKey: os.Getenv("ANTHROPIC_API_KEY")}
}

// Assistant serves the assistant routes.
type Assistant struct {
	opts   Options
	client anthropic.Client
	ts     *openapimcp.Toolset
	tools  []anthropic.ToolUnionParam
	// wire name (Anthropic's constraints) -> operation id
	toolIDs map[string]string
	system  string
	app     http.Handler
	mux     *http.ServeMux
}

var wireNameRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// New builds the assistant from api's operations. Register every operation
// before calling New. app is the handler tool calls are dispatched to
// (typically the ServeMux api was registered on).
func New(api huma.API, app http.Handler, opts Options) (*Assistant, error) {
	if opts.Store == nil {
		return nil, errors.New("assistant: Options.Store is required")
	}
	if opts.APIKey == "" {
		return nil, errors.New("assistant: Options.APIKey (ANTHROPIC_API_KEY) is required")
	}
	if opts.Model == "" {
		opts.Model = DefaultModel
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = DefaultMaxTokens
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = DefaultMaxIterations
	}
	if opts.Effort == "" {
		opts.Effort = "low"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Spec == "" {
		f := opts.SpecFile
		if f == "" {
			f = "SPEC.md"
		}
		if b, err := os.ReadFile(f); err == nil {
			opts.Spec = string(b)
		}
	}

	ts, err := openapimcp.NewToolset(api)
	if err != nil {
		return nil, fmt.Errorf("assistant: deriving tools: %w", err)
	}
	a := &Assistant{opts: opts, ts: ts, app: app, toolIDs: map[string]string{}}
	if opts.Auth != nil {
		a.app = opts.Auth(app)
	}
	for _, t := range ts.Tools() {
		wire := wireName(t.Name)
		if prev, dup := a.toolIDs[wire]; dup {
			return nil, fmt.Errorf("assistant: operation ids %q and %q map to the same tool name %q", prev, t.Name, wire)
		}
		a.toolIDs[wire] = t.Name
		schema := anthropic.ToolInputSchemaParam{ExtraFields: map[string]any{}}
		if p, ok := t.InputSchema["properties"]; ok {
			schema.Properties = p
		}
		if r, ok := t.InputSchema["required"].([]string); ok {
			schema.Required = r
		}
		if d, ok := t.InputSchema["$defs"]; ok {
			schema.ExtraFields["$defs"] = d
		}
		a.tools = append(a.tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name: wire, Description: anthropic.String(t.Description), InputSchema: schema,
		}})
	}
	a.system = opts.SystemPrompt
	if a.system == "" {
		a.system = buildSystemPrompt(opts.AppName, opts.Spec, ts)
	}

	a.client = anthropic.NewClient(append([]option.RequestOption{option.WithAPIKey(opts.APIKey)}, opts.ClientOptions...)...)

	a.mux = http.NewServeMux()
	a.mux.HandleFunc("GET /api/assistant/conversations", a.listConversations)
	a.mux.HandleFunc("POST /api/assistant/conversations", a.createConversation)
	a.mux.HandleFunc("GET /api/assistant/conversations/{id}/messages", a.listMessages)
	a.mux.HandleFunc("POST /api/assistant/conversations/{id}/messages", a.postMessage)
	return a, nil
}

// Handler is New for callers that treat a misconfiguration as a programming
// error: it panics if New fails. Mount the result at "/api/assistant/".
func Handler(api huma.API, app http.Handler, opts Options) http.Handler {
	a, err := New(api, app, opts)
	if err != nil {
		panic(fmt.Sprintf("assistant: %v", err))
	}
	return a
}

// ServeHTTP implements http.Handler.
func (a *Assistant) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// SystemPrompt returns the system prompt in use.
func (a *Assistant) SystemPrompt() string { return a.system }

// ToolNames returns the names the model sees, sorted by operation id.
func (a *Assistant) ToolNames() []string {
	var out []string
	for _, t := range a.ts.Tools() {
		out = append(out, wireName(t.Name))
	}
	return out
}

func wireName(id string) string {
	n := wireNameRE.ReplaceAllString(id, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// ---- HTTP handlers ----

func (a *Assistant) user(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	u, ok := auth.FromContext(r.Context())
	if !ok {
		problem.Write(w, http.StatusUnauthorized, "Unauthorized", "authentication required")
		return auth.User{}, false
	}
	return u, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type conversationView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func viewOf(c Conversation) conversationView {
	return conversationView{ID: c.ID, Title: c.Title,
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339)}
}

func (a *Assistant) listConversations(w http.ResponseWriter, r *http.Request) {
	u, ok := a.user(w, r)
	if !ok {
		return
	}
	cs, err := a.opts.Store.ListConversations(r.Context(), u.Subject, 200)
	if err != nil {
		problem.Write(w, http.StatusInternalServerError, "Could not list conversations", "The conversation store failed.")
		return
	}
	out := make([]conversationView, 0, len(cs))
	for _, c := range cs {
		out = append(out, viewOf(c))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func (a *Assistant) createConversation(w http.ResponseWriter, r *http.Request) {
	u, ok := a.user(w, r)
	if !ok {
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if r.Body != nil {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			problem.Write(w, http.StatusBadRequest, "Bad request", "The request body is too large or unreadable.")
			return
		}
		if len(strings.TrimSpace(string(b))) > 0 {
			if err := json.Unmarshal(b, &body); err != nil {
				problem.Write(w, http.StatusBadRequest, "Bad request", "The request body must be JSON like {\"title\": \"...\"}.")
				return
			}
		}
	}
	now := a.opts.Now().UTC()
	c := Conversation{ID: uuidv7.New(), UserID: u.Subject, Title: truncateRunes(strings.TrimSpace(body.Title), 80), CreatedAt: now, UpdatedAt: now}
	if err := a.opts.Store.CreateConversation(r.Context(), c); err != nil {
		problem.Write(w, http.StatusInternalServerError, "Could not create the conversation", "The conversation store failed.")
		return
	}
	writeJSON(w, http.StatusCreated, viewOf(c))
}

type messageView struct {
	ID        string           `json:"id"`
	Role      string           `json:"role"`
	Content   []map[string]any `json:"content"`
	CreatedAt string           `json:"created_at"`
}

func (a *Assistant) listMessages(w http.ResponseWriter, r *http.Request) {
	u, ok := a.user(w, r)
	if !ok {
		return
	}
	msgs, err := a.opts.Store.ListMessages(r.Context(), u.Subject, r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		problem.Write(w, http.StatusNotFound, "Not found", "No such conversation.")
		return
	}
	if err != nil {
		problem.Write(w, http.StatusInternalServerError, "Could not read the conversation", "The conversation store failed.")
		return
	}
	out := make([]messageView, 0, len(msgs))
	for _, m := range msgs {
		var blocks []map[string]any
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		visible := blocks[:0]
		for _, b := range blocks {
			if t, _ := b["type"].(string); t == "thinking" || t == "redacted_thinking" {
				continue
			}
			visible = append(visible, b)
		}
		out = append(out, messageView{ID: m.ID, Role: m.Role, Content: visible, CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339)})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// ---- SSE ----

type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (s *sse) send(event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (a *Assistant) postMessage(w http.ResponseWriter, r *http.Request) {
	u, ok := a.user(w, r)
	if !ok {
		return
	}
	convID := r.PathValue("id")
	conv, err := a.opts.Store.GetConversation(r.Context(), u.Subject, convID)
	if errors.Is(err, ErrNotFound) {
		problem.Write(w, http.StatusNotFound, "Not found", "No such conversation.")
		return
	}
	if err != nil {
		problem.Write(w, http.StatusInternalServerError, "Could not read the conversation", "The conversation store failed.")
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUserMessageBytes+1024))
	if err := dec.Decode(&body); err != nil {
		problem.Write(w, http.StatusBadRequest, "Bad request", "The request body must be JSON like {\"content\": \"...\"}.")
		return
	}
	body.Content = strings.TrimSpace(body.Content)
	if body.Content == "" || len(body.Content) > maxUserMessageBytes {
		problem.Write(w, http.StatusUnprocessableEntity, "Invalid message", fmt.Sprintf("content must be between 1 and %d bytes.", maxUserMessageBytes))
		return
	}

	history, err := a.loadHistory(r.Context(), u.Subject, convID)
	if err != nil {
		problem.Write(w, http.StatusInternalServerError, "Could not read the conversation", "The conversation store failed.")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	out := &sse{w: w, rc: http.NewResponseController(w)}
	_ = out.rc.Flush()

	a.run(r.Context(), out, u, r.Header, conv, history, body.Content)
}

func (a *Assistant) loadHistory(ctx context.Context, userID, convID string) ([]anthropic.MessageParam, error) {
	stored, err := a.opts.Store.ListMessages(ctx, userID, convID)
	if err != nil {
		return nil, err
	}
	out := make([]anthropic.MessageParam, 0, len(stored))
	for _, m := range stored {
		var blocks []anthropic.ContentBlockParamUnion
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil, fmt.Errorf("assistant: stored message %s is unreadable: %w", m.ID, err)
		}
		out = append(out, anthropic.MessageParam{Role: anthropic.MessageParamRole(m.Role), Content: blocks})
	}
	return out, nil
}

func (a *Assistant) toStored(convID string, p anthropic.MessageParam) (Message, error) {
	b, err := json.Marshal(p.Content)
	if err != nil {
		return Message{}, fmt.Errorf("assistant: encoding message: %w", err)
	}
	return Message{ID: uuidv7.New(), ConversationID: convID, Role: string(p.Role), Content: b, CreatedAt: a.opts.Now().UTC()}, nil
}

func (a *Assistant) persist(ctx context.Context, userID, convID string, ps ...anthropic.MessageParam) error {
	msgs := make([]Message, 0, len(ps))
	for _, p := range ps {
		m, err := a.toStored(convID, p)
		if err != nil {
			return err
		}
		msgs = append(msgs, m)
	}
	// Persist even if the client has gone away: the model call already happened.
	return a.opts.Store.AppendMessages(context.WithoutCancel(ctx), userID, convID, msgs)
}

// run is the tool-use loop for one user message.
func (a *Assistant) run(ctx context.Context, out *sse, u auth.User, hdr http.Header, conv Conversation, history []anthropic.MessageParam, text string) {
	fail := func(detail string) { _ = out.send("error", map[string]any{"detail": detail}) }

	userMsg := anthropic.NewUserMessage(anthropic.NewTextBlock(text))
	if err := a.persist(ctx, u.Subject, conv.ID, userMsg); err != nil {
		fail("The message could not be saved.")
		return
	}
	if conv.Title == "" {
		// Best effort; a missing title is cosmetic.
		_ = a.opts.Store.SetConversationTitle(context.WithoutCancel(ctx), u.Subject, conv.ID, truncateRunes(strings.Join(strings.Fields(text), " "), 60))
	}
	msgs := append(append([]anthropic.MessageParam(nil), history...), userMsg)

	system := []anthropic.TextBlockParam{{Text: a.system, CacheControl: anthropic.NewCacheControlEphemeralParam()}}
	iterations := 0
	for iterations < a.opts.MaxIterations {
		iterations++
		params := anthropic.MessageNewParams{
			Model: anthropic.Model(a.opts.Model), MaxTokens: a.opts.MaxTokens,
			System: system, Messages: msgs, Tools: a.tools,
		}
		if a.opts.Effort != "none" {
			params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(a.opts.Effort)}
		}

		stream := a.client.Messages.NewStreaming(ctx, params)
		var msg anthropic.Message
		for stream.Next() {
			ev := stream.Current()
			if err := msg.Accumulate(ev); err != nil {
				_ = stream.Close()
				fail("The model's reply could not be read.")
				return
			}
			if d, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
				if td, ok := d.Delta.AsAny().(anthropic.TextDelta); ok && td.Text != "" {
					if err := out.send("text", map[string]any{"text": td.Text}); err != nil {
						_ = stream.Close()
						return // client went away
					}
				}
			}
		}
		streamErr := stream.Err()
		_ = stream.Close()
		if streamErr != nil {
			if ctx.Err() != nil {
				return
			}
			fail(describeAPIError(streamErr))
			return
		}

		assistantMsg := msg.ToParam()
		if len(assistantMsg.Content) == 0 {
			assistantMsg = anthropic.NewAssistantMessage(anthropic.NewTextBlock("(No response.)"))
		}

		switch msg.StopReason {
		case anthropic.StopReasonToolUse:
			results := a.executeTools(ctx, out, hdr, msg)
			if ctx.Err() != nil {
				return // do not persist a tool_use without its results
			}
			resultMsg := anthropic.NewUserMessage(results...)
			if err := a.persist(ctx, u.Subject, conv.ID, assistantMsg, resultMsg); err != nil {
				fail("The reply could not be saved.")
				return
			}
			msgs = append(msgs, assistantMsg, resultMsg)
			continue

		case anthropic.StopReasonRefusal:
			const refused = "I can't help with that request."
			_ = out.send("text", map[string]any{"text": refused})
			if err := a.persist(ctx, u.Subject, conv.ID, anthropic.NewAssistantMessage(anthropic.NewTextBlock(refused))); err != nil {
				fail("The reply could not be saved.")
				return
			}
			_ = out.send("done", doneEvent("refusal"))
			return

		default:
			if err := a.persist(ctx, u.Subject, conv.ID, assistantMsg); err != nil {
				fail("The reply could not be saved.")
				return
			}
			_ = out.send("done", doneEvent(string(msg.StopReason)))
			return
		}
	}

	// Iteration budget spent while the model still wanted tools. The last
	// stored message is a tool_result turn, so close the exchange with text
	// to keep the history valid.
	note := fmt.Sprintf("I stopped after %d steps for one message. Tell me to continue if you want me to keep going.", a.opts.MaxIterations)
	_ = out.send("text", map[string]any{"text": note})
	if err := a.persist(ctx, u.Subject, conv.ID, anthropic.NewAssistantMessage(anthropic.NewTextBlock(note))); err != nil {
		fail("The reply could not be saved.")
		return
	}
	_ = out.send("done", doneEvent("max_iterations"))
}

// doneEvent is exactly {stop_reason}, the shape the web package's
// useEventStream<AssistantEvents> expects.
func doneEvent(stop string) map[string]any {
	return map[string]any{"stop_reason": stop}
}

func describeAPIError(err error) string {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 401, 403:
			return "The assistant is not configured correctly (the model service rejected its credentials)."
		case 429:
			return "The assistant is busy. Try again in a moment."
		case 529:
			return "The model service is overloaded. Try again in a moment."
		}
		return fmt.Sprintf("The model service returned an error (status %d).", apiErr.StatusCode)
	}
	return "The model service could not be reached."
}

// executeTools runs every tool_use block in msg in order, in-process as the
// caller, and returns the tool_result blocks for one user message.
func (a *Assistant) executeTools(ctx context.Context, out *sse, hdr http.Header, msg anthropic.Message) []anthropic.ContentBlockParamUnion {
	var results []anthropic.ContentBlockParamUnion
	for _, block := range msg.Content {
		tu, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			continue
		}
		input := tu.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		var shown any
		if err := json.Unmarshal(input, &shown); err != nil {
			shown = string(input)
		}
		opID, known := a.toolIDs[tu.Name]
		_ = out.send("tool_call", map[string]any{"id": tu.ID, "name": tu.Name, "input": shown})

		var content string
		isErr := false
		switch {
		case !known:
			content, isErr = fmt.Sprintf("unknown tool %q", tu.Name), true
		default:
			res, err := a.ts.Call(ctx, a.app, hdr, opID, input)
			switch {
			case err != nil:
				content, isErr = err.Error(), true
			default:
				content, isErr = res.Text(), res.IsError
			}
		}
		if len(content) > maxToolResultBytes {
			content = content[:maxToolResultBytes] + "\n[truncated]"
		}
		payload := map[string]any{"id": tu.ID, "content": content}
		if isErr {
			payload["is_error"] = true // optional field: present only when true
		}
		_ = out.send("tool_result", payload)
		results = append(results, anthropic.NewToolResultBlock(tu.ID, content, isErr))
	}
	return results
}
