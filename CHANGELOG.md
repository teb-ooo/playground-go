# Changelog

One entry per tag, newest first. "Upgrade" says what an app must do; "none" means a `go get` is enough. A new tag is annotated and its message is its entry here. Library features that pair with a `@teb-ooo/ui` version say which.

## v0.24.1 (2026-10-10)
- Config: `LogValue` no longer emits `app_owner` and `playground_domain` twice (v0.24.0 logged both fields two times in the config group, so a JSON reader kept one copy at random).
- Upgrade: none.

## v0.24.0 (2026-10-09)
- BREAKING (auth): a bearer token (JWT or opaque) is accepted only if it carries a scope `<app>:read|write|admin` for this app or names the app in `aud`; otherwise 401 `invalid_token`. A token with only `openid`, `email` or `profile` is refused, so an app's own sign-in access token no longer works as an API credential. Platform keys (`pk_`) and sessions are unchanged; apps without an app name (`WithAppName`) are unchanged.
- BREAKING (auth): `PATPrefix`, `WithTokenVerifier`, `CredentialToken` and the `pat_` texts are removed (the personal access tokens left with `apitoken` in v0.9.0). Platform keys (`WithKeyVerifier`) are the only token verifier.
- auth: OIDC discovery no longer blocks every caller on one lock (single in-flight discovery, a 5 s negative cache after a failure). New `Auth.Close` and `auth.OnClose`; `Config.NewAuth` registers the keys verifier, so `defer a.Close()` stops its poller.
- log: an MCP tool call leaves one info line (`op`, `status`, `duration_ms`, `user`, `credential`, `surface=mcp`, `request_id`). The request line carries `user`, `credential`, `surface` and `op` once the caller is identified (`log.Annotate`, `log.Fields`). No app code change needed.
- openapimcp: a tool call inherits the caller's identity from the outer request instead of verifying the credential again, and the inner request keeps the outer client IP and `X-Forwarded-For`, so per-IP limits see the real client.
- ParityCheck now fails on an `x-scope` other than read, write or admin, on a `WithExempt` id that matches no operation, and on a non-bool `x-require-user-interaction` or `x-mcp`.
- apierr: new `WriteProblem` and `Internal`; apps can delete their copies of `writeProblem` and `internalError`.
- live: `Relay`, `SSEUpstream` and the other relay types are removed (no app used them; bd keeps its own stream).
- Config: `LogValue` logs `app_owner` and `playground_domain`; the direct `jackc/pgx/v5` requirement is gone.
- Upgrade: rb replaces its `auth.CredentialToken` comparison (`c.Owner = u.IsAdmin() && c.Cred != auth.CredentialKey`), lore likewise (`return c == auth.CredentialKey || c == auth.CredentialBearer`); every app wraps its top handler with `surface.Middleware` (outermost) as the template does; an app that called its own sign-in access token as an API credential must use a platform key.

## v0.23.0 (2026-10-08)
- surface: the `Assistant` surface is removed (no app has an in-app assistant).
- Upgrade: replace any use of `surface.Assistant` (no known app uses it).

## v0.22.0 (2026-10-07)
- No user content in logs or validation answers (rule DAT-cld): the address of a transport error is removed from every logged line, and a validation error no longer echoes the offending value.
- Upgrade: none. Apps below v0.22.0 still echo values; take this one.

## v0.21.0 (2026-10-06)
- log: `LevelFromEnv` (`LOG_LEVEL`). Request lines: 4xx at warn, successful polls (`/healthz`, `/mcp`, `/api/live`, `/auth/me`) at debug.
- Upgrade: none.

## v0.20.0 (2026-10-06)
- auth: `WithEntrance`, sign-in problems go to the app's own entrance page as `?problem=<code>`.
- Upgrade: pass `auth.WithEntrance("/enter")` to `NewAuth` (the template does).

## v0.19.1 (2026-10-06)
- palette: `Options.Also`, fixed values before the list items (`@teb-ooo/ui` 0.82).

## v0.19.0 (2026-10-06)
- palette: `Form.Fields` (question order) and `Form.Options` (select from a list operation) (`@teb-ooo/ui` 0.81).

## v0.18.0 (2026-10-06)
- palette: prompt arguments and `Form{Submit}` for the form step, checked against the request body (`@teb-ooo/ui` 0.80).

## v0.17.0 (2026-10-06)
- palette: `When.Field`, `When.Differs` and `Role`, validated in `Ext`, `Validate` and `Check` (`@teb-ooo/ui` 0.77 to 0.79).

## v0.16.1 (2026-10-06)
- Messages cite docs/go-api.md (Pagination) instead of the retired rules.

## v0.16.0 (2026-10-06)
- ParityCheck: a `list-*` operation must answer `{items, next_cursor}` and be paginated, even when it returns a bare array.
- live: `WithID` does not narrow queries.
- Upgrade: run the contract test; fix any list operation it names.

## v0.15.1 (2026-10-06)
- Wording only: a human is a user.

## v0.15.0 (2026-10-05)
- palette package: `x-palette` tag builders (`Action`, `Source`, `None`), `Merge` and `Check` (the contract test).
- Upgrade: none (add the palette check to the contract test to use it).

## v0.14.0 (2026-10-05)
- page package (`Params`, `Body`, opaque keyset cursor, `Trim`) and the ParityCheck pagination rule.
- Upgrade: list operations must paginate with `page`; the contract test says which do not.

## v0.13.0 (2026-10-05)
- openapimcp: `x-mcp: false` keeps an operation in the OpenAPI document but out of the tools; ParityCheck honours it.

## v0.12.0 (2026-10-03)
- The end-user assistant package, its config fields (`ASSISTANT_MODEL`, `ANTHROPIC_API_KEY`, `PLAYGROUND_ASSISTANT`) and the spa assistant flag are removed (owner decision; lore owns its assistant). Rule ids in docs and comments are the rules app's codes.
- Upgrade: delete the removed config fields and any import of the assistant package.

Earlier tags (v0.1.0 to v0.11.x) are in `git log`; v0.9.0 removed `apitoken`, v0.4.0 changed the auth wiring (see the README).
