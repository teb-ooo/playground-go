# factory-go

factory-go is the shared Go library every factory app is built on: it turns a Huma API into an MCP server, signs users in through the factory's OIDC issuer, and provides health, logging, rate limiting, mail, an in-app assistant, SPA serving and test helpers. Apps import its packages and never reimplement them, so behaviour is identical everywhere. The module path is `github.com/teb-ooo/factory-go`.

## Tests

```bash
go vet ./... && go test ./... -race
```

Two optional integration checks skip themselves unless configured: the Postgres store test (`FACTORY_TEST_DATABASE_URL=postgres://postgres:x@127.0.0.1:55432/postgres`, a throwaway database, for example `docker run --rm -d -p 55432:5432 -e POSTGRES_PASSWORD=x postgres:17.11`) and the mail template drift check (`FACTORY_UI_EMAIL_DIR=/srv/factory/lib/ui/email`). No test calls the real Anthropic API or any real mail provider; they use httptest fakes.

## Usage

```go
cfg, err := factory.LoadConfig()          // PORT, DATABASE_URL, OIDC_*, SESSION_KEY, APP_NAME, APP_ENV, PUBLIC_URL, MAIL_*, ASSISTANT_MODEL, ANTHROPIC_API_KEY, FACTORY_DOMAIN
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

handler := factorylog.Middleware(authn.Middleware(mux))
```

Register every operation before `openapimcp.Handler` and `assistant.Handler`; they derive their tools when constructed. The template's parity test is:

```go
openapimcp.ParityCheck(t, api, mcpH) // operation ids == MCP tool names; every op has Summary and Description
```

Tests and the agent's browser sign in with `testkit.MintSession(sessionKey, auth.User{...})` and set the cookie with `testkit.SetCookieString(cookie)`. Everything in `testkit` refuses to run when `APP_ENV=production`.

## Packages

| Package | What it does |
|---|---|
| `factory` (root) | `Config` from the environment with validation; helpers `NewAuth`, `NewMailer`, `AssistantOptions`. Secrets never appear in errors or in `LogValue`. |
| `openapimcp` | One MCP tool per OpenAPI operation, executed in-process with the caller's credentials forwarded. `Handler`, `New`, `Tools()`, `ParityCheck`. |
| `auth` | OIDC code flow with PKCE, state and nonce; encrypted session cookie; bearer tokens; `Require`, `RequireAdmin`, `User`. |
| `ratelimit` | In-memory token bucket for net/http and Huma, keyed by client IP (last `X-Forwarded-For` hop from a trusted proxy) and an optional extra key. |
| `health` | `GET /healthz` returning `{version, env, db, uptime_seconds}`. |
| `log` | slog JSON to stdout, request middleware, redaction of `*_KEY`, `*_SECRET`, `*_TOKEN`, `*_PASSWORD` values. |
| `assistant` | The end-user assistant: tool-use loop over the app's own API as the signed-in user, SSE replies, `Store` interface with `PgxStore` and `MemoryStore`. Migration SQL in `assistant/migrations/`. |
| `mail` | Resend and Postmark over HTTP, staging redirect to `MAIL_STAGING_SINK`, text and HTML templates on the base layout copied from `@teb-ooo/ui`. |
| `spa` | Serves an embedded `fs.FS`, falls back to `index.html`, injects `window.__FACTORY__` (`app_name`, `env`, `agent_url`, `claude_session_url`, `locale`, `timezone`, `assistant`). `assistant` is a boolean from `spa.Config.Assistant` (`FACTORY_ASSISTANT=true`, default false; use `cfg.SPA()`). |
| `testkit` | Minted session cookies and Kratos sessions for staging and tests. |

### Error shape

The one error shape is Huma's default RFC 9457 `application/problem+json`: `title`, `status`, `detail` and, for validation failures, `errors[]` (each with `message`, `location`, `value`). Handlers written outside Huma (the auth routes, the assistant routes, the rate limiter) return the same body with `title`, `status` and `detail`. `@teb-ooo/web` types its `ApiError` from this, so do not invent another error format.

### openapimcp input schema

A tool's input is one JSON object. Path and query parameters are top-level properties; the request body is nested under `body`, except that a body that is the operation's only input (no path or query parameters) and an object is flattened to the top level. A parameter named `body` is an error at construction. Component schemas are made self-contained under `$defs`; header and cookie parameters are not exposed. Operations registered with `Hidden: true` are omitted from the OpenAPI document by Huma and therefore are not tools. The choice is documented in the package comment and in `docs/adr/0020`.

### Assistant events

`POST /api/assistant/conversations/{id}/messages` answers with server-sent events, one JSON object per `data:` line, the SSE `event:` name equal to the type: `text {text}`, `tool_call {id, name, input}`, `tool_result {id, content, is_error?}` (`is_error` only when true), `done {stop_reason}`. A failure after the stream has started sends `error {detail}` and ends it. These are the shapes `useEventStream<AssistantEvents>` in `@teb-ooo/web` expects.

### Cookies

Every cookie factory-go sets is `Secure; HttpOnly; SameSite=Lax` and host-only (no `Domain`). Tests in `auth` and `testkit` assert this for the session cookie, the login-state cookie and the cleared cookies.

### Decisions

Choices the document leaves open are in `/srv/factory/docs/adr/0020` to `0024`.
