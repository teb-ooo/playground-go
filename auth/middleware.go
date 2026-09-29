package auth

import (
	"net/http"
	"strings"
)

// identify resolves the caller. A bearer token, when present and valid, wins;
// otherwise the session cookie is consulted. bearerRejected reports that a
// bearer token was presented but not accepted.
func (a *Auth) identify(r *http.Request) (u User, ok bool, bearerRejected bool) {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		if bu, err := a.verifyBearer(r.Context(), strings.TrimSpace(h[7:])); err == nil {
			return bu, true, false
		}
		bearerRejected = true
	}
	if c, err := r.Cookie(CookieName); err == nil {
		if su, err := DecodeSession(a.key, c.Value, a.now()); err == nil {
			return su, true, false
		}
	}
	return User{}, false, bearerRejected
}

// Middleware puts the signed-in user (session cookie or bearer token) in the
// request context when there is one. It never rejects a request; handlers
// that need a user call Require.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok, _ := a.identify(r); ok {
			r = r.WithContext(WithUser(r.Context(), u))
		}
		next.ServeHTTP(w, r)
	})
}

// BearerOrSession is Middleware for the API and MCP dispatch: it accepts the
// session cookie or a bearer token validated against the issuer's JWKS. It
// differs from Middleware in one respect: a bearer token that was presented
// but is invalid (and no valid session cookie alongside) is answered with 401
// and a WWW-Authenticate header instead of being silently treated as
// anonymous, so API clients learn that their token is bad.
func (a *Auth) BearerOrSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok, rejected := a.identify(r)
		if !ok && rejected {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeProblem(w, http.StatusUnauthorized, "Unauthorized", "The bearer token is invalid or expired.")
			return
		}
		if ok {
			r = r.WithContext(WithUser(r.Context(), u))
		}
		next.ServeHTTP(w, r)
	})
}
