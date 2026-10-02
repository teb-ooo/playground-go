# playground-go

playground-go is the shared Go library every playground app is built on: it turns a Huma API into an MCP server, signs users in through the playground's OIDC issuer, and provides health, logging, rate limiting, mail, an in-app assistant, SPA serving and test helpers. Apps import its packages and never reimplement them, so behaviour is identical everywhere. The module path is `github.com/teb-ooo/playground-go`.

## Tests

```bash
go vet ./... && go test ./... -race
```

One optional integration check skips itself unless configured: the Postgres store test (`PLAYGROUND_TEST_DATABASE_URL=postgres://postgres:x@127.0.0.1:55432/postgres`, used by the assistant store tests; a throwaway database, for example `docker run --rm -d -p 55432:5432 -e POSTGRES_PASSWORD=x postgres:17.11`). No test calls the real Anthropic API or any real mail provider; they use httptest fakes.

## Usage

```go
cfg, err := playground.LoadConfig()          // PORT, DATABASE_URL, OIDC_*, SESSION_KEY, APP_NAME, APP_ENV, PUBLIC_URL, MAIL_*, ASSISTANT_MODEL, ANTHROPIC_API_KEY, PLAYGROUND_DOMAIN
authn, err := cfg.NewAuth()

mux := http.NewServeMux()
api := humago.New(mux, huma.DefaultConfig(cfg.AppName, cfg.Version))
auth.AddSecuritySchemes(api.OpenAPI())     // declares "session" and "bearer"
authn.Register(api, mux)                   // /auth/login, /auth/callback, /auth/logout, GET /auth/me (hidden)
health.Register(api, pool, cfg.Version)    // GET /healthz (hidden)
registerItems(api, pool)                   // your operations: OperationID, Summary, Description are mandatory

mcpH := openapimcp.Handler(api, mux, openapimcp.Options{Name: cfg.AppName, Auth: authn.BearerOrSession})
mux.Handle("/mcp", mcpH)
mux.Handle("/api/assistant/", assistant.Handler(api, mux, cfg.AssistantOptions(assistant.NewPgxStore(pool))))
mux.Handle("/", spa.Handler(webFS))

handler := playgroundlog.Middleware(authn.Middleware(mux))
```

Register every operation before `openapimcp.Handler` and `assistant.Handler`; they derive their tools when constructed. The template's parity test is:

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
| `playground` (root) | `Config` from the environment with validation; helpers `NewAuth`, `NewMailer`, `AssistantOptions`. Secrets never appear in errors or in `LogValue`. |
| `openapimcp` | One MCP tool per OpenAPI operation, executed in-process with the caller's credentials forwarded. `Handler`, `New`, `Tools()`, `ParityCheck`. |
| `surface` | Server-side marker of the door a request came through (`ui`, `api`, `mcp`, `assistant`): `surface.From(ctx)` in an operation handler, `surface.Middleware` mounted outermost. Set by the MCP server and the assistant for their tool calls; cannot be forged by a client header. |
| `auth` | OIDC code flow with PKCE, state and nonce; encrypted session cookie; bearer tokens; `Require`, `RequireAdmin`, `User`. |
| `keys` | Platform API keys (`pk_` + ES256 JWS issued by playd), verified offline against `https://id.<domain>/.keys/jwks.json` with a revocation list; `NewAuth` installs it, so an app needs no code. Scopes `<app>:read`/`write`/`admin` are enforced by `auth.ScopeMiddleware` and filter MCP tools. See `keys/README.md`. |
| `ratelimit` | In-memory token bucket for net/http and Huma, keyed by client IP (last `X-Forwarded-For` hop from a trusted proxy) and an optional extra key. |
| `live` | Rule UI-7, the server half: `Hub` (`Publish(resource, audience)` after a write, never blocks, per-subscriber coalescing), audiences (`Everyone`, `Subject`, `Admins`, `Project`), `Mount(mux, hub)` for `GET /api/live` (SSE: `: live`, `change`, `degraded`, `: ping` every 25 s, 1 h lifetime, 8 streams per person, 401/429 problem+json), and `Relay` for an upstream stream such as playd's `/v1/work/events`. Not a Huma operation. |
| `health` | `GET /healthz` returning `{version, env, db, uptime_seconds}`. |
| `log` | slog JSON to stdout, request middleware, redaction of `*_KEY`, `*_SECRET`, `*_TOKEN`, `*_PASSWORD` values. |
| `assistant` | The end-user assistant: tool-use loop over the app's own API as the signed-in user, SSE replies, `Store` interface with `PgxStore` and `MemoryStore`. Migration SQL in `assistant/migrations/`. |
| `mail` | Transport only: `Mailer.Send` over Resend or Postmark HTTP, staging redirect to `MAIL_STAGING_SINK` and the `[staging <app>]` subject prefix. No layout and no templates: each app owns its email templates (complete text and HTML documents in its own repo) and passes the rendered `Message` to `Send`. Breaking change in v0.4.0: `Templates`, `Page`, `SendTemplate`, `Config.Templates`, `Config.PlaygroundName` and the `NewMailer` argument are removed. |
| `spa` | Serves an embedded `fs.FS`, falls back to `index.html`, serves `window.__PLAYGROUND__` at `/playground.js` (a script tag in index.html; the CSP forbids inline scripts) (`app_name`, `env`, `claude_session_url`, `locale`, `timezone`, `assistant`). `assistant` is a boolean from `spa.Config.Assistant` (`PLAYGROUND_ASSISTANT=true`, default false; use `cfg.SPA()`). |
| `testkit` | Minted session cookies and Kratos sessions for staging and tests. |

### Error shape

The one error shape is Huma's default RFC 9457 `application/problem+json`: `title`, `status`, `detail` and, for validation failures, `errors[]` (each with `message`, `location`, `value`). Handlers written outside Huma (the auth routes, the assistant routes, the rate limiter) return the same body with `title`, `status` and `detail`. `@teb-ooo/web` types its `ApiError` from this, so do not invent another error format.

### Assistant context hook

`assistant.Options.Context func(ctx, assistant.ContextRequest) ([]assistant.ContextBlock, error)` supplies extra context for one message (injected before the user's text in the user turn, never stored as typed, streamed as the SSE `context` event, recorded compactly and returned as `context` on the message when reloaded); `Options.SystemPromptFunc func(ctx, auth.User, assistant.Conversation) (string, error)` computes the system prompt per message. Errors are soft (a warning block) unless the hook returns `assistant.Abort("reason")` (`assistant.ErrAbort`). Details and the caching rationale are in the package doc. The SSE events are now `text`, `tool_call`, `tool_result`, `done`, `error` and (only with a hook) `context {blocks:[{kind,label,tokens_estimate,metadata?,text?}]}`.

### MCP instructions, resources and prompts

`openapimcp.Options` also takes `Instructions` (shown at initialize), `Resources []openapimcp.Resource`, `ResourceTemplates []openapimcp.ResourceTemplate` and `Prompts []openapimcp.Prompt`. Handlers: `Read func(ctx, uri) (ResourceContent{Text, Blob, MIMEType}, error)` (return `openapimcp.ErrResourceNotFound` for an unknown id; `openapimcp.MatchTemplate(tmpl, uri)` parses `{vars}`) and `Get func(ctx, args map[string]string) ([]PromptMessage{Role, Text}, error)`. They run behind the same `Auth` as tool calls, as the caller. Not operations: `ParityCheck` ignores them.

### Surface marker

`handler := playgroundlog.Middleware(surface.Middleware(authn.Middleware(mux)))` (surface outside auth). An operation calls `surface.From(ctx)` and gets `surface.UI` (browser, no Authorization header), `surface.API` (anything else direct), `surface.MCP` (a tool call through `openapimcp`) or `surface.Assistant` (a tool call by the in-app assistant). The dispatchers set the marker on the context of the in-process request they build; it is never a header, and `surface.Middleware` deletes `X-Playground-Surface` from incoming requests. Use `surface.From(ctx).IsAI()` for "AI authors in draft" rules.

### openapimcp input schema

A tool's input is one JSON object. Path and query parameters are top-level properties; the request body is nested under `body`, except that a body that is the operation's only input (no path or query parameters) and an object is flattened to the top level. A parameter named `body` is an error at construction. Component schemas are made self-contained under `$defs`; header and cookie parameters are not exposed. Operations registered with `Hidden: true` are omitted from the OpenAPI document by Huma and therefore are not tools. The choice is documented in the package comment and in `brain/docs/decisions/0020-openapimcp-dispatch-and-auth.md`.

### Assistant events

`POST /api/assistant/conversations/{id}/messages` answers with server-sent events, one JSON object per `data:` line, the SSE `event:` name equal to the type: `text {text}`, `tool_call {id, name, input}`, `tool_result {id, content, is_error?}` (`is_error` only when true), `done {stop_reason}`. A failure after the stream has started sends `error {detail}` and ends it. These are the shapes `useEventStream<AssistantEvents>` in `@teb-ooo/web` expects.

### Cookies

Every cookie playground-go sets is `Secure; HttpOnly; SameSite=Lax` and host-only (no `Domain`). Tests in `auth` and `testkit` assert this for the session cookie, the login-state cookie and the cleared cookies.

### Decisions

Choices the document leaves open are in `/srv/playground/brain/docs/decisions/0020` to `0024`.
