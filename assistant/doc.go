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
// is_error?} with is_error present only when true; done {stop_reason}. A failure
// mid-stream sends error {detail} and ends the stream. Defaults: model from ASSISTANT_MODEL
// (claude-sonnet-5-5), max_tokens 4096, at most 12 model calls per message.
//
// Conversations are stored through the Store interface; PgxStore implements it
// on the two tables in migrations/00001_assistant.sql and MemoryStore is an
// in-memory implementation for tests. Every store call is scoped to the user.
package assistant
