package assistant

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

// MigrationsFS holds the SQL migration for the two assistant tables. The
// template copies it into the app's own migrations; it is embedded here as
// documentation and so tests can apply it.
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

// ErrNotFound is returned when a conversation does not exist or belongs to
// another user (the two are deliberately indistinguishable).
var ErrNotFound = errors.New("assistant: conversation not found")

// Conversation is a chat thread owned by one user.
type Conversation struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Message is one turn. Role is "user" or "assistant"; tool results travel in
// user messages. Content is a JSON array of Anthropic content blocks, kept
// verbatim so tool_use and tool_result pairs replay exactly.
type Message struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"-"`
	Role           string          `json:"role"`
	Content        json.RawMessage `json:"content"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Store persists conversations. Implementations must scope every call to
// userID: a conversation belonging to someone else is ErrNotFound.
type Store interface {
	// CreateConversation stores c (ID, UserID, Title and timestamps are set by the caller).
	CreateConversation(ctx context.Context, c Conversation) error
	// ListConversations returns the user's conversations, most recently updated first.
	ListConversations(ctx context.Context, userID string, limit int) ([]Conversation, error)
	// GetConversation returns one conversation or ErrNotFound.
	GetConversation(ctx context.Context, userID, id string) (Conversation, error)
	// SetConversationTitle renames a conversation. ErrNotFound if it is not the user's.
	SetConversationTitle(ctx context.Context, userID, id, title string) error
	// AppendMessages atomically adds msgs to the conversation and bumps its
	// updated_at to the latest message time. ErrNotFound if it is not the user's.
	AppendMessages(ctx context.Context, userID, conversationID string, msgs []Message) error
	// ListMessages returns the conversation's messages oldest first, or ErrNotFound.
	ListMessages(ctx context.Context, userID, conversationID string) ([]Message, error)
}

// MemoryStore is a Store held in memory, for tests and demos.
type MemoryStore struct {
	mu    sync.Mutex
	convs map[string]Conversation
	msgs  map[string][]Message
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{convs: map[string]Conversation{}, msgs: map[string][]Message{}}
}

func (s *MemoryStore) owned(userID, id string) (Conversation, bool) {
	c, ok := s.convs[id]
	if !ok || c.UserID != userID {
		return Conversation{}, false
	}
	return c, true
}

// CreateConversation implements Store.
func (s *MemoryStore) CreateConversation(_ context.Context, c Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.convs[c.ID]; dup {
		return errors.New("assistant: conversation id already exists")
	}
	s.convs[c.ID] = c
	return nil
}

// ListConversations implements Store.
func (s *MemoryStore) ListConversations(_ context.Context, userID string, limit int) ([]Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Conversation{}
	for _, c := range s.convs {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetConversation implements Store.
func (s *MemoryStore) GetConversation(_ context.Context, userID, id string) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.owned(userID, id)
	if !ok {
		return Conversation{}, ErrNotFound
	}
	return c, nil
}

// SetConversationTitle implements Store.
func (s *MemoryStore) SetConversationTitle(_ context.Context, userID, id, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.owned(userID, id)
	if !ok {
		return ErrNotFound
	}
	c.Title = title
	s.convs[id] = c
	return nil
}

// AppendMessages implements Store.
func (s *MemoryStore) AppendMessages(_ context.Context, userID, conversationID string, msgs []Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.owned(userID, conversationID)
	if !ok {
		return ErrNotFound
	}
	seen := map[string]bool{}
	for _, m := range s.msgs[conversationID] {
		seen[m.ID] = true
	}
	for _, m := range msgs {
		if seen[m.ID] {
			return errors.New("assistant: duplicate message id")
		}
		seen[m.ID] = true
	}
	for _, m := range msgs {
		m.ConversationID = conversationID
		m.Content = append(json.RawMessage(nil), m.Content...)
		s.msgs[conversationID] = append(s.msgs[conversationID], m)
		if m.CreatedAt.After(c.UpdatedAt) {
			c.UpdatedAt = m.CreatedAt
		}
	}
	s.convs[conversationID] = c
	return nil
}

// ListMessages implements Store.
func (s *MemoryStore) ListMessages(_ context.Context, userID, conversationID string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.owned(userID, conversationID); !ok {
		return nil, ErrNotFound
	}
	out := append([]Message(nil), s.msgs[conversationID]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
