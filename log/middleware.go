package log

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/teb-ooo/playground-go/internal/uuidv7"
)

type ridKey struct{}

// RequestID returns the request id Middleware stored in ctx.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(ridKey{}).(string)
	return s
}

type callKey struct{}

// Fields describe who did what in a request; Annotate fills them in. All values are identifiers (a user subject, an operation id, a
// credential kind, a surface name), never user content or a token.
type Fields struct {
	// User is the subject of the signed-in user.
	User string
	// Credential is how the user proved who they are: session, bearer, token or key.
	Credential string
	// Surface is the client surface (ui, api, mcp, key).
	Surface string
	// Op is the operation id.
	Op string
}

// holder is the mutable slot Middleware puts in the context. The middleware runs outermost, so code further in (the auth middleware, an
// operation) cannot hand it a new context; it fills this slot instead.
type holder struct {
	mu sync.Mutex
	f  Fields
}

// Annotate records f on the request line of the Middleware that wraps ctx; non-empty fields overwrite earlier values. It does
// nothing when ctx has no Middleware, so libraries can call it unconditionally.
func Annotate(ctx context.Context, f Fields) {
	h, _ := ctx.Value(callKey{}).(*holder)
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if f.User != "" {
		h.f.User = f.User
	}
	if f.Credential != "" {
		h.f.Credential = f.Credential
	}
	if f.Surface != "" {
		h.f.Surface = f.Surface
	}
	if f.Op != "" {
		h.f.Op = f.Op
	}
}

func (h *holder) get() Fields {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.f
}

// Middleware logs every request with the default slog logger. See
// MiddlewareWith.
func Middleware(next http.Handler) http.Handler { return MiddlewareWith(nil, next) }

// MiddlewareWith logs one line per request with request_id, method, path,
// status and duration_ms, plus user, credential, surface and op when something further in called Annotate (error for a 5xx, warn for a 4xx, debug for a successful request to /healthz, /mcp, /api/live or /auth/me, info otherwise). The request id is taken from an incoming
// X-Request-Id header when it is a sane token, otherwise a UUIDv7 is
// generated; it is echoed in the response header and available through
// RequestID and as a logger attribute via Logger. A nil logger means slog.Default() at request time.
func MiddlewareWith(l *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := l
		if logger == nil {
			logger = slog.Default()
		}
		start := time.Now()
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = uuidv7.New()
		}
		w.Header().Set("X-Request-Id", id)
		h := &holder{}
		ctx := context.WithValue(context.WithValue(r.Context(), ridKey{}, id), callKey{}, h)
		rw := &statusWriter{ResponseWriter: w}
		defer func() {
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []slog.Attr{
				slog.String("request_id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			}
			f := h.get()
			for _, a := range [...]slog.Attr{slog.String("user", f.User), slog.String("credential", f.Credential),
				slog.String("surface", f.Surface), slog.String("op", f.Op)} {
				if a.Value.String() != "" {
					attrs = append(attrs, a)
				}
			}
			logger.LogAttrs(ctx, requestLevel(r.URL.Path, status), "request", attrs...)
		}()
		next.ServeHTTP(rw, r.WithContext(ctx))
	})
}

// quietPaths are polled or held open by clients (health checks, MCP sessions, the live stream, the session probe): a successful
// request to one is logged at debug, so it does not bury what matters at the default level.
var quietPaths = map[string]bool{"/healthz": true, "/mcp": true, "/api/live": true, "/auth/me": true}

// requestLevel is the level of a request line: error for a 5xx, warn for a 4xx, debug for a successful request to a quiet path,
// info for the rest.
func requestLevel(path string, status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	case quietPaths[path]:
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func validRequestID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps SSE working through the middleware.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Logger returns slog.Default() with the request_id attribute of ctx when Middleware ran, the request-scoped
// logger handlers and libraries use.
func Logger(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if id := RequestID(ctx); id != "" {
		l = l.With("request_id", id)
	}
	return l
}
