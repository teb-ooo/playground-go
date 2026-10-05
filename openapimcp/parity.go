package openapimcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ParityOption customises ParityCheck.
type ParityOption func(*parityConfig)

type parityConfig struct {
	header http.Header
	exempt map[string]bool
}

// WithExempt exempts the named operation ids from the contract checks (OperationID form, Path, Summary,
// Description, Tags, Security, snake_case properties, date-time format and id rule). It is the documented way to
// accept a known exception, for example a protocol ceremony whose wire shape is not ours. The id-to-tool
// equality check still runs. Prefer the per-operation Extension ExemptExtension, which keeps the reason next to
// the operation.
func WithExempt(operationIDs ...string) ParityOption {
	return func(c *parityConfig) {
		for _, id := range operationIDs {
			c.exempt[id] = true
		}
	}
}

// ExemptExtension is the operation Extension key that exempts one operation from the contract checks. Its value
// must be a non-empty string giving the reason:
//
//	huma.Operation{..., Extensions: map[string]any{openapimcp.ExemptExtension: "WebAuthn ceremony: shapes fixed by the spec"}}
const ExemptExtension = "x-parity-exempt"

// WithHeader adds a header (for example Cookie or Authorization) to every MCP
// request ParityCheck makes, for handlers that sit behind authentication.
func WithHeader(key, value string) ParityOption {
	return func(c *parityConfig) { c.header.Add(key, value) }
}

type headerTransport struct {
	base http.RoundTripper
	h    http.Header
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, vs := range t.h {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	return t.base.RoundTrip(r)
}

// ParityCheck is the parity test every playground app runs. It asserts that
//
//	(a) the sorted operation ids of api (Hidden operations are not part of
//	    the OpenAPI document and so are excluded, and so are operations with `x-mcp: false`, see NoToolExtension) equal the sorted tool names
//	    listed by mcpHandler over the streamable HTTP transport, and
//	(b) every operation has a non-empty Summary and Description, and
//	(c) the API contract of docs/go-api.md holds, see checkContract: kebab-case verb-noun OperationID, Method,
//	    Path under /api/, Tags, Security covering session and bearer, snake_case request and response property
//	    names, date-time format on timestamps (names ending _at) and ids (id, *_id) that are strings with format uuid (ids the app
//	generates), or with another format or a pattern (ids it does not generate); integer ids need an exemption.
//
// Operations can be exempted from (b) and (c) with WithExempt or the ExemptExtension Extension.
func ParityCheck(t testing.TB, api huma.API, mcpHandler http.Handler, opts ...ParityOption) {
	t.Helper()
	cfg := &parityConfig{header: http.Header{}, exempt: map[string]bool{}}
	for _, o := range opts {
		o(cfg)
	}

	var ids []string
	for path, item := range api.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{
			"GET": item.Get, "PUT": item.Put, "POST": item.Post, "DELETE": item.Delete,
			"OPTIONS": item.Options, "HEAD": item.Head, "PATCH": item.Patch, "TRACE": item.Trace,
		} {
			if op == nil {
				continue
			}
			if !IsNotATool(op) {
				ids = append(ids, op.OperationID)
			}
			checkContract(t, api, cfg, method, path, op)
		}
	}
	slices.Sort(ids)

	ts := httptest.NewServer(mcpHandler)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "parity-check", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: headerTransport{base: http.DefaultTransport, h: cfg.header}},
	}, nil)
	if err != nil {
		t.Fatalf("parity: connecting to MCP handler: %v", err)
	}
	defer session.Close()

	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("parity: listing MCP tools: %v", err)
		}
		names = append(names, tool.Name)
	}
	slices.Sort(names)

	if !slices.Equal(ids, names) {
		t.Errorf("operation ids and MCP tools differ\n  operations: %v\n  tools:      %v", ids, names)
	}
}

const docPointer = "see docs/go-api.md"

var (
	opIDRE     = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)+$`)
	snakeRE    = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	timestampN = regexp.MustCompile(`(^|_)(at|timestamp)$`)
)

func checkContract(t testing.TB, api huma.API, cfg *parityConfig, method, path string, op *huma.Operation) {
	t.Helper()
	label := fmt.Sprintf("operation %q (%s %s)", op.OperationID, method, path)
	fail := func(problem, fix string) {
		t.Errorf("%s: %s; fix: %s; %s", label, problem, fix, docPointer)
	}
	if v, ok := op.Extensions[ExemptExtension]; ok {
		if r, _ := v.(string); strings.TrimSpace(r) == "" {
			fail("extension "+ExemptExtension+" needs a non-empty string reason", `set it to the reason, for example "WebAuthn ceremony: shapes fixed by the spec"`)
		}
		return
	}
	if cfg.exempt[op.OperationID] {
		return
	}
	if !opIDRE.MatchString(op.OperationID) {
		fail("OperationID is not kebab-case verb-noun", `set OperationID to a name like "list-items" or "create-note-comment"`)
	}
	if op.Method == "" || !strings.EqualFold(op.Method, method) {
		fail(fmt.Sprintf("Method %q is empty or does not match %s", op.Method, method), "set Method to a net/http constant such as http.MethodGet")
	}
	if !strings.HasPrefix(path, "/api/") {
		fail("Path does not start with /api/", `register the operation under "/api/<resource>"`)
	}
	if strings.TrimSpace(op.Summary) == "" {
		fail("Summary is empty", "set a one-line Summary")
	}
	if strings.TrimSpace(op.Description) == "" {
		fail("Description is empty", "set a Description a model can use to decide when to call the tool")
	}
	if len(op.Tags) == 0 {
		fail("Tags is empty", `set Tags to the resource name, for example []string{"items"}`)
	}
	var session, bearer bool
	for _, req := range op.Security {
		_, s := req["session"]
		_, b := req["bearer"]
		session, bearer = session || s, bearer || b
	}
	if !session || !bearer {
		fail("Security does not cover both the session and bearer schemes", `set Security to []map[string][]string{{"session": {}}, {"bearer": {}}}`)
	}

	seen := map[*huma.Schema]bool{}
	walk := func(where string, s *huma.Schema) {
		checkSchema(api, s, where, "", seen, fail)
	}
	if op.RequestBody != nil {
		for _, mt := range op.RequestBody.Content {
			walk("request body", mt.Schema)
		}
	}
	for code, r := range op.Responses {
		if r == nil {
			continue
		}
		for _, mt := range r.Content {
			walk("response "+code, mt.Schema)
		}
	}
}

func checkSchema(api huma.API, s *huma.Schema, where, name string, seen map[*huma.Schema]bool, fail func(problem, fix string)) {
	if s == nil {
		return
	}
	if s.Ref != "" {
		reg := api.OpenAPI().Components
		if reg == nil || reg.Schemas == nil {
			return
		}
		s = reg.Schemas.SchemaFromRef(s.Ref)
		if s == nil {
			return
		}
	}
	if seen[s] {
		return
	}
	seen[s] = true
	props := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		props = append(props, n)
	}
	slices.Sort(props)
	for _, n := range props {
		p := s.Properties[n]
		if n == "$schema" {
			continue
		}
		if !snakeRE.MatchString(n) {
			fail(fmt.Sprintf("%s property %q is not snake_case", where, n), fmt.Sprintf(`change its json tag to %q`, toSnake(n)))
		}
		if pr := resolve(api, p); pr != nil {
			if n == "id" || strings.HasSuffix(n, "_id") {
				checkID(pr, where, n, fail)
			}
			if timestampN.MatchString(n) && pr.Format != "date-time" && pr.Type != "array" && pr.Type != "object" {
				fail(fmt.Sprintf("%s timestamp property %q has format %q, not date-time", where, n, pr.Format), "use time.Time (RFC 3339 UTC) or the struct tag format:\"date-time\"")
			}
		}
		checkSchema(api, p, where, n, seen, fail)
	}
	checkSchema(api, s.Items, where, name, seen, fail)
	if ap, ok := s.AdditionalProperties.(*huma.Schema); ok {
		checkSchema(api, ap, where, name, seen, fail)
	}
	for _, group := range [][]*huma.Schema{s.AllOf, s.AnyOf, s.OneOf} {
		for _, g := range group {
			checkSchema(api, g, where, name, seen, fail)
		}
	}
}

// idFix explains every way to satisfy the id rule.
const idFix = `ids the app generates: add the struct tag format:"uuid" (UUIDv7 strings); ids it does not generate (bead ids, external ids): ` +
	`declare format:"<name>" or pattern:"<regexp>" so the shape is documented; anything else (integer ids): exempt the operation ` +
	`with openapimcp.WithExempt or the ExemptExtension and a reason`

// checkID applies the id rule to a property named id or *_id. A string is accepted with format uuid, with any
// other explicit format, or with a pattern; a bare string and any number or integer are rejected. Arrays and
// untyped schemas are left to their items.
func checkID(pr *huma.Schema, where, name string, fail func(problem, fix string)) {
	switch pr.Type {
	case "string":
		if pr.Format == "" && pr.Pattern == "" {
			fail(fmt.Sprintf("%s id property %q is a string with neither format nor pattern", where, name), idFix)
		}
	case "", "array":
	default:
		fail(fmt.Sprintf("%s id property %q has type %q, not a string", where, name, pr.Type), idFix)
	}
}

func resolve(api huma.API, s *huma.Schema) *huma.Schema {
	if s != nil && s.Ref != "" {
		if c := api.OpenAPI().Components; c != nil && c.Schemas != nil {
			return c.Schemas.SchemaFromRef(s.Ref)
		}
		return nil
	}
	return s
}

func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		} else if r == '-' {
			r = '_'
		}
		b.WriteRune(r)
	}
	return b.String()
}
