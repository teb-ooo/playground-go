-- +goose Up
-- The two tables the assistant package persists conversations in. The template
-- ships this migration (renumbered into the app's own sequence); playground-go
-- embeds a copy as assistant.MigrationsFS for documentation and tests.
-- IDs are UUIDv7, so ordering by id is ordering by time.

CREATE TABLE assistant_conversations (
    id         uuid PRIMARY KEY,
    user_id    text        NOT NULL,            -- OIDC subject of the owner
    title      text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX assistant_conversations_user_idx
    ON assistant_conversations (user_id, updated_at DESC);

CREATE TABLE assistant_messages (
    id              uuid PRIMARY KEY,
    conversation_id uuid        NOT NULL REFERENCES assistant_conversations (id) ON DELETE CASCADE,
    role            text        NOT NULL CHECK (role IN ('user', 'assistant')),
    content         jsonb       NOT NULL,       -- array of content blocks, exactly as sent to and received from the model
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX assistant_messages_conversation_idx
    ON assistant_messages (conversation_id, id);

-- +goose Down
DROP TABLE assistant_messages;
DROP TABLE assistant_conversations;
