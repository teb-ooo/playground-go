package assistant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PgxPool is the subset of *pgxpool.Pool (and pgx.Tx, *pgx.Conn) PgxStore needs.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PgxStore is the Postgres Store, on the tables in migrations/00001_assistant.sql.
type PgxStore struct{ pool PgxPool }

// NewPgxStore returns a Store backed by pool.
func NewPgxStore(pool PgxPool) *PgxStore { return &PgxStore{pool: pool} }

// CreateConversation implements Store.
func (s *PgxStore) CreateConversation(ctx context.Context, c Conversation) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO assistant_conversations (id, user_id, title, created_at, updated_at) VALUES ($1, $2, $3, $4, $5)`,
		c.ID, c.UserID, c.Title, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("assistant: creating conversation: %w", err)
	}
	return nil
}

// ListConversations implements Store.
func (s *PgxStore) ListConversations(ctx context.Context, userID string, limit int) ([]Conversation, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, user_id, title, created_at, updated_at FROM assistant_conversations
		 WHERE user_id = $1 ORDER BY updated_at DESC, id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("assistant: listing conversations: %w", err)
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.UserID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("assistant: scanning conversation: %w", err)
		}
		c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assistant: listing conversations: %w", err)
	}
	return out, nil
}

// GetConversation implements Store.
func (s *PgxStore) GetConversation(ctx context.Context, userID, id string) (Conversation, error) {
	var c Conversation
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, user_id, title, created_at, updated_at FROM assistant_conversations
		 WHERE id = $1 AND user_id = $2`, id, userID).
		Scan(&c.ID, &c.UserID, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("assistant: getting conversation: %w", err)
	}
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return c, nil
}

// SetConversationTitle implements Store.
func (s *PgxStore) SetConversationTitle(ctx context.Context, userID, id, title string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE assistant_conversations SET title = $3 WHERE id = $1 AND user_id = $2`, id, userID, title)
	if isInvalidUUID(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("assistant: setting title: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendMessages implements Store. The insert and the updated_at bump run in one transaction.
func (s *PgxStore) AppendMessages(ctx context.Context, userID, conversationID string, msgs []Message) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("assistant: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked string
	err = tx.QueryRow(ctx,
		`SELECT id::text FROM assistant_conversations WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		conversationID, userID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("assistant: locking conversation: %w", err)
	}

	var latest time.Time
	for _, m := range msgs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO assistant_messages (id, conversation_id, role, content, created_at) VALUES ($1, $2, $3, $4, $5)`,
			m.ID, conversationID, m.Role, []byte(m.Content), m.CreatedAt); err != nil {
			return fmt.Errorf("assistant: inserting message: %w", err)
		}
		if m.CreatedAt.After(latest) {
			latest = m.CreatedAt
		}
	}
	if !latest.IsZero() {
		if _, err := tx.Exec(ctx,
			`UPDATE assistant_conversations SET updated_at = GREATEST(updated_at, $2) WHERE id = $1`,
			conversationID, latest); err != nil {
			return fmt.Errorf("assistant: touching conversation: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("assistant: committing messages: %w", err)
	}
	return nil
}

// ListMessages implements Store.
func (s *PgxStore) ListMessages(ctx context.Context, userID, conversationID string) ([]Message, error) {
	if _, err := s.GetConversation(ctx, userID, conversationID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, role, content, created_at FROM assistant_messages WHERE conversation_id = $1 ORDER BY id`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("assistant: listing messages: %w", err)
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var content []byte
		if err := rows.Scan(&m.ID, &m.Role, &content, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("assistant: scanning message: %w", err)
		}
		m.ConversationID = conversationID
		m.Content = content
		m.CreatedAt = m.CreatedAt.UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assistant: listing messages: %w", err)
	}
	return out, nil
}

// isInvalidUUID reports Postgres error 22P02 (a malformed uuid in the URL).
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
