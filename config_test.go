package playground_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	playground "github.com/teb-ooo/playground-go"
)

func env(overrides map[string]string, drop ...string) func(string) string {
	m := map[string]string{
		"APP_NAME": "hello", "APP_ENV": "staging", "DATABASE_URL": "postgres://u:p@db:5432/hello",
		"OIDC_ISSUER": "https://oidc.teb.ooo", "OIDC_CLIENT_ID": "hello", "OIDC_CLIENT_SECRET": "s3cret-value",
		"SESSION_KEY": "0123456789abcdef0123456789abcdef", "PUBLIC_URL": "https://hello-staging.teb.ooo/",
		"PLAYGROUND_DOMAIN": "teb.ooo",
	}
	for k, v := range overrides {
		m[k] = v
	}
	for _, k := range drop {
		delete(m, k)
	}
	return func(k string) string { return m[k] }
}

func TestFromEnvDefaults(t *testing.T) {
	c, err := playground.FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != "8080" || c.Addr() != ":8080" || c.AssistantModel != "claude-sonnet-5-5" || c.IsProduction() || c.AssistantEnabled() {
		t.Errorf("defaults wrong: %+v", c)
	}
	if c.PublicURL != "https://hello-staging.teb.ooo" || c.OIDC.PublicURL != c.PublicURL || c.OIDC.RedirectURL() != "https://hello-staging.teb.ooo/auth/callback" {
		t.Errorf("public url handling: %q / %q", c.PublicURL, c.OIDC.RedirectURL())
	}
	if len(c.SessionKey) != 32 || c.OIDC.Issuer != "https://oidc.teb.ooo" || c.Mail.AppName != "hello" || c.Mail.Provider != "" {
		t.Errorf("config = %+v", c)
	}
	if _, err := c.NewAuth(); err != nil {
		t.Errorf("NewAuth: %v", err)
	}
	if _, err := c.NewMailer(); err == nil {
		t.Error("NewMailer without provider should fail")
	}
}

func TestFromEnvValidation(t *testing.T) {
	tests := []struct {
		name    string
		over    map[string]string
		drop    []string
		wantErr string // substring; empty = ok
	}{
		{"valid", nil, nil, ""},
		{"valid production", map[string]string{"APP_ENV": "production", "PUBLIC_URL": "https://hello.teb.ooo"}, nil, ""},
		{"missing app name", nil, []string{"APP_NAME"}, "APP_NAME"},
		{"bad app name", map[string]string{"APP_NAME": "Hello_World"}, nil, "APP_NAME"},
		{"missing env", nil, []string{"APP_ENV"}, "APP_ENV"},
		{"bad env", map[string]string{"APP_ENV": "dev"}, nil, "APP_ENV"},
		{"bad port", map[string]string{"PORT": "http"}, nil, "PORT"},
		{"port out of range", map[string]string{"PORT": "70000"}, nil, "PORT"},
		{"custom port ok", map[string]string{"PORT": "9000"}, nil, ""},
		{"missing db", nil, []string{"DATABASE_URL"}, "DATABASE_URL"},
		{"non-postgres db", map[string]string{"DATABASE_URL": "mysql://x"}, nil, "DATABASE_URL"},
		{"missing issuer", nil, []string{"OIDC_ISSUER"}, "OIDC_ISSUER"},
		{"relative issuer", map[string]string{"OIDC_ISSUER": "oidc.teb.ooo"}, nil, "OIDC_ISSUER"},
		{"missing client id", nil, []string{"OIDC_CLIENT_ID"}, "OIDC_CLIENT_ID"},
		{"missing client secret", nil, []string{"OIDC_CLIENT_SECRET"}, "OIDC_CLIENT_SECRET"},
		{"missing session key", nil, []string{"SESSION_KEY"}, "SESSION_KEY"},
		{"short session key", map[string]string{"SESSION_KEY": "short"}, nil, "SESSION_KEY"},
		{"missing public url", nil, []string{"PUBLIC_URL"}, "PUBLIC_URL"},
		{"http public url in production", map[string]string{"APP_ENV": "production", "PUBLIC_URL": "http://hello.teb.ooo"}, nil, "https in production"},
		{"http public url in staging ok", map[string]string{"PUBLIC_URL": "http://localhost:8080"}, nil, ""},
		{"mail unknown provider", map[string]string{"MAIL_PROVIDER": "smtp", "MAIL_API_KEY": "k", "MAIL_STAGING_SINK": "a@b.co"}, nil, "MAIL_PROVIDER"},
		{"mail without key", map[string]string{"MAIL_PROVIDER": "resend", "MAIL_STAGING_SINK": "a@b.co"}, nil, "MAIL_API_KEY"},
		{"mail staging without sink", map[string]string{"MAIL_PROVIDER": "resend", "MAIL_API_KEY": "k"}, nil, "MAIL_STAGING_SINK"},
		{"mail production without sink ok", map[string]string{"APP_ENV": "production", "PUBLIC_URL": "https://h.teb.ooo", "MAIL_PROVIDER": "postmark", "MAIL_API_KEY": "k"}, nil, ""},
		{"mail without domain", map[string]string{"MAIL_PROVIDER": "resend", "MAIL_API_KEY": "k", "MAIL_STAGING_SINK": "a@b.co"}, []string{"PLAYGROUND_DOMAIN"}, "PLAYGROUND_DOMAIN"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := playground.FromEnv(env(tc.over, tc.drop...))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestAllProblemsReportedAtOnceWithoutSecrets(t *testing.T) {
	_, err := playground.FromEnv(env(map[string]string{"APP_ENV": "x", "PORT": "0", "OIDC_CLIENT_SECRET": "", "SESSION_KEY": "very-secret-but-wrong-length"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"APP_ENV", "PORT", "OIDC_CLIENT_SECRET", "SESSION_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "very-secret") {
		t.Error("error leaks a secret value")
	}
}

func TestMailAndAssistantWiring(t *testing.T) {
	c, err := playground.FromEnv(env(map[string]string{
		"MAIL_PROVIDER": "resend", "MAIL_API_KEY": "mk", "MAIL_STAGING_SINK": "sink@example.org",
		"ANTHROPIC_API_KEY": "ak", "ASSISTANT_MODEL": "claude-opus-5-5",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.NewMailer(); err != nil {
		t.Errorf("NewMailer: %v", err)
	}
	if !c.AssistantEnabled() {
		t.Error("assistant should be enabled")
	}
	o := c.AssistantOptions(nil)
	if o.Model != "claude-opus-5-5" || o.APIKey != "ak" || o.AppName != "hello" {
		t.Errorf("assistant options = %+v", o)
	}
}

func TestValidateOnHandBuiltConfig(t *testing.T) {
	c, _ := playground.FromEnv(env(nil))
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.SessionKey = []byte("short")
	if err := c.Validate(); err == nil {
		t.Error("expected error for short key")
	}
}

func TestLogValueRedactsSecrets(t *testing.T) {
	c, _ := playground.FromEnv(env(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-topsecret"}))
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", c)
	out := buf.String()
	for _, secret := range []string{"s3cret-value", "sk-ant-topsecret", "0123456789abcdef", "postgres://u:p@"} {
		if strings.Contains(out, secret) {
			t.Errorf("log leaks %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, `"app_name":"hello"`) || !strings.Contains(out, `"anthropic_api_key":"set"`) {
		t.Errorf("log = %s", out)
	}
}

func TestPlaygroundAssistantFlag(t *testing.T) {
	tests := []struct {
		val     string
		want    bool
		wantErr bool
	}{{"", false, false}, {"true", true, false}, {"TRUE", true, false}, {"false", false, false}, {"1", true, false}, {"yes", false, true}}
	for _, tc := range tests {
		t.Run(tc.val, func(t *testing.T) {
			c, err := playground.FromEnv(env(map[string]string{"PLAYGROUND_ASSISTANT": tc.val}))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err == nil {
				if c.Assistant != tc.want || c.SPA().Assistant != tc.want || c.SPA().AppName != "hello" || c.SPA().Env != "staging" {
					t.Errorf("config = %+v spa = %+v", c.Assistant, c.SPA())
				}
			}
		})
	}
}

func TestNewAuthAcceptsPlatformKeysByDefault(t *testing.T) {
	c, err := playground.FromEnv(env(map[string]string{"PLAYGROUND_DOMAIN": "keys.invalid"}))
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.NewAuth()
	if err != nil {
		t.Fatal(err)
	}
	// A well-formed pk_ token goes to the keys verifier, not the OIDC path: the issuer's
	// key endpoint cannot be reached here, which is a 503 (the OIDC path
	// would answer 401).
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer pk_eyJhbGciOiJFUzI1NiIsImtpZCI6IngifQ.e30.AAAA")
	w := httptest.NewRecorder()
	a.BearerOrSession(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", w.Code)
	}
	// Without a domain no verifier is installed.
	c, _ = playground.FromEnv(env(nil, "PLAYGROUND_DOMAIN"))
	if _, err := c.NewAuth(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPOptions(t *testing.T) {
	c, _ := playground.FromEnv(env(nil))
	a, _ := c.NewAuth()
	o := c.MCPOptions(a)
	if o.PublicURL != "https://hello-staging.teb.ooo" || o.App != "hello" || o.AuthorizationServer != "https://oidc.teb.ooo" || o.Auth == nil {
		t.Fatalf("%+v", o)
	}
}
