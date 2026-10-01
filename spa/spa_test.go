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

	"github.com/teb-ooo/playground-go/spa"
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
		{"root gets index with config", "GET", "/", 200, []string{"<title>t</title>", `<script src="/playground.js"></script>`}, []string{"window.__PLAYGROUND__"}, "no-store"},
		{"playground.js carries the config", "GET", "/playground.js", 200, []string{"window.__PLAYGROUND__ = ", `"app_name":"hello"`, `"env":"staging"`, `"claude_session_url":"https://claude.ai/code/session_abc"`}, []string{"<script"}, "no-store"},
		{"config script is inside head", "GET", "/", 200, []string{`<script src="/playground.js"></script></head>`}, nil, ""},
		{"unknown route falls back", "GET", "/items/42", 200, []string{`<script src="/playground.js">`, "<body>app</body>"}, nil, "no-store"},
		{"asset served", "GET", "/assets/app.js", 200, []string{"console.log(1)"}, []string{"__PLAYGROUND__"}, "public, max-age=31536000, immutable"},
		{"root file served", "GET", "/favicon.svg", 200, []string{"<svg/>"}, nil, ""},
		{"missing asset is 404", "GET", "/assets/missing.js", 404, nil, nil, ""},
		{"api path is 404", "GET", "/api/nothing", 404, nil, nil, ""},
		{"directory falls back", "GET", "/docs", 200, []string{"/playground.js"}, nil, ""},
		{"traversal is cleaned to a client route", "GET", "/../../etc/passwd", 200, []string{"/playground.js"}, []string{"root:"}, ""},
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
		{"from env", map[string]string{"PLAYGROUND_LOCALE": "sv-SE", "PLAYGROUND_TIMEZONE": "Europe/Stockholm"}, spa.Config{}, "sv-SE", "Europe/Stockholm"},
		{"config wins", map[string]string{"PLAYGROUND_LOCALE": "sv-SE"}, spa.Config{Locale: "de-DE", Timezone: "Europe/Berlin"}, "de-DE", "Europe/Berlin"},
		{"blank env is default", map[string]string{"PLAYGROUND_LOCALE": "  ", "PLAYGROUND_TIMEZONE": ""}, spa.Config{}, "en-US", "UTC"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PLAYGROUND_LOCALE", "")
			t.Setenv("PLAYGROUND_TIMEZONE", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			tc.cfg.SessionURLFile = "/nonexistent"
			body := get(spa.Handler(site, tc.cfg), "GET", "/playground.js").Body.String()
			for _, want := range []string{`"locale":"` + tc.wantLocale + `"`, `"timezone":"` + tc.wantTZ + `"`} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
		})
	}
}

func TestNoAgentTerminalConfig(t *testing.T) {
	h := spa.Handler(site, spa.Config{AppName: "hello", Env: "staging", SessionURLFile: "/nonexistent/file"})
	body := get(h, "GET", "/playground.js").Body.String()
	if strings.Contains(body, "agent_url") || !strings.Contains(body, `"claude_session_url":""`) {
		t.Errorf("body = %s", body)
	}
}

func TestEnvDefaultsAndEscaping(t *testing.T) {
	t.Setenv("APP_NAME", `</script><b>&`)
	t.Setenv("APP_ENV", "production")
	h := spa.Handler(site, spa.Config{SessionURLFile: "/nonexistent"})
	body := get(h, "GET", "/playground.js").Body.String()
	if strings.Contains(body, "</script><b>") {
		t.Fatalf("app name is not escaped: %s", body)
	}
	re := regexp.MustCompile(`^window\.__PLAYGROUND__ = (\{.*\});$`)
	if !re.MatchString(body) {
		t.Fatalf("no config: %s", body)
	}
}

func TestIndexWithoutHead(t *testing.T) {
	h := spa.Handler(fstest.MapFS{"index.html": {Data: []byte("<div>bare</div>")}}, spa.Config{AppName: "x", Env: "staging", SessionURLFile: "/nonexistent"})
	body := get(h, "GET", "/").Body.String()
	if !strings.HasPrefix(body, `<script src="/playground.js"></script>`) || !strings.HasSuffix(body, "<div>bare</div>") {
		t.Errorf("body = %s", body)
	}
}

func TestMissingIndex(t *testing.T) {
	h := spa.Handler(fstest.MapFS{}, spa.Config{})
	if w := get(h, "GET", "/"); w.Code != 500 {
		t.Errorf("code = %d", w.Code)
	}
}

func TestAssistantFlag(t *testing.T) {
	tests := []struct {
		name string
		on   bool
		want string
	}{{"default off", false, `"assistant":false`}, {"on", true, `"assistant":true`}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := spa.Handler(site, spa.Config{Assistant: tc.on, SessionURLFile: "/nonexistent"})
			if body := get(h, "GET", "/playground.js").Body.String(); !strings.Contains(body, tc.want) {
				t.Errorf("body missing %s: %s", tc.want, body)
			}
		})
	}
}

func TestPlatformDomainInjected(t *testing.T) {
	site := fstest.MapFS{"index.html": {Data: []byte("<head></head>")}}
	for _, tc := range []struct{ name, env, cfg, want string }{
		{"config", "", "teb.ooo", `"platform_domain":"teb.ooo"`},
		{"env", "example.test", "", `"platform_domain":"example.test"`},
		{"unset", "", "", `"platform_domain":""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PLAYGROUND_DOMAIN", tc.env)
			h := spa.Handler(site, spa.Config{PlatformDomain: tc.cfg, SessionURLFile: "/nonexistent"})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/playground.js", nil))
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("got %s, want %s", w.Body.String(), tc.want)
			}
		})
	}
}
