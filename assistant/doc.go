// Package assistant is the end-user assistant: a chat endpoint whose model can
// use the app's own API as tools. It runs an anthropic-sdk-go tool-use loop
// over the tools openapimcp derives from the OpenAPI document, and executes
// each tool call in-process as the signed-in user by forwarding their
// Authorization header and session cookie, so the assistant can do exactly what
// that user can do and nothing more.
//
// Routes served by Handler (mount it at "/api/assistant/"):
//
//	GET  /api/assistant/conversations                    list the caller's conversations
//	POST /api/assistant/conversations                    create one
//	GET  /api/assistant/conversations/{id}/messages      read its messages
//	POST /api/assistant/conversations/{id}/messages      send a message; SSE reply
//
// The SSE stream carries one JSON object per data line and the event name is the
// type. Payloads (the shapes @teb-ooo/web's useEventStream<AssistantEvents>
// expects): text {text}; tool_call {id, name, input}; tool_result {id, content,
// is_error?} with is_error present only when true; done {stop_reason}; context {blocks} (only with Options.Context, see below). A failure
// mid-stream sends error {detail} and ends the stream. Defaults: model from ASSISTANT_MODEL
// (claude-sonnet-5-5), max_tokens 4096, at most 12 model calls per message.
//
// # Per-message context
//
// Options.Context is called once per user message, before the model call, with
// the user, conversation, text and request headers (ContextRequest) and
// returns ContextBlocks (kind, label, text, optional metadata, expose). The
// blocks are injected for that message only, as one clearly labelled text
// block placed before the user's own text in the user turn (not in the system
// prompt). Why: the system prompt, the tools and the replayed history stay a
// byte-stable prefix, so prompt caching keeps working; a cache breakpoint on
// the last block of the turn caches the injected context across the tool loop
// of that message; and the context sits next to the question, where the model
// uses it best. The text tells the model the items are reference data the user
// did not type, and closing tags inside them are neutralised. Blocks are never
// stored as the user's words and are not replayed from history on later
// messages: the hook runs again for every message.
//
// A `context` SSE event {blocks:[{kind,label,tokens_estimate,metadata?,text?}]}
// is sent before the model's first text event whenever the hook is configured
// (an empty list clears the tray). text is present only for blocks with
// Expose. tokens_estimate is about len(text)/4. A compact record of the same
// list is stored with the user message and returned as the message's
// `context` field by GET .../messages (the message's content stays just what
// the user typed). The hook failing, panicking or returning too much (50
// blocks, 32 KiB each, 256 KiB total) never breaks the chat: the client gets a
// {kind:"warning"} block and the model is called with what is usable. A hook
// that returns an error wrapping ErrAbort (assistant.Abort("reason")) stops the
// message instead: an `error` event, nothing stored, no model call.
//
// Options.SystemPromptFunc computes the system prompt per message from the
// user and conversation (keep it stable per conversation to keep the cache).
//
// Conversations are stored through the Store interface; PgxStore implements it
// on the two tables in migrations/00001_assistant.sql and MemoryStore is an
// in-memory implementation for tests. Every store call is scoped to the user.
//
// # Surface
//
// Tool calls the assistant dispatches run with surface.Assistant in the
// request context (package surface): an operation sees surface.From(ctx) ==
// surface.Assistant, never a client-supplied value.
package assistant
