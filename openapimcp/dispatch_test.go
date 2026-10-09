package openapimcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/teb-ooo/playground-go/auth"
	playgroundlog "github.com/teb-ooo/playground-go/log"
	"github.com/teb-ooo/playground-go/openapimcp"
)

// fakeAuth identifies the caller "Bearer good" as user u1 and counts how often it runs; it stands in for the real credential check.
type fakeAuth struct{ verifications atomic.Int32 }

func (f *fakeAuth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer good" {
			f.verifications.Add(1)
			ctx := auth.WithCredential(auth.WithUser(r.Context(), auth.User{Subject: "u1"}), auth.CredentialKey)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

type seen struct {
	mu    sync.Mutex
	inner []string // RemoteAddr|X-Forwarded-For of the /api requests
	outer []string // the same for /mcp
}

func (s *seen) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		rec := r.RemoteAddr + "|" + r.Header.Get("X-Forwarded-For")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.inner = append(s.inner, rec)
		} else {
			s.outer = append(s.outer, rec)
		}
		s.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

// templateApp wires an app as the template does: log middleware outermost, auth, a mux with the API and /mcp.
func templateApp(t *testing.T, fa *fakeAuth, sn *seen) http.Handler {
	t.Helper()
	api, mux := newAPI(t)
	var app http.Handler = mux
	if sn != nil {
		app = sn.wrap(mux)
	}
	mux.Handle("/mcp", openapimcp.Handler(api, app, openapimcp.Options{Auth: fa.middleware}))
	return playgroundlog.Middleware(fa.middleware(app))
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func TestToolCallLeavesInfoLine(t *testing.T) {
	buf := captureLogs(t)
	h := templateApp(t, &fakeAuth{}, nil)
	s := connectAt(t, h, "/mcp", http.Header{"Authorization": {"Bearer good"}})
	if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "create-item", Arguments: map[string]any{"name": "secret-milk"}}); err != nil {
		t.Fatal(err)
	}
	var calls []map[string]any
	for _, m := range logLines(t, buf) {
		if m["msg"] == "mcp tool call" {
			calls = append(calls, m)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("tool call lines = %d, want 1: %s", len(calls), buf)
	}
	m := calls[0]
	if m["level"] != "INFO" || m["op"] != "create-item" || m["user"] != "u1" || m["surface"] != "mcp" || m["credential"] != "key" ||
		m["status"] != float64(201) || m["request_id"] == nil || m["request_id"] == "" || m["duration_ms"] == nil {
		t.Errorf("tool call line = %v", m)
	}
	if strings.Contains(buf.String(), "secret-milk") || strings.Contains(buf.String(), "Bearer") {
		t.Errorf("log carries user content or a token: %s", buf)
	}
}

func TestToolCallErrorLevels(t *testing.T) {
	buf := captureLogs(t)
	h := templateApp(t, &fakeAuth{}, nil)
	s := connectAt(t, h, "/mcp", http.Header{"Authorization": {"Bearer good"}})
	for _, id := range []string{"missing"} {
		if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "get-item", Arguments: map[string]any{"id": id}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range logLines(t, buf) {
		if m["msg"] == "mcp tool call" {
			if m["level"] != "WARN" || m["status"] != float64(404) || m["op"] != "get-item" {
				t.Errorf("404 tool line = %v", m)
			}
			return
		}
	}
	t.Fatalf("no tool call line: %s", buf)
}

func TestRequestLineCarriesAnnotation(t *testing.T) {
	buf := captureLogs(t)
	h := playgroundlog.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		playgroundlog.Annotate(r.Context(), playgroundlog.Fields{User: "u1", Credential: "session", Surface: "ui"})
		playgroundlog.Annotate(r.Context(), playgroundlog.Fields{Op: "create-item"})
		w.WriteHeader(http.StatusCreated)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/items", nil))
	ls := logLines(t, buf)
	if len(ls) != 1 {
		t.Fatalf("lines: %s", buf)
	}
	m := ls[0]
	if m["user"] != "u1" || m["credential"] != "session" || m["surface"] != "ui" || m["op"] != "create-item" || m["level"] != "INFO" {
		t.Errorf("request line = %v", m)
	}
	// No middleware in the context: Annotate is a no-op.
	playgroundlog.Annotate(context.Background(), playgroundlog.Fields{User: "x"})
}

func TestToolCallVerifiesOnceAndKeepsClientIP(t *testing.T) {
	fa, sn := &fakeAuth{}, &seen{}
	h := templateApp(t, fa, sn)
	s := connectAt(t, h, "/mcp", http.Header{"Authorization": {"Bearer good"}, "X-Forwarded-For": {"198.51.100.7"}})

	before := fa.verifications.Load()
	sn.mu.Lock()
	sn.outer, sn.inner = nil, nil
	sn.mu.Unlock()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami"})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %v", err, res)
	}
	// The outer middleware verifies once; neither the MCP door nor the tool dispatch verifies again.
	if got := fa.verifications.Load() - before; got != 1 {
		t.Errorf("verifications for one tool call = %d, want 1", got)
	}
	sn.mu.Lock()
	defer sn.mu.Unlock()
	if len(sn.outer) != 1 || len(sn.inner) != 1 {
		t.Fatalf("outer %v inner %v", sn.outer, sn.inner)
	}
	if sn.outer[0] != sn.inner[0] {
		t.Errorf("inner client = %q, outer = %q: want equal", sn.inner[0], sn.outer[0])
	}
	if strings.HasPrefix(sn.inner[0], "127.0.0.1:0|") || !strings.HasSuffix(sn.inner[0], "|198.51.100.7") {
		t.Errorf("inner client = %q", sn.inner[0])
	}
}

// The inner request cannot claim another identity: the user in the tool call's context is the outer one, whatever the forwarded
// headers say.
func TestDispatchIdentityComesFromOuterContext(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "1.0.0"))
	huma.Register(api, huma.Operation{OperationID: "who-am-i", Method: http.MethodGet, Path: "/api/me", Summary: "s", Description: "d"},
		func(ctx context.Context, in *struct{}) (*struct{ Body map[string]string }, error) {
			u, _ := auth.FromContext(ctx)
			return &struct{ Body map[string]string }{map[string]string{"subject": u.Subject}}, nil
		})
	fa := &fakeAuth{}
	mux.Handle("/mcp", openapimcp.Handler(api, mux, openapimcp.Options{Auth: fa.middleware}))
	s := connectAt(t, fa.middleware(mux), "/mcp", http.Header{"Authorization": {"Bearer good"}})
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "who-am-i"})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); !strings.Contains(got, `"subject":"u1"`) {
		t.Errorf("subject = %s", got)
	}
}
