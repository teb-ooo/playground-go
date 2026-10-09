package openapimcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/teb-ooo/playground-go/auth"
)

// ToolAllowed reports whether the caller in ctx may use t. Callers whose
// scopes are not enforced (anonymous, session, a token without app scopes)
// may use every tool, as before; a platform key or scoped OAuth token needs
// the tool's scope (see auth.ScopesEnforced). Public operations are always
// allowed.
func ToolAllowed(ctx context.Context, t *Tool) bool {
	return !t.Secured || auth.ScopeAllowed(ctx, t.Scope)
}

// scopeError is the text a client sees when it calls a tool its credential
// does not allow.
func scopeError(ctx context.Context, t *Tool) string {
	return "forbidden: tool " + t.Name + " needs the scope " + auth.QualifyScope(appOf(ctx), t.Scope) +
		", which this credential does not carry"
}

func appOf(ctx context.Context) string {
	// auth records the app name next to the credential; the empty case is
	// only reachable for a context nobody authenticated.
	return auth.AppFromContext(ctx)
}

// filterToolsList removes the tools the caller's scopes do not allow from a
// tools/list result.
func (s *Server) filterToolsList(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != "tools/list" || !auth.ScopesEnforced(ctx) {
			return res, err
		}
		if lr, ok := res.(*mcp.ListToolsResult); ok && lr != nil {
			kept := make([]*mcp.Tool, 0, len(lr.Tools))
			for _, t := range lr.Tools {
				if tool, ok := s.ts.Lookup(t.Name); ok && !ToolAllowed(ctx, tool) {
					continue
				}
				kept = append(kept, t)
			}
			lr.Tools = kept
		}
		return res, err
	}
}

// ---- protected resource metadata and the 401 challenge ----

// MetadataPath is the RFC 9728 well-known path of the protected resource
// metadata.
const MetadataPath = "/.well-known/oauth-protected-resource"

func (o Options) path() string {
	if o.Path == "" {
		return "/mcp"
	}
	return o.Path
}

func (o Options) metadataURL() string {
	if o.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(o.PublicURL, "/") + MetadataPath
}

// Metadata is the RFC 9728 document served for this MCP endpoint. It is nil
// when Options.PublicURL, App or AuthorizationServer is not set.
func (s *Server) metadataBody() []byte {
	o := s.opts
	if o.PublicURL == "" || o.App == "" || o.AuthorizationServer == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]any{
		"resource":                 strings.TrimRight(o.PublicURL, "/") + o.path(),
		"authorization_servers":    []string{strings.TrimRight(o.AuthorizationServer, "/")},
		"scopes_supported":         []string{o.App + ":read", o.App + ":write"},
		"bearer_methods_supported": []string{"header"},
	})
	return b
}

// MetadataHandler serves the protected resource metadata (RFC 9728) as JSON,
// or answers 404 when it is not configured. It is public and cacheable.
func (s *Server) MetadataHandler() http.Handler {
	body := s.metadataBody()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(body)
	})
}

// RegisterMetadata mounts the metadata on mux at MetadataPath and at the
// path-suffixed form RFC 9728 derives for the resource ("<MetadataPath>/mcp").
// The /mcp handler itself lives under the MCP path, so the app mounts these
// beside it:
//
//	mcpH := openapimcp.Handler(api, mux, opts)
//	mux.Handle("/mcp", mcpH)
//	mcpH.(*openapimcp.Server).RegisterMetadata(mux)
func (s *Server) RegisterMetadata(mux *http.ServeMux) {
	h := s.MetadataHandler()
	mux.Handle("GET "+MetadataPath, h)
	mux.Handle("GET "+MetadataPath+s.opts.path(), h)
}

func (s *Server) challenge(invalidToken bool) string {
	c := `Bearer resource_metadata="` + s.opts.metadataURL() + `"`
	if invalidToken {
		c += `, error="invalid_token"`
	}
	return c
}

// challengeWriter replaces the WWW-Authenticate header of a 401 with the one
// that points at the protected resource metadata.
type challengeWriter struct {
	http.ResponseWriter
	value string
}

func (w *challengeWriter) WriteHeader(code int) {
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", w.value)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *challengeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// serve identifies the caller (Options.Auth), answers a refused credential
// and, with RequireAuth, an anonymous call with 401 and the challenge, then
// hands over to the MCP transport. The identity is in the context the tool
// calls and the list filter see.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	// The tool calls run in-process; they need the outer client address so per-IP limits see the real client, not loopback.
	r = r.WithContext(withClient(r.Context(), client{remoteAddr: r.RemoteAddr}))
	if s.opts.metadataURL() != "" {
		w = &challengeWriter{ResponseWriter: w, value: s.challenge(r.Header.Get("Authorization") != "")}
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.RequireAuth {
			if _, ok := auth.FromContext(r.Context()); !ok {
				w.Header().Set("Content-Type", "application/problem+json")
				w.Header().Set("WWW-Authenticate", `Bearer`)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"Authentication is required to use this MCP endpoint."}`))
				return
			}
		}
		s.handler.ServeHTTP(w, r)
	})
	// An outer middleware (the template mounts auth.Middleware around the whole mux) may have identified the caller already: then
	// the credential is not verified a second time at the door.
	if s.opts.Auth != nil && auth.CredentialFromContext(r.Context()) == "" {
		s.opts.Auth(inner).ServeHTTP(w, r)
		return
	}
	inner.ServeHTTP(w, r)
}

// client is the outer request's network identity, carried to the in-process dispatch.
type client struct{ remoteAddr string }

type clientKey struct{}

func withClient(ctx context.Context, c client) context.Context {
	return context.WithValue(ctx, clientKey{}, c)
}

func clientFrom(ctx context.Context) (client, bool) {
	c, ok := ctx.Value(clientKey{}).(client)
	return c, ok
}
