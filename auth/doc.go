// Package auth implements OIDC sign-in for playground apps. The issuer (Ory
// Hydra at OIDC_ISSUER in the playground) is configuration, never code: the
// package runs the authorization-code flow with PKCE, state and nonce, keeps
// the result in a signed and encrypted (AES-GCM) host-only cookie, and also
// accepts bearer access tokens validated against the issuer's JWKS.
//
// Claims read from the ID token: sub, email, preferred_username, picture and
// groups. A groups entry of "admin" makes User.IsAdmin true.
//
// Routes (register with Auth.Register): GET /auth/login, GET /auth/callback,
// GET|POST /auth/logout and the Huma operation get-current-user at GET
// /auth/me, which is hidden from MCP.
//
// /auth/login and /auth/callback are rate limited by client IP (default 10
// requests per minute, burst 5; WithRateLimit, WithoutRateLimit).
//
// Every cookie this package sets is Secure, HttpOnly, SameSite=Lax and
// host-only (no Domain attribute).
//
// Personal access tokens: WithTokenVerifier plugs in an app's own verifier
// (platform API keys and MCP sign-in replace the old apitoken package, removed in v0.9.0) for bearer tokens that start with PATPrefix ("pat_"). They are
// checked there instead of by the OIDC path, so a long-lived revocable token
// can serve external MCP clients. Middleware and BearerOrSession record how
// the caller proved who they are (CredentialFromContext: session, bearer or
// token); RequireSession admits only a browser session, for operations that
// manage credentials.
//
// Platform API keys: WithKeyVerifier plugs in the keys package's verifier for
// bearer tokens that start with KeyPrefix ("pk_"); playground.Config.NewAuth
// installs it by default (with WithAppName). The credential is CredentialKey.
//
// Scopes ("<app>:read", "<app>:write", "<app>:admin", User.Scopes) restrict
// machine credentials only:
//
//	session (browser)         never restricted
//	key (pk_...)              always restricted to its scopes; no scopes, no access;
//	                          admin does not bypass (admin operations need the
//	                          admin group claim AND the <app>:admin scope)
//	bearer (OIDC/Hydra JWT)   restricted only when its scp (array or string) or
//	                          scope claim holds app scopes; openid/email/profile
//	                          only, or no claim, stays unrestricted
//	token (pat_, legacy)      restricted only if the verifier returned Scopes
//
// ScopesEnforced says which applies; ScopeMiddleware (installed by Register,
// before the operations are registered) checks each operation that has a
// Security requirement: read for GET and HEAD, write otherwise, or the
// Extensions "x-scope" value; a missing scope is a 403 problem naming it.
// Handlers call RequireScope(ctx, "admin"). A verifier that cannot decide
// (the key revocation list is unreachable) returns *UnavailableError: 503.
//
// Middleware never rejects; operations that need a user call Require, which
// yields a 401 problem+json through Huma.
package auth
