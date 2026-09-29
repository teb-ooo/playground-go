// Package spa serves an embedded single-page app: it serves files from an
// fs.FS, falls back to index.html for unknown paths, and injects
// window.__FACTORY__ (app_name, env, agent_url, claude_session_url, locale,
// timezone) into
// index.html so the frontend knows where it runs.
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
const DefaultSessionURLFile = "/app/.factory/session_url"

// AgentPath is the staging-only path of the agent terminal.
const AgentPath = "/_agent/tty/"

// Config is the data injected as window.__FACTORY__.
type Config struct {
	AppName string
	Env     string
	// AgentURL overrides the derived value (AgentPath on staging, empty on
	// production).
	AgentURL *string
	// Locale is FACTORY_LOCALE (default en-US) and Timezone FACTORY_TIMEZONE
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
		c.Locale = envOr("FACTORY_LOCALE", "en-US")
	}
	if c.Timezone == "" {
		c.Timezone = envOr("FACTORY_TIMEZONE", "UTC")
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

func (h *handler) factoryScript() []byte {
	agent := ""
	if h.cfg.Env != "production" && h.cfg.Env != "" {
		agent = AgentPath
	}
	if h.cfg.AgentURL != nil {
		agent = *h.cfg.AgentURL
	}
	session := ""
	if b, err := os.ReadFile(h.cfg.SessionURLFile); err == nil {
		session = strings.TrimSpace(string(b))
	}
	// json.Marshal escapes <, > and & so the value cannot break out of the script element.
	data, _ := json.Marshal(map[string]string{
		"app_name": h.cfg.AppName, "env": h.cfg.Env, "agent_url": agent, "claude_session_url": session,
		"locale": h.cfg.Locale, "timezone": h.cfg.Timezone,
	})
	return append(append([]byte("<script>window.__FACTORY__ = "), data...), []byte(";</script>")...)
}

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
	script := h.factoryScript()
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
