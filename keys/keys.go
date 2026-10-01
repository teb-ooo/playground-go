// Package keys verifies platform API keys: signed JWTs ("pk_" + a compact
// ES256 JWS) that playd issues and any app accepts offline.
//
// An app needs no code: playground.Config.NewAuth installs a Verifier for
// every "pk_" bearer token. The Verifier checks the signature against the
// issuer's public JWKS (https://id.<domain>/.keys/jwks.json), the issuer,
// exp/nbf (30 s leeway), that the key's audience names this app, and that the
// key id (jti) is not on the revocation list
// (https://id.<domain>/.keys/revoked.json). See the README for the rules.
package keys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/ratelimit"
)

// Prefix starts every platform API key.
const Prefix = auth.KeyPrefix

// Defaults.
const (
	DefaultLeeway          = 30 * time.Second
	DefaultJWKSRefresh     = 10 * time.Minute
	DefaultUnknownKidRetry = time.Minute
	DefaultRevokedPoll     = 30 * time.Second
	DefaultRevokedStale    = 10 * time.Minute

	maxTokenLen = 4096
	maxBody     = 4 << 20
)

// ErrRevocationUnavailable is returned (as a 503) when the revocation list
// has never been loaded, or could not be refreshed for longer than
// Options.RevokedStale. Failing closed keeps revocation from being bypassed
// by blocking the endpoint.
var ErrRevocationUnavailable = &auth.UnavailableError{Err: errors.New("keys: revocation list unavailable")}

// Options configures New. Zero values mean the defaults.
type Options struct {
	// App is this app's name (Config.AppName); a key must list it in aud.
	App string
	// Domain is the playground domain (Config.PlaygroundDomain). It gives the
	// default Issuer https://id.<Domain>/.keys.
	Domain string
	// Issuer overrides the expected iss claim and the base of both endpoints.
	Issuer string
	// JWKSURL and RevokedURL override Issuer + "/jwks.json" and "/revoked.json".
	JWKSURL, RevokedURL string
	// HTTPClient fetches both documents (default: 5 s timeout).
	HTTPClient *http.Client
	// FailLimit limits failed verifications (malformed, unknown kid, bad
	// signature) per client IP: default 10 per minute, burst 10. A client over
	// the limit is refused before any parsing.
	FailLimit ratelimit.Options

	Leeway          time.Duration // default 30 s
	JWKSRefresh     time.Duration // default 10 min
	UnknownKidRetry time.Duration // least time between refreshes caused by unknown kids, default 1 min
	RevokedPoll     time.Duration // background poll, default 30 s
	RevokedStale    time.Duration // longest the last good list is used while the endpoint is down, default 10 min

	// Now replaces time.Now, for tests.
	Now func() time.Time
	// NoBackground disables the background poller (tests drive it directly).
	NoBackground bool
}

// Verifier checks platform API keys.
type Verifier struct {
	o     Options
	fails *ratelimit.Limiter

	startOnce sync.Once
	stop      context.CancelFunc
	ctx       context.Context

	jmu       sync.Mutex // serialises JWKS fetches
	kmu       sync.RWMutex
	jwks      map[string]jose.JSONWebKey
	jwksAt    time.Time // last successful fetch (or 304)
	jwksTry   time.Time // last attempt
	jwksETag  string
	jwksOut   outage
	revMu     sync.Mutex // serialises revocation fetches
	rmu       sync.RWMutex
	revoked   map[string]struct{}
	revLoaded bool
	revAt     time.Time // last success (or 304)
	revETag   string
	revOut    outage
}

// outage logs the first failure of a run of failures and the recovery.
type outage struct {
	mu   sync.Mutex
	down bool
}

func (o *outage) fail(what string, err error) {
	o.mu.Lock()
	first := !o.down
	o.down = true
	o.mu.Unlock()
	if first {
		slog.Error("keys: "+what+" unavailable", "error", err)
	}
}

func (o *outage) ok(what string) {
	o.mu.Lock()
	was := o.down
	o.down = false
	o.mu.Unlock()
	if was {
		slog.Info("keys: " + what + " available again")
	}
}

// New returns a Verifier. Install it with auth.WithKeyVerifier(v.Verify).
func New(o Options) (*Verifier, error) {
	if o.App == "" {
		return nil, errors.New("keys: App is required")
	}
	if o.Issuer == "" {
		if o.Domain == "" {
			return nil, errors.New("keys: Domain or Issuer is required")
		}
		o.Issuer = "https://id." + o.Domain + "/.keys"
	}
	o.Issuer = strings.TrimRight(o.Issuer, "/")
	if o.JWKSURL == "" {
		o.JWKSURL = o.Issuer + "/jwks.json"
	}
	if o.RevokedURL == "" {
		o.RevokedURL = o.Issuer + "/revoked.json"
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Leeway, DefaultLeeway)
	def(&o.JWKSRefresh, DefaultJWKSRefresh)
	def(&o.UnknownKidRetry, DefaultUnknownKidRetry)
	def(&o.RevokedPoll, DefaultRevokedPoll)
	def(&o.RevokedStale, DefaultRevokedStale)
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
	v := &Verifier{o: o, fails: ratelimit.New(o.FailLimit)}
	v.ctx, v.stop = context.WithCancel(context.Background())
	return v, nil
}

// Close stops the background poller.
func (v *Verifier) Close() { v.stop() }

func (v *Verifier) start() {
	v.startOnce.Do(func() {
		if v.o.NoBackground {
			return
		}
		go func() {
			t := time.NewTicker(v.o.RevokedPoll)
			defer t.Stop()
			for {
				select {
				case <-v.ctx.Done():
					return
				case <-t.C:
					_ = v.pollRevoked(v.ctx)
				}
			}
		}()
	})
}

type claims struct {
	Issuer   string          `json:"iss"`
	Subject  string          `json:"sub"`
	Email    string          `json:"email"`
	Username string          `json:"preferred_username"`
	Scope    string          `json:"scp"`
	Aud      json.RawMessage `json:"aud"`
	JTI      string          `json:"jti"`
	Exp      *float64        `json:"exp"`
	NBF      *float64        `json:"nbf"`
	Groups   []string        `json:"groups"`
}

func (c claims) audience() []string {
	var many []string
	if json.Unmarshal(c.Aud, &many) == nil {
		return many
	}
	var one string
	if json.Unmarshal(c.Aud, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

// Verify implements auth.TokenVerifier for a "pk_" key. ok=false with a nil
// error is a plain 401; a *auth.RateLimitedError is a 429; an
// *auth.UnavailableError (ErrRevocationUnavailable, or a JWKS that was never
// loaded) is a 503. The token is never logged or returned in an error.
func (v *Verifier) Verify(r *http.Request, token string) (auth.User, bool, error) {
	v.start()
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
	if !strings.HasPrefix(token, Prefix) || len(token) > maxTokenLen {
		return fail()
	}
	// Only ES256 is accepted: alg none, HS256 and RS256 never get as far as a key.
	jws, err := jose.ParseSigned(token[len(Prefix):], []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(jws.Signatures) != 1 {
		return fail()
	}
	kid := jws.Signatures[0].Header.KeyID
	if kid == "" {
		return fail()
	}
	ctx := r.Context()
	jwk, found, err := v.lookupKey(ctx, kid)
	if err != nil {
		return auth.User{}, false, err
	}
	if !found {
		return fail()
	}
	payload, err := jws.Verify(jwk.Key)
	if err != nil {
		return fail()
	}
	var c claims
	if json.Unmarshal(payload, &c) != nil {
		return fail()
	}
	// From here on the signature is valid: the caller holds a real key, so the
	// remaining refusals are not charged to the failure limiter.
	now := v.o.Now()
	if c.Issuer != v.o.Issuer || c.Subject == "" || c.JTI == "" || c.Exp == nil {
		return auth.User{}, false, nil
	}
	if now.After(unix(*c.Exp).Add(v.o.Leeway)) {
		return auth.User{}, false, nil
	}
	if c.NBF != nil && now.Add(v.o.Leeway).Before(unix(*c.NBF)) {
		return auth.User{}, false, nil
	}
	if !slices.Contains(c.audience(), v.o.App) {
		return auth.User{}, false, nil
	}
	isRevoked, err := v.isRevoked(ctx, c.JTI)
	if err != nil {
		return auth.User{}, false, err
	}
	if isRevoked {
		return auth.User{}, false, nil
	}
	scopes := strings.Fields(c.Scope)
	if scopes == nil {
		scopes = []string{}
	}
	return auth.User{Subject: c.Subject, Email: c.Email, Username: c.Username, Groups: c.Groups, Scopes: scopes}, true, nil
}

func unix(f float64) time.Time { return time.Unix(int64(f), 0) }

// ---- JWKS ----

type jwksDoc = jose.JSONWebKeySet

// lookupKey returns the key for kid, refreshing the cache when it was never
// loaded, when it is older than JWKSRefresh, or when kid is unknown; refreshes
// after the first are at least UnknownKidRetry apart. A failed refresh keeps
// the old keys.
func (v *Verifier) lookupKey(ctx context.Context, kid string) (jose.JSONWebKey, bool, error) {
	now := v.o.Now()
	v.kmu.RLock()
	k, ok := v.jwks[kid]
	loaded := v.jwks != nil
	stale := now.Sub(v.jwksAt) >= v.o.JWKSRefresh
	canTry := now.Sub(v.jwksTry) >= v.o.UnknownKidRetry
	v.kmu.RUnlock()
	switch {
	case !loaded:
		if err := v.refreshJWKS(ctx, true); err != nil {
			return jose.JSONWebKey{}, false, &auth.UnavailableError{Err: errors.New("keys: signing keys unavailable")}
		}
	case canTry && (!ok || stale):
		_ = v.refreshJWKS(ctx, false)
	default:
		return k, ok, nil
	}
	v.kmu.RLock()
	k, ok = v.jwks[kid]
	v.kmu.RUnlock()
	return k, ok, nil
}

func (v *Verifier) refreshJWKS(ctx context.Context, force bool) error {
	v.jmu.Lock()
	defer v.jmu.Unlock()
	now := v.o.Now()
	v.kmu.RLock()
	recent := v.jwks != nil && now.Sub(v.jwksTry) < v.o.UnknownKidRetry
	etag := v.jwksETag
	v.kmu.RUnlock()
	if recent && !force {
		return nil // another request just refreshed
	}
	v.kmu.Lock()
	v.jwksTry = now
	v.kmu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, newETag, notModified, err := v.get(cctx, v.o.JWKSURL, etag)
	if err != nil {
		v.jwksOut.fail("signing key set", err)
		return err
	}
	if notModified {
		v.kmu.Lock()
		v.jwksAt = now
		v.kmu.Unlock()
		v.jwksOut.ok("signing key set")
		return nil
	}
	var set jwksDoc
	if err := json.Unmarshal(body, &set); err != nil {
		v.jwksOut.fail("signing key set", errors.New("malformed document"))
		return errors.New("keys: malformed jwks")
	}
	m := map[string]jose.JSONWebKey{}
	for _, k := range set.Keys {
		if k.KeyID == "" || !k.Valid() || !k.IsPublic() || (k.Algorithm != "" && k.Algorithm != string(jose.ES256)) || (k.Use != "" && k.Use != "sig") {
			continue
		}
		m[k.KeyID] = k
	}
	v.kmu.Lock()
	v.jwks, v.jwksAt, v.jwksETag = m, now, newETag
	v.kmu.Unlock()
	v.jwksOut.ok("signing key set")
	return nil
}

// get fetches url with a conditional request. Errors never include the body.
func (v *Verifier) get(ctx context.Context, url, etag string) (body []byte, newETag string, notModified bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, err
	}
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := v.o.HTTPClient.Do(req)
	if err != nil {
		return nil, "", false, fmt.Errorf("keys: fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, etag, true, nil
	case http.StatusOK:
	default:
		return nil, "", false, fmt.Errorf("keys: fetching %s: status %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, "", false, fmt.Errorf("keys: reading %s: %w", url, err)
	}
	return b, resp.Header.Get("ETag"), false, nil
}

// ---- revocation ----

type revokedDoc struct {
	UpdatedAt string `json:"updated_at"`
	Revoked   []struct {
		JTI string  `json:"jti"`
		Exp float64 `json:"exp"`
	} `json:"revoked"`
}

// isRevoked reports whether jti is on the list. It loads the list first if it
// never was, and refuses with ErrRevocationUnavailable when the last good list
// is older than RevokedStale.
func (v *Verifier) isRevoked(ctx context.Context, jti string) (bool, error) {
	v.rmu.RLock()
	loaded := v.revLoaded
	v.rmu.RUnlock()
	if !loaded {
		if err := v.pollRevoked(ctx); err != nil {
			return false, ErrRevocationUnavailable
		}
	}
	now := v.o.Now()
	v.rmu.RLock()
	age := now.Sub(v.revAt)
	_, hit := v.revoked[jti]
	v.rmu.RUnlock()
	if age > v.o.RevokedStale {
		// The poller may be behind (or not running): try once before giving up.
		if err := v.pollRevoked(ctx); err != nil {
			return false, ErrRevocationUnavailable
		}
		v.rmu.RLock()
		_, hit = v.revoked[jti]
		v.rmu.RUnlock()
	}
	return hit, nil
}

// pollRevoked fetches the revocation list (ETag aware). The background poller
// calls it every RevokedPoll.
func (v *Verifier) pollRevoked(ctx context.Context) error {
	v.revMu.Lock()
	defer v.revMu.Unlock()
	v.rmu.RLock()
	etag := v.revETag
	v.rmu.RUnlock()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, newETag, notModified, err := v.get(cctx, v.o.RevokedURL, etag)
	now := v.o.Now()
	if err != nil {
		v.revOut.fail("revocation list", err)
		return err
	}
	if notModified {
		v.rmu.Lock()
		v.revAt = now
		v.rmu.Unlock()
		v.revOut.ok("revocation list")
		return nil
	}
	var d revokedDoc
	if err := json.Unmarshal(body, &d); err != nil {
		v.revOut.fail("revocation list", errors.New("malformed document"))
		return errors.New("keys: malformed revocation list")
	}
	m := make(map[string]struct{}, len(d.Revoked))
	for _, e := range d.Revoked {
		// An entry whose key has expired beyond the leeway can never match again.
		if e.JTI == "" || (e.Exp > 0 && now.After(unix(e.Exp).Add(v.o.Leeway))) {
			continue
		}
		m[e.JTI] = struct{}{}
	}
	v.rmu.Lock()
	v.revoked, v.revLoaded, v.revAt, v.revETag = m, true, now, newETag
	v.rmu.Unlock()
	v.revOut.ok("revocation list")
	return nil
}
