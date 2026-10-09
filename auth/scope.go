package auth

import (
	"context"
	"errors"
	playgroundlog "github.com/teb-ooo/playground-go/log"
	"net/http"
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// KeyPrefix starts every platform API key (see package keys). A bearer token
// with this prefix goes to the verifier installed by WithKeyVerifier, never to
// the OIDC path.
const KeyPrefix = "pk_"

// UnavailableError is returned by a TokenVerifier that cannot decide right now
// (for example the key revocation list is unreachable): the caller gets 503
// and may retry. Unlike any other error it is not logged by auth (the
// verifier logs outages itself, once). It must never contain the token.
type UnavailableError struct{ Err error }

func (e *UnavailableError) Error() string { return e.Err.Error() }
func (e *UnavailableError) Unwrap() error { return e.Err }

// WithKeyVerifier lets Middleware and BearerOrSession accept platform API
// keys: a bearer token starting with KeyPrefix is checked by v, and the users
// it returns are recorded with CredentialKey. playground.Config.NewAuth
// installs the keys package's verifier by default. It requires WithAppName.
func WithKeyVerifier(v TokenVerifier) Option { return func(a *Auth) { a.keyVerifier = v } }

// WithAppName sets the app's name (APP_NAME), the prefix of its scopes:
// "<app>:read", "<app>:write", "<app>:admin". Config.NewAuth sets it.
func WithAppName(name string) Option { return func(a *Auth) { a.appName = name } }

var errNoApp = errors.New("auth: WithAppName is required with WithKeyVerifier")

// ---- the scope rules ----
//
// Scopes restrict machine credentials and nothing else:
//
//   - CredentialSession (browser): never restricted.
//   - CredentialKey (platform API key): always restricted to User.Scopes
//     (a key with no scopes can do nothing). Admin does not bypass this.
//   - CredentialBearer (OIDC access token): restricted when the token carries
//     app scopes (an scp or scope claim with entries of the form
//     "<app>:read|write|admin"). A token without any scope for this app is
//     refused before it gets here (401) unless aud names this app; such a
//     token has no scopes and is unrestricted.
//
// An administrator needs both the admin group (from the identity, or the key's
// groups claim) and the "<app>:admin" scope for an admin operation reached
// with a key.

var appScopeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*:(read|write|admin)$`)

// appScopes keeps only app scopes; nil when there are none (unrestricted).
func appScopes(all []string) []string {
	var out []string
	for _, s := range all {
		if appScopeRE.MatchString(s) {
			out = append(out, s)
		}
	}
	return out
}

// HasScope reports whether the user's scopes include s (fully qualified, for
// example "notes:read"). It looks only at User.Scopes: whether scopes apply to
// this caller at all is ScopesEnforced.
func (u User) HasScope(s string) bool {
	for _, have := range u.Scopes {
		if have == s {
			return true
		}
	}
	return false
}

// ScopesEnforced reports whether the caller in ctx is restricted by its
// scopes (see the rules above). False when nobody is identified.
func ScopesEnforced(ctx context.Context) bool {
	u, ok := FromContext(ctx)
	if !ok {
		return false
	}
	switch CredentialFromContext(ctx) {
	case CredentialKey:
		return true
	case CredentialBearer:
		return u.Scopes != nil
	}
	return false
}

type appKey struct{}

// WithApp returns ctx carrying the app name used for short scope names.
func WithApp(ctx context.Context, app string) context.Context {
	return context.WithValue(ctx, appKey{}, app)
}

// AppFromContext returns the app name auth recorded next to the credential.
func AppFromContext(ctx context.Context) string { return appFromContext(ctx) }

func appFromContext(ctx context.Context) string {
	s, _ := ctx.Value(appKey{}).(string)
	return s
}

// QualifyScope turns a short scope ("read", "write", "admin") into
// "<app>:<scope>"; a scope that already contains ":" is returned as is.
func QualifyScope(app, scope string) string {
	if strings.Contains(scope, ":") {
		return scope
	}
	return app + ":" + scope
}

// ScopeAllowed reports whether the caller in ctx may use an operation that
// needs the short scope (read, write, admin). It is true for callers whose
// scopes are not enforced. A restricted caller in a context without an app
// name is refused.
func ScopeAllowed(ctx context.Context, scope string) bool {
	if !ScopesEnforced(ctx) {
		return true
	}
	app := appFromContext(ctx)
	if app == "" {
		return false
	}
	u, _ := FromContext(ctx)
	return u.HasScope(QualifyScope(app, scope))
}

// RequireScope is for handlers: it returns nil when the caller may use the
// short scope ("admin", "write", or a full "other:read") and a 403 Huma error
// naming the missing scope otherwise. Session callers always pass. Use it for
// finer rules than the per-operation one, for example "only an :admin key
// may do this branch".
func RequireScope(ctx context.Context, scope string) error {
	if ScopeAllowed(ctx, scope) {
		return nil
	}
	return huma.Error403Forbidden("missing scope " + QualifyScope(appFromContext(ctx), scope))
}

// OperationScope is the short scope an operation needs: the operation's
// Extensions "x-scope" ("read", "write", "admin") when set, otherwise "read"
// for GET and HEAD and "write" for every other method. secured is false for
// an operation without a security requirement (public): nothing is checked.
func OperationScope(method string, op *huma.Operation) (scope string, secured bool) {
	if op == nil || len(op.Security) == 0 {
		return "", false
	}
	if x, ok := op.Extensions["x-scope"].(string); ok && x != "" {
		return x, true
	}
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead:
		return "read", true
	}
	return "write", true
}

// ScopeMiddleware installs the per-operation scope check on api: for every
// operation with a security requirement, a caller whose scopes are enforced
// needs OperationScope; a missing scope is a 403 problem naming it. Auth's
// Register installs it (with the app name from WithAppName); it must run
// before the operations are registered, because Huma captures middleware at
// registration.
func ScopeMiddleware(api huma.API, app string) {
	api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		op := ctx.Operation()
		if op == nil {
			next(ctx)
			return
		}
		scope, secured := OperationScope(op.Method, op)
		playgroundlog.Annotate(ctx.Context(), playgroundlog.Fields{Op: op.OperationID})
		c := WithApp(ctx.Context(), app)
		if secured && ScopesEnforced(c) {
			if need := QualifyScope(app, scope); !userHas(c, need) {
				_ = huma.WriteErr(api, ctx, http.StatusForbidden, "missing scope "+need)
				return
			}
		}
		next(huma.WithContext(ctx, c))
	})
}

func userHas(ctx context.Context, scope string) bool {
	u, _ := FromContext(ctx)
	return u.HasScope(scope)
}
