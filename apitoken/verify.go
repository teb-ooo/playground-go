package apitoken

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/ratelimit"
)

// TouchInterval is the least time between two last_used_at writes for one
// token.
const TouchInterval = time.Minute

// VerifierOptions configures NewVerifier. Zero values mean the defaults.
type VerifierOptions struct {
	// FailLimit limits failed lookups (unknown or malformed tokens) per client
	// IP: default 10 per minute with a burst of 10. Revoked and expired tokens
	// are not charged, the caller already holds a real token. A client over
	// the limit is refused before any lookup.
	FailLimit ratelimit.Options
	// Groups grants groups to the user a token authenticates as. The default
	// is none: a token never carries admin (or any group) unless the app
	// decides so, for example by re-resolving the owner's groups itself.
	Groups func(ctx context.Context, r Record) []string
	// Now replaces time.Now, for tests.
	Now func() time.Time
}

// Verifier checks personal access tokens against a Store.
type Verifier struct {
	store  Store
	o      VerifierOptions
	fails  *ratelimit.Limiter
	mu     sync.Mutex
	touchd map[string]time.Time // token id -> last write
}

// NewVerifier returns a Verifier. Install it with
// auth.WithTokenVerifier(v.Verify).
func NewVerifier(store Store, o VerifierOptions) *Verifier {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.FailLimit.Burst <= 0 {
		o.FailLimit.Burst = 10
	}
	if o.FailLimit.Requests <= 0 {
		o.FailLimit.Requests = 10
		o.FailLimit.Per = time.Minute
	}
	if o.FailLimit.Now == nil {
		o.FailLimit.Now = o.Now
	}
	return &Verifier{store: store, o: o, fails: ratelimit.New(o.FailLimit), touchd: map[string]time.Time{}}
}

// Verify implements auth.TokenVerifier. It returns the owning user as
// snapshotted at creation: subject, email and username, and the groups from
// VerifierOptions.Groups (none by default). It never logs or returns the
// token in an error.
func (v *Verifier) Verify(r *http.Request, token string) (auth.User, bool, error) {
	key := "ip:" + v.fails.ClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
	if blocked, wait := v.fails.Peek(key); blocked {
		return auth.User{}, false, &auth.RateLimitedError{RetryAfter: wait}
	}
	fail := func() (auth.User, bool, error) {
		if ok, wait := v.fails.Allow(key); !ok {
			return auth.User{}, false, &auth.RateLimitedError{RetryAfter: wait}
		}
		return auth.User{}, false, nil
	}
	if !wellFormed(token) {
		return fail()
	}
	hash := Hash(token)
	rec, err := v.store.Lookup(r.Context(), hash)
	if err == ErrNotFound {
		return fail()
	}
	if err != nil {
		return auth.User{}, false, err
	}
	// The store found the row by hash; compare again in constant time so a
	// store that matches loosely can never authenticate the wrong secret.
	if subtle.ConstantTimeCompare(rec.Hash, hash) != 1 {
		return fail()
	}
	now := v.o.Now()
	if !rec.Active(now) {
		return auth.User{}, false, nil
	}
	v.touch(r.Context(), rec, now)
	u := auth.User{Subject: rec.UserID, Email: rec.UserEmail, Username: rec.UserUsername}
	if v.o.Groups != nil {
		u.Groups = v.o.Groups(r.Context(), rec)
	}
	return u, true, nil
}

// touch writes last_used_at at most once per TouchInterval per token. A
// failed write is logged (without the token) and does not fail the request.
func (v *Verifier) touch(ctx context.Context, rec Record, now time.Time) {
	v.mu.Lock()
	last, seen := v.touchd[rec.ID]
	if !seen && rec.LastUsedAt != nil {
		last, seen = *rec.LastUsedAt, true
	}
	if seen && now.Sub(last) < TouchInterval {
		v.mu.Unlock()
		return
	}
	if len(v.touchd) >= 10000 {
		v.touchd = map[string]time.Time{}
	}
	v.touchd[rec.ID] = now
	v.mu.Unlock()
	if err := v.store.Touch(ctx, rec.ID, now); err != nil {
		slog.Warn("apitoken: recording last use failed", "token_id", rec.ID, "error", err)
	}
}
