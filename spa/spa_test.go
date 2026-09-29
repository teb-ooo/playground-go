package spa_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/teb-ooo/factory-go/spa"
)

var site = fstest.MapFS{
	"index.html":     {Data: []byte("<!doctype html><html><head><title>t</title></head><body>app</body></html>")},
	"assets/app.js":  {Data: []byte("console.log(1)")},
	"favicon.svg":    {Data: []byte("<svg/>")},
	"docs/index.txt": {Data: []byte("doc")},
}

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func TestServing(t *testing.T) {
	dir := t.TempDir()
	sessFile := filepath.Join(dir, "session_url")
	os.WriteFile(sessFile, []byte("https://claude.ai/code/session_abc\n"), 0o600)
	h := spa.Handler(site, spa.Config{AppName: "hello", Env: "staging", SessionURLFile: sessFile})

	tests := []struct {
		name         string
		method, path string
		code         int
		contain      []string
		notContain   []string
		cache        string
	}{
		{"root gets index with config", "GET", "/", 200, []string{"<title>t</title>", "window.__FACTORY__", `"app_name":"hello"`, `"env":"staging"`, `"agent_url":"/_agent/tty/"`, `"claude_session_url":"https://claude.ai/code/session_abc"`}, nil, "no-store"},
		{"config is inside head", "GET", "/", 200, []string{"</script></head>"}, nil, ""},
		{"unknown route falls back", "GET", "/items/42", 200, []string{"window.__FACTORY__", "<body>app</body>"}, nil, "no-store"},
		{"asset served", "GET", "/assets/app.js", 200, []string{"console.log(1)"}, []string{"__FACTORY__"}, "public, max-age=31536000, immutable"},
		{"root file served", "GET", "/favicon.svg", 200, []string{"<svg/>"}, nil, ""},
		{"missing asset is 404", "GET", "/assets/missing.js", 404, nil, nil, ""},
		{"api path is 404", "GET", "/api/nothing", 404, nil, nil, ""},
		{"directory falls back", "GET", "/docs", 200, []string{"window.__FACTORY__"}, nil, ""},
		{"traversal is cleaned to a client route", "GET", "/../../etc/passwd", 200, []string{"window.__FACTORY__"}, []string{"root:"}, ""},
		{"HEAD has no body", "HEAD", "/", 200, nil, []string{"<title>"}, ""},
		{"POST not allowed", "POST", "/", 405, nil, nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := get(h, tc.method, tc.path)
			if w.Code != tc.code {
				t.Fatalf("code = %d, want %d", w.Code, tc.code)
			}
			body := w.Body.String()
			for _, s := range tc.contain {
				if !strings.Contains(body, s) {
					t.Errorf("body missing %q:\n%s", s, body)
				}
			}
			for _, s := range tc.notContain {
				if strings.Contains(body, s) {
					t.Errorf("body should not contain %q", s)
				}
			}
			if tc.cache != "" && w.Header().Get("Cache-Control") != tc.cache {
				t.Errorf("cache-control = %q", w.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestLocaleAndTimezone(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		cfg        spa.Config
		wantLocale string
		wantTZ     string
	}{
		{"defaults", nil, spa.Config{}, "en-US", "UTC"},
		{"from env", map[string]string{"FACTORY_LOCALE": "sv-SE", "FACTORY_TIMEZONE": "Europe/Stockholm"}, spa.Config{}, "sv-SE", "Europe/Stockholm"},
		{"config wins", map[string]string{"FACTORY_LOCALE": "sv-SE"}, spa.Config{Locale: "de-DE", Timezone: "Europe/Berlin"}, "de-DE", "Europe/Berlin"},
		{"blank env is default", map[string]string{"FACTORY_LOCALE": "  ", "FACTORY_TIMEZONE": ""}, spa.Config{}, "en-US", "UTC"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FACTORY_LOCALE", "")
			t.Setenv("FACTORY_TIMEZONE", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			tc.cfg.SessionURLFile = "/nonexistent"
			body := get(spa.Handler(site, tc.cfg), "GET", "/").Body.String()
			for _, want := range []string{`"locale":"` + tc.wantLocale + `"`, `"timezone":"` + tc.wantTZ + `"`} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
		})
	}
}

func TestProductionHasNoAgent(t *testing.T) {
	h := spa.Handler(site, spa.Config{AppName: "hello", Env: "production", SessionURLFile: "/nonexistent/file"})
	body := get(h, "GET", "/").Body.String()
	if !strings.Contains(body, `"agent_url":""`) || !strings.Contains(body, `"claude_session_url":""`) {
		t.Errorf("body = %s", body)
	}
}

func TestEnvDefaultsAndEscaping(t *testing.T) {
	t.Setenv("APP_NAME", `</script><b>&`)
	t.Setenv("APP_ENV", "production")
	h := spa.Handler(site, spa.Config{SessionURLFile: "/nonexistent"})
	body := get(h, "GET", "/").Body.String()
	if strings.Contains(body, "</script><b>") {
		t.Fatalf("app name broke out of the script element: %s", body)
	}
	re := regexp.MustCompile(`window\.__FACTORY__ = (\{.*\});</script>`)
	if !re.MatchString(body) {
		t.Fatalf("no config: %s", body)
	}
}

func TestIndexWithoutHead(t *testing.T) {
	h := spa.Handler(fstest.MapFS{"index.html": {Data: []byte("<div>bare</div>")}}, spa.Config{AppName: "x", Env: "staging", SessionURLFile: "/nonexistent"})
	body := get(h, "GET", "/").Body.String()
	if !strings.HasPrefix(body, "<script>window.__FACTORY__") || !strings.HasSuffix(body, "<div>bare</div>") {
		t.Errorf("body = %s", body)
	}
}

func TestMissingIndex(t *testing.T) {
	h := spa.Handler(fstest.MapFS{}, spa.Config{})
	if w := get(h, "GET", "/"); w.Code != 500 {
		t.Errorf("code = %d", w.Code)
	}
}
