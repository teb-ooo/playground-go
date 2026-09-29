package mail

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	nmail "net/mail"
	"os"
	"strings"
	texttemplate "text/template"
	"time"
)

//go:embed templates/base.html.tmpl templates/base.txt.tmpl
var baseFS embed.FS

// Provider names.
const (
	ProviderResend   = "resend"
	ProviderPostmark = "postmark"
)

// Message is one outgoing email.
type Message struct {
	To      []string
	Subject string
	Text    string
	HTML    string
	ReplyTo string
}

// Config configures a Mailer.
type Config struct {
	// Provider is "resend" or "postmark" (MAIL_PROVIDER).
	Provider string
	// APIKey is the provider credential (MAIL_API_KEY).
	APIKey string
	// AppName is the sending app (APP_NAME); it names the default sender and
	// the staging subject prefix.
	AppName string
	// FactoryDomain is FACTORY_DOMAIN; the default sender is AppName@FactoryDomain.
	FactoryDomain string
	// From overrides the default sender address.
	From string
	// Env is APP_ENV. Anything other than "production" is treated as staging.
	Env string
	// StagingSink is the inbox every staging message is redirected to
	// (MAIL_STAGING_SINK). Required outside production.
	StagingSink string
	// FactoryName is shown in the layout header; default FactoryDomain.
	FactoryName string
	// Templates holds the app's own templates; may be nil.
	Templates fs.FS
	// BaseURL overrides the provider endpoint (tests).
	BaseURL string
	// HTTPClient overrides the client (default: 15s timeout).
	HTTPClient *http.Client
}

// ConfigFromEnv reads MAIL_PROVIDER, MAIL_API_KEY, MAIL_STAGING_SINK,
// APP_NAME, APP_ENV and FACTORY_DOMAIN.
func ConfigFromEnv() Config {
	return Config{
		Provider: os.Getenv("MAIL_PROVIDER"), APIKey: os.Getenv("MAIL_API_KEY"),
		StagingSink: os.Getenv("MAIL_STAGING_SINK"), AppName: os.Getenv("APP_NAME"),
		Env: os.Getenv("APP_ENV"), FactoryDomain: os.Getenv("FACTORY_DOMAIN"),
	}
}

// Mailer sends messages through one provider.
type Mailer struct {
	cfg  Config
	from string
	prov provider
	tpl  *Templates
}

// New validates cfg and returns a Mailer.
func New(cfg Config) (*Mailer, error) {
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.APIKey == "" {
		return nil, errors.New("mail: MAIL_API_KEY is required")
	}
	if cfg.AppName == "" {
		return nil, errors.New("mail: app name is required")
	}
	from := cfg.From
	if from == "" {
		if cfg.FactoryDomain == "" {
			return nil, errors.New("mail: FACTORY_DOMAIN (or an explicit From) is required")
		}
		from = cfg.AppName + "@" + cfg.FactoryDomain
	}
	if _, err := nmail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("mail: sender %q: %w", from, err)
	}
	if cfg.Env != "production" {
		if cfg.StagingSink == "" {
			return nil, errors.New("mail: MAIL_STAGING_SINK is required outside production")
		}
		if _, err := nmail.ParseAddress(cfg.StagingSink); err != nil {
			return nil, fmt.Errorf("mail: staging sink %q: %w", cfg.StagingSink, err)
		}
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	var prov provider
	switch cfg.Provider {
	case ProviderResend:
		prov = &resend{key: cfg.APIKey, base: orDefault(cfg.BaseURL, "https://api.resend.com"), client: client}
	case ProviderPostmark:
		prov = &postmark{key: cfg.APIKey, base: orDefault(cfg.BaseURL, "https://api.postmarkapp.com"), client: client}
	default:
		return nil, fmt.Errorf("mail: unknown MAIL_PROVIDER %q (want resend or postmark)", cfg.Provider)
	}
	if cfg.FactoryName == "" {
		cfg.FactoryName = cfg.FactoryDomain
	}
	tpl, err := NewTemplates(cfg.Templates, cfg.FactoryName)
	if err != nil {
		return nil, err
	}
	return &Mailer{cfg: cfg, from: from, prov: prov, tpl: tpl}, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return strings.TrimRight(d, "/")
	}
	return strings.TrimRight(s, "/")
}

// outgoing is the provider-neutral wire message after validation and staging
// rewriting.
type outgoing struct {
	From, Subject, Text, HTML, ReplyTo string
	To                                 []string
}

type provider interface {
	name() string
	send(ctx context.Context, m outgoing) error
}

// Send validates m, applies the staging rewrite, and sends it. From defaults
// to APP_NAME@FACTORY_DOMAIN. Bodies are never logged.
func (m *Mailer) Send(ctx context.Context, msg Message) error {
	out, err := m.prepare(msg)
	if err != nil {
		return err
	}
	if err := m.prov.send(ctx, out); err != nil {
		return fmt.Errorf("mail: sending via %s: %w", m.prov.name(), err)
	}
	slog.InfoContext(ctx, "mail sent", "provider", m.prov.name(), "recipients", len(out.To), "staging_rewrite", m.cfg.Env != "production")
	return nil
}

func (m *Mailer) prepare(msg Message) (outgoing, error) {
	if len(msg.To) == 0 {
		return outgoing{}, errors.New("mail: no recipients")
	}
	if strings.TrimSpace(msg.Subject) == "" {
		return outgoing{}, errors.New("mail: empty subject")
	}
	if msg.Text == "" && msg.HTML == "" {
		return outgoing{}, errors.New("mail: message has neither text nor HTML body")
	}
	for _, s := range append([]string{msg.Subject, msg.ReplyTo}, msg.To...) {
		if strings.ContainsAny(s, "\r\n") {
			return outgoing{}, errors.New("mail: line breaks are not allowed in headers")
		}
	}
	for _, a := range msg.To {
		if _, err := nmail.ParseAddress(a); err != nil {
			return outgoing{}, fmt.Errorf("mail: recipient %q: %w", a, err)
		}
	}
	if msg.ReplyTo != "" {
		if _, err := nmail.ParseAddress(msg.ReplyTo); err != nil {
			return outgoing{}, fmt.Errorf("mail: reply-to %q: %w", msg.ReplyTo, err)
		}
	}
	out := outgoing{From: m.from, To: append([]string(nil), msg.To...), Subject: msg.Subject,
		Text: msg.Text, HTML: msg.HTML, ReplyTo: msg.ReplyTo}
	if m.cfg.Env != "production" {
		out.To = []string{m.cfg.StagingSink}
		out.Subject = "[staging " + m.cfg.AppName + "] " + msg.Subject
	}
	return out, nil
}

// Template renders the app template name into a text and HTML pair. See the
// package documentation for the template contract.
func (m *Mailer) Template(name string, data Page) (text, html string, err error) {
	r, err := m.tpl.Render(name, data)
	return r.Text, r.HTML, err
}

// SendTemplate renders name with data and sends it to `to`, with data.Title
// as the subject unless subject is non-empty.
func (m *Mailer) SendTemplate(ctx context.Context, to []string, subject, name string, data Page) error {
	text, html, err := m.Template(name, data)
	if err != nil {
		return err
	}
	if subject == "" {
		subject = data.Title
	}
	return m.Send(ctx, Message{To: to, Subject: subject, Text: text, HTML: html})
}

// ---- templates ----

// Page is the root data for a template: the base layout's placeholders plus
// the app's own data under .Data.
type Page struct {
	Title       string
	Preheader   string
	FactoryName string // defaults to the Mailer's factory name
	Footer      string
	Data        any
}

// Rendered is a rendered text and HTML pair.
type Rendered struct{ Text, HTML string }

// Templates renders app templates on top of the embedded base layout.
type Templates struct {
	app         fs.FS
	factoryName string
}

// NewTemplates returns a renderer for app templates in appFS (may be nil, in
// which case only the base layout is available and every Render fails).
func NewTemplates(appFS fs.FS, factoryName string) (*Templates, error) {
	t := &Templates{app: appFS, factoryName: factoryName}
	// Fail early if the embedded base does not parse.
	if _, err := htmltemplate.New("base").ParseFS(baseFS, "templates/base.html.tmpl"); err != nil {
		return nil, fmt.Errorf("mail: parsing base HTML layout: %w", err)
	}
	if _, err := texttemplate.New("base").ParseFS(baseFS, "templates/base.txt.tmpl"); err != nil {
		return nil, fmt.Errorf("mail: parsing base text layout: %w", err)
	}
	return t, nil
}

// Render renders <name>.html.tmpl and <name>.txt.tmpl from the app FS, each on
// top of its base layout. Both must define "content".
func (t *Templates) Render(name string, data Page) (Rendered, error) {
	if t.app == nil {
		return Rendered{}, errors.New("mail: no application templates configured")
	}
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return Rendered{}, fmt.Errorf("mail: invalid template name %q", name)
	}
	if data.FactoryName == "" {
		data.FactoryName = t.factoryName
	}

	htmlSrc, err := fs.ReadFile(t.app, name+".html.tmpl")
	if err != nil {
		return Rendered{}, fmt.Errorf("mail: reading %s.html.tmpl: %w", name, err)
	}
	textSrc, err := fs.ReadFile(t.app, name+".txt.tmpl")
	if err != nil {
		return Rendered{}, fmt.Errorf("mail: reading %s.txt.tmpl: %w", name, err)
	}

	ht, err := htmltemplate.New("base.html.tmpl").Option("missingkey=error").ParseFS(baseFS, "templates/base.html.tmpl")
	if err != nil {
		return Rendered{}, fmt.Errorf("mail: parsing base HTML layout: %w", err)
	}
	if _, err := ht.New(name + ".html.tmpl").Parse(string(htmlSrc)); err != nil {
		return Rendered{}, fmt.Errorf("mail: parsing %s.html.tmpl: %w", name, err)
	}
	tt, err := texttemplate.New("base.txt.tmpl").Option("missingkey=error").ParseFS(baseFS, "templates/base.txt.tmpl")
	if err != nil {
		return Rendered{}, fmt.Errorf("mail: parsing base text layout: %w", err)
	}
	if _, err := tt.New(name + ".txt.tmpl").Parse(string(textSrc)); err != nil {
		return Rendered{}, fmt.Errorf("mail: parsing %s.txt.tmpl: %w", name, err)
	}

	var hb, tb bytes.Buffer
	if err := ht.ExecuteTemplate(&hb, "base.html.tmpl", data); err != nil {
		return Rendered{}, fmt.Errorf("mail: rendering %s HTML: %w", name, err)
	}
	if err := tt.ExecuteTemplate(&tb, "base.txt.tmpl", data); err != nil {
		return Rendered{}, fmt.Errorf("mail: rendering %s text: %w", name, err)
	}
	return Rendered{Text: tb.String(), HTML: hb.String()}, nil
}

// ---- providers ----

func doJSON(ctx context.Context, client *http.Client, url string, hdr map[string]string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response: %w", err)
	}
	return resp.StatusCode, rb, nil
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

type resend struct {
	key, base string
	client    *http.Client
}

func (*resend) name() string { return ProviderResend }

func (p *resend) send(ctx context.Context, m outgoing) error {
	payload := map[string]any{"from": m.From, "to": m.To, "subject": m.Subject}
	if m.Text != "" {
		payload["text"] = m.Text
	}
	if m.HTML != "" {
		payload["html"] = m.HTML
	}
	if m.ReplyTo != "" {
		payload["reply_to"] = m.ReplyTo
	}
	status, body, err := doJSON(ctx, p.client, p.base+"/emails", map[string]string{"Authorization": "Bearer " + p.key}, payload)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("status %d: %s", status, snippet(body))
	}
	return nil
}

type postmark struct {
	key, base string
	client    *http.Client
}

func (*postmark) name() string { return ProviderPostmark }

func (p *postmark) send(ctx context.Context, m outgoing) error {
	payload := map[string]any{"From": m.From, "To": strings.Join(m.To, ","), "Subject": m.Subject, "MessageStream": "outbound"}
	if m.Text != "" {
		payload["TextBody"] = m.Text
	}
	if m.HTML != "" {
		payload["HtmlBody"] = m.HTML
	}
	if m.ReplyTo != "" {
		payload["ReplyTo"] = m.ReplyTo
	}
	status, body, err := doJSON(ctx, p.client, p.base+"/email", map[string]string{"X-Postmark-Server-Token": p.key}, payload)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("status %d: %s", status, snippet(body))
	}
	var r struct {
		ErrorCode int
		Message   string
	}
	if json.Unmarshal(body, &r) == nil && r.ErrorCode != 0 {
		return fmt.Errorf("error code %d: %s", r.ErrorCode, r.Message)
	}
	return nil
}
