package live

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Connected is the Name of the Event an Upstream sends once its connection is established, before any change. The
// relay uses it to end a degraded state.
const Connected = "connected"

// DegradedReason is what users are told while a relayed source is down. No internals.
const DegradedReason = "live updates are delayed: the source of changes is unavailable"

// UpstreamEvent is one event of the upstream stream: its SSE name, id and data.
type UpstreamEvent struct {
	Name string
	ID   string
	Data string
}

// Upstream is a stream of changes made elsewhere (playd's /v1/work/events, for instance). Stream connects, calls fn
// with Event{Name: Connected} once connected, then once per event, and returns when the stream ends or ctx is done.
// It returns a non-nil error when the stream could not be opened or ended abnormally; a clean end returns nil.
type Upstream interface {
	Stream(ctx context.Context, fn func(UpstreamEvent)) error
}

// Translate turns an upstream event into a hub publication. ok false skips the event. It must not trust the data
// beyond what it parses: the resource name and audience are the app's decision.
type Translate func(UpstreamEvent) (resource string, aud Audience, ok bool)

type relayConfig struct{ min, max time.Duration }

// RelayOption configures Relay.
type RelayOption func(*relayConfig)

// WithBackoff sets the reconnect delay: it starts at min and doubles up to max (default 500 ms and 30 s).
func WithBackoff(min, max time.Duration) RelayOption {
	return func(c *relayConfig) { c.min, c.max = min, max }
}

// Relay copies the upstream stream into hub until ctx is done: each event goes through translate to hub.Publish. It
// reconnects with backoff. While the upstream is down it calls hub.Degraded; when it is back it publishes an event
// naming no resource (everything may have changed meanwhile), which clears the degraded state. Run it in its own
// goroutine; it returns only when ctx ends.
func Relay(ctx context.Context, hub *Hub, up Upstream, translate Translate, opts ...RelayOption) {
	c := relayConfig{min: 500 * time.Millisecond, max: 30 * time.Second}
	for _, o := range opts {
		o(&c)
	}
	delay := c.min
	first, down := true, false
	for ctx.Err() == nil {
		connected := false
		err := up.Stream(ctx, func(e UpstreamEvent) {
			if e.Name == Connected {
				connected = true
				delay = c.min
				if down || !first {
					hub.Publish("", Everyone()) // changes were missed while we were away
				}
				first, down = false, false
				return
			}
			if res, aud, ok := translate(e); ok {
				hub.Publish(res, aud)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if !connected || err != nil {
			down = true
			hub.Degraded(DegradedReason)
		}
		if !sleepCtx(ctx, delay) {
			return
		}
		if delay *= 2; delay > c.max {
			delay = c.max
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// SSEUpstream is an Upstream over an HTTP server-sent-events response. Open performs the request (with the app's
// credentials for the source) and returns the response; a non-2xx status is an error.
type SSEUpstream struct {
	Open func(ctx context.Context) (*http.Response, error)
}

// Stream implements Upstream.
func (s SSEUpstream) Stream(ctx context.Context, fn func(UpstreamEvent)) error {
	res, err := s.Open(ctx)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("live: upstream answered %d", res.StatusCode)
	}
	fn(UpstreamEvent{Name: Connected})
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	var e UpstreamEvent
	var have bool
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if have {
				fn(e)
			}
			e, have = UpstreamEvent{}, false
		case strings.HasPrefix(line, ":"):
		default:
			k, v, _ := strings.Cut(line, ":")
			v = strings.TrimPrefix(v, " ")
			switch k {
			case "event":
				e.Name, have = v, true
			case "id":
				e.ID = v
			case "data":
				if e.Data != "" {
					e.Data += "\n"
				}
				e.Data += v
				have = true
			}
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
