package live

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/internal/problem"
)

// Path is where the stream is mounted. @teb-ooo/web's useLive() opens it by default.
const Path = "/api/live"

// Defaults of the handler.
const (
	DefaultHeartbeat    = 25 * time.Second
	DefaultMaxLifetime  = time.Hour
	DefaultWriteTimeout = 10 * time.Second
)

type handlerConfig struct {
	auth         func(http.Handler) http.Handler
	user         func(*http.Request) (auth.User, bool)
	heartbeat    time.Duration
	maxLifetime  time.Duration
	writeTimeout time.Duration
}

// Option configures Handler and Mount.
type Option func(*handlerConfig)

// WithAuth wraps the stream in the app's own middleware (the id app uses its Kratos session). The middleware may
// answer 401 itself; when it lets the request through, the handler still needs a user (see WithUser).
func WithAuth(mw func(http.Handler) http.Handler) Option {
	return func(c *handlerConfig) { c.auth = mw }
}

// WithUser says how the handler learns who is asking. The default is auth.FromContext, which is what the playground-go
// auth middleware (authn.Middleware, installed by the template around the whole mux) fills in. An app with its own
// session scheme sets it, together with WithAuth. A request without a user is answered 401 problem+json.
func WithUser(f func(*http.Request) (auth.User, bool)) Option {
	return func(c *handlerConfig) { c.user = f }
}

// WithHeartbeat sets how often a `: ping` comment is written on a quiet stream (default 25 s; proxies close silent
// connections).
func WithHeartbeat(d time.Duration) Option { return func(c *handlerConfig) { c.heartbeat = d } }

// WithMaxLifetime sets how long a stream lives before the server closes it (default one hour). The client then
// reconnects and signs in again, so an expired session cannot keep a stream.
func WithMaxLifetime(d time.Duration) Option { return func(c *handlerConfig) { c.maxLifetime = d } }

// WithWriteTimeout sets the per-write deadline (default 10 s): a client that stops reading is dropped.
func WithWriteTimeout(d time.Duration) Option { return func(c *handlerConfig) { c.writeTimeout = d } }

// Mount registers the stream as GET /api/live on mux. It is a plain mux handler, not a Huma operation: the OpenAPI
// document, the MCP tools and the parity check never see it.
func Mount(mux *http.ServeMux, hub *Hub, opts ...Option) {
	mux.Handle("GET "+Path, Handler(hub, opts...))
}

// Handler is the stream. It answers 401 problem+json when nobody is signed in, 429 problem+json (Retry-After: 5)
// when the user already holds the maximum number of streams, and otherwise 200 text/event-stream: a `: live`
// comment at once (headers out, so the client counts the stream live), then `event: change` frames, `: ping`
// comments, and `event: degraded`. It ends when the client leaves, the hub's subscription is cancelled or the maximum
// lifetime passes.
func Handler(hub *Hub, opts ...Option) http.Handler {
	c := handlerConfig{user: func(r *http.Request) (auth.User, bool) { return auth.FromContext(r.Context()) }, heartbeat: DefaultHeartbeat, maxLifetime: DefaultMaxLifetime, writeTimeout: DefaultWriteTimeout}
	for _, o := range opts {
		o(&c)
	}
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(hub, &c, w, r) })
	if c.auth != nil {
		h = c.auth(h)
	}
	return h
}

func serve(hub *Hub, c *handlerConfig, w http.ResponseWriter, r *http.Request) {
	u, ok := c.user(r)
	if !ok {
		problem.Write(w, http.StatusUnauthorized, "Unauthorized", "sign in to follow changes")
		return
	}
	sub, cancel, err := hub.Subscribe(u)
	if err != nil {
		if errors.Is(err, ErrTooManyStreams) {
			w.Header().Set("Retry-After", "5")
			problem.Write(w, http.StatusTooManyRequests, "Too Many Requests", "too many live streams are open for you; close one and retry")
			return
		}
		problem.Write(w, http.StatusInternalServerError, "Internal Server Error", "internal error")
		return
	}
	defer cancel()

	rc := http.NewResponseController(w)
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(s string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(c.writeTimeout)) // a dead client must not hold the stream
		if _, err := w.Write([]byte(s)); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write(": live\n\n") {
		return
	}
	// No data event on connect: the client counts the open stream as live and, after a reconnect, refreshes
	// everything itself.

	beat := time.NewTicker(c.heartbeat)
	defer beat.Stop()
	life := time.NewTimer(c.maxLifetime)
	defer life.Stop()
	// Frames queued before the first write (a degraded hub) go out at once.
	pump := func() bool {
		for _, f := range sub.Take() {
			if !write(f.wire()) {
				return false
			}
		}
		return true
	}
	if !pump() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-life.C:
			return
		case <-beat.C:
			if !write(": ping\n\n") {
				return
			}
		case <-sub.Ready():
			if !pump() {
				return
			}
		}
	}
}

type changeData struct {
	Resource string `json:"resource,omitempty"`
	Project  string `json:"project,omitempty"`
	ID       string `json:"id,omitempty"`
}

// wire renders the frame as one SSE message.
func (f Frame) wire() string {
	if f.Degraded {
		b, _ := json.Marshal(map[string]string{"reason": f.Reason})
		return "event: degraded\ndata: " + string(b) + "\n\n"
	}
	b, _ := json.Marshal(changeData{Resource: oneLine(f.Event.Resource), Project: oneLine(f.Event.Project), ID: oneLine(f.Event.ID)})
	return fmt.Sprintf("event: change\nid: %d\ndata: %s\n\n", f.Seq, b)
}

// oneLine keeps a stray newline out of the framing (JSON escapes it anyway; this is belt and braces for the id).
func oneLine(s string) string { return strings.NewReplacer("\r", "", "\n", "").Replace(s) }
