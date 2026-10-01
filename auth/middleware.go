package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
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
		if a.tokenVerifier != nil && strings.HasPrefix(tok, PATPrefix) {
			// A personal access token is never handed to the OIDC path.
			switch tu, ok, err := a.tokenVerifier(r, tok); {
			case err == nil && ok:
				return tu, CredentialToken, nil
			case err == nil:
				rej = rejectInvalid
			default:
				var rl *RateLimitedError
				if errors.As(err, &rl) {
					rej = &rejection{status: http.StatusTooManyRequests, title: "Too Many Requests",
						detail: "Too many failed attempts. Try again later.", retryAfter: retrySeconds(rl)}
				} else {
					// The error is not shown to the caller and never carries the token.
					slog.Error("auth: token verifier failed", "error", err)
					rej = rejectBackend
				}
			}
		} else {
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
			r = r.WithContext(WithCredential(WithUser(r.Context(), u), cred))
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
			r = r.WithContext(WithCredential(WithUser(r.Context(), u), cred))
		}
		next.ServeHTTP(w, r)
	})
}
