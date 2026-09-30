// Package spa serves an embedded single-page app: it serves files from an
// fs.FS, falls back to index.html for unknown paths, and injects
// window.__PLAYGROUND__ (app_name, env, claude_session_url, locale,
// timezone, assistant) so the frontend knows where it runs. The statement is
// served at /playground.js and index.html loads it with a classic script tag in
// <head>: the site's Content-Security-Policy (script-src 'self') blocks inline
// scripts, so it cannot be injected inline.
package spa

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// DefaultSessionURLFile is where the s6 claude service writes the Claude app
// session URL.
const DefaultSessionURLFile = "/app/.playground/session_url"

// PlaygroundJSPath is where the window.__PLAYGROUND__ statement is served.
const PlaygroundJSPath = "/playground.js"

// Config is the data injected as window.__PLAYGROUND__.
type Config struct {
	AppName string
	Env     string
	// Assistant tells the frontend the end-user assistant is enabled (the
	// command palette shows "Ask assistant..." only then). Default false.
	Assistant bool
	// Locale is PLAYGROUND_LOCALE (default en-US) and Timezone PLAYGROUND_TIMEZONE
	// (default UTC); the web package's fmt helpers format with them.
	Locale   string
	Timezone string
	// SessionURLFile is read on every index.html request; default
	// DefaultSessionURLFile. A missing file means an empty claude_session_url.
	SessionURLFile string
}

// Handler serves fsys as a SPA. AppName and Env default to the APP_NAME and
// APP_ENV environment variables.
func Handler(fsys fs.FS, cfg ...Config) http.Handler {
	var c Config
	if len(cfg) > 0 {
		c = cfg[0]
	}
	if c.AppName == "" {
		c.AppName = os.Getenv("APP_NAME")
	}
	if c.Env == "" {
		c.Env = os.Getenv("APP_ENV")
	}
	if c.Locale == "" {
		c.Locale = envOr("PLAYGROUND_LOCALE", "en-US")
	}
	if c.Timezone == "" {
		c.Timezone = envOr("PLAYGROUND_TIMEZONE", "UTC")
	}
	if c.SessionURLFile == "" {
		c.SessionURLFile = DefaultSessionURLFile
	}
	return &handler{fsys: fsys, cfg: c}
}

func envOr(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}

type handler struct {
	fsys fs.FS
	cfg  Config
}

func (h *handler) playgroundScript() []byte {
	session := ""
	if b, err := os.ReadFile(h.cfg.SessionURLFile); err == nil {
		session = strings.TrimSpace(string(b))
	}
	data, _ := json.Marshal(map[string]any{
		"app_name": h.cfg.AppName, "env": h.cfg.Env, "claude_session_url": session,
		"locale": h.cfg.Locale, "timezone": h.cfg.Timezone, "assistant": h.cfg.Assistant,
	})
	return append(append([]byte("window.__PLAYGROUND__ = "), data...), ';')
}

// scriptTag is what index.html gets: a same-origin classic script, allowed by script-src 'self'.
var scriptTag = []byte(`<script src="` + PlaygroundJSPath + `"></script>`)

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if "/"+name == PlaygroundJSPath {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(h.playgroundScript())
		}
		return
	}

	if name != "index.html" {
		if st, err := fs.Stat(h.fsys, name); err == nil && !st.IsDir() {
			f, err := h.fsys.Open(name)
			if err == nil {
				defer f.Close()
				if rs, ok := f.(readSeeker); ok {
					if strings.HasPrefix(name, "assets/") {
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					}
					http.ServeContent(w, r, name, modTime(st), rs)
					return
				}
			}
		}
		// A missing file that looks like an asset is a real 404; anything else is a client-side route.
		if path.Ext(name) != "" || strings.HasPrefix(name, "api/") {
			http.NotFound(w, r)
			return
		}
	}
	h.serveIndex(w, r)
}

type readSeeker interface {
	Read([]byte) (int, error)
	Seek(int64, int) (int64, error)
}

func modTime(st fs.FileInfo) time.Time {
	if t := st.ModTime(); !t.IsZero() {
		return t
	}
	return time.Time{}
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	idx, err := fs.ReadFile(h.fsys, "index.html")
	if err != nil {
		http.Error(w, "index.html not found in the embedded frontend", http.StatusInternalServerError)
		return
	}
	script := scriptTag
	if i := bytes.Index(bytes.ToLower(idx), []byte("</head>")); i >= 0 {
		idx = append(idx[:i:i], append(script, idx[i:]...)...)
	} else {
		idx = append(script, idx...)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(idx)
	}
}
