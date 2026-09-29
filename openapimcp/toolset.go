package openapimcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// forwardedHeaders are copied from the caller onto the in-process request so
// the operation runs as the same user.
var forwardedHeaders = []string{"Authorization", "Cookie", "X-Request-Id", "Accept-Language"}

type paramSpec struct {
	name     string
	in       string // "path" or "query"
	required bool
	// csv is true for array query parameters declared explode=false (the Huma
	// default): values are sent comma separated in a single key.
	csv bool
}

// Tool is one MCP tool derived from one OpenAPI operation.
type Tool struct {
	// Name is the operation id.
	Name string
	// Description is "Summary\n\nDescription".
	Description string
	// Summary is the operation's summary alone (used by the assistant's system prompt).
	Summary string
	// InputSchema is a JSON Schema object describing the tool input.
	InputSchema map[string]any
	// Method and Path are the HTTP method and OpenAPI path template.
	Method string
	Path   string

	params      []paramSpec
	hasBody     bool
	flattenBody bool
	bodyRequire bool
}

// Result is the outcome of executing a tool in-process.
type Result struct {
	// Status is the HTTP status the operation answered with.
	Status int
	// Body is the response body as text.
	Body string
	// IsError is true for any non-2xx status.
	IsError bool
}

// Text renders the result for a model or an MCP client. Errors carry the
// status so the caller can tell a 404 from a 422.
func (r Result) Text() string {
	if r.IsError {
		return fmt.Sprintf("HTTP %d: %s", r.Status, r.Body)
	}
	if r.Body == "" {
		return fmt.Sprintf("HTTP %d (no content)", r.Status)
	}
	return r.Body
}

// Toolset is the set of tools derived from one API. It is immutable after
// construction.
type Toolset struct {
	tools map[string]*Tool
	names []string
}

// NewToolset walks api.OpenAPI().Paths and derives one Tool per operation.
// Operations registered with Hidden: true are not present in the OpenAPI
// document (Huma omits them) and so are never tools. Register every operation
// before calling NewToolset.
func NewToolset(api huma.API) (*Toolset, error) {
	oapi := api.OpenAPI()
	ts := &Toolset{tools: map[string]*Tool{}}
	reg := oapi.Components.Schemas

	for path, item := range oapi.Paths {
		ops := map[string]*huma.Operation{
			http.MethodGet: item.Get, http.MethodPut: item.Put, http.MethodPost: item.Post,
			http.MethodDelete: item.Delete, http.MethodOptions: item.Options,
			http.MethodHead: item.Head, http.MethodPatch: item.Patch, http.MethodTrace: item.Trace,
		}
		for method, op := range ops {
			if op == nil {
				continue
			}
			t, err := buildTool(reg, method, path, item.Parameters, op)
			if err != nil {
				return nil, fmt.Errorf("openapimcp: %s %s: %w", method, path, err)
			}
			if _, dup := ts.tools[t.Name]; dup {
				return nil, fmt.Errorf("openapimcp: duplicate operation id %q", t.Name)
			}
			ts.tools[t.Name] = t
		}
	}
	for n := range ts.tools {
		ts.names = append(ts.names, n)
	}
	sort.Strings(ts.names)
	return ts, nil
}

// Names returns the sorted tool names.
func (ts *Toolset) Names() []string { return append([]string(nil), ts.names...) }

// Tools returns the tools sorted by name.
func (ts *Toolset) Tools() []*Tool {
	out := make([]*Tool, 0, len(ts.names))
	for _, n := range ts.names {
		out = append(out, ts.tools[n])
	}
	return out
}

// Lookup returns the tool with the given name.
func (ts *Toolset) Lookup(name string) (*Tool, bool) {
	t, ok := ts.tools[name]
	return t, ok
}

func buildTool(reg huma.Registry, method, path string, shared []*huma.Param, op *huma.Operation) (*Tool, error) {
	if !toolNameRE.MatchString(op.OperationID) {
		return nil, fmt.Errorf("operation id %q is not a valid MCP tool name", op.OperationID)
	}
	t := &Tool{Name: op.OperationID, Method: method, Path: path, Summary: op.Summary}
	t.Description = op.Summary
	if op.Description != "" {
		t.Description = op.Summary + "\n\n" + op.Description
	}

	props := map[string]any{}
	var required []string
	defs := newDefs(reg)

	params := append(append([]*huma.Param(nil), shared...), op.Parameters...)
	for _, p := range params {
		if p == nil || (p.In != "path" && p.In != "query") {
			continue
		}
		if p.Name == "body" {
			return nil, fmt.Errorf("parameter named %q collides with the body property", p.Name)
		}
		s := defs.schema(p.Schema)
		if s == nil {
			s = map[string]any{}
		}
		if p.Description != "" {
			if _, ok := s["description"]; !ok {
				s["description"] = p.Description
			}
		}
		props[p.Name] = s
		req := p.Required || p.In == "path"
		if req {
			required = append(required, p.Name)
		}
		t.params = append(t.params, paramSpec{name: p.Name, in: p.In, required: req, csv: p.Explode != nil && !*p.Explode})
	}

	if op.RequestBody != nil {
		mt := op.RequestBody.Content["application/json"]
		if mt == nil {
			for _, v := range op.RequestBody.Content {
				mt = v
				break
			}
		}
		if mt != nil && mt.Schema != nil {
			t.hasBody = true
			t.bodyRequire = op.RequestBody.Required
			body := defs.schema(mt.Schema)
			delete(propsOf(body), "$schema")
			bodyProps := propsOf(body)
			if len(t.params) == 0 && body["type"] == "object" && bodyProps != nil {
				t.flattenBody = true
				for k, v := range bodyProps {
					props[k] = v
				}
				required = append(required, stringsOf(body["required"])...)
			} else {
				if op.RequestBody.Required {
					required = append(required, "body")
				}
				props["body"] = body
			}
		}
	}

	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	if d := defs.emit(); len(d) > 0 {
		schema["$defs"] = d
	}
	t.InputSchema = schema
	return t, nil
}

func propsOf(m map[string]any) map[string]any {
	p, _ := m["properties"].(map[string]any)
	return p
}

func stringsOf(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// defs collects component schemas referenced by a tool and rewrites $ref
// values from #/components/schemas/X to #/$defs/X.
type defs struct {
	reg  huma.Registry
	used map[string]bool
	todo []string
}

func newDefs(reg huma.Registry) *defs { return &defs{reg: reg, used: map[string]bool{}} }

const componentPrefix = "#/components/schemas/"

func (d *defs) schema(s *huma.Schema) map[string]any {
	if s == nil {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	if ref, ok := m["$ref"].(string); ok && len(m) == 1 && strings.HasPrefix(ref, componentPrefix) {
		// Resolve a top level reference so flattening can see properties.
		if c := d.reg.SchemaFromRef(ref); c != nil {
			return d.schema(c)
		}
	}
	d.rewrite(m)
	return m
}

func (d *defs) rewrite(v any) {
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok && strings.HasPrefix(ref, componentPrefix) {
			name := strings.TrimPrefix(ref, componentPrefix)
			x["$ref"] = "#/$defs/" + name
			if !d.used[name] {
				d.used[name] = true
				d.todo = append(d.todo, name)
			}
		}
		for _, e := range x {
			d.rewrite(e)
		}
	case []any:
		for _, e := range x {
			d.rewrite(e)
		}
	}
}

func (d *defs) emit() map[string]any {
	out := map[string]any{}
	for len(d.todo) > 0 {
		name := d.todo[0]
		d.todo = d.todo[1:]
		c := d.reg.SchemaFromRef(componentPrefix + name)
		if c == nil {
			out[name] = map[string]any{}
			continue
		}
		b, _ := json.Marshal(c)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		delete(propsOf(m), "$schema")
		d.rewrite(m)
		out[name] = m
	}
	return out
}

// Call executes the named tool against app in-process. hdr supplies the
// caller's credentials (Authorization, Cookie); args is the tool input as a
// JSON object.
func (ts *Toolset) Call(ctx context.Context, app http.Handler, hdr http.Header, name string, args json.RawMessage) (Result, error) {
	t, ok := ts.tools[name]
	if !ok {
		return Result{}, fmt.Errorf("openapimcp: unknown tool %q", name)
	}
	return t.Call(ctx, app, hdr, args)
}

// Call executes the tool against app in-process.
func (t *Tool) Call(ctx context.Context, app http.Handler, hdr http.Header, args json.RawMessage) (Result, error) {
	in := map[string]any{}
	if len(bytes.TrimSpace(args)) > 0 && string(bytes.TrimSpace(args)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.UseNumber()
		if err := dec.Decode(&in); err != nil {
			return Result{}, fmt.Errorf("openapimcp: tool %s: input must be a JSON object: %w", t.Name, err)
		}
	}

	path := t.Path
	q := url.Values{}
	used := map[string]bool{}
	for _, p := range t.params {
		v, present := in[p.name]
		if !present || v == nil {
			if p.required {
				return Result{}, fmt.Errorf("openapimcp: tool %s: missing required %s parameter %q", t.Name, p.in, p.name)
			}
			continue
		}
		used[p.name] = true
		switch p.in {
		case "path":
			path = strings.ReplaceAll(path, "{"+p.name+"}", url.PathEscape(scalar(v)))
		case "query":
			if arr, ok := v.([]any); ok {
				parts := make([]string, 0, len(arr))
				for _, e := range arr {
					parts = append(parts, scalar(e))
				}
				if p.csv {
					q.Set(p.name, strings.Join(parts, ","))
				} else {
					for _, e := range parts {
						q.Add(p.name, e)
					}
				}
			} else {
				q.Set(p.name, scalar(v))
			}
		}
	}

	var body io.Reader
	if t.hasBody {
		var payload any
		if t.flattenBody {
			flat := map[string]any{}
			for k, v := range in {
				if !used[k] {
					flat[k] = v
				}
			}
			payload = flat
		} else if b, ok := in["body"]; ok {
			payload = b
		} else if t.bodyRequire {
			return Result{}, fmt.Errorf("openapimcp: tool %s: missing required property %q", t.Name, "body")
		}
		if payload != nil {
			b, err := json.Marshal(payload)
			if err != nil {
				return Result{}, fmt.Errorf("openapimcp: tool %s: encoding body: %w", t.Name, err)
			}
			body = bytes.NewReader(b)
		}
	}

	target := path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, t.Method, target, body)
	if err != nil {
		return Result{}, fmt.Errorf("openapimcp: tool %s: building request: %w", t.Name, err)
	}
	req.Host = "localhost"
	req.RemoteAddr = "127.0.0.1:0"
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range forwardedHeaders {
		for _, v := range hdr.Values(h) {
			req.Header.Add(h, v)
		}
	}

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return Result{}, fmt.Errorf("openapimcp: tool %s: reading response: %w", t.Name, err)
	}
	return Result{
		Status:  res.StatusCode,
		Body:    string(b),
		IsError: res.StatusCode < 200 || res.StatusCode > 299,
	}, nil
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
