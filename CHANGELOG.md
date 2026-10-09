# Changelog

One entry per tag, newest first. "Upgrade" says what an app must do; "none" means a `go get` is enough. A new tag is annotated and its message is its entry here. Library features that pair with a `@teb-ooo/ui` version say which.

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
