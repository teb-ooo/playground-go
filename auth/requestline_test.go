package auth_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/teb-ooo/playground-go/auth"
	playgroundlog "github.com/teb-ooo/playground-go/log"
)

// The request line names who called (user, credential, surface) once the auth middleware identified the caller.
func TestRequestLineNamesTheCaller(t *testing.T) {
	e := newEnv(t)
	a := newAuthWith(t, e, func(r *http.Request, tok string) (auth.User, bool, error) {
		return auth.User{Subject: "u-key", Email: "k@x", Username: "k"}, tok == "pk_good", nil
	})
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })
	h := playgroundlog.Middleware(a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	req := httptest.NewRequest("POST", "/api/things", nil)
	req.Header.Set("Authorization", "Bearer pk_good")
	h.ServeHTTP(httptest.NewRecorder(), req)
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("one JSON line expected: %q %v", buf.String(), err)
	}
	if line["user"] != "u-key" || line["credential"] != "key" || line["surface"] != "key" {
		t.Fatalf("the request line must name the caller: %v", line)
	}
}
