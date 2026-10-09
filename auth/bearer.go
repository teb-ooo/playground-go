package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	bearerCacheMax = 1024
	bearerCacheTTL = 60 * time.Second
)

type bearerEntry struct {
	user User
	exp  time.Time
}

type bearerCache struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[[32]byte]bearerEntry
}

func newBearerCache(now func() time.Time) *bearerCache {
	return &bearerCache{now: now, m: map[[32]byte]bearerEntry{}}
}

func (c *bearerCache) get(tok string) (User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[sha256.Sum256([]byte(tok))]
	if !ok || !c.now().Before(e.exp) {
		return User{}, false
	}
	return e.user, true
}

func (c *bearerCache) put(tok string, u User, exp time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= bearerCacheMax {
		now := c.now()
		for k, e := range c.m {
			if !now.Before(e.exp) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= bearerCacheMax {
			c.m = map[[32]byte]bearerEntry{}
		}
	}
	c.m[sha256.Sum256([]byte(tok))] = bearerEntry{user: u, exp: exp}
}

var errBearer = errors.New("auth: invalid bearer token")

// verifyBearer validates an access token. JWT access tokens (Hydra's
// strategies.access_token: jwt) are verified against the issuer's JWKS,
// including issuer and expiry; the audience is not checked because an access
// token's audience is the resource server, not this client. Because Hydra puts
// email, username and groups in the ID token only, the user's claims are then
// filled in from the userinfo endpoint with the same token. Opaque tokens are
// validated by userinfo alone.
//
// When the app name is known (WithAppName) a token is accepted only if it
// carries at least one scope for this app ("<app>:read|write|admin") or names
// this app in aud (see forThisApp); anything else is errBearer (401). The
// scopes and aud of an opaque token can only come from its userinfo claims.
func (a *Auth) verifyBearer(ctx context.Context, tok string) (User, error) {
	if tok == "" {
		return User{}, errBearer
	}
	if u, ok := a.bearer.get(tok); ok {
		return u, nil
	}
	p, err := a.getProvider(ctx)
	if err != nil {
		return User{}, fmt.Errorf("auth: verifying bearer token: %w", err)
	}
	ctx, cancel := context.WithTimeout(a.clientCtx(ctx), 5*time.Second)
	defer cancel()

	var (
		user User
		exp  = a.now().Add(bearerCacheTTL)
	)
	if strings.Count(tok, ".") == 2 {
		idt, err := p.Verifier(&oidc.Config{SkipClientIDCheck: true, Now: a.now}).Verify(ctx, tok)
		if err != nil {
			return User{}, errBearer
		}
		if idt.Expiry.Before(exp) {
			exp = idt.Expiry
		}
		user, err = userFromClaims(idt.Subject, idt.Claims)
		if err != nil {
			return User{}, errBearer
		}
		scopes := scopesFromClaims(idt.Claims)
		if !a.forThisApp(scopes, idt.Audience) {
			return User{}, errBearer
		}
		if info, err := p.UserInfo(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, TokenType: "Bearer"})); err == nil && info.Subject == user.Subject {
			if enriched, err := userFromClaims(info.Subject, info.Claims); err == nil {
				user = enriched
			}
			user.Scopes = scopes
		} else {
			// Could not enrich: serve the request with the token's own claims
			// but do not cache, so the next request retries userinfo.
			user.Scopes = scopes
			return user, nil
		}
	} else {
		info, err := p.UserInfo(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, TokenType: "Bearer"}))
		if err != nil {
			return User{}, errBearer
		}
		user, err = userFromClaims(info.Subject, info.Claims)
		if err != nil {
			return User{}, errBearer
		}
		user.Scopes = scopesFromClaims(info.Claims)
		if !a.forThisApp(user.Scopes, audFromClaims(info.Claims)) {
			return User{}, errBearer
		}
	}
	a.bearer.put(tok, user, exp)
	return user, nil
}

// forThisApp reports whether a bearer token with these app scopes and audience
// is meant for this app: it holds a scope of the form "<app>:..." or lists the
// app name in aud. An Auth without an app name (WithAppName not used) cannot
// tell and accepts every verified token, as before.
func (a *Auth) forThisApp(scopes, aud []string) bool {
	if a.appName == "" {
		return true
	}
	for _, s := range scopes {
		if strings.HasPrefix(s, a.appName+":") {
			return true
		}
	}
	for _, x := range aud {
		if x == a.appName {
			return true
		}
	}
	return false
}

// audFromClaims reads aud (a string or an array) from userinfo claims.
func audFromClaims(claims claimsFunc) []string {
	var c struct {
		Aud json.RawMessage `json:"aud"`
	}
	if claims(&c) != nil {
		return nil
	}
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

// scopesFromClaims reads the granted scopes of a JWT access token. Hydra puts
// them in "scp" (an array; some issuers use a space separated string, or the
// RFC 9068 "scope" string). Only app scopes ("<app>:read|write|admin") are
// kept; nil means the token carries none (such a token is refused by
// verifyBearer unless it names this app in aud).
func scopesFromClaims(claims claimsFunc) []string {
	var c struct {
		Scp   json.RawMessage `json:"scp"`
		Scope string          `json:"scope"`
	}
	if claims(&c) != nil {
		return nil
	}
	var all []string
	var arr []string
	var str string
	switch {
	case json.Unmarshal(c.Scp, &arr) == nil && arr != nil:
		all = arr
	case json.Unmarshal(c.Scp, &str) == nil && str != "":
		all = strings.Fields(str)
	}
	all = append(all, strings.Fields(c.Scope)...)
	return appScopes(all)
}
