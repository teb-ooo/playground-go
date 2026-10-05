// Package page is the playground's one way to paginate a list operation (docs/go-api.md, rule API-jvg): the standard query
// parameters, the standard response body, and an opaque keyset cursor.
//
// An operation takes Params in its input, fetches limit+1 rows after the cursor's key (keyset paging, never OFFSET), and answers
// Body[T]:
//
//	type listIn struct {
//		page.Params
//		Search string `query:"search"`
//	}
//	type listOut struct{ Body page.Body[Item] }
//
//	func list(ctx context.Context, in *listIn) (*listOut, error) {
//		var after struct{ ID string `json:"id"` }
//		hasCursor, err := page.Decode(in.Cursor, &after) // a malformed cursor is a 400
//		if err != nil { return nil, err }
//		rows := query(ctx, after.ID, hasCursor, in.Limit+1) // ORDER BY the key plus the unique id
//		items, next := page.Trim(rows, in.Limit, func(r Item) any { return struct{ ID string `json:"id"` }{r.ID} })
//		return &listOut{Body: page.Body[Item]{Items: items, NextCursor: next}}, nil
//	}
//
// openapimcp.ParityCheck enforces the shape: a GET whose 200 body has an `items` array needs the `limit` and `cursor` query
// parameters (limit an integer with minimum, maximum and default) and a `next_cursor` property.
package page

import (
	"encoding/base64"
	"encoding/json"

	"github.com/danielgtaylor/huma/v2"
)

// Default and maximum page sizes of Params. An operation that needs other bounds declares its own limit and cursor parameters
// with `minimum`, `maximum` and `default` tags; ParityCheck accepts any bounds.
const (
	DefaultLimit = 50
	MaxLimit     = 100
)

// Params are the standard list query parameters; embed it in an operation's input struct.
type Params struct {
	Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50" doc:"Page size, 1 to 100."`
	Cursor string `query:"cursor" doc:"Opaque cursor: the next_cursor of the previous page. Omit for the first page."`
}

// Body is the standard list response: the page's items and the cursor of the next page.
type Body[T any] struct {
	Items      []T     `json:"items" doc:"One page of results."`
	NextCursor *string `json:"next_cursor" doc:"Pass as cursor to get the next page; null on the last page."`
}

// Encode turns a keyset (any JSON-encodable value that identifies where a page ended) into an opaque cursor. Clients never parse it.
func Encode(key any) (string, error) {
	b, err := json.Marshal(key)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Decode reads a cursor into key. An empty cursor is the first page: it returns false and leaves key alone. A malformed cursor is
// a 400 "invalid cursor" (huma error), as the rules require.
func Decode(cursor string, key any) (bool, error) {
	if cursor == "" {
		return false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return false, huma.Error400BadRequest("invalid cursor")
	}
	if err := json.Unmarshal(raw, key); err != nil {
		return false, huma.Error400BadRequest("invalid cursor")
	}
	return true, nil
}

// Trim applies the fetch-limit-plus-one pattern: rows were read with LIMIT limit+1; when there is an extra row the page is the first
// limit rows and next is the cursor of the last row's key, otherwise next is nil (the last page). A non-positive limit uses DefaultLimit.
// The key function returns what Decode reads back.
func Trim[T any](rows []T, limit int, key func(T) any) (items []T, next *string) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if len(rows) <= limit {
		if rows == nil {
			rows = []T{}
		}
		return rows, nil
	}
	items = rows[:limit]
	if c, err := Encode(key(items[limit-1])); err == nil {
		next = &c
	}
	return items, next
}
