package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/teb-ooo/playground-go/surface"
)

// rejection describes a bearer credential that was presented but refused.
type rejection struct {
	status     int
	title      string
	detail     string
	retryAfter int // seconds, 429 only
}

var (
	rejectInvalid = &rejection{status: http.StatusUnauthorized, title: "Unauthorized", detail: "The bearer token is invalid or expired."}
	rejectBackend = &rejection{status: http.StatusServiceUnavailable, title: "Service unavailable", detail: "The bearer token could not be checked. Try again shortly."}
)

// identify resolves the caller. A bearer token, when present and valid, wins;
// otherwise the session cookie is consulted. rej is non-nil when a bearer
// token was presented but not accepted. cred says which credential vouched
// for u and is empty when nobody is identified.
func (a *Auth) identify(r *http.Request) (u User, cred Credential, rej *rejection) {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		tok := strings.TrimSpace(h[7:])
		switch {
		case a.keyVerifier != nil && strings.HasPrefix(tok, KeyPrefix):
			// A platform key is never handed to the OIDC path.
			if ku, rj := a.runVerifier(a.keyVerifier, r, tok); rj == nil {
				return ku, CredentialKey, nil
			} else {
				rej = rj
			}
		case a.tokenVerifier != nil && strings.HasPrefix(tok, PATPrefix):
			// A personal access token is never handed to the OIDC path.
			if tu, rj := a.runVerifier(a.tokenVerifier, r, tok); rj == nil {
				return tu, CredentialToken, nil
			} else {
				rej = rj
			}
		default:
			if bu, err := a.verifyBearer(r.Context(), tok); err == nil {
				return bu, CredentialBearer, nil
			}
			rej = rejectInvalid
		}
	}
	if c, err := r.Cookie(CookieName); err == nil {
		if su, err := DecodeSession(a.key, c.Value, a.now()); err == nil {
			return su, CredentialSession, nil
		}
	}
	return User{}, "", rej
}

// runVerifier runs a token or key verifier and maps its outcome to a rejection.
func (a *Auth) runVerifier(v TokenVerifier, r *http.Request, tok string) (User, *rejection) {
	u, ok, err := v(r, tok)
	switch {
	case err == nil && ok:
		return u, nil
	case err == nil:
		return User{}, rejectInvalid
	}
	var rl *RateLimitedError
	var un *UnavailableError
	switch {
	case errors.As(err, &rl):
		return User{}, &rejection{status: http.StatusTooManyRequests, title: "Too Many Requests",
			detail: "Too many failed attempts. Try again later.", retryAfter: retrySeconds(rl)}
	case errors.As(err, &un):
		return User{}, rejectBackend
	}
	// The error is not shown to the caller and never carries the token.
	slog.Error("auth: token verifier failed", "error", err)
	return User{}, rejectBackend
}

// identified records who the caller is in the request context: the user, the
// credential, the app name (for short scope names) and, for a platform key,
// the AI-caller surface.
func (a *Auth) identified(r *http.Request, u User, cred Credential) *http.Request {
	ctx := WithApp(WithCredential(WithUser(r.Context(), u), cred), a.appName)
	if cred == CredentialKey {
		if s := surface.From(ctx); !s.IsAI() {
			ctx = surface.With(ctx, surface.Key)
		}
	}
	return r.WithContext(ctx)
}

func retrySeconds(e *RateLimitedError) int {
	s := int((e.RetryAfter + 999_999_999) / 1_000_000_000)
	if s < 1 {
		s = 1
	}
	return s
}

// Middleware puts the signed-in user (session cookie or bearer token) in the
// request context when there is one. It never rejects a request; handlers
// that need a user call Require.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, cred, _ := a.identify(r); cred != "" {
			r = a.identified(r, u, cred)
		}
		next.ServeHTTP(w, r)
	})
}

// BearerOrSession is Middleware for the API and MCP dispatch: it accepts the
// session cookie, a bearer token validated against the issuer's JWKS, or (see
// WithTokenVerifier) a personal access token. It differs from Middleware in
// one respect: a bearer token that was presented but is invalid (and no valid
// session cookie alongside) is answered with 401 and a WWW-Authenticate
// header instead of being silently treated as anonymous, so API clients learn
// that their token is bad. A personal access token that cannot be checked is
// answered with 503, and one from a client that keeps presenting unknown
// tokens with 429 and Retry-After.
func (a *Auth) BearerOrSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, cred, rej := a.identify(r)
		if cred == "" && rej != nil {
			switch rej.status {
			case http.StatusUnauthorized:
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			case http.StatusTooManyRequests:
				w.Header().Set("Retry-After", strconv.Itoa(rej.retryAfter))
			}
			writeProblem(w, rej.status, rej.title, rej.detail)
			return
		}
		if cred != "" {
			r = a.identified(r, u, cred)
		}
		next.ServeHTTP(w, r)
	})
}
