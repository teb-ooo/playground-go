package openapimcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/teb-ooo/playground-go/surface"
)

// ErrResourceNotFound is what a ResourceHandler returns for a URI it does not
// know (for example lore://worlds/{id}/context-tray with an id the caller
// cannot see). The client gets MCP's "resource not found" error.
var ErrResourceNotFound = errors.New("openapimcp: resource not found")

// ResourceContent is what a ResourceHandler returns: Text or Blob, and an
// optional MIME type (the resource's own MIMEType is used if empty).
type ResourceContent struct {
	Text     string
	Blob     []byte
	MIMEType string
}

// ResourceHandler reads one resource. ctx carries the caller exactly as an
// operation's handler would see it (the same Options.Auth runs first, so
// auth.FromContext works, and surface.From(ctx) is surface.MCP). uri is the
// full requested URI; for a template use MatchTemplate to pull the variables.
type ResourceHandler func(ctx context.Context, uri string) (ResourceContent, error)

// Resource is a fixed, app-defined MCP resource.
type Resource struct {
	URI         string // absolute, for example "lore://about"
	Name        string
	Title       string
	Description string
	MIMEType    string // default "text/plain"
	Read        ResourceHandler
}

// ResourceTemplate is an app-defined family of resources, an RFC 6570 URI
// template such as "lore://worlds/{id}/context-tray".
type ResourceTemplate struct {
	URITemplate string
	Name        string
	Title       string
	Description string
	MIMEType    string // default "text/plain"
	Read        ResourceHandler
}

// PromptArgument describes one argument of a Prompt.
type PromptArgument struct {
	Name        string
	Description string
	Required    bool
}

// PromptMessage is one message a prompt expands to. Role is "user" or
// "assistant" (default "user").
type PromptMessage struct {
	Role string
	Text string
}

// Prompt is an app-defined MCP prompt. Get runs as the caller (see
// ResourceHandler). Missing required arguments are rejected before Get runs.
type Prompt struct {
	Name        string
	Title       string
	Description string
	Arguments   []PromptArgument
	Get         func(ctx context.Context, args map[string]string) ([]PromptMessage, error)
}

// MatchTemplate matches uri against a URI template made of literal text and
// simple {name} variables (no RFC 6570 operators) and returns the variables.
// A variable matches one or more characters other than "/", "?" and "#".
func MatchTemplate(template, uri string) (map[string]string, bool) {
	var names []string
	var re strings.Builder
	re.WriteString("^")
	rest := template
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			re.WriteString(regexp.QuoteMeta(rest))
			break
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return nil, false
		}
		re.WriteString(regexp.QuoteMeta(rest[:i]))
		names = append(names, rest[i+1:i+j])
		re.WriteString(`([^/?#]+)`)
		rest = rest[i+j+1:]
	}
	re.WriteString("$")
	m := regexp.MustCompile(re.String()).FindStringSubmatch(uri)
	if m == nil {
		return nil, false
	}
	out := make(map[string]string, len(names))
	for k, n := range names {
		out[n] = m[k+1]
	}
	return out, true
}

// asCaller runs fn as the MCP caller: a synthetic in-process request carrying
// the caller's forwarded credentials goes through the same Auth wrapper the
// tool calls use, and fn runs with that request's context (user attached,
// surface set to MCP). If Auth answers the request itself (a rejection), fn
// does not run and the status is returned as an error.
func asCaller(ctx context.Context, auth func(http.Handler) http.Handler, hdr http.Header, fn func(ctx context.Context) error) error {
	ctx = surface.With(ctx, surface.MCP)
	if auth == nil {
		return fn(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return err
	}
	req.Host = "localhost"
	req.RemoteAddr = "127.0.0.1:0"
	for _, h := range forwardedHeaders {
		for _, v := range hdr.Values(h) {
			req.Header.Add(h, v)
		}
	}
	ran := false
	var ferr error
	rec := httptest.NewRecorder()
	auth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ran = true
		ferr = fn(surface.With(r.Context(), surface.MCP))
	})).ServeHTTP(rec, req)
	if !ran {
		return fmt.Errorf("authentication failed (HTTP %d)", rec.Code)
	}
	return ferr
}

func extraHeader(extra *mcp.RequestExtra) http.Header {
	if extra == nil {
		return nil
	}
	return extra.Header
}

func readHandler(auth func(http.Handler) http.Handler, defMIME string, read ResourceHandler) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		var c ResourceContent
		err := asCaller(ctx, auth, extraHeader(req.Extra), func(ctx context.Context) error {
			var e error
			c, e = read(ctx, uri)
			return e
		})
		if errors.Is(err, ErrResourceNotFound) {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		if err != nil {
			return nil, err
		}
		mime := c.MIMEType
		if mime == "" {
			mime = defMIME
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: mime, Text: c.Text, Blob: c.Blob}}}, nil
	}
}

// addExtras registers resources, templates and prompts on srv.
func addExtras(srv *mcp.Server, opts Options) error {
	for _, r := range opts.Resources {
		if r.URI == "" || r.Name == "" || r.Read == nil {
			return fmt.Errorf("openapimcp: resource %q needs URI, Name and Read", r.URI)
		}
		mime := orDefault(r.MIMEType, "text/plain")
		srv.AddResource(&mcp.Resource{URI: r.URI, Name: r.Name, Title: r.Title, Description: r.Description, MIMEType: mime},
			readHandler(opts.Auth, mime, r.Read))
	}
	for _, t := range opts.ResourceTemplates {
		if t.URITemplate == "" || t.Name == "" || t.Read == nil {
			return fmt.Errorf("openapimcp: resource template %q needs URITemplate, Name and Read", t.URITemplate)
		}
		mime := orDefault(t.MIMEType, "text/plain")
		srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: t.URITemplate, Name: t.Name, Title: t.Title, Description: t.Description, MIMEType: mime},
			readHandler(opts.Auth, mime, t.Read))
	}
	seen := map[string]bool{}
	for _, p := range opts.Prompts {
		if p.Name == "" || p.Get == nil {
			return fmt.Errorf("openapimcp: prompt %q needs Name and Get", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("openapimcp: duplicate prompt %q", p.Name)
		}
		seen[p.Name] = true
		p := p
		mp := &mcp.Prompt{Name: p.Name, Title: p.Title, Description: p.Description}
		for _, a := range p.Arguments {
			mp.Arguments = append(mp.Arguments, &mcp.PromptArgument{Name: a.Name, Description: a.Description, Required: a.Required})
		}
		srv.AddPrompt(mp, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			for _, a := range p.Arguments {
				if a.Required && req.Params.Arguments[a.Name] == "" {
					return nil, fmt.Errorf("missing required argument %q", a.Name)
				}
			}
			var msgs []PromptMessage
			err := asCaller(ctx, opts.Auth, extraHeader(req.Extra), func(ctx context.Context) error {
				var e error
				msgs, e = p.Get(ctx, req.Params.Arguments)
				return e
			})
			if err != nil {
				return nil, err
			}
			res := &mcp.GetPromptResult{Description: p.Description}
			for _, m := range msgs {
				role := mcp.Role("user")
				if m.Role == "assistant" {
					role = "assistant"
				}
				res.Messages = append(res.Messages, &mcp.PromptMessage{Role: role, Content: &mcp.TextContent{Text: m.Text}})
			}
			return res, nil
		})
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
