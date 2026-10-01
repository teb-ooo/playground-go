// Package openapimcp turns a Huma API into an MCP server. Every non-hidden
// OpenAPI operation becomes exactly one MCP tool named after its operation id;
// calling the tool dispatches an in-process HTTP request to the app handler
// with the caller's credentials forwarded, so a tool can do exactly what the
// caller could do over HTTP and nothing more.
//
// # Input schema merge rule
//
// A tool's input schema is one JSON object built from the operation's inputs:
//
//   - path parameters and query parameters become top-level properties
//     (path parameters are always required);
//   - the request body, if any, is nested under the property "body";
//   - EXCEPT when the body is the operation's only input (no path or query
//     parameters) and is an object schema, in which case its properties are
//     flattened to the top level;
//   - header and cookie parameters are never exposed.
//
// If a path or query parameter is itself called "body" the operation cannot be
// represented unambiguously and construction fails with an error. Schemas
// that reference components are made self-contained under "$defs".
//
// # Instructions, resources and prompts
//
// Options.Instructions is returned to clients on initialize. Options.Resources
// (fixed URIs), Options.ResourceTemplates (RFC 6570 URI templates such as
// "lore://worlds/{id}/context-tray") and Options.Prompts are app-defined MCP
// resources and prompts. They are not operations: they are not tools and
// ParityCheck ignores them, so apps that do not set them are unchanged. Their
// handlers run as the caller: the same Options.Auth that wraps tool calls
// runs first (so auth.FromContext works, and a rejected caller gets an
// error), and surface.From(ctx) is surface.MCP. A handler returns text or a
// blob with a MIME type; ErrResourceNotFound maps to MCP's not-found error;
// MatchTemplate extracts the {variables} of a template from the URI.
//
// # Surface marker
//
// Every tool call runs with surface.With(ctx, surface.MCP) on the context of
// the in-process request, so an operation handler can tell an MCP caller from
// a browser with surface.From(ctx). See package surface.
package openapimcp
