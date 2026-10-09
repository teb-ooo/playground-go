package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/ratelimit"
	"github.com/teb-ooo/playground-go/testkit"
)

func newAuthWith(t *testing.T, e *env, v auth.TokenVerifier) *auth.Auth {
	t.Helper()
	a, err := auth.New(auth.OIDCConfig{Issuer: e.idp.srv.URL, ClientID: "app", PublicURL: "https://app.example"}, testKey,
		auth.WithRateLimit(ratelimit.Options{Burst: 1000, Requests: 1000}), auth.WithAppName("app"), auth.WithKeyVerifier(v))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

type seen struct {
	user auth.User
	cred auth.Credential
	ok   bool
	hit  bool
}

func probe(h func(http.Handler) http.Handler) (http.Handler, *seen) {
	s := &seen{}
	return h(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		s.hit = true
		s.user, s.ok = auth.FromContext(r.Context())
		s.cred = auth.CredentialFromContext(r.Context())
	})), s
}

func TestTokenVerifier(t *testing.T) {
	e := newEnv(t)
	var calls []string
	v := func(r *http.Request, tok string) (auth.User, bool, error) {
		calls = append(calls, tok)
		switch tok {
		case "pk_good":
			return auth.User{Subject: "u-pat", Email: "p@x", Username: "pat"}, true, nil
		case "pk_limited":
			return auth.User{}, false, &auth.RateLimitedError{RetryAfter: 1500 * time.Millisecond}
		case "pk_broken":
			return auth.User{}, false, errors.New("db down")
		}
		return auth.User{}, false, nil
	}
	a := newAuthWith(t, e, v)

	tests := []struct {
		name     string
		tok      string
		wantCode int // BearerOrSession; 0 = passed through
		wantRA   string
	}{
		{"valid", "pk_good", 0, ""},
		{"unknown", "pk_nope", 401, ""},
		{"rate limited", "pk_limited", 429, "2"},
		{"backend failure", "pk_broken", 503, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, s := probe(a.BearerOrSession)
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/x", nil)
			r.Header.Set("Authorization", "Bearer "+tc.tok)
			h.ServeHTTP(w, r)
			if tc.wantCode == 0 {
				if !s.hit || !s.ok || s.user.Subject != "u-pat" || s.cred != auth.CredentialKey || s.user.IsAdmin() {
					t.Fatalf("seen = %+v", s)
				}
				return
			}
			if s.hit || w.Code != tc.wantCode || w.Header().Get("Retry-After") != tc.wantRA {
				t.Fatalf("code=%d hit=%v hdr=%v", w.Code, s.hit, w.Header())
			}
			if tc.wantCode == 401 && w.Header().Get("WWW-Authenticate") == "" {
				t.Error("no WWW-Authenticate on 401")
			}
			if b := w.Body.String(); strings.Contains(b, "db down") || strings.Contains(b, tc.tok) {
				t.Errorf("body leaks: %s", b)
			}
		})
	}

	t.Run("middleware stays anonymous on failure", func(t *testing.T) {
		h, s := probe(a.Middleware)
		r := httptest.NewRequest("GET", "/x", nil)
		r.Header.Set("Authorization", "Bearer pk_nope")
		h.ServeHTTP(httptest.NewRecorder(), r)
		if !s.hit || s.ok {
			t.Fatalf("seen = %+v", s)
		}
	})

	t.Run("OIDC bearer is unchanged and never reaches the verifier", func(t *testing.T) {
		calls = nil
		h, s := probe(a.BearerOrSession)
		r := httptest.NewRequest("GET", "/x", nil)
		r.Header.Set("Authorization", "Bearer "+e.idp.accessTokenFor("user-1", []string{"openid", "app:read"}, nil))
		h.ServeHTTP(httptest.NewRecorder(), r)
		if !s.ok || s.user.Subject != "user-1" || !s.user.IsAdmin() || s.cred != auth.CredentialBearer || len(calls) != 0 {
			t.Fatalf("seen = %+v calls=%v", s, calls)
		}
	})

	t.Run("a bad token does not beat a valid session cookie", func(t *testing.T) {
		c, err := testkit.MintSession(testKey, auth.User{Subject: "cookie-user"})
		if err != nil {
			t.Fatal(err)
		}
		h, s := probe(a.BearerOrSession)
		r := httptest.NewRequest("GET", "/x", nil)
		r.AddCookie(c)
		r.Header.Set("Authorization", "Bearer pk_nope")
		h.ServeHTTP(httptest.NewRecorder(), r)
		if !s.ok || s.user.Subject != "cookie-user" || s.cred != auth.CredentialSession {
			t.Fatalf("seen = %+v", s)
		}
	})

	t.Run("without a verifier a pk_ token is an invalid bearer", func(t *testing.T) {
		h, s := probe(e.auth.BearerOrSession)
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/x", nil)
		r.Header.Set("Authorization", "Bearer pk_good")
		h.ServeHTTP(w, r)
		if s.hit || w.Code != 401 {
			t.Fatalf("code=%d hit=%v", w.Code, s.hit)
		}
	})
}

func TestRequireSession(t *testing.T) {
	e := newEnv(t)
	a := newAuthWith(t, e, func(*http.Request, string) (auth.User, bool, error) {
		return auth.User{Subject: "u"}, true, nil
	})
	cookie, _ := testkit.MintSession(testKey, auth.User{Subject: "c"})
	for _, tc := range []struct {
		name string
		hdr  string
		ck   bool
		want int
	}{
		{"cookie", "", true, 200},
		{"key", "Bearer pk_x", false, 403},
		{"oidc bearer", "Bearer " + e.idp.accessTokenFor("user-1", []string{"openid", "app:read"}, nil), false, 403},
		{"nobody", "", false, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := a.BearerOrSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := auth.RequireSession(r.Context()); err != nil {
					type st interface{ GetStatus() int }
					w.WriteHeader(err.(st).GetStatus())
				}
			}))
			r := httptest.NewRequest("GET", "/x", nil)
			if tc.hdr != "" {
				r.Header.Set("Authorization", tc.hdr)
			}
			if tc.ck {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("code = %d, want %d", w.Code, tc.want)
			}
		})
	}
}
