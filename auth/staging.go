package auth

import (
	"net/http"
	"os"
)

// The shared staging account. In a gated staging site the owner and the app's agent must see the same pages, and a worlds-per-subject app
// shows each of them a different set when one signs in through the identity service and the other mints a session with a made-up subject.
// So staging has one synthetic account, [StagingUser]: the agent mints its session with it (testkit.StagingUser), and /auth/login signs the
// owner's browser in as it. The sign-in through the identity service stays at /auth/login?account=real.
//
// It exists only when APP_ENV is exactly "staging" and playd set STAGING_SHARED_ACCOUNT=1, which it does for gated staging sites and never
// for a production container or for a site marked public-staging (nothing in front of it would stop anyone from signing in).
const (
	// StagingSubject is the subject of the shared staging account.
	StagingSubject = "staging-shared"
	// StagingEmail is its email: the .invalid domain can never be a real address.
	StagingEmail = "staging-shared@staging.invalid"
)

// StagingUser is the shared staging account: synthetic, an administrator (staging only).
func StagingUser() User {
	return User{Subject: StagingSubject, Email: StagingEmail, Username: "staging", Groups: []string{AdminGroup}}
}

// stagingAccountOn reports whether this process is a gated staging site that signs people in as the shared account.
func stagingAccountOn() bool {
	return os.Getenv("APP_ENV") == "staging" && os.Getenv("STAGING_SHARED_ACCOUNT") == "1"
}

// signInAsStagingAccount sets the session of the shared account and goes on to next. ok false: the cookie could not be made.
func (a *Auth) signInAsStagingAccount(w http.ResponseWriter, r *http.Request) bool {
	c, err := NewSessionCookie(a.key, StagingUser(), DefaultSessionTTL, a.now())
	if err != nil {
		return false
	}
	http.SetCookie(w, c)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, sanitizeNext(r.URL.Query().Get("next")), http.StatusFound)
	return true
}
