package apitoken_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teb-ooo/playground-go/apitoken"
	"github.com/teb-ooo/playground-go/internal/uuidv7"
)

func TestMemoryStore(t *testing.T) {
	storeConformance(t, func(t *testing.T) apitoken.Store { return apitoken.NewMemoryStore() })
}

// TestPgxStore runs the same suite against Postgres when
// PLAYGROUND_TEST_DATABASE_URL points at a throwaway database (see the
// assistant package). It applies migrations/00001_api_tokens.sql to a fresh
// schema, after defining the set_updated_at() function the app's own 00001
// migration provides.
func TestPgxStore(t *testing.T) {
	if os.Getenv("PLAYGROUND_TEST_DATABASE_URL") == "" {
		t.Skip("PLAYGROUND_TEST_DATABASE_URL not set")
	}
	storeConformance(t, newPgxStore)
}

const setUpdatedAt = `CREATE FUNCTION set_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END $$;`

func newPgxStore(t *testing.T) apitoken.Store {
	dsn := os.Getenv("PLAYGROUND_TEST_DATABASE_URL")
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "pat_" + strings.ReplaceAll(uuidv7.New()[24:], "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	if _, err := pool.Exec(ctx, setUpdatedAt); err != nil {
		t.Fatal(err)
	}
	sql, err := apitoken.MigrationsFS.ReadFile("migrations/00001_api_tokens.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := strings.SplitN(strings.TrimPrefix(string(sql), "-- +goose Up"), "-- +goose Down", 2)[0]
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("applying migration: %v", err)
	}
	return apitoken.NewPgxStore(pool)
}

func rec(user, name string, at time.Time) apitoken.Record {
	tok, _ := apitoken.Generate()
	return apitoken.Record{ID: uuidv7.New(), UserID: user, UserEmail: user + "@x", UserUsername: user, Name: name,
		Hash: apitoken.Hash(tok), Prefix: apitoken.DisplayPrefix(tok), CreatedAt: at}
}

func storeConformance(t *testing.T, mk func(t *testing.T) apitoken.Store) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)

	t.Run("create and lookup by hash", func(t *testing.T) {
		s := mk(t)
		exp := base.Add(time.Hour)
		r := rec("u1", "laptop", base)
		r.ExpiresAt = &exp
		if err := s.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, err := s.Lookup(ctx, r.Hash)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != r.ID || got.UserID != "u1" || got.UserEmail != "u1@x" || got.Name != "laptop" || got.Prefix != r.Prefix ||
			!got.CreatedAt.Equal(base) || got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) || got.LastUsedAt != nil || got.RevokedAt != nil {
			t.Fatalf("got %+v", got)
		}
		if _, err := s.Lookup(ctx, apitoken.Hash("pat_unknown")); !errors.Is(err, apitoken.ErrNotFound) {
			t.Fatalf("unknown hash: %v", err)
		}
		if err := s.Create(ctx, r); !errors.Is(err, apitoken.ErrDuplicate) {
			t.Fatalf("duplicate: %v", err)
		}
	})

	t.Run("list is per user, newest first, keyset paged", func(t *testing.T) {
		s := mk(t)
		var ids []string
		for i := 0; i < 5; i++ {
			r := rec("u1", "t", base)
			ids = append(ids, r.ID)
			if err := s.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Create(ctx, rec("u2", "other", base)); err != nil {
			t.Fatal(err)
		}
		p1, err := s.ListByUser(ctx, "u1", 2, "")
		if err != nil || len(p1) != 2 || p1[0].ID != ids[4] || p1[1].ID != ids[3] {
			t.Fatalf("page 1: %v %+v", err, p1)
		}
		p2, err := s.ListByUser(ctx, "u1", 10, p1[1].ID)
		if err != nil || len(p2) != 3 || p2[0].ID != ids[2] || p2[2].ID != ids[0] {
			t.Fatalf("page 2: %v %+v", err, p2)
		}
		if l, _ := s.ListByUser(ctx, "nobody", 10, ""); len(l) != 0 {
			t.Fatalf("other user sees %d", len(l))
		}
	})

	t.Run("revoke is scoped to the owner and idempotent", func(t *testing.T) {
		s := mk(t)
		r := rec("u1", "t", base)
		if err := s.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := s.Revoke(ctx, "u2", r.ID, base); !errors.Is(err, apitoken.ErrNotFound) {
			t.Fatalf("other user revoked: %v", err)
		}
		if g, _ := s.Lookup(ctx, r.Hash); g.RevokedAt != nil {
			t.Fatal("revoked by a stranger")
		}
		at := base.Add(time.Minute)
		if err := s.Revoke(ctx, "u1", r.ID, at); err != nil {
			t.Fatal(err)
		}
		if err := s.Revoke(ctx, "u1", r.ID, at.Add(time.Hour)); err != nil {
			t.Fatalf("second revoke: %v", err)
		}
		g, _ := s.Lookup(ctx, r.Hash)
		if g.RevokedAt == nil || !g.RevokedAt.Equal(at) {
			t.Fatalf("revoked_at = %v, want first time %v", g.RevokedAt, at)
		}
		for _, id := range []string{uuidv7.New(), "not-a-uuid"} {
			if err := s.Revoke(ctx, "u1", id, at); !errors.Is(err, apitoken.ErrNotFound) {
				t.Fatalf("revoke %q: %v", id, err)
			}
		}
		l, _ := s.ListByUser(ctx, "u1", 10, "")
		if len(l) != 1 || l[0].RevokedAt == nil {
			t.Fatalf("revoked token missing from list: %+v", l)
		}
	})

	t.Run("touch", func(t *testing.T) {
		s := mk(t)
		r := rec("u1", "t", base)
		s.Create(ctx, r)
		at := base.Add(time.Minute)
		if err := s.Touch(ctx, r.ID, at); err != nil {
			t.Fatal(err)
		}
		g, _ := s.Lookup(ctx, r.Hash)
		if g.LastUsedAt == nil || !g.LastUsedAt.Equal(at) {
			t.Fatalf("last_used_at = %v", g.LastUsedAt)
		}
		if err := s.Touch(ctx, uuidv7.New(), at); !errors.Is(err, apitoken.ErrNotFound) {
			t.Fatalf("touch unknown: %v", err)
		}
	})
}
