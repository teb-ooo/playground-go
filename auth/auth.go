package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/oauth2"

	"github.com/teb-ooo/playground-go/internal/problem"
	"github.com/teb-ooo/playground-go/ratelimit"
)

// OIDCConfig is the provider configuration, read from OIDC_ISSUER,
// OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and PUBLIC_URL.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// PublicURL is the app's external origin, for example https://hello.teb.ooo.
	// The redirect URI is PublicURL + "/auth/callback".
	PublicURL string
	// Scopes defaults to openid, email, profile, groups.
	Scopes []string
}

// RedirectURL is the OAuth redirect URI registered for this app.
func (c OIDCConfig) RedirectURL() string {
	return strings.TrimRight(c.PublicURL, "/") + "/auth/callback"
}

// Auth runs the sign-in flow and identifies callers.
type Auth struct {
	cfg    OIDCConfig
	key    []byte
	ttl    time.Duration
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
	bearer   *bearerCache

	tokenVerifier TokenVerifier
	keyVerifier   TokenVerifier
	appName       string

	owner      string
	rl         ratelimit.Options
	noRL       bool
	entrance   string
	loginRL    *ratelimit.Limiter
	callbackRL *ratelimit.Limiter
}

// Option customises New.
type Option func(*Auth)

// WithHTTPClient sets the client used to talk to the issuer.
func WithHTTPClient(c *http.Client) Option { return func(a *Auth) { a.client = c } }

// WithOwner sets the app owner's email (APP_OWNER). GET /auth/me then reports is_owner for the user whose email
// matches it, compared case-insensitively. An empty owner means nobody is the owner.
func WithOwner(email string) Option {
	return func(a *Auth) { a.owner = strings.ToLower(strings.TrimSpace(email)) }
}

// PATPrefix starts every personal access token. A bearer token with this
// prefix goes to the TokenVerifier (never to the OIDC path); without a
// verifier it is treated as any other bearer token and fails.
const PATPrefix = "pat_"

// TokenVerifier checks a personal access token (the whole bearer value, with
// its PATPrefix) presented on request r and returns the user it belongs to.
// ok=false with a nil error means unknown, revoked or expired (401); a
// *RateLimitedError means the client is making too many failed attempts (429);
// any other error is a backend failure (503, logged, not shown). The error
// must never contain the token. r is passed so the verifier can rate limit by
// client IP. Platform API keys (package keys) cover the common case.
type TokenVerifier func(r *http.Request, token string) (u User, ok bool, err error)

// RateLimitedError is returned by a TokenVerifier that refused a client.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return "auth: too many failed token attempts" }

// WithTokenVerifier lets Middleware and BearerOrSession accept personal access
// tokens: a bearer token starting with PATPrefix is checked by v before, and
// instead of, the OIDC path. Users it returns are recorded with
// CredentialToken. Without this option nothing changes.
func WithTokenVerifier(v TokenVerifier) Option { return func(a *Auth) { a.tokenVerifier = v } }

// Problem codes are the closed list the entrance page receives as `?problem=<code>` (see WithEntrance).
const (
	ProblemExpired     = "expired"     // the sign-in attempt expired, or cookies are disabled
	ProblemRefused     = "refused"     // the identity service refused the sign-in
	ProblemUnavailable = "unavailable" // the identity service could not be reached
	ProblemFailed      = "failed"      // anything else that went wrong
)

// WithEntrance sends a sign-in problem on /auth/login and /auth/callback to the app's own entrance page (for example "/enter") as
// `<path>?problem=<code>` (303), where the page shows it in its own words, instead of answering with the bare problem document.
// Without it nothing changes. The path must be an absolute path of the app that does not redirect a signed-out visitor.
func WithEntrance(path string) Option { return func(a *Auth) { a.entrance = path } }

// WithSessionTTL sets the session lifetime (default DefaultSessionTTL).
func WithSessionTTL(d time.Duration) Option { return func(a *Auth) { a.ttl = d } }

// WithRateLimit configures the limiter on /auth/login and /auth/callback. The
// default is 10 requests per minute per client IP with a burst of 5 (see the
// ratelimit package for how the client IP is derived behind Caddy). Login and
// callback have separate buckets.
func WithRateLimit(o ratelimit.Options) Option { return func(a *Auth) { a.rl = o } }

// WithoutRateLimit disables rate limiting on the auth routes.
func WithoutRateLimit() Option { return func(a *Auth) { a.noRL = true } }

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option { return func(a *Auth) { a.now = now } }

// New validates the configuration and returns an Auth. Discovery of the
// issuer is deferred to first use, so an app still boots (and serves
// /healthz) while the identity service is briefly unavailable.
func New(cfg OIDCConfig, sessionKey []byte, opts ...Option) (*Auth, error) {
	switch {
	case cfg.Issuer == "":
		return nil, errors.New("auth: OIDC issuer is required")
	case cfg.ClientID == "":
		return nil, errors.New("auth: OIDC client id is required")
	case cfg.PublicURL == "":
		return nil, errors.New("auth: public URL is required")
	}
	if u, err := url.Parse(cfg.PublicURL); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("auth: public URL %q is not an absolute URL", cfg.PublicURL)
	}
	if _, err := newGCM(sessionKey); err != nil {
		return nil, err
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "email", "profile", "groups"}
	}
	a := &Auth{cfg: cfg, key: append([]byte(nil), sessionKey...), ttl: DefaultSessionTTL,
		client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
	for _, o := range opts {
		o(a)
	}
	if a.keyVerifier != nil && a.appName == "" {
		return nil, errNoApp
	}
	a.bearer = newBearerCache(a.now)
	if !a.noRL {
		if a.rl.Now == nil {
			a.rl.Now = a.now
		}
		a.loginRL, a.callbackRL = ratelimit.New(a.rl), ratelimit.New(a.rl)
	}
	return a, nil
}

// clientCtx attaches the issuer HTTP client to ctx.
func (a *Auth) clientCtx(ctx context.Context) context.Context {
	return oidc.ClientContext(ctx, a.client)
}

func (a *Auth) getProvider(ctx context.Context) (*oidc.Provider, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider != nil {
		return a.provider, nil
	}
	dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(a.clientCtx(dctx), a.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: OIDC discovery for %s: %w", a.cfg.Issuer, err)
	}
	a.provider = p
	return p, nil
}

func (a *Auth) oauthConfig(p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID: a.cfg.ClientID, ClientSecret: a.cfg.ClientSecret,
		Endpoint: p.Endpoint(), RedirectURL: a.cfg.RedirectURL(), Scopes: a.cfg.Scopes,
	}
}

// Register mounts the auth routes on mux and the get-current-user operation on
// api. Call it before openapimcp.Handler so the ordering of registrations is
// irrelevant to the tool set (get-current-user is hidden either way).
func (a *Auth) Register(api huma.API, mux *http.ServeMux) {
	mux.Handle("GET /auth/login", a.limited(a.loginRL, a.handleLogin))
	mux.Handle("GET /auth/callback", a.limited(a.callbackRL, a.handleCallback))
	mux.HandleFunc("GET /auth/logout", a.handleLogout)
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
	if a.appName != "" {
		ScopeMiddleware(api, a.appName)
	}
	registerMe(api, a.owner)
}

func (a *Auth) limited(l *ratelimit.Limiter, h http.HandlerFunc) http.Handler {
	if l == nil {
		return h
	}
	return l.Middleware(h)
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sanitizeNext keeps post-login redirects on this origin.
func sanitizeNext(next string) string {
	if next == "" || next[0] != '/' || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) || strings.ContainsAny(next, "\r\n") {
		return "/"
	}
	return next
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	p, err := a.getProvider(r.Context())
	if err != nil {
		a.fail(w, r, http.StatusBadGateway, ProblemUnavailable, "Identity service unavailable", "The sign-in service could not be reached. Try again shortly.")
		return
	}
	state, err1 := randomString(24)
	nonce, err2 := randomString(24)
	verifier := oauth2.GenerateVerifier()
	if err1 != nil || err2 != nil {
		a.fail(w, r, http.StatusInternalServerError, ProblemFailed, "Sign-in failed", "Could not generate login state.")
		return
	}
	exp := a.now().Add(10 * time.Minute)
	v, err := seal(a.key, LoginCookieName, loginState{State: state, Nonce: nonce, Verifier: verifier,
		Next: sanitizeNext(r.URL.Query().Get("next")), Expires: exp.Unix()})
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, ProblemFailed, "Sign-in failed", "Could not store login state.")
		return
	}
	http.SetCookie(w, baseCookie(LoginCookieName, v, "/auth", exp, 600))
	u := a.oauthConfig(p).AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u, http.StatusFound)
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c, err := r.Cookie(LoginCookieName)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, ProblemExpired, "Sign-in expired", "The sign-in attempt has expired or cookies are disabled. Start again from the login page.")
		return
	}
	var ls loginState
	if err := open(a.key, LoginCookieName, c.Value, &ls); err != nil || a.now().Unix() >= ls.Expires {
		a.fail(w, r, http.StatusBadRequest, ProblemExpired, "Sign-in expired", "The sign-in attempt has expired. Start again from the login page.")
		return
	}
	http.SetCookie(w, expiredCookie(LoginCookieName, "/auth"))

	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(ls.State)) != 1 {
		a.fail(w, r, http.StatusBadRequest, ProblemFailed, "Sign-in failed", "The sign-in response did not match the request. Start again.")
		return
	}
	if e := q.Get("error"); e != "" {
		a.fail(w, r, http.StatusUnauthorized, ProblemRefused, "Sign-in refused", "The identity service refused the sign-in ("+sanitizeToken(e)+").")
		return
	}
	code := q.Get("code")
	if code == "" {
		a.fail(w, r, http.StatusBadRequest, ProblemFailed, "Sign-in failed", "The sign-in response carried no code.")
		return
	}
	p, err := a.getProvider(r.Context())
	if err != nil {
		a.fail(w, r, http.StatusBadGateway, ProblemUnavailable, "Identity service unavailable", "The sign-in service could not be reached. Try again shortly.")
		return
	}
	ctx := a.clientCtx(r.Context())
	tok, err := a.oauthConfig(p).Exchange(ctx, code, oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		a.fail(w, r, http.StatusUnauthorized, ProblemFailed, "Sign-in failed", "The sign-in code could not be exchanged.")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		a.fail(w, r, http.StatusUnauthorized, ProblemFailed, "Sign-in failed", "The identity service returned no ID token.")
		return
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: a.cfg.ClientID, Now: a.now}).Verify(ctx, raw)
	if err != nil {
		a.fail(w, r, http.StatusUnauthorized, ProblemFailed, "Sign-in failed", "The ID token could not be verified.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(ls.Nonce)) != 1 {
		a.fail(w, r, http.StatusUnauthorized, ProblemFailed, "Sign-in failed", "The ID token nonce did not match.")
		return
	}
	u, err := userFromClaims(idt.Subject, idt.Claims)
	if err != nil {
		a.fail(w, r, http.StatusUnauthorized, ProblemFailed, "Sign-in failed", "The ID token claims could not be read.")
		return
	}
	cookie, err := NewSessionCookie(a.key, u, a.ttl, a.now())
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, ProblemFailed, "Sign-in failed", "Could not create the session.")
		return
	}
	http.SetCookie(w, cookie)
	http.Redirect(w, r, sanitizeNext(ls.Next), http.StatusSeeOther)
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, expiredCookie(CookieName, "/"))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, sanitizeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
}

// claimsFunc is the shape of oidc.IDToken.Claims and oidc.UserInfo.Claims.
type claimsFunc func(v any) error

type claimSet struct {
	Email    string   `json:"email"`
	Username string   `json:"preferred_username"`
	Picture  string   `json:"picture"`
	Groups   []string `json:"groups"`
}

func userFromClaims(sub string, claims claimsFunc) (User, error) {
	var c claimSet
	if err := claims(&c); err != nil {
		return User{}, fmt.Errorf("auth: reading claims: %w", err)
	}
	if sub == "" {
		return User{}, errors.New("auth: token has no subject")
	}
	return User{Subject: sub, Email: c.Email, Username: c.Username, Picture: c.Picture, Groups: c.Groups}, nil
}

func sanitizeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
		if b.Len() >= 40 {
			break
		}
	}
	return b.String()
}

// fail answers a sign-in problem: a redirect to the entrance page when WithEntrance is set, the problem document otherwise.
func (a *Auth) fail(w http.ResponseWriter, r *http.Request, status int, code, title, detail string) {
	if a.entrance != "" {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, a.entrance+"?problem="+code, http.StatusSeeOther)
		return
	}
	writeProblem(w, status, title, detail)
}

func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	problem.Write(w, status, title, detail)
}
