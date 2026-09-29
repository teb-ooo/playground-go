// Package auth implements OIDC sign-in for factory apps. The issuer (Ory
// Hydra at OIDC_ISSUER in the factory) is configuration, never code: the
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
// Middleware never rejects; operations that need a user call Require, which
// yields a 401 problem+json through Huma.
package auth
