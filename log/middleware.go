package log

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/teb-ooo/playground-go/internal/uuidv7"
)

type ridKey struct{}

// RequestID returns the request id Middleware stored in ctx.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(ridKey{}).(string)
	return s
}

// Middleware logs every request with the default slog logger. See
// MiddlewareWith.
func Middleware(next http.Handler) http.Handler { return MiddlewareWith(nil, next) }

// MiddlewareWith logs one line per request with request_id, method, path,
// status and duration_ms. The request id is taken from an incoming
// X-Request-Id header when it is a sane token, otherwise a UUIDv7 is
// generated; it is echoed in the response header and available through
// RequestID and as a logger attribute via Logger. Requests to /healthz are
// logged at debug level. A nil logger means slog.Default() at request time.
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
		ctx := context.WithValue(r.Context(), ridKey{}, id)
		rw := &statusWriter{ResponseWriter: w}
		defer func() {
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			level := slog.LevelInfo
			if r.URL.Path == "/healthz" {
				level = slog.LevelDebug
			} else if status >= 500 {
				level = slog.LevelError
			}
			logger.LogAttrs(ctx, level, "request",
				slog.String("request_id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
		}()
		next.ServeHTTP(rw, r.WithContext(ctx))
	})
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
