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
// # Scopes, discovery and the 401 challenge
//
// A caller whose scopes are enforced (a platform API key, an OAuth token with
// app scopes; see package auth) sees only the tools its scopes allow in
// tools/list (tool scope: read for GET and HEAD, write otherwise, or the
// operation's x-scope extension; public operations are always listed), and a
// tools/call of any other tool answers a tool error naming the missing scope.
// Anonymous callers, sessions and unscoped tokens see every tool, as before.
// This needs Options.Auth: the endpoint runs it to identify the caller.
//
// Options.PublicURL, App and AuthorizationServer (Config.MCPOptions fills
// them) turn on OAuth discovery for MCP clients. Server.RegisterMetadata
// serves the RFC 9728 protected resource metadata at
// /.well-known/oauth-protected-resource (and the path-suffixed form):
// {resource: <PublicURL>/mcp, authorization_servers, scopes_supported:
// [<app>:read, <app>:write], bearer_methods_supported: ["header"]}. Every 401
// of the endpoint carries WWW-Authenticate: Bearer
// resource_metadata="<PublicURL>/.well-known/oauth-protected-resource". With
// Options.RequireAuth a call with no credential at all is such a 401, which is
// what starts a client's OAuth sign-in; by default anonymous initialize and
// tools/list stay public. The metadata is not an operation: ParityCheck is
// unaffected.
//
// # Surface marker
//
// Every tool call runs with surface.With(ctx, surface.MCP) on the context of
// the in-process request, so an operation handler can tell an MCP caller from
// a browser with surface.From(ctx). See package surface.
package openapimcp
