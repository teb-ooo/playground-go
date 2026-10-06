package log_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	flog "github.com/teb-ooo/playground-go/log"
)

func TestSecretsFromEnv(t *testing.T) {
	env := []string{
		"ANTHROPIC_API_KEY=sk-ant-abcdef", "DB_PASSWORD=hunter22", "OIDC_CLIENT_SECRET=s3cretvalue",
		"GITHUB_TOKEN=ghp_xxxxxxxx", "PORT=8080", "SHORT_KEY=abc", "EMPTY_TOKEN=", "APP_NAME=hello-world",
		"KEYBOARD=qwertyuiop", "MAIL_API_KEY=ghp_xxxxxxxx",
	}
	got := flog.SecretsFromEnv(env)
	want := map[string]bool{"sk-ant-abcdef": true, "hunter22": true, "s3cretvalue": true, "ghp_xxxxxxxx": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected secret %q", g)
		}
	}
}

func TestRedaction(t *testing.T) {
	secret := "sk-ant-verysecret"
	var buf bytes.Buffer
	l := flog.NewWithSecrets(&buf, nil, []string{secret})

	type wrapper struct{ Token string }
	l = l.With("preset", "pre-"+secret)
	l.Info("calling with "+secret,
		"plain", "nothing here",
		"key", secret,
		"embedded", "Bearer "+secret+" trailing",
		slog.Group("g", slog.String("inner", secret)),
		"err", errors.New("failed using "+secret),
		"struct", wrapper{Token: secret},
		"n", 42,
	)
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("secret leaked: %s", out)
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["plain"] != "nothing here" || rec["n"] != 42.0 {
		t.Errorf("non-secret values changed: %v", rec)
	}
	if rec["embedded"] != "Bearer [REDACTED] trailing" {
		t.Errorf("embedded = %v", rec["embedded"])
	}
	if rec["msg"] != "calling with [REDACTED]" {
		t.Errorf("msg = %v", rec["msg"])
	}
}

func TestMiddleware(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		inID       string
		handler    http.HandlerFunc
		wantStatus float64
		wantLevel  string
		wantSameID bool
	}{
		{"ok default 200", "/items", "", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) }, 200, "INFO", false},
		{"client error is warn level", "/nope", "", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }, 404, "WARN", false},
		{"quiet path is debug", "/mcp", "", func(w http.ResponseWriter, r *http.Request) {}, 200, "DEBUG", false},
		{"quiet path failure is not hidden", "/mcp", "", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }, 401, "WARN", false},
		{"server error is error level", "/boom", "", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, 500, "ERROR", false},
		{"incoming request id kept", "/x", "abc-123", func(w http.ResponseWriter, r *http.Request) {}, 200, "INFO", true},
		{"bad request id replaced", "/x", "bad id!\n", func(w http.ResponseWriter, r *http.Request) {}, 200, "INFO", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := flog.NewWithSecrets(&buf, slog.LevelDebug, nil)
			var ctxID string
			h := flog.MiddlewareWith(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxID = flog.RequestID(r.Context())
				tc.handler(w, r)
			}))
			r := httptest.NewRequest("GET", tc.path, nil)
			if tc.inID != "" {
				r.Header.Set("X-Request-Id", tc.inID)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("log line: %v (%s)", err, buf.String())
			}
			if rec["status"] != tc.wantStatus || rec["method"] != "GET" || rec["path"] != tc.path || rec["level"] != tc.wantLevel {
				t.Errorf("record = %v", rec)
			}
			if _, ok := rec["duration_ms"]; !ok {
				t.Error("no duration_ms")
			}
			id, _ := rec["request_id"].(string)
			if id == "" || id != w.Header().Get("X-Request-Id") || id != ctxID {
				t.Errorf("request id mismatch: log=%q header=%q ctx=%q", id, w.Header().Get("X-Request-Id"), ctxID)
			}
			if tc.wantSameID && id != tc.inID {
				t.Errorf("id = %q, want %q", id, tc.inID)
			}
		})
	}
}

func TestHealthzIsDebug(t *testing.T) {
	var buf bytes.Buffer
	l := flog.NewWithSecrets(&buf, slog.LevelInfo, nil)
	flog.MiddlewareWith(l, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	if buf.Len() != 0 {
		t.Errorf("healthz logged at info: %s", buf.String())
	}
}

func TestFlushPassesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	flog.MiddlewareWith(flog.NewWithSecrets(&bytes.Buffer{}, nil, nil), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
	})).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !rec.Flushed {
		t.Error("flush did not reach the recorder")
	}
}

func TestLevelFromEnv(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, " warn ": slog.LevelWarn, "Warning": slog.LevelWarn, "error": slog.LevelError, "": slog.LevelWarn + 1, "loud": slog.LevelWarn + 1} {
		t.Setenv("TEST_LOG_LEVEL", in)
		if got := flog.LevelFromEnv("TEST_LOG_LEVEL", slog.LevelWarn+1); got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}
