package auth

import (
	"net/http"
	"time"
)

// SetDiscoveryTimings shortens the discovery timeout and the failure cache
// for tests.
func (a *Auth) SetDiscoveryTimings(timeout, retry time.Duration) {
	a.discoveryTimeout, a.retryAfter = timeout, retry
}

// ServeLogin runs the login handler directly (it is not exported).
func (a *Auth) ServeLogin(w http.ResponseWriter, r *http.Request) { a.handleLogin(w, r) }
