# playground-go

playground-go is the shared Go library every playground app is built on: it turns a Huma API into an MCP server, signs users in through the playground's OIDC issuer, and provides health, logging, rate limiting, mail, SPA serving and test helpers. Apps import its packages and never reimplement them, so behaviour is identical everywhere. The module path is `github.com/teb-ooo/playground-go`.

## Tests

```bash
go vet ./... && go test ./... -race
```

One optional integration check skips itself unless configured: the Postgres store test (`PLAYGROUND_TEST_DATABASE_URL=postgres://postgres:x@127.0.0.1:55432/postgres`; a throwaway database, for example `docker run --rm -d -p 55432:5432 -e POSTGRES_PASSWORD=x postgres:17.11`). No test calls the real Anthropic API or any real mail provider; they use httptest fakes.

## Usage

```go
cfg, err := playground.LoadConfig()          // PORT, DATABASE_URL, OIDC_*, SESSION_KEY, APP_NAME, APP_ENV, PUBLIC_URL, MAIL_*, PLAYGROUND_DOMAIN
authn, err := cfg.NewAuth()

mux := http.NewServeMux()
api := humago.New(mux, huma.DefaultConfig(cfg.AppName, cfg.Version))
auth.AddSecuritySchemes(api.OpenAPI())     // declares "session" and "bearer"
authn.Register(api, mux)                   // /auth/login, /auth/callback, /auth/logout, GET /auth/me (hidden)
health.Register(api, pool, cfg.Version)    // GET /healthz (hidden)
registerItems(api, pool)                   // your operations: OperationID, Summary, Description are mandatory

mcpH := openapimcp.Handler(api, mux, openapimcp.Options{Name: cfg.AppName, Auth: authn.BearerOrSession})
mux.Handle("/mcp", mcpH)
mux.Handle("/", spa.Handler(webFS))

handler := playgroundlog.Middleware(authn.Middleware(mux))
```

Register every operation before `openapimcp.Handler`; it derives its tools when constructed. The template's parity test is:

```go
openapimcp.ParityCheck(t, api, mcpH) // operation ids == MCP tool names, plus the API contract below
```

`ParityCheck` also enforces the API contract of docs/go-api.md on every non-hidden operation. Each failure names the operation, the problem and the fix, and ends with `see docs/go-api.md`: OperationID kebab-case verb-noun (`list-items`), Method matching its route, Path under `/api/`, non-empty Summary, Description and Tags, Security covering `session` and `bearer`, every request and response property name snake_case, properties named `*_at` or `timestamp` with format `date-time`, properties named `id` or `*_id` a string with format `uuid` (ids the app generates, UUIDv7) or a string that declares another `format` or a `pattern` (ids it does not generate, for example bead ids `pattern:"^[a-z]+-[a-z0-9.]+$"`); a bare string or an integer id fails, and an integer id needs an exemption with a reason. Known exceptions are exempted per operation, never globally: `openapimcp.WithExempt("operation-id", ...)` as a ParityCheck option, or the operation Extension `Extensions: map[string]any{openapimcp.ExemptExtension: "reason"}` (the reason is mandatory). An exempt operation still takes part in the ids-equal-tools check.

## 5xx responses

The package `apierr` makes every error with status >= 500 a fixed problem+json (`title` = status text, `detail` = "internal error", no `errors` list); the real message and causes are logged with slog (error level, `request_id` from the log middleware) and never sent. Errors below 500 (`huma.Error404NotFound("no such item")`) are untouched. It installs itself from the init of the root `playground` package, so any app importing `playground` (all do) has it; `apierr.Install()` is idempotent if you want the dependency explicit. `log.Logger(ctx)` returns the request-scoped logger.

Tests and the agent's browser sign in with `testkit.MintSession(sessionKey, auth.User{...})` and set the cookie with `testkit.SetCookieString(cookie)`. Everything in `testkit` refuses to run when `APP_ENV=production`.

## Packages

| Package | What it does |
|---|---|
| `playground` (root) | `Config` from the environment with validation; helpers `NewAuth`, `NewMailer`. Secrets never appear in errors or in `LogValue`. |
| `openapimcp` | One MCP tool per OpenAPI operation, executed in-process with the caller's credentials forwarded. `Handler`, `New`, `Tools()`, `ParityCheck`. |
| `surface` | Server-side marker of the door a request came through (`ui`, `api`, `mcp`, `assistant`): `surface.From(ctx)` in an operation handler, `surface.Middleware` mounted outermost. Set by the MCP server for its tool calls (an app's own in-app assistant sets `surface.Assistant` for its tool calls); cannot be forged by a client header. |
| `auth` | OIDC code flow with PKCE, state and nonce; encrypted session cookie; bearer tokens; `Require`, `RequireAdmin`, `User`. |
| `keys` | Platform API keys (`pk_` + ES256 JWS issued by playd), verified offline against `https://id.<domain>/.keys/jwks.json` with a revocation list; `NewAuth` installs it, so an app needs no code. Scopes `<app>:read`/`write`/`admin` are enforced by `auth.ScopeMiddleware` and filter MCP tools. See `keys/README.md`. |
| `ratelimit` | In-memory token bucket for net/http and Huma, keyed by client IP (last `X-Forwarded-For` hop from a trusted proxy) and an optional extra key. |
| `live` | Rule UI-yvn, the server half: `Hub` (`Publish(resource, audience)` after a write, never blocks, per-subscriber coalescing), audiences (`Everyone`, `Subject`, `Admins`, `Project`), `Mount(mux, hub)` for `GET /api/live` (SSE: `: live`, `change`, `degraded`, `: ping` every 25 s, 1 h lifetime, 8 streams per person, 401/429 problem+json), and `Relay` for an upstream stream such as playd's `/v1/work/events`. Not a Huma operation. |
| `health` | `GET /healthz` returning `{version, env, db, uptime_seconds}`. |
| `log` | slog JSON to stdout, request middleware, redaction of `*_KEY`, `*_SECRET`, `*_TOKEN`, `*_PASSWORD` values. |
| `mail` | Transport only: `Mailer.Send` over Resend or Postmark HTTP, staging redirect to `MAIL_STAGING_SINK` and the `[staging <app>]` subject prefix. No layout and no templates: each app owns its email templates (complete text and HTML documents in its own repo) and passes the rendered `Message` to `Send`. Breaking change in v0.4.0: `Templates`, `Page`, `SendTemplate`, `Config.Templates`, `Config.PlaygroundName` and the `NewMailer` argument are removed. |
| `spa` | Serves an embedded `fs.FS`, falls back to `index.html`, serves `window.__PLAYGROUND__` at `/playground.js` (a script tag in index.html; the CSP forbids inline scripts) (`app_name`, `env`, `claude_session_url`, `locale`, `timezone`, `platform_domain`; use `cfg.SPA()`). |
| `testkit` | Minted session cookies and Kratos sessions for staging and tests. |

### Error shape

The one error shape is Huma's default RFC 9457 `application/problem+json`: `title`, `status`, `detail` and, for validation failures, `errors[]` (each with `message`, `location`, `value`). Handlers written outside Huma (the auth routes, the rate limiter) return the same body with `title`, `status` and `detail`. `@teb-ooo/web` types its `ApiError` from this, so do not invent another error format.

### MCP instructions, resources and prompts

`openapimcp.Options` also takes `Instructions` (shown at initialize), `Resources []openapimcp.Resource`, `ResourceTemplates []openapimcp.ResourceTemplate` and `Prompts []openapimcp.Prompt`. Handlers: `Read func(ctx, uri) (ResourceContent{Text, Blob, MIMEType}, error)` (return `openapimcp.ErrResourceNotFound` for an unknown id; `openapimcp.MatchTemplate(tmpl, uri)` parses `{vars}`) and `Get func(ctx, args map[string]string) ([]PromptMessage{Role, Text}, error)`. They run behind the same `Auth` as tool calls, as the caller. Not operations: `ParityCheck` ignores them.

### Surface marker

`handler := playgroundlog.Middleware(surface.Middleware(authn.Middleware(mux)))` (surface outside auth). An operation calls `surface.From(ctx)` and gets `surface.UI` (browser, no Authorization header), `surface.API` (anything else direct), `surface.MCP` (a tool call through `openapimcp`) or `surface.Assistant` (a tool call by an app's own in-app assistant, set by that app). The dispatchers set the marker on the context of the in-process request they build; it is never a header, and `surface.Middleware` deletes `X-Playground-Surface` from incoming requests. Use `surface.From(ctx).IsAI()` for "AI authors in draft" rules.

### openapimcp input schema

A tool's input is one JSON object. Path and query parameters are top-level properties; the request body is nested under `body`, except that a body that is the operation's only input (no path or query parameters) and an object is flattened to the top level. A parameter named `body` is an error at construction. Component schemas are made self-contained under `$defs`; header and cookie parameters are not exposed. Operations registered with `Hidden: true` are omitted from the OpenAPI document by Huma and therefore are not tools. The choice is documented in the package comment and in `brain/docs/decisions/0020-openapimcp-dispatch-and-auth.md`.

**An operation that is in the document but is not a tool: `x-mcp: false`** (playground-go v0.13.0, owner decision 2026-10-05). `Hidden: true` removes an operation from the OpenAPI document, so a browser-only operation (key management, a session route) had no generated client types. Set the operation extension `openapimcp.NoToolExtension` (`x-mcp`) to `false` instead: the operation stays in the document and the generated web client, `NewToolset` skips it and `ParityCheck` ignores it when comparing operations with tools (it still checks the rest of the contract). `huma.Operation{..., Extensions: map[string]any{openapimcp.NoToolExtension: false}}`. It is an explicit marker and not a way around [API-qan](https://rb.teb.ooo/API-qan): use it only for an operation that has an approved API-qan exception for your app (the exceptions for browser-only operations are the first uses); every user action without an exception still needs its tool.

### Cookies

Every cookie playground-go sets is `Secure; HttpOnly; SameSite=Lax` and host-only (no `Domain`). Tests in `auth` and `testkit` assert this for the session cookie, the login-state cookie and the cleared cookies.

### Decisions

Choices the document leaves open are in `/srv/playground/brain/docs/decisions/0020` to `0024`.

## page

`page` is the one way to paginate a list operation (rule API-jvg): `page.Params` (the `limit` and `cursor` query parameters), `page.Body[T]` (`items`, `next_cursor`), `page.Encode`/`page.Decode` (an opaque keyset cursor; a malformed cursor is a 400) and `page.Trim` (the fetch-limit-plus-one pattern). `openapimcp.ParityCheck` fails a GET whose 200 body has an `items` array but lacks `limit`, `cursor` or `next_cursor`. See the package comment and docs/go-api.md.
