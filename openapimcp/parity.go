package openapimcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ParityOption customises ParityCheck.
type ParityOption func(*parityConfig)

type parityConfig struct{ header http.Header }

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
//	    the OpenAPI document and so are excluded) equal the sorted tool names
//	    listed by mcpHandler over the streamable HTTP transport, and
//	(b) every operation has a non-empty Summary and Description.
func ParityCheck(t testing.TB, api huma.API, mcpHandler http.Handler, opts ...ParityOption) {
	t.Helper()
	cfg := &parityConfig{header: http.Header{}}
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
			ids = append(ids, op.OperationID)
			if strings.TrimSpace(op.Summary) == "" {
				t.Errorf("operation %s (%s %s) has an empty summary", op.OperationID, method, path)
			}
			if strings.TrimSpace(op.Description) == "" {
				t.Errorf("operation %s (%s %s) has an empty description", op.OperationID, method, path)
			}
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
