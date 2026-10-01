-- +goose Up
-- Personal access tokens for the apitoken package. The template ships this
-- migration (renumbered into the app's own sequence); playground-go embeds a
-- copy as apitoken.MigrationsFS for documentation and tests. IDs are UUIDv7,
-- so ordering by id is ordering by time. The set_updated_at() function comes
-- from the app's own 00001 migration, as for every other table.
-- Only the SHA-256 of the token is stored, never the token itself.

CREATE TABLE api_tokens (
    id            uuid PRIMARY KEY,
    user_id       text        NOT NULL,            -- OIDC subject of the owner
    user_email    text        NOT NULL DEFAULT '', -- snapshot at creation
    user_username text        NOT NULL DEFAULT '', -- snapshot at creation
    name          text        NOT NULL,
    token_hash    bytea       NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    prefix        text        NOT NULL,            -- first 8 characters of the token, for display
    last_used_at  timestamptz,
    expires_at    timestamptz,
    revoked_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX api_tokens_user_idx ON api_tokens (user_id, id DESC);

CREATE TRIGGER api_tokens_set_updated_at BEFORE UPDATE ON api_tokens
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE api_tokens;
