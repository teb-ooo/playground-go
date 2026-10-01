package apitoken

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
}

// PgxStore is the Postgres Store, on the table in migrations/00001_api_tokens.sql.
type PgxStore struct{ pool PgxPool }

// NewPgxStore returns a Store backed by pool.
func NewPgxStore(pool PgxPool) *PgxStore { return &PgxStore{pool: pool} }

const cols = `id::text, user_id, user_email, user_username, name, token_hash, prefix, created_at, last_used_at, expires_at, revoked_at`

func scan(row pgx.Row) (Record, error) {
	var r Record
	if err := row.Scan(&r.ID, &r.UserID, &r.UserEmail, &r.UserUsername, &r.Name, &r.Hash, &r.Prefix,
		&r.CreatedAt, &r.LastUsedAt, &r.ExpiresAt, &r.RevokedAt); err != nil {
		return Record{}, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	for _, p := range []**time.Time{&r.LastUsedAt, &r.ExpiresAt, &r.RevokedAt} {
		if *p != nil {
			t := (*p).UTC()
			*p = &t
		}
	}
	return r, nil
}

// Create implements Store.
func (s *PgxStore) Create(ctx context.Context, r Record) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO api_tokens (id, user_id, user_email, user_username, name, token_hash, prefix, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		r.ID, r.UserID, r.UserEmail, r.UserUsername, r.Name, r.Hash, r.Prefix, r.CreatedAt, r.ExpiresAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("apitoken: creating token: %w", err)
	}
	return nil
}

// ListByUser implements Store.
func (s *PgxStore) ListByUser(ctx context.Context, userID string, limit int, after string) ([]Record, error) {
	if limit <= 0 {
		limit = 1000
	}
	var afterArg any
	if after != "" {
		afterArg = after
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+cols+` FROM api_tokens
		 WHERE user_id = $1 AND ($2::uuid IS NULL OR id < $2::uuid)
		 ORDER BY id DESC LIMIT $3`, userID, afterArg, limit)
	if err != nil {
		if isInvalidUUID(err) {
			return []Record{}, nil
		}
		return nil, fmt.Errorf("apitoken: listing tokens: %w", err)
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("apitoken: scanning token: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		if isInvalidUUID(err) {
			return []Record{}, nil
		}
		return nil, fmt.Errorf("apitoken: listing tokens: %w", err)
	}
	return out, nil
}

// Revoke implements Store.
func (s *PgxStore) Revoke(ctx context.Context, userID, id string, now time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, $3) WHERE id = $1 AND user_id = $2`, id, userID, now)
	if isInvalidUUID(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("apitoken: revoking token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Lookup implements Store.
func (s *PgxStore) Lookup(ctx context.Context, hash []byte) (Record, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM api_tokens WHERE token_hash = $1`, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("apitoken: looking up token: %w", err)
	}
	return r, nil
}

// Touch implements Store.
func (s *PgxStore) Touch(ctx context.Context, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("apitoken: recording token use: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// isInvalidUUID reports Postgres error 22P02 (a malformed uuid).
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
