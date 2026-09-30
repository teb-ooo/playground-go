package testkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/testkit"
)

var key = []byte("0123456789abcdef0123456789abcdef")

func TestMintSession(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	u := auth.User{Subject: "id-1", Email: "a@b.co", Username: "ada", Groups: []string{"admin"}}
	c, err := testkit.MintSession(key, u)
	if err != nil {
		t.Fatal(err)
	}
	got, err := auth.DecodeSession(key, c.Value, time.Now())
	if err != nil || got.Subject != "id-1" || !got.IsAdmin() {
		t.Fatalf("decoded = %+v err=%v", got, err)
	}
	if testkit.CookieHeader(c) != auth.CookieName+"="+c.Value {
		t.Error("CookieHeader")
	}
	sc := testkit.SetCookieString(c)
	for _, want := range []string{auth.CookieName + "=" + c.Value, "HttpOnly", "Secure", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie %q missing %q", sc, want)
		}
	}
	if strings.Contains(sc, "Domain=") {
		t.Error("cookie must be host-only")
	}

	// The minted cookie is accepted by the real middleware.
	a, err := auth.New(auth.OIDCConfig{Issuer: "http://unused", ClientID: "c", PublicURL: "https://a.example"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var seen auth.User
	h := a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen, _ = auth.FromContext(r.Context()) }))
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen.Subject != "id-1" {
		t.Errorf("middleware saw %+v", seen)
	}
}

func TestProductionGuard(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	if _, err := testkit.MintSession(key, auth.User{Subject: "x"}); !errors.Is(err, testkit.ErrProduction) {
		t.Errorf("MintSession err = %v", err)
	}
	if _, err := testkit.MintKratosSession(context.Background(), "http://kratos:4434", "id"); !errors.Is(err, testkit.ErrProduction) {
		t.Errorf("MintKratosSession err = %v", err)
	}
	if _, err := testkit.MintKratosSessionCookie(context.Background(), "http://kratos:4434", "id"); !errors.Is(err, testkit.ErrProduction) {
		t.Errorf("MintKratosSessionCookie err = %v", err)
	}
	if !errors.Is(testkit.Guard(), testkit.ErrProduction) {
		t.Error("Guard")
	}
}

func TestMintSessionBadInput(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	tests := []struct {
		name string
		key  []byte
		user auth.User
	}{
		{"short key", []byte("short"), auth.User{Subject: "x"}},
		{"no subject", key, auth.User{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := testkit.MintSession(tc.key, tc.user); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// fakeKratos implements just the two calls MintKratosSession makes.
type fakeKratos struct {
	*httptest.Server
	continueWith   bool
	recoveryStatus int
	linkEnabled    bool
	adminBody      map[string]string
	adminPaths     []string
}

func newFakeKratos(t *testing.T) *fakeKratos {
	f := &fakeKratos{continueWith: true, recoveryStatus: 200, linkEnabled: true}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/recovery/link", func(w http.ResponseWriter, r *http.Request) {
		f.adminPaths = append(f.adminPaths, r.URL.Path)
		if !f.linkEnabled {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"message":"This endpoint was disabled by system administrator."}}`)
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"recovery_link": "https://id.teb.ooo/.k/self-service/recovery?flow=flow-2&token=tok-abc"})
	})
	mux.HandleFunc("GET /self-service/recovery", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("flow") != "flow-2" || r.URL.Query().Get("token") != "tok-abc" {
			w.WriteHeader(400)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "ory_kratos_session", Value: "LINKCOOKIE", Path: "/", HttpOnly: true})
		http.Redirect(w, r, "https://id.teb.ooo/profile?flow=x", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /admin/recovery/code", func(w http.ResponseWriter, r *http.Request) {
		f.adminPaths = append(f.adminPaths, r.URL.Path)
		json.NewDecoder(r.Body).Decode(&f.adminBody)
		if f.adminBody["identity_id"] == "missing" {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":404,"message":"not found"}}`)
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"recovery_link": "https://id.teb.ooo/invite?flow=flow-1", "recovery_code": "123456", "expires_at": time.Now().Add(5 * time.Minute)})
	})
	mux.HandleFunc("POST /self-service/recovery", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("flow") != "flow-1" {
			w.WriteHeader(404)
			return
		}
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/json") {
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			if b["method"] != "code" || b["code"] != "123456" {
				w.WriteHeader(400)
				return
			}
			if !f.continueWith {
				w.WriteHeader(422)
				io.WriteString(w, `{"error":{"id":"browser_location_change_required"}}`)
				return
			}
			w.WriteHeader(f.recoveryStatus)
			io.WriteString(w, `{"id":"flow-1","continue_with":[{"action":"show_settings_ui","flow":{"id":"s"}},{"action":"set_ory_session_token","ory_session_token":"ory_st_TOKEN"}]}`)
			return
		}
		r.ParseForm()
		if r.Form.Get("code") != "123456" {
			w.WriteHeader(400)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "ory_kratos_session", Value: "COOKIEVALUE", Domain: "id.teb.ooo", Path: "/", HttpOnly: true})
		http.Redirect(w, r, "https://id.teb.ooo/profile?flow=x", http.StatusSeeOther)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func TestMintKratosSession(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	tests := []struct {
		name         string
		identity     string
		continueWith bool
		status       int
		want         string
		wantErr      string
	}{
		{"ok", "id-1", true, 200, "ory_st_TOKEN", ""},
		{"unknown identity", "missing", true, 200, "", "status 404"},
		{"transitions disabled", "id-1", false, 200, "", "use_continue_with_transitions"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeKratos(t)
			f.continueWith = tc.continueWith
			tok, err := testkit.MintKratosSession(context.Background(), f.URL, tc.identity, testkit.WithKratosPublicURL(f.URL))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || tok != tc.want {
				t.Fatalf("tok=%q err=%v", tok, err)
			}
			if f.adminBody["flow_type"] != "api" || f.adminBody["identity_id"] != "id-1" {
				t.Errorf("admin request = %v", f.adminBody)
			}
		})
	}
}

func TestMintKratosSessionCookie(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	tests := []struct {
		name        string
		linkEnabled bool
		identity    string
		wantValue   string
		wantPaths   []string
		wantErr     string
	}{
		{"link strategy preferred", true, "id-1", "LINKCOOKIE", []string{"/admin/recovery/link"}, ""},
		{"falls back to code", false, "id-1", "COOKIEVALUE", []string{"/admin/recovery/link", "/admin/recovery/code"}, ""},
		{"unknown identity", false, "missing", "", nil, "status 404"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeKratos(t)
			f.linkEnabled = tc.linkEnabled
			c, err := testkit.MintKratosSessionCookie(context.Background(), f.URL, tc.identity, testkit.WithKratosPublicURL(f.URL))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Name != "ory_kratos_session" || c.Value != tc.wantValue {
				t.Errorf("cookie = %+v", c)
			}
			if strings.Join(f.adminPaths, ",") != strings.Join(tc.wantPaths, ",") {
				t.Errorf("admin calls = %v, want %v", f.adminPaths, tc.wantPaths)
			}
		})
	}
}

func TestPublicURLDerivedFromAdmin(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	t.Setenv("KRATOS_PUBLIC_URL", "")
	// Admin on :4434 that cannot be reached; error should name the derived public host only after the admin call fails,
	// so just check bad URLs are rejected up front.
	if _, err := testkit.MintKratosSession(context.Background(), "::not a url", "x"); err == nil {
		t.Fatal("expected error for a bad admin URL")
	}
}
