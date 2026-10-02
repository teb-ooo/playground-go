// Package ratelimit is an in-memory token-bucket rate limiter for net/http and
// Huma. Requests are keyed by client IP and, optionally, by a caller-supplied
// key such as the username in a form body; every key involved must have a token
// or the request is refused with 429, a Retry-After header and a problem+json
// body.
//
// Client IP: when the connection comes from a trusted proxy (by default any
// loopback, private or link-local address, which covers Caddy on the docker
// network) the LAST entry of X-Forwarded-For is used, because that is the
// address the proxy itself observed; earlier entries are client-controlled and
// ignored. Connections from anywhere else are keyed by their own address, and
// any X-Forwarded-For header is ignored. IPv6 clients are keyed by their /64.
//
// State is per process and in memory: it resets on restart and is not shared
// between replicas.
package ratelimit

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/teb-ooo/playground-go/internal/problem"
)

// Defaults: 10 requests per minute, burst 5.
const (
	DefaultRequests = 10
	DefaultPer      = time.Minute
	DefaultBurst    = 5
	DefaultMaxKeys  = 10000
)

// Options configures a Limiter. Zero values mean the defaults.
type Options struct {
	// Requests per Per is the sustained rate (default 10 per minute).
	Requests int
	Per      time.Duration
	// Burst is the bucket size (default 5).
	Burst int
	// Key, if set, adds a second key per request (for example the username
	// from the form body); returning "" skips it. Both the IP and this key
	// must have a token.
	Key func(*http.Request) string
	// TrustedProxy decides whether a connection's remote address may set
	// X-Forwarded-For. Default: loopback, private and link-local addresses.
	TrustedProxy func(netip.Addr) bool
	// MaxKeys bounds memory: when reached, full buckets are dropped first and
	// then all buckets (default 10000).
	MaxKeys int
	// Now replaces time.Now, for tests.
	Now func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a set of token buckets.
type Limiter struct {
	o    Options
	rate float64 // tokens per second

	mu      sync.Mutex
	buckets map[string]*bucket
}

// New returns a Limiter.
func New(o Options) *Limiter {
	if o.Requests <= 0 {
		o.Requests = DefaultRequests
	}
	if o.Per <= 0 {
		o.Per = DefaultPer
	}
	if o.Burst <= 0 {
		o.Burst = DefaultBurst
	}
	if o.MaxKeys <= 0 {
		o.MaxKeys = DefaultMaxKeys
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.TrustedProxy == nil {
		o.TrustedProxy = DefaultTrustedProxy
	}
	return &Limiter{o: o, rate: float64(o.Requests) / o.Per.Seconds(), buckets: map[string]*bucket{}}
}

// DefaultTrustedProxy trusts loopback, private and link-local addresses.
func DefaultTrustedProxy(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast()
}

// Allow takes one token from each key's bucket. If any bucket is empty it
// takes nothing and returns false with how long until every key would have a
// token.
func (l *Limiter) Allow(keys ...string) (ok bool, retryAfter time.Duration) {
	now := l.o.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	var wait float64
	for _, k := range keys {
		b := l.get(k, now)
		if b.tokens < 1 {
			if w := (1 - b.tokens) / l.rate; w > wait {
				wait = w
			}
		}
	}
	if wait > 0 {
		return false, time.Duration(math.Ceil(wait*1000)) * time.Millisecond
	}
	for _, k := range keys {
		l.buckets[k].tokens--
	}
	return true, 0
}

// Peek reports, without taking a token, whether Allow would refuse these keys
// now, and how long until every key would have a token. Callers that charge a
// key only for failures use Peek to refuse a key that
// has run out before doing any work for it. A key never seen is not created.
func (l *Limiter) Peek(keys ...string) (blocked bool, retryAfter time.Duration) {
	now := l.o.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var wait float64
	for _, k := range keys {
		b, ok := l.buckets[k]
		if !ok {
			continue
		}
		tokens := math.Min(float64(l.o.Burst), b.tokens+math.Max(0, now.Sub(b.last).Seconds())*l.rate)
		if tokens < 1 {
			if w := (1 - tokens) / l.rate; w > wait {
				wait = w
			}
		}
	}
	if wait > 0 {
		return true, time.Duration(math.Ceil(wait*1000)) * time.Millisecond
	}
	return false, 0
}

// get returns the refilled bucket for k, creating a full one if needed.
func (l *Limiter) get(k string, now time.Time) *bucket {
	b, ok := l.buckets[k]
	if !ok {
		if len(l.buckets) >= l.o.MaxKeys {
			l.prune(now)
		}
		b = &bucket{tokens: float64(l.o.Burst), last: now}
		l.buckets[k] = b
		return b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(float64(l.o.Burst), b.tokens+el*l.rate)
		b.last = now
	}
	return b
}

func (l *Limiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= float64(l.o.Burst) {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= l.o.MaxKeys {
		l.buckets = map[string]*bucket{}
	}
}

// ClientIP returns the key address for a request: see the package comment.
func (l *Limiter) ClientIP(remoteAddr, xForwardedFor string) string {
	remote := hostOf(remoteAddr)
	ra, err := netip.ParseAddr(remote)
	if err != nil {
		return remote
	}
	client := ra
	if l.o.TrustedProxy(ra) && xForwardedFor != "" {
		last := xForwardedFor
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		if a, err := netip.ParseAddr(strings.TrimSpace(last)); err == nil {
			client = a
		}
	}
	client = client.Unmap()
	if client.Is6() {
		if p, err := client.Prefix(64); err == nil {
			return p.Masked().String()
		}
	}
	return client.String()
}

func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func retryAfterSeconds(d time.Duration) string {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		s = 1
	}
	return strconv.Itoa(s)
}

// Middleware refuses requests over the limit with 429, Retry-After and a
// problem+json body.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys := []string{"ip:" + l.ClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))}
		if l.o.Key != nil {
			if k := l.o.Key(r); k != "" {
				keys = append(keys, "key:"+k)
			}
		}
		if ok, wait := l.Allow(keys...); !ok {
			w.Header().Set("Retry-After", retryAfterSeconds(wait))
			problem.Write(w, http.StatusTooManyRequests, "Too Many Requests", "Too many requests. Try again in "+retryAfterSeconds(wait)+" seconds.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Handler wraps a single handler function.
func (l *Limiter) Handler(h http.HandlerFunc) http.Handler { return l.Middleware(h) }

// HumaMiddleware is a Huma middleware (api.UseMiddleware, or an operation's
// Middlewares) keyed by client IP.
func (l *Limiter) HumaMiddleware(api huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		ip := l.ClientIP(ctx.RemoteAddr(), ctx.Header("X-Forwarded-For"))
		if ok, wait := l.Allow("ip:" + ip); !ok {
			ctx.SetHeader("Retry-After", retryAfterSeconds(wait))
			_ = huma.WriteErr(api, ctx, http.StatusTooManyRequests, "Too many requests. Try again in "+retryAfterSeconds(wait)+" seconds.")
			return
		}
		next(ctx)
	}
}

// Check is for Huma handlers that key on something only the handler knows,
// such as a username in the request body: it takes a token from each key
// (used verbatim; prefix them yourself, for example "user:"+name) and returns
// a 429 huma error with a Retry-After header when one is empty.
func (l *Limiter) Check(keys ...string) error {
	if ok, wait := l.Allow(keys...); !ok {
		return huma.ErrorWithHeaders(
			huma.NewError(http.StatusTooManyRequests, "Too many requests. Try again in "+retryAfterSeconds(wait)+" seconds."),
			http.Header{"Retry-After": {retryAfterSeconds(wait)}})
	}
	return nil
}
