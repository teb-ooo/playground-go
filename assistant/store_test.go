package assistant_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teb-ooo/factory-go/assistant"
	"github.com/teb-ooo/factory-go/internal/uuidv7"
)

func TestMemoryStore(t *testing.T) {
	storeConformance(t, func(t *testing.T) assistant.Store { return assistant.NewMemoryStore() })
}

// TestPgxStore runs the same suite against Postgres when
// FACTORY_TEST_DATABASE_URL points at a throwaway database, for example
//
//	docker run --rm -d -p 55432:5432 -e POSTGRES_PASSWORD=x postgres:17.11
//	FACTORY_TEST_DATABASE_URL=postgres://postgres:x@127.0.0.1:55432/postgres go test ./assistant
func TestPgxStore(t *testing.T) {
	dsn := os.Getenv("FACTORY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FACTORY_TEST_DATABASE_URL not set")
	}
	storeConformance(t, newPgxStore)
}

// newPgxStore returns a PgxStore on a fresh schema of the throwaway database.
func newPgxStore(t *testing.T) assistant.Store {
	dsn := os.Getenv("FACTORY_TEST_DATABASE_URL")
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "asst_" + strings.ReplaceAll(uuidv7.New()[24:], "-", "")
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
	sql, err := assistant.MigrationsFS.ReadFile("migrations/00001_assistant.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := strings.SplitN(strings.TrimPrefix(string(sql), "-- +goose Up"), "-- +goose Down", 2)[0]
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("applying migration: %v", err)
	}
	return assistant.NewPgxStore(pool)
}

func msg(role, content string, at time.Time) assistant.Message {
	b, _ := json.Marshal([]map[string]any{{"type": "text", "text": content}})
	return assistant.Message{ID: uuidv7.New(), Role: role, Content: b, CreatedAt: at}
}

func storeConformance(t *testing.T, mk func(t *testing.T) assistant.Store) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)
	newConv := func(t *testing.T, s assistant.Store, user, title string, at time.Time) assistant.Conversation {
		t.Helper()
		c := assistant.Conversation{ID: uuidv7.New(), UserID: user, Title: title, CreatedAt: at, UpdatedAt: at}
		if err := s.CreateConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c
	}

	t.Run("create get list ordered and scoped", func(t *testing.T) {
		s := mk(t)
		c1 := newConv(t, s, "u1", "first", base)
		c2 := newConv(t, s, "u1", "second", base.Add(time.Second))
		newConv(t, s, "u2", "other", base.Add(2*time.Second))

		got, err := s.GetConversation(ctx, "u1", c1.ID)
		if err != nil || got.Title != "first" || got.UserID != "u1" || !got.CreatedAt.Equal(base) {
			t.Fatalf("get = %+v err=%v", got, err)
		}
		list, err := s.ListConversations(ctx, "u1", 10)
		if err != nil || len(list) != 2 || list[0].ID != c2.ID || list[1].ID != c1.ID {
			t.Fatalf("list = %+v err=%v", list, err)
		}
		if l, _ := s.ListConversations(ctx, "u1", 1); len(l) != 1 {
			t.Errorf("limit not applied: %d", len(l))
		}
		if l, err := s.ListConversations(ctx, "nobody", 10); err != nil || l == nil || len(l) != 0 {
			t.Errorf("empty list = %v err=%v (want empty non-nil)", l, err)
		}
	})

	t.Run("other users and unknown ids are not found", func(t *testing.T) {
		s := mk(t)
		c := newConv(t, s, "u1", "x", base)
		bad := []string{c.ID, uuidv7.New(), "not-a-uuid"}
		for _, id := range bad {
			user := "u2"
			if id != c.ID {
				user = "u1"
			}
			if _, err := s.GetConversation(ctx, user, id); !errors.Is(err, assistant.ErrNotFound) {
				t.Errorf("Get(%s,%s) = %v", user, id, err)
			}
			if _, err := s.ListMessages(ctx, user, id); !errors.Is(err, assistant.ErrNotFound) {
				t.Errorf("ListMessages(%s,%s) = %v", user, id, err)
			}
			if err := s.AppendMessages(ctx, user, id, []assistant.Message{msg("user", "x", base)}); !errors.Is(err, assistant.ErrNotFound) {
				t.Errorf("Append(%s,%s) = %v", user, id, err)
			}
			if err := s.SetConversationTitle(ctx, user, id, "hijack"); !errors.Is(err, assistant.ErrNotFound) {
				t.Errorf("SetTitle(%s,%s) = %v", user, id, err)
			}
		}
		if got, _ := s.GetConversation(ctx, "u1", c.ID); got.Title != "x" {
			t.Errorf("title changed by another user: %q", got.Title)
		}
	})

	t.Run("messages append in order and bump updated_at", func(t *testing.T) {
		s := mk(t)
		c := newConv(t, s, "u1", "x", base)
		var want []string
		for i := 0; i < 5; i++ {
			role := []string{"user", "assistant"}[i%2]
			text := fmt.Sprintf("m%d", i)
			want = append(want, text)
			if err := s.AppendMessages(ctx, "u1", c.ID, []assistant.Message{msg(role, text, base.Add(time.Duration(i+1)*time.Minute))}); err != nil {
				t.Fatal(err)
			}
		}
		// A batch keeps its own order too.
		b := []assistant.Message{msg("user", "b1", base.Add(time.Hour)), msg("assistant", "b2", base.Add(time.Hour))}
		if err := s.AppendMessages(ctx, "u1", c.ID, b); err != nil {
			t.Fatal(err)
		}
		want = append(want, "b1", "b2")
		got, err := s.ListMessages(ctx, "u1", c.ID)
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, m := range got {
			var blocks []map[string]any
			json.Unmarshal(m.Content, &blocks)
			texts = append(texts, blocks[0]["text"].(string))
			if m.ConversationID != c.ID || m.ID == "" || m.CreatedAt.IsZero() {
				t.Errorf("message = %+v", m)
			}
		}
		if strings.Join(texts, ",") != strings.Join(want, ",") {
			t.Errorf("order = %v, want %v", texts, want)
		}
		if got[0].Role != "user" || got[1].Role != "assistant" {
			t.Errorf("roles = %s,%s", got[0].Role, got[1].Role)
		}
		cc, _ := s.GetConversation(ctx, "u1", c.ID)
		if !cc.UpdatedAt.Equal(base.Add(time.Hour)) {
			t.Errorf("updated_at = %v, want %v", cc.UpdatedAt, base.Add(time.Hour))
		}
	})

	t.Run("content round trips as JSON", func(t *testing.T) {
		s := mk(t)
		c := newConv(t, s, "u1", "x", base)
		content := `[{"type":"tool_use","id":"toolu_1","name":"get-item","input":{"id":"42","n":1.5,"tags":["a","b"]}}]`
		m := assistant.Message{ID: uuidv7.New(), Role: "assistant", Content: json.RawMessage(content), CreatedAt: base}
		if err := s.AppendMessages(ctx, "u1", c.ID, []assistant.Message{m}); err != nil {
			t.Fatal(err)
		}
		got, _ := s.ListMessages(ctx, "u1", c.ID)
		var a, b any
		json.Unmarshal([]byte(content), &a)
		json.Unmarshal(got[0].Content, &b)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Errorf("content = %s", got[0].Content)
		}
	})

	t.Run("a failed batch stores nothing", func(t *testing.T) {
		s := mk(t)
		c := newConv(t, s, "u1", "x", base)
		good := msg("user", "ok", base)
		dup := good // same id twice: the second insert must fail and roll back the first
		err := s.AppendMessages(ctx, "u1", c.ID, []assistant.Message{good, dup})
		if err == nil {
			t.Skip("store does not reject duplicate message ids")
		}
		got, _ := s.ListMessages(ctx, "u1", c.ID)
		if len(got) != 0 {
			t.Errorf("partial batch stored: %d messages", len(got))
		}
	})

	t.Run("set title", func(t *testing.T) {
		s := mk(t)
		c := newConv(t, s, "u1", "", base)
		if err := s.SetConversationTitle(ctx, "u1", c.ID, "Named"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetConversation(ctx, "u1", c.ID); got.Title != "Named" {
			t.Errorf("title = %q", got.Title)
		}
	})
}
