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
