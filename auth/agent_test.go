package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/ratelimit"
)

func TestAgentFromContext(t *testing.T) {
	e := newEnv(t)
	// Every verifier claims an agent; only the key credential may keep it.
	mk := func(sub string) auth.TokenVerifier {
		return func(_ *http.Request, tok string) (auth.User, bool, error) {
			return auth.User{Subject: sub, Agent: "bd"}, true, nil
		}
	}
	a, err := auth.New(auth.OIDCConfig{Issuer: e.idp.srv.URL, ClientID: "app", PublicURL: "https://app.example"}, testKey,
		auth.WithRateLimit(ratelimit.Options{Burst: 1000, Requests: 1000}),
		auth.WithAppName("app"), auth.WithKeyVerifier(mk("k")), auth.WithTokenVerifier(mk("p")))
	if err != nil {
		t.Fatal(err)
	}
	var user auth.User
	var agent string
	h := a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		user, _ = auth.FromContext(r.Context())
		agent = auth.AgentFromContext(r.Context())
	}))
	do := func(tok string) {
		user, agent = auth.User{}, ""
		r := httptest.NewRequest("GET", "/", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	do("pk_x")
	if user.Subject != "k" || user.Agent != "bd" || agent != "bd" {
		t.Fatalf("key: %+v %q", user, agent)
	}
	do("pat_x")
	if user.Subject != "p" || user.Agent != "" || agent != "" {
		t.Fatalf("token: %+v %q", user, agent)
	}
	do("")
	if user.Agent != "" || agent != "" {
		t.Fatalf("anonymous: %+v %q", user, agent)
	}
	// A context made by WithUser alone has no credential, so no agent.
	if got := auth.AgentFromContext(auth.WithUser(t.Context(), auth.User{Agent: "bd"})); got != "" {
		t.Fatalf("WithUser only: %q", got)
	}
}
