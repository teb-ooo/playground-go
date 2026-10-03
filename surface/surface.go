// Package surface records, server-side, which door a request came in through,
// so an operation can treat a human at the UI differently from an AI acting
// through MCP or the in-app assistant ("AI authors in draft").
//
// # Reading it
//
// An operation's handler calls surface.From(ctx):
//
//	huma.Register(api, op, func(ctx context.Context, in *In) (*Out, error) {
//		if surface.From(ctx).IsAI() {
//			// store as a draft
//		}
//		...
//	})
//
// The values are UI (a browser session), API (any other direct HTTP caller,
// for example a script with a bearer token), Key (a direct HTTP caller
// authenticated by a platform API key, set by the auth package and AI like MCP), MCP (a tool call made through
// openapimcp) and Assistant (a tool call made by an app's own in-app assistant, which sets it itself with With).
// From never fails: a context nobody marked reads as API.
//
// # Why it cannot be forged
//
// The marker lives in the request context under an unexported key. A context
// is built inside the process, so nothing a client sends can set it. The
// dispatchers in this library (openapimcp.Server for MCP; an app's own assistant, if it has one, for its
// tools) call With on the context of the in-process request they build, which
// overrides whatever the outer request was classified as. The marker is NOT
// carried in a header between them, so there is no header to forge.
//
// Plain requests are classified by Middleware, which mounts outermost: a
// request that carries an Authorization header is API, anything else (a
// browser with its session cookie) is UI. Middleware also deletes
// the X-Playground-Surface header from every incoming request, so a handler,
// proxy rule or log line that looked at such a header would never see a
// client-supplied value. Apps must not read that header; use From.
//
// Mounting: wrap the app's top handler once, outside auth.
//
//	handler = surface.Middleware(mux)
//
// If an app does not mount Middleware, From still returns MCP and Assistant
// correctly for those two doors; every other request reads as API.
package surface

import (
	"context"
	"net/http"
)

// Surface names the door a request came through.
type Surface string

// The surfaces.
const (
	UI        Surface = "ui"
	API       Surface = "api"
	MCP       Surface = "mcp"
	Assistant Surface = "assistant"
	// Key is a direct HTTP call authenticated by a platform API key (pk_...):
	// an automation or AI caller. Set by the auth package, never by a client.
	Key Surface = "key"
)

// Header is the name of a header that clients might send to claim a surface.
// It is never trusted: Middleware strips it.
const Header = "X-Playground-Surface"

type ctxKey struct{}

// With returns ctx marked with s. Only server code that dispatches a request
// on behalf of someone else (the MCP server, the assistant) should call it.
func With(ctx context.Context, s Surface) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// From returns the surface in ctx, or API if none was set.
func From(ctx context.Context) Surface {
	if s, ok := ctx.Value(ctxKey{}).(Surface); ok && s != "" {
		return s
	}
	return API
}

// IsAI reports whether the request was made by an AI on the user's behalf
// (MCP, Assistant, or a direct call with a platform API key).
func (s Surface) IsAI() bool { return s == MCP || s == Assistant || s == Key }

// String implements fmt.Stringer.
func (s Surface) String() string { return string(s) }

// Middleware classifies plain requests (UI without an Authorization header,
// otherwise API) and strips the client-controllable Header. A marker already
// set in the request context by a dispatcher is kept.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(Header)
		if _, ok := r.Context().Value(ctxKey{}).(Surface); !ok {
			s := UI
			if r.Header.Get("Authorization") != "" {
				s = API
			}
			r = r.WithContext(With(r.Context(), s))
		}
		next.ServeHTTP(w, r)
	})
}
