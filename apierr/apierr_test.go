package apierr_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teb-ooo/playground-go/apierr"
	playgroundlog "github.com/teb-ooo/playground-go/log"
)

func setup(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	apierr.Install()
	apierr.Install() // idempotent
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	reg := func(id, path string, h func(context.Context, *struct{}) (*struct{}, error)) {
		huma.Register(api, huma.Operation{OperationID: id, Method: "GET", Path: path}, h)
	}
	reg("wrapped-sql", "/sql", func(context.Context, *struct{}) (*struct{}, error) {
		return nil, fmt.Errorf("list items: %w", fmt.Errorf(`ERROR: relation "items" does not exist (SQLSTATE 42P01) SELECT * FROM items`))
	})
	reg("pg", "/pg", func(context.Context, *struct{}) (*struct{}, error) {
		return nil, fmt.Errorf("insert: %w", &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint items_pkey", Detail: "Key (id)=(secret-value)"})
	})
	reg("explicit-500", "/e500", func(context.Context, *struct{}) (*struct{}, error) {
		return nil, huma.Error500InternalServerError("select failed: password authentication failed", fmt.Errorf("dial tcp 10.0.0.5"))
	})
	reg("client-404", "/e404", func(context.Context, *struct{}) (*struct{}, error) {
		return nil, huma.Error404NotFound("no such item")
	})
	return playgroundlog.Middleware(mux), &buf
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("X-Request-Id", "req-123")
	h.ServeHTTP(rec, req)
	return rec
}

func TestFiveXXBodyIsGenericAndCauseIsLogged(t *testing.T) {
	h, logs := setup(t)
	for _, c := range []struct{ path, secret string }{
		{"/sql", "SELECT * FROM items"},
		{"/pg", "items_pkey"},
		{"/e500", "password authentication failed"},
		{"/e500", "10.0.0.5"},
	} {
		rec := get(h, c.path)
		if rec.Code != 500 {
			t.Fatalf("%s: status %d", c.path, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, c.secret) || strings.Contains(body, "SQLSTATE") || strings.Contains(body, "items") {
			t.Errorf("%s: body leaks %q: %s", c.path, c.secret, body)
		}
		if !strings.Contains(body, `"detail":"internal error"`) || !strings.Contains(body, `"title":"Internal Server Error"`) {
			t.Errorf("%s: body is not the generic problem: %s", c.path, body)
		}
		if strings.Contains(body, `"errors"`) {
			t.Errorf("%s: body has an errors list: %s", c.path, body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "problem+json") {
			t.Errorf("%s: content type %q", c.path, ct)
		}
		if !strings.Contains(logs.String(), c.secret) {
			t.Errorf("%s: log lacks %q:\n%s", c.path, c.secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "request_id=req-123") {
		t.Errorf("cause log lacks the request id:\n%s", logs.String())
	}
}

func TestFourXXKeepsMessage(t *testing.T) {
	h, _ := setup(t)
	rec := get(h, "/e404")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "no such item") {
		t.Fatalf("404 changed: %d %s", rec.Code, rec.Body.String())
	}
}

// A validation answer keeps its message and location but never echoes the value that failed (user content must not travel back).
func TestValidationAnswerDoesNotEchoTheValue(t *testing.T) {
	apierr.Install()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	type in struct {
		Body struct {
			Title string `json:"title" maxLength:"5"`
		}
	}
	huma.Register(api, huma.Operation{OperationID: "make", Method: "POST", Path: "/make"}, func(context.Context, *in) (*struct{}, error) { return nil, nil })
	const secret = "a-very-private-note-title"
	req := httptest.NewRequest("POST", "/make", strings.NewReader(`{"title":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != 422 || strings.Contains(body, secret) || !strings.Contains(body, "body.title") || !strings.Contains(body, "expected length") {
		t.Fatalf("%d %s", w.Code, body)
	}
}
