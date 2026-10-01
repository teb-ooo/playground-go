package openapimcp

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/teb-ooo/playground-go/surface"
)

// Options configures the MCP server.
type Options struct {
	// Name and Version identify the server to MCP clients. Name defaults to
	// the API's title and Version to the API's version.
	Name    string
	Version string
	// Auth, if set, wraps the in-process dispatch of every tool call. It is
	// where the caller's forwarded Authorization header or session cookie is
	// turned into a user in the request context: pass auth.BearerOrSession.
	// It runs twice: around the endpoint itself, where it identifies the
	// caller (a rejected credential is a 401 at the door, and tools/list is
	// filtered by the caller's scopes), and around each tool call. The
	// endpoint does not reject anonymous initialize or tools/list calls
	// (the OpenAPI document is public anyway) unless RequireAuth is set; each
	// operation enforces its own access rules when it runs.
	Auth func(http.Handler) http.Handler
	// Instructions is optional guidance returned to MCP clients on initialize.
	Instructions string
	// Resources, ResourceTemplates and Prompts are app-defined MCP resources
	// and prompts (see extras.go). They are not operations: ParityCheck does
	// not look at them. Each handler runs as the caller, behind Auth.
	Resources         []Resource
	ResourceTemplates []ResourceTemplate
	Prompts           []Prompt

	// PublicURL (the app's external origin), App (its name) and
	// AuthorizationServer (https://oidc.<domain>) turn on the OAuth discovery
	// MCP clients need: the protected resource metadata (RFC 9728, see
	// Server.RegisterMetadata) and, on every 401 from the endpoint, the
	// header WWW-Authenticate: Bearer resource_metadata="<PublicURL>/.well-known/oauth-protected-resource".
	// playground.Config.MCPOptions fills them in. Empty means off.
	PublicURL           string
	App                 string
	AuthorizationServer string
	// Path is where the app mounts the handler (default /mcp), for the
	// resource URL in the metadata.
	Path string
	// RequireAuth answers a call with no credential at all with 401 and the
	// challenge, as the MCP authorization spec expects (a client starts its
	// OAuth sign-in from that 401). The default is off: anonymous initialize
	// and tools/list stay public, and each operation enforces its own access
	// when a tool is called. An app whose tools all need a user should set it.
	RequireAuth bool
}

// Server is an MCP server over the tools derived from a Huma API. It
// implements http.Handler (streamable HTTP transport, stateless, JSON
// responses).
type Server struct {
	ts      *Toolset
	handler http.Handler
	opts    Options
}

// New derives the tools from api and builds the server. Register every API
// operation before calling New; operations added later are not exposed.
// app is the handler that executes operations in-process, typically the
// ServeMux the API was registered on.
func New(api huma.API, app http.Handler, opts Options) (*Server, error) {
	ts, err := NewToolset(api)
	if err != nil {
		return nil, err
	}
	name, version := opts.Name, opts.Version
	if info := api.OpenAPI().Info; info != nil {
		if name == "" {
			name = info.Title
		}
		if version == "" {
			version = info.Version
		}
	}
	if name == "" {
		name = "playground-app"
	}
	if version == "" {
		version = "0.0.0"
	}

	dispatch := app
	if opts.Auth != nil {
		dispatch = opts.Auth(app)
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, &mcp.ServerOptions{Instructions: opts.Instructions})
	if err := addExtras(srv, opts); err != nil {
		return nil, err
	}
	for _, t := range ts.Tools() {
		t := t
		srv.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var hdr http.Header
				if req.Extra != nil {
					hdr = req.Extra.Header
				}
				// Server-side marker: the dispatched request's context says "mcp",
				// whatever the client sent (see package surface).
				if !ToolAllowed(ctx, t) {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: scopeError(ctx, t)}}}, nil
				}
				res, err := t.Call(surface.With(ctx, surface.MCP), dispatch, hdr, req.Params.Arguments)
				if err != nil {
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
					}, nil
				}
				return &mcp.CallToolResult{
					IsError: res.IsError,
					Content: []mcp.Content{&mcp.TextContent{Text: res.Text()}},
				}, nil
			})
	}

	s := &Server{ts: ts, opts: opts}
	srv.AddReceivingMiddleware(s.filterToolsList)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	s.handler = h
	return s, nil
}

// Handler is New for callers that treat a broken API definition as a
// programming error: it panics if the tools cannot be derived. The returned
// value also has a Tools() []string method.
func Handler(api huma.API, app http.Handler, opts Options) http.Handler {
	s, err := New(api, app, opts)
	if err != nil {
		panic(fmt.Sprintf("openapimcp: %v", err))
	}
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.serve(w, r) }

// Tools returns the sorted tool names, for the parity test.
func (s *Server) Tools() []string { return s.ts.Names() }

// Toolset returns the derived tools, for callers such as the assistant.
func (s *Server) Toolset() *Toolset { return s.ts }
