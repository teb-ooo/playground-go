package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/teb-ooo/playground-go/health"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

type ptrPinger struct{}

func (*ptrPinger) Ping(context.Context) error { return errors.New("would panic if called") }

func TestHealthz(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var nilPtr *ptrPinger
	tests := []struct {
		name     string
		db       health.Pinger
		wantCode int
		wantDB   string
	}{
		{"db ok", pinger{}, 200, "ok"},
		{"db down", pinger{errors.New("boom")}, 503, "error"},
		{"no db", nil, 200, "ok"},
		{"typed nil pool", nilPtr, 200, "ok"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			api := humago.New(mux, huma.DefaultConfig("t", "1"))
			health.Register(api, tc.db, "v1.2.3", health.WithEnv("staging"),
				health.WithClock(start, func() time.Time { return start.Add(90 * time.Second) }))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tc.wantCode, w.Body)
			}
			var b map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
				t.Fatal(err)
			}
			if b["version"] != "v1.2.3" || b["env"] != "staging" || b["db"] != tc.wantDB || b["uptime_seconds"] != 90.0 {
				t.Errorf("body = %v", b)
			}
			// Hidden: not in the OpenAPI document.
			ow := httptest.NewRecorder()
			mux.ServeHTTP(ow, httptest.NewRequest("GET", "/openapi.json", nil))
			if strings.Contains(ow.Body.String(), "/healthz") {
				t.Error("/healthz leaked into the OpenAPI document")
			}
		})
	}
}
