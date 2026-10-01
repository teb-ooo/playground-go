package mail_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teb-ooo/playground-go/mail"
)

type captured struct {
	path   string
	header http.Header
	body   map[string]any
}

func fakeProvider(t *testing.T, status int, reply string) (*httptest.Server, *[]captured) {
	t.Helper()
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		got = append(got, captured{path: r.URL.Path, header: r.Header.Clone(), body: m})
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func base(srv *httptest.Server, provider, env string) mail.Config {
	return mail.Config{Provider: provider, APIKey: "key-123456", AppName: "hello", PlaygroundDomain: "teb.ooo",
		Env: env, StagingSink: "sink@example.org", BaseURL: srv.URL}
}

func TestResend(t *testing.T) {
	srv, got := fakeProvider(t, 200, `{"id":"abc"}`)
	m, err := mail.New(base(srv, "resend", "production"))
	if err != nil {
		t.Fatal(err)
	}
	err = m.Send(context.Background(), mail.Message{To: []string{"Ada <ada@teb.ooo>", "bob@teb.ooo"}, Subject: "Hi", Text: "t", HTML: "<p>h</p>", ReplyTo: "help@teb.ooo"})
	if err != nil {
		t.Fatal(err)
	}
	c := (*got)[0]
	if c.path != "/emails" || c.header.Get("Authorization") != "Bearer key-123456" {
		t.Errorf("path=%s auth=%s", c.path, c.header.Get("Authorization"))
	}
	if c.body["from"] != "hello@teb.ooo" || c.body["subject"] != "Hi" || c.body["text"] != "t" || c.body["html"] != "<p>h</p>" || c.body["reply_to"] != "help@teb.ooo" {
		t.Errorf("body = %v", c.body)
	}
	to := c.body["to"].([]any)
	if len(to) != 2 || to[0] != "Ada <ada@teb.ooo>" {
		t.Errorf("to = %v", to)
	}
}

func TestPostmark(t *testing.T) {
	srv, got := fakeProvider(t, 200, `{"ErrorCode":0,"Message":"OK"}`)
	m, err := mail.New(base(srv, "Postmark", "production"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(context.Background(), mail.Message{To: []string{"a@b.co", "c@d.co"}, Subject: "S", HTML: "<b>x</b>"}); err != nil {
		t.Fatal(err)
	}
	c := (*got)[0]
	if c.path != "/email" || c.header.Get("X-Postmark-Server-Token") != "key-123456" {
		t.Errorf("path=%s token=%s", c.path, c.header.Get("X-Postmark-Server-Token"))
	}
	if c.body["From"] != "hello@teb.ooo" || c.body["To"] != "a@b.co,c@d.co" || c.body["HtmlBody"] != "<b>x</b>" || c.body["TextBody"] != nil {
		t.Errorf("body = %v", c.body)
	}
}

func TestProviderErrors(t *testing.T) {
	tests := []struct {
		name, provider string
		status         int
		reply          string
		wantErr        string
	}{
		{"resend 422", "resend", 422, `{"message":"bad from"}`, "status 422"},
		{"resend 500", "resend", 500, `oops`, "status 500"},
		{"postmark 401", "postmark", 401, `{"ErrorCode":10,"Message":"bad token"}`, "status 401"},
		{"postmark error code in 200", "postmark", 200, `{"ErrorCode":406,"Message":"inactive recipient"}`, "error code 406"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeProvider(t, tc.status, tc.reply)
			m, _ := mail.New(base(srv, tc.provider, "production"))
			err := m.Send(context.Background(), mail.Message{To: []string{"a@b.co"}, Subject: "S", Text: "SECRET-BODY"})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "SECRET-BODY") {
				t.Error("error leaks the body")
			}
		})
	}
}

func TestStagingRewrite(t *testing.T) {
	for _, prov := range []string{"resend", "postmark"} {
		t.Run(prov, func(t *testing.T) {
			srv, got := fakeProvider(t, 200, `{"ErrorCode":0}`)
			m, err := mail.New(base(srv, prov, "staging"))
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Send(context.Background(), mail.Message{To: []string{"real@customer.com", "other@customer.com"}, Subject: "Invoice", Text: "body"}); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal((*got)[0].body)
			s := string(raw)
			if strings.Contains(s, "customer.com") {
				t.Errorf("real recipient reached the provider: %s", s)
			}
			if !strings.Contains(s, "sink@example.org") || !strings.Contains(s, "[staging hello] Invoice") {
				t.Errorf("payload = %s", s)
			}
		})
	}
}

func TestNonProductionEnvIsTreatedAsStaging(t *testing.T) {
	srv, got := fakeProvider(t, 200, `{}`)
	m, _ := mail.New(base(srv, "resend", ""))
	m.Send(context.Background(), mail.Message{To: []string{"real@customer.com"}, Subject: "x", Text: "y"})
	if strings.Contains(string((*got)[0].body["to"].([]any)[0].(string)), "customer") {
		t.Error("empty APP_ENV must rewrite recipients")
	}
}

func TestNewValidation(t *testing.T) {
	srv, _ := fakeProvider(t, 200, `{}`)
	ok := base(srv, "resend", "staging")
	tests := []struct {
		name   string
		mutate func(*mail.Config)
		ok     bool
	}{
		{"valid", func(*mail.Config) {}, true},
		{"unknown provider", func(c *mail.Config) { c.Provider = "sendgrid" }, false},
		{"no provider", func(c *mail.Config) { c.Provider = "" }, false},
		{"no key", func(c *mail.Config) { c.APIKey = "" }, false},
		{"no app", func(c *mail.Config) { c.AppName = "" }, false},
		{"no domain and no from", func(c *mail.Config) { c.PlaygroundDomain = "" }, false},
		{"explicit from", func(c *mail.Config) { c.PlaygroundDomain = ""; c.From = "Team <t@teb.ooo>" }, true},
		{"staging needs sink", func(c *mail.Config) { c.StagingSink = "" }, false},
		{"production does not need sink", func(c *mail.Config) { c.StagingSink = ""; c.Env = "production" }, true},
		{"bad sink", func(c *mail.Config) { c.StagingSink = "not an address" }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			tc.mutate(&c)
			_, err := mail.New(c)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestSendValidation(t *testing.T) {
	srv, got := fakeProvider(t, 200, `{}`)
	m, _ := mail.New(base(srv, "resend", "production"))
	tests := []struct {
		name string
		msg  mail.Message
	}{
		{"no recipients", mail.Message{Subject: "s", Text: "t"}},
		{"no subject", mail.Message{To: []string{"a@b.co"}, Text: "t"}},
		{"no body", mail.Message{To: []string{"a@b.co"}, Subject: "s"}},
		{"bad recipient", mail.Message{To: []string{"nope"}, Subject: "s", Text: "t"}},
		{"header injection in subject", mail.Message{To: []string{"a@b.co"}, Subject: "s\r\nBcc: x@y.z", Text: "t"}},
		{"header injection in recipient", mail.Message{To: []string{"a@b.co\r\nBcc: x@y.z"}, Subject: "s", Text: "t"}},
		{"bad reply-to", mail.Message{To: []string{"a@b.co"}, Subject: "s", Text: "t", ReplyTo: "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.Send(context.Background(), tc.msg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if len(*got) != 0 {
		t.Errorf("invalid messages reached the provider: %d", len(*got))
	}
}

func TestNeverLogsBodies(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	srv, _ := fakeProvider(t, 200, `{}`)
	m, _ := mail.New(base(srv, "resend", "production"))
	m.Send(context.Background(), mail.Message{To: []string{"a@b.co"}, Subject: "subj-XYZ", Text: "TOP-SECRET-TEXT", HTML: "<p>TOP-SECRET-HTML</p>"})
	out := buf.String()
	if out == "" {
		t.Fatal("expected a log line")
	}
	for _, s := range []string{"TOP-SECRET", "subj-XYZ", "a@b.co"} {
		if strings.Contains(out, s) {
			t.Errorf("log leaks %q: %s", s, out)
		}
	}
}
