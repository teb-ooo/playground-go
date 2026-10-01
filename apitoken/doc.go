// Package apitoken adds personal access tokens (PATs) to a playground app, so
// external MCP clients (Claude Code, Claude Desktop) and scripts can call
// https://<app>/mcp and the API for a long time with
//
//	Authorization: Bearer pat_...
//
// Hydra access tokens expire after an hour, which is useless for them.
//
// # Token
//
// "pat_" followed by 32 random bytes in unpadded base64url (47 characters).
// The secret is shown once, in the response of create-api-token. Only its
// SHA-256 is stored (plus the first 8 characters for display), it is compared
// again in constant time after the lookup, and it is never logged or put in an
// error.
//
// # Wiring
//
//	store := apitoken.NewPgxStore(pool)                       // table from apitoken.MigrationsFS
//	v := apitoken.NewVerifier(store, apitoken.VerifierOptions{})
//	authn, _ := auth.New(oidcCfg, key, auth.WithTokenVerifier(v.Verify))
//	apitoken.Register(api, store, apitoken.Options{})         // before openapimcp.Handler
//
// auth.Middleware and auth.BearerOrSession then accept a pat_ bearer token
// before, and instead of, the OIDC path. The token is never forwarded to the
// issuer.
//
// # Identity
//
// A token authenticates as its owner, with the subject, email and username
// snapshotted at creation. Groups are NOT stored: a token carries none, so it
// is never an administrator unless the app grants groups explicitly through
// VerifierOptions.Groups. A snapshot of admin would outlive the owner losing
// the role, and a leaked token must not be the strongest credential the owner
// has. A renamed email or username keeps its old value on existing tokens
// until they are recreated; the subject, which is what data is keyed on, is
// stable.
//
// # Verification
//
// Unknown or malformed tokens are 401; revoked and expired ones too, checked
// on every request against the store, so revocation is immediate. Failed
// lookups are charged to the client IP (ratelimit package; default 10 per
// minute, burst 10) and a client over the limit gets 429 with Retry-After
// before any lookup. last_used_at is written at most once a minute per token.
// A store failure is a 503, never a 401, so clients do not discard a good
// token during a database blip.
//
// # Operations
//
// Register adds create-api-token (POST /api/tokens), list-api-tokens (GET
// /api/tokens, paginated with limit and cursor, never returns a secret) and
// revoke-api-token (DELETE /api/tokens/{id}), tagged tokens. They accept only a
// browser session (auth.RequireSession): a PAT, or an OIDC bearer token, is
// refused with 403 there, so a leaked token cannot mint its successor. They
// are Hidden, like GET /auth/me: out of the OpenAPI document and not MCP
// tools, because an AI client must not be offered token management. The app's
// web client calls them by path. Being hidden they do not take part in
// openapimcp.ParityCheck.
//
// # Migration
//
// MigrationsFS embeds migrations/00001_api_tokens.sql. The app copies it into
// its own migrations directory under the next number (for example
// 00004_api_tokens.sql); it needs the set_updated_at() function of the app's
// 00001 migration. The table is api_tokens.
package apitoken
