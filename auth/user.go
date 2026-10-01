package auth

import (
	"context"
	"slices"

	"github.com/danielgtaylor/huma/v2"
)

// AdminGroup is the groups claim value that grants administrator rights.
const AdminGroup = "admin"

// User is the signed-in person, as carried by the session cookie or derived
// from a bearer token.
type User struct {
	Subject  string
	Email    string
	Username string
	Picture  string
	Groups   []string
}

// IsAdmin reports whether the user belongs to the admin group.
func (u User) IsAdmin() bool { return slices.Contains(u.Groups, AdminGroup) }

type ctxKey struct{}

// WithUser returns ctx carrying u.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// FromContext returns the user Middleware put in ctx, if any.
func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// Credential says how the caller proved who they are.
type Credential string

const (
	// CredentialSession is the encrypted session cookie of a browser sign-in.
	CredentialSession Credential = "session"
	// CredentialBearer is an OIDC access token validated against the issuer.
	CredentialBearer Credential = "bearer"
	// CredentialToken is a personal access token (see WithTokenVerifier).
	CredentialToken Credential = "token"
)

type credKey struct{}

// WithCredential returns ctx recording which credential identified the user.
// Middleware and BearerOrSession set it; it cannot be set by a client.
func WithCredential(ctx context.Context, c Credential) context.Context {
	return context.WithValue(ctx, credKey{}, c)
}

// CredentialFromContext returns the credential that identified the user, or
// "" when nobody is identified (or the user was put in ctx by WithUser alone).
func CredentialFromContext(ctx context.Context) Credential {
	c, _ := ctx.Value(credKey{}).(Credential)
	return c
}

// RequireSession returns the user only when they signed in through the
// browser (session cookie): 401 if nobody is identified, 403 if the caller
// came with a bearer token or a personal access token. Operations that manage
// credentials (minting, listing and revoking personal access tokens) use it,
// so a leaked token can never create another one. It fails closed: a context
// whose credential was not recorded by this package's middleware is refused.
func RequireSession(ctx context.Context) (User, error) {
	u, err := Require(ctx)
	if err != nil {
		return User{}, err
	}
	if CredentialFromContext(ctx) != CredentialSession {
		return User{}, huma.Error403Forbidden("this operation needs a signed-in browser session; tokens are not accepted")
	}
	return u, nil
}

// Require returns the signed-in user or a 401 Huma error. Handlers that need
// a user call it first.
func Require(ctx context.Context) (User, error) {
	if u, ok := FromContext(ctx); ok {
		return u, nil
	}
	return User{}, huma.Error401Unauthorized("authentication required")
}

// RequireAdmin returns the signed-in administrator, a 401 if nobody is signed
// in, or a 403 if the user is not an administrator.
func RequireAdmin(ctx context.Context) (User, error) {
	u, err := Require(ctx)
	if err != nil {
		return User{}, err
	}
	if !u.IsAdmin() {
		return User{}, huma.Error403Forbidden("administrator access required")
	}
	return u, nil
}
