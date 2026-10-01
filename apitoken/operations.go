package apitoken

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/internal/uuidv7"
)

// Options configures Register.
type Options struct {
	// Now replaces time.Now, for tests.
	Now func() time.Time
}

// Tag is the OpenAPI tag of the operations.
const Tag = "tokens"

// CreateInput is the request of create-api-token.
type CreateInput struct {
	Body struct {
		Name          string `json:"name" minLength:"1" maxLength:"80" doc:"What the token is for, for example the client it is given to."`
		ExpiresInDays *int   `json:"expires_in_days,omitempty" minimum:"1" maximum:"3650" required:"false" doc:"Days until the token stops working. Omit for a token that does not expire."`
	}
}

// CreatedToken is the response of create-api-token. Token is shown only here.
type CreatedToken struct {
	ID        string     `json:"id" format:"uuid" doc:"Token id, used to revoke it."`
	Name      string     `json:"name" doc:"The name given at creation."`
	Token     string     `json:"token" doc:"The secret. Shown once, never again; only its hash is stored."`
	Prefix    string     `json:"prefix" doc:"First 8 characters of the token, to recognise it in a list."`
	CreatedAt time.Time  `json:"created_at" doc:"Creation time, RFC 3339 UTC."`
	ExpiresAt *time.Time `json:"expires_at,omitempty" doc:"When the token stops working; absent when it does not expire."`
}

// CreateOutput wraps CreatedToken with no-store caching.
type CreateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CreatedToken
}

// TokenInfo is one token in list-api-tokens. It never carries the secret.
type TokenInfo struct {
	ID         string     `json:"id" format:"uuid" doc:"Token id."`
	Name       string     `json:"name" doc:"The name given at creation."`
	Prefix     string     `json:"prefix" doc:"First 8 characters of the token."`
	CreatedAt  time.Time  `json:"created_at" doc:"Creation time, RFC 3339 UTC."`
	LastUsedAt *time.Time `json:"last_used_at,omitempty" doc:"Last use, accurate to about a minute; absent if never used."`
	ExpiresAt  *time.Time `json:"expires_at,omitempty" doc:"When the token stops working; absent when it does not expire."`
	RevokedAt  *time.Time `json:"revoked_at,omitempty" doc:"When the token was revoked; absent while it is not."`
}

// ListInput is the request of list-api-tokens.
type ListInput struct {
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"200" doc:"Page size."`
	Cursor string `query:"cursor" doc:"next_cursor of the previous page. Opaque."`
}

// ListOutput is the response of list-api-tokens.
type ListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []TokenInfo `json:"items" doc:"The caller's tokens, newest first, revoked ones included."`
		NextCursor string      `json:"next_cursor,omitempty" doc:"Pass as cursor for the next page; absent on the last page."`
	}
}

// RevokeInput is the request of revoke-api-token.
type RevokeInput struct {
	ID string `path:"id" format:"uuid" doc:"Token id."`
}

const (
	cursorPrefix = "t1:"
	// MaxName is the longest token name.
	MaxName = 80
)

// Register adds create-api-token (POST /api/tokens), list-api-tokens (GET
// /api/tokens) and revoke-api-token (DELETE /api/tokens/{id}) to api. They
// act on the signed-in user's own tokens and accept only a browser session
// (auth.RequireSession): a personal access token, or an OIDC bearer token,
// can neither mint nor list nor revoke tokens.
//
// The operations are Hidden: left out of the OpenAPI document and therefore
// not MCP tools (an AI client must never be offered token minting, and the
// MCP surface runs on tokens anyway), exactly like GET /auth/me. The app's own
// web client calls them by path. Register them before openapimcp.Handler;
// being hidden they do not take part in ParityCheck.
func Register(api huma.API, store Store, opts Options) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	session := []map[string][]string{{"session": {}}}

	huma.Register(api, huma.Operation{
		OperationID:   "create-api-token",
		Method:        http.MethodPost,
		Path:          "/api/tokens",
		Summary:       "Create a personal access token",
		Description:   "Creates a long-lived personal access token for the signed-in user, for external clients such as Claude Code or Claude Desktop that call /mcp or the API with 'Authorization: Bearer <token>'. The token is returned once, in this response only; it cannot be shown again. Needs a browser session; a token cannot create tokens.",
		Tags:          []string{Tag},
		DefaultStatus: http.StatusCreated,
		Hidden:        true,
		Security:      session,
	}, func(ctx context.Context, in *CreateInput) (*CreateOutput, error) {
		u, err := auth.RequireSession(ctx)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(in.Body.Name)
		if name == "" {
			return nil, huma.Error422UnprocessableEntity("name must not be blank")
		}
		tok, err := Generate()
		if err != nil {
			return nil, err
		}
		t := now().UTC().Truncate(time.Microsecond)
		rec := Record{ID: uuidv7.New(), UserID: u.Subject, UserEmail: u.Email, UserUsername: u.Username,
			Name: name, Hash: Hash(tok), Prefix: DisplayPrefix(tok), CreatedAt: t}
		if d := in.Body.ExpiresInDays; d != nil {
			e := t.Add(time.Duration(*d) * 24 * time.Hour)
			rec.ExpiresAt = &e
		}
		if err := store.Create(ctx, rec); err != nil {
			return nil, err
		}
		return &CreateOutput{CacheControl: "no-store", Body: CreatedToken{
			ID: rec.ID, Name: rec.Name, Token: tok, Prefix: rec.Prefix, CreatedAt: rec.CreatedAt, ExpiresAt: rec.ExpiresAt}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-api-tokens",
		Method:      http.MethodGet,
		Path:        "/api/tokens",
		Summary:     "List personal access tokens",
		Description: "Lists the signed-in user's personal access tokens, newest first, revoked ones included, with name, prefix, creation, last use, expiry and revocation. The secret is never returned. Needs a browser session.",
		Tags:        []string{Tag},
		Hidden:      true,
		Security:    session,
	}, func(ctx context.Context, in *ListInput) (*ListOutput, error) {
		u, err := auth.RequireSession(ctx)
		if err != nil {
			return nil, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		var after string
		if in.Cursor != "" {
			b, err := base64.RawURLEncoding.DecodeString(in.Cursor)
			if err != nil || !strings.HasPrefix(string(b), cursorPrefix) || !uuidv7.Valid(string(b)[len(cursorPrefix):]) {
				return nil, huma.Error400BadRequest("invalid cursor")
			}
			after = string(b)[len(cursorPrefix):]
		}
		recs, err := store.ListByUser(ctx, u.Subject, limit+1, after)
		if err != nil {
			return nil, err
		}
		out := &ListOutput{CacheControl: "no-store"}
		out.Body.Items = []TokenInfo{}
		if len(recs) > limit {
			recs = recs[:limit]
			out.Body.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + recs[limit-1].ID))
		}
		for _, r := range recs {
			out.Body.Items = append(out.Body.Items, TokenInfo{ID: r.ID, Name: r.Name, Prefix: r.Prefix, CreatedAt: r.CreatedAt,
				LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "revoke-api-token",
		Method:        http.MethodDelete,
		Path:          "/api/tokens/{id}",
		Summary:       "Revoke a personal access token",
		Description:   "Revokes one of the signed-in user's personal access tokens; it stops working at once. Revoking an already revoked token succeeds. Needs a browser session.",
		Tags:          []string{Tag},
		DefaultStatus: http.StatusNoContent,
		Hidden:        true,
		Security:      session,
	}, func(ctx context.Context, in *RevokeInput) (*struct{}, error) {
		u, err := auth.RequireSession(ctx)
		if err != nil {
			return nil, err
		}
		if err := store.Revoke(ctx, u.Subject, in.ID, now().UTC()); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, huma.Error404NotFound("no such token")
			}
			return nil, err
		}
		return nil, nil
	})
}
