// Package testkit is for staging and tests only: it mints credentials that let
// tests and the agent's browser skip the passkey ceremony. Every function
// returns an error when APP_ENV=production.
//
// MintSession produces a valid session cookie for an app's own auth package.
// MintKratosSession and MintKratosSessionCookie obtain a session from a
// running Kratos through its admin API, for apps (the id app) that talk to
// Kratos directly.
package testkit

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/teb-ooo/factory-go/auth"
)

// ErrProduction is returned by every testkit function when APP_ENV=production.
var ErrProduction = errors.New("testkit: refusing to run with APP_ENV=production")

// Guard returns ErrProduction when APP_ENV is production.
func Guard() error {
	if os.Getenv("APP_ENV") == "production" {
		return ErrProduction
	}
	return nil
}

// MintSession returns a valid auth session cookie for user, signed and
// encrypted with sessionKey (the app's SESSION_KEY, 32 bytes; use
// auth.ParseKey for the encoded form). The cookie is valid for 24 hours.
func MintSession(sessionKey []byte, user auth.User) (cookie *http.Cookie, err error) {
	if err := Guard(); err != nil {
		return nil, err
	}
	return auth.NewSessionCookie(sessionKey, user, 24*time.Hour, time.Now())
}

// CookieHeader returns the cookie as "name=value", the form of a Cookie
// request header.
func CookieHeader(c *http.Cookie) string { return c.Name + "=" + c.Value }

// SetCookieString returns the cookie as a Set-Cookie header value
// (attributes included), the form `agent-browser cookie set` takes.
func SetCookieString(c *http.Cookie) string { return c.String() }
