package surface_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/teb-ooo/playground-go/surface"
)

func TestFromDefaultsToAPIAndWithOverrides(t *testing.T) {
	ctx := context.Background()
	if surface.From(ctx) != surface.API {
		t.Fatal("unmarked context should read as api")
	}
	if got := surface.From(surface.With(surface.With(ctx, surface.UI), surface.MCP)); got != surface.MCP {
		t.Fatalf("got %q", got)
	}
	if !surface.MCP.IsAI() || !surface.Assistant.IsAI() || surface.UI.IsAI() || surface.API.IsAI() {
		t.Fatal("IsAI wrong")
	}
}

func TestMiddlewareClassifiesAndStrips(t *testing.T) {
	var seen surface.Surface
	var hdr string
	h := surface.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, hdr = surface.From(r.Context()), r.Header.Get(surface.Header)
	}))
	for _, tc := range []struct {
		name, auth, claim string
		want              surface.Surface
	}{
		{"browser", "", "", surface.UI},
		{"bearer", "Bearer x", "", surface.API},
		{"claims mcp from browser", "", "mcp", surface.UI},
		{"claims ui with bearer", "Bearer x", "ui", surface.API},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.auth != "" {
			r.Header.Set("Authorization", tc.auth)
		}
		if tc.claim != "" {
			r.Header.Set(surface.Header, tc.claim)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if seen != tc.want || hdr != "" {
			t.Errorf("%s: surface %q header %q, want %q and no header", tc.name, seen, hdr, tc.want)
		}
	}
}

func TestMiddlewareKeepsServerSetMarker(t *testing.T) {
	var seen surface.Surface
	h := surface.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = surface.From(r.Context()) }))
	r := httptest.NewRequest("GET", "/", nil).WithContext(surface.With(context.Background(), surface.Assistant))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen != surface.Assistant {
		t.Fatalf("got %q", seen)
	}
}
