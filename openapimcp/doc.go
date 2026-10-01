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
// # Surface marker
//
// Every tool call runs with surface.With(ctx, surface.MCP) on the context of
// the in-process request, so an operation handler can tell an MCP caller from
// a browser with surface.From(ctx). See package surface.
package openapimcp
