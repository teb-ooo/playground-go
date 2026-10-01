package apitoken

import (
	"context"
	"embed"
	"errors"
	"sort"
	"sync"
	"time"
)

// MigrationsFS holds the SQL migration for the api_tokens table. The template
// copies it into the app's own migrations (renumbered); it is embedded here as
// documentation and so tests can apply it.
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

// ErrNotFound is returned when a token does not exist or belongs to another
// user (the two are deliberately indistinguishable).
var ErrNotFound = errors.New("apitoken: token not found")

// ErrDuplicate is returned by Create when the id or the hash already exists.
var ErrDuplicate = errors.New("apitoken: duplicate token")

// Record is a stored token. It never holds the token itself, only its hash.
type Record struct {
	ID           string
	UserID       string // OIDC subject of the owner
	UserEmail    string // snapshot at creation
	UserUsername string // snapshot at creation
	Name         string
	Hash         []byte // SHA-256 of the full token, 32 bytes
	Prefix       string // first 8 characters of the token, for display
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
}

// Active reports whether the token may be used at now.
func (r Record) Active(now time.Time) bool {
	return r.RevokedAt == nil && (r.ExpiresAt == nil || now.Before(*r.ExpiresAt))
}

// Store persists tokens. List and Revoke are scoped to one user; Lookup is the
// only method that crosses users and is used by the verifier.
type Store interface {
	// Create stores r (ID, hash and timestamps are chosen by the caller).
	Create(ctx context.Context, r Record) error
	// ListByUser returns the user's tokens, revoked ones included, newest
	// first (id descending). With after set it returns those older than the
	// token with that id. limit <= 0 means no limit.
	ListByUser(ctx context.Context, userID string, limit int, after string) ([]Record, error)
	// Revoke marks the user's token revoked at now. Revoking a revoked token
	// succeeds and keeps its first revocation time. A token that does not
	// exist or belongs to someone else is ErrNotFound.
	Revoke(ctx context.Context, userID, id string, now time.Time) error
	// Lookup returns the token with this hash, whatever its state, or
	// ErrNotFound.
	Lookup(ctx context.Context, hash []byte) (Record, error)
	// Touch records last_used_at.
	Touch(ctx context.Context, id string, at time.Time) error
}

// MemoryStore is an in-memory Store for tests.
type MemoryStore struct {
	mu   sync.Mutex
	recs map[string]Record // by id
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{recs: map[string]Record{}} }

func clone(r Record) Record {
	r.Hash = append([]byte(nil), r.Hash...)
	return r
}

// Create implements Store.
func (m *MemoryStore) Create(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.recs {
		if o.ID == r.ID || string(o.Hash) == string(r.Hash) {
			return ErrDuplicate
		}
	}
	m.recs[r.ID] = clone(r)
	return nil
}

// ListByUser implements Store.
func (m *MemoryStore) ListByUser(_ context.Context, userID string, limit int, after string) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Record{}
	for _, r := range m.recs {
		if r.UserID == userID && (after == "" || r.ID < after) {
			out = append(out, clone(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Revoke implements Store.
func (m *MemoryStore) Revoke(_ context.Context, userID, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok || r.UserID != userID {
		return ErrNotFound
	}
	if r.RevokedAt == nil {
		r.RevokedAt = &now
		m.recs[id] = r
	}
	return nil
}

// Lookup implements Store.
func (m *MemoryStore) Lookup(_ context.Context, hash []byte) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.recs {
		if string(r.Hash) == string(hash) {
			return clone(r), nil
		}
	}
	return Record{}, ErrNotFound
}

// Touch implements Store.
func (m *MemoryStore) Touch(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return ErrNotFound
	}
	r.LastUsedAt = &at
	m.recs[id] = r
	return nil
}
