# keys

Platform API keys, verified offline in every app. A key is `pk_` followed by a compact ES256 JWS that playd issues for a user; an app needs no code, no table and no secret to accept it: `playground.Config.NewAuth()` installs the verifier (when `PLAYGROUND_DOMAIN` is set).

A client sends `Authorization: Bearer pk_...` to `/mcp` or any `/api/` route.

## What is checked

- Signature: ES256 only, against the public JWKS `https://id.<domain>/.keys/jwks.json` (cached; refreshed every 10 minutes, and when a token carries an unknown `kid`, at most once a minute; ETag). `alg` none, HS256 and RS256 are refused.
- `iss` equals `https://id.<domain>/.keys`; `exp` and `nbf` with 30 s leeway; `aud` contains this app's name (`APP_NAME`); `sub` and `jti` present.
- `jti` is not on `https://id.<domain>/.keys/revoked.json`, which a background poller fetches every 30 s with an ETag. A revoked key stops working within about a minute.

Result: `auth.User{Subject, Email, Username, Groups, Scopes, Agent}` recorded with `auth.CredentialKey`. Groups come from the key's `groups` claim only (`["admin"]` makes `IsAdmin` true); scopes from `scp` (space separated, `notes:read notes:write`).

## Failure behaviour

| Case | Answer |
| --- | --- |
| bad signature, wrong issuer, audience, expired, revoked, unknown kid | 401, nothing else said |
| a client keeps sending bad keys (10 failures a minute per IP; refusals of validly signed keys are not charged) | 429 with `Retry-After` |
| signing keys never loaded (issuer unreachable at the first key) | 503 |
| revocation list never loaded, or not refreshed for more than 10 minutes | 503 `keys: revocation list unavailable` |

The last good revocation list is used for up to 10 minutes while the endpoint is down; after that keys are refused, so blocking the endpoint cannot keep a revoked key alive. An outage is logged once, with its end. The token is never logged or returned.

## Scopes

Scopes are `<app>:read`, `<app>:write` and `<app>:admin`. They are enforced by `auth.ScopeMiddleware` (installed by `Auth.Register`; register your operations after it): for an operation with a `Security` requirement a key needs `<app>:read` for GET and HEAD, `<app>:write` for everything else, or the scope named by the operation's extension `x-scope` (`"read"`, `"write"`, `"admin"`). A missing scope is `403` problem JSON `missing scope notes:write`. Admin does not bypass scopes: an admin operation reached with a key needs the `admin` group claim and the `<app>:admin` scope. Handlers use `auth.RequireScope(ctx, "admin")` for finer rules. Sessions (browser) are never scope-checked. See package `auth` for the full rule per credential kind.

```go
huma.Register(api, huma.Operation{
	OperationID: "purge-notes", Method: http.MethodDelete, Path: "/api/notes",
	Security:   []map[string][]string{{"session": {}}, {"bearer": {}}},
	Extensions: map[string]any{"x-scope": "admin"},
}, handler)
```

A key reaching the app through MCP is filtered by the same rules (see `openapimcp`). A direct HTTP call with a key has `surface.Key` (`IsAI()` is true).

## Agent keys and `act`

A key issued to an agent carries one more signed claim, `act`: the name of the app the agent acts for (for example `bd`). User keys have none. The verifier checks that a present `act` is an app name (`^[a-z][a-z0-9-]{1,30}$`) and refuses the key (401) otherwise. It is exposed as `auth.User.Agent` and `auth.AgentFromContext(ctx)`: the app an agent key acts for, empty for sessions, user keys and tokens (only the key credential can set it). The key's `sub` and `email` are the shared `platform` identity, so `Subject` alone cannot tell two agents apart.

`act` grants nothing and the library enforces nothing on it: the scope (`<app>:read|write|admin`) only says the caller may use this app at all. The recommended handler pattern is that the app compares `act` with the owner of the resource it touches and answers 403 when they differ:

```go
if agent := auth.AgentFromContext(ctx); agent != "" && agent != note.OwnerApp {
	return nil, huma.Error403Forbidden("this agent key acts for another app")
}
```

## Use without Config

```go
v, err := keys.New(keys.Options{App: "notes", Domain: "teb.ooo"})
a, err := auth.New(oidcCfg, sessionKey, auth.WithAppName("notes"), auth.WithKeyVerifier(v.Verify))
```

`Options` also takes `Issuer`, `JWKSURL`, `RevokedURL`, `HTTPClient`, `FailLimit` and the intervals (`Leeway`, `JWKSRefresh`, `UnknownKidRetry`, `RevokedPoll`, `RevokedStale`). `Close` stops the poller.
