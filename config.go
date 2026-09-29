// Package factory is the convenience entry point of factory-go: Config, loaded
// from the environment every factory container gets, with validation, so
// apps do not reimplement it. The library packages live beside it: openapimcp,
// auth, health, log, assistant, mail, spa and testkit.
package factory

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/teb-ooo/factory-go/assistant"
	"github.com/teb-ooo/factory-go/auth"
	"github.com/teb-ooo/factory-go/mail"
	"github.com/teb-ooo/factory-go/spa"
)

// Environments.
const (
	EnvStaging    = "staging"
	EnvProduction = "production"
)

// DefaultAssistantModel is the factory-wide model (BOOTSTRAP section 6b).
const DefaultAssistantModel = assistant.DefaultModel

// Config is an app's configuration. Load it with LoadConfig.
type Config struct {
	// Port is PORT (default 8080).
	Port string
	// DatabaseURL is DATABASE_URL.
	DatabaseURL string
	// OIDC is OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and PUBLIC_URL.
	OIDC auth.OIDCConfig
	// SessionKey is SESSION_KEY decoded to 32 bytes.
	SessionKey []byte
	// AppName is APP_NAME.
	AppName string
	// Env is APP_ENV: staging or production.
	Env string
	// PublicURL is PUBLIC_URL, the app's external origin.
	PublicURL string
	// FactoryDomain is FACTORY_DOMAIN, for example teb.ooo.
	FactoryDomain string
	// Mail is MAIL_PROVIDER, MAIL_API_KEY, MAIL_STAGING_SINK, MAIL_FROM plus
	// AppName, Env and FactoryDomain. Provider is empty when mail is not configured.
	Mail mail.Config
	// AssistantModel is ASSISTANT_MODEL (default claude-sonnet-5-5).
	AssistantModel string
	// AnthropicAPIKey is ANTHROPIC_API_KEY; empty means the assistant is off.
	AnthropicAPIKey string
	// Assistant is FACTORY_ASSISTANT (true|false, default false): whether the
	// frontend offers the assistant. Independent of AnthropicAPIKey.
	Assistant bool
	// Version is the build version. Not read from the environment: set it from
	// an -ldflags variable.
	Version string
}

// LoadConfig reads the process environment and validates it.
func LoadConfig() (Config, error) { return FromEnv(os.Getenv) }

// FromEnv is LoadConfig with an explicit lookup function (for tests).
func FromEnv(getenv func(string) string) (Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	c := Config{
		Port:            get("PORT"),
		DatabaseURL:     get("DATABASE_URL"),
		AppName:         get("APP_NAME"),
		Env:             get("APP_ENV"),
		PublicURL:       strings.TrimRight(get("PUBLIC_URL"), "/"),
		FactoryDomain:   get("FACTORY_DOMAIN"),
		AssistantModel:  get("ASSISTANT_MODEL"),
		AnthropicAPIKey: get("ANTHROPIC_API_KEY"),
	}
	if c.Port == "" {
		c.Port = "8080"
	}
	if c.AssistantModel == "" {
		c.AssistantModel = DefaultAssistantModel
	}
	c.OIDC = auth.OIDCConfig{
		Issuer: get("OIDC_ISSUER"), ClientID: get("OIDC_CLIENT_ID"), ClientSecret: get("OIDC_CLIENT_SECRET"),
		PublicURL: c.PublicURL,
	}
	c.Mail = mail.Config{
		Provider: get("MAIL_PROVIDER"), APIKey: get("MAIL_API_KEY"), StagingSink: get("MAIL_STAGING_SINK"),
		From: get("MAIL_FROM"), AppName: c.AppName, Env: c.Env, FactoryDomain: c.FactoryDomain,
	}

	var errs []error
	if raw := strings.ToLower(get("FACTORY_ASSISTANT")); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, errors.New("FACTORY_ASSISTANT must be true or false"))
		}
		c.Assistant = b
	}
	if raw := get("SESSION_KEY"); raw == "" {
		errs = append(errs, errors.New("SESSION_KEY is required"))
	} else if k, err := auth.ParseKey(raw); err != nil {
		errs = append(errs, errors.New("SESSION_KEY must be 32 bytes (raw, 64 hex characters, or base64)"))
	} else {
		c.SessionKey = k
	}
	if err := c.validate(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("factory: invalid configuration: %w", errors.Join(errs...))
	}
	return c, nil
}

var appNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Validate checks the configuration. Errors name the variable at fault and
// never include secret values.
func (c Config) Validate() error {
	var errs []error
	if len(c.SessionKey) != 32 {
		errs = append(errs, errors.New("SESSION_KEY must decode to 32 bytes"))
	}
	if err := c.validate(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("factory: invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}

func (c Config) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !appNameRE.MatchString(c.AppName) {
		bad("APP_NAME is required and must be lowercase letters, digits and hyphens")
	}
	if c.Env != EnvStaging && c.Env != EnvProduction {
		bad("APP_ENV must be %q or %q", EnvStaging, EnvProduction)
	}
	if n, err := strconv.Atoi(c.Port); err != nil || n < 1 || n > 65535 {
		bad("PORT must be a number between 1 and 65535")
	}
	if c.DatabaseURL == "" {
		bad("DATABASE_URL is required")
	} else if u, err := url.Parse(c.DatabaseURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		bad("DATABASE_URL must be a postgres:// URL")
	}
	if err := checkURL("OIDC_ISSUER", c.OIDC.Issuer, c.Env == EnvProduction); err != nil {
		errs = append(errs, err)
	}
	if err := checkURL("PUBLIC_URL", c.PublicURL, c.Env == EnvProduction); err != nil {
		errs = append(errs, err)
	}
	if c.OIDC.ClientID == "" {
		bad("OIDC_CLIENT_ID is required")
	}
	if c.OIDC.ClientSecret == "" {
		bad("OIDC_CLIENT_SECRET is required")
	}

	if c.Mail.Provider != "" {
		switch strings.ToLower(c.Mail.Provider) {
		case mail.ProviderResend, mail.ProviderPostmark:
		default:
			bad("MAIL_PROVIDER must be %q or %q", mail.ProviderResend, mail.ProviderPostmark)
		}
		if c.Mail.APIKey == "" {
			bad("MAIL_API_KEY is required when MAIL_PROVIDER is set")
		}
		if c.FactoryDomain == "" && c.Mail.From == "" {
			bad("FACTORY_DOMAIN (or MAIL_FROM) is required when MAIL_PROVIDER is set")
		}
		if c.Env == EnvStaging && c.Mail.StagingSink == "" {
			bad("MAIL_STAGING_SINK is required on staging when MAIL_PROVIDER is set")
		}
	}
	return errors.Join(errs...)
}

func checkURL(name, raw string, requireHTTPS bool) error {
	if raw == "" {
		return fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%s must be an absolute http(s) URL", name)
	}
	if requireHTTPS && u.Scheme != "https" {
		return fmt.Errorf("%s must be https in production", name)
	}
	return nil
}

// IsProduction reports whether APP_ENV is production.
func (c Config) IsProduction() bool { return c.Env == EnvProduction }

// Addr is the listen address, ":" plus Port.
func (c Config) Addr() string { return ":" + c.Port }

// AssistantEnabled reports whether an Anthropic API key is configured.
func (c Config) AssistantEnabled() bool { return c.AnthropicAPIKey != "" }

// SPA returns the spa.Config for this app: name, environment and the assistant
// flag. Locale and timezone come from FACTORY_LOCALE and FACTORY_TIMEZONE.
func (c Config) SPA() spa.Config {
	return spa.Config{AppName: c.AppName, Env: c.Env, Assistant: c.Assistant}
}

// NewAuth builds the auth package's Auth from the config.
func (c Config) NewAuth(opts ...auth.Option) (*auth.Auth, error) {
	return auth.New(c.OIDC, c.SessionKey, opts...)
}

// NewMailer builds the mailer, with app templates from templates (may be nil).
// It returns an error if mail is not configured.
func (c Config) NewMailer(templates fs.FS) (*mail.Mailer, error) {
	if c.Mail.Provider == "" {
		return nil, errors.New("factory: mail is not configured (MAIL_PROVIDER is empty)")
	}
	m := c.Mail
	m.Templates = templates
	return mail.New(m)
}

// AssistantOptions returns assistant options from the config, using store.
func (c Config) AssistantOptions(store assistant.Store) assistant.Options {
	return assistant.Options{AppName: c.AppName, Model: c.AssistantModel, APIKey: c.AnthropicAPIKey, Store: store}
}

// LogValue implements slog.LogValuer: secrets are reduced to whether they are set.
func (c Config) LogValue() slog.Value {
	set := func(s string) string {
		if s == "" {
			return "unset"
		}
		return "set"
	}
	return slog.GroupValue(
		slog.String("app_name", c.AppName), slog.String("env", c.Env), slog.String("port", c.Port),
		slog.String("public_url", c.PublicURL), slog.String("oidc_issuer", c.OIDC.Issuer),
		slog.String("oidc_client_id", c.OIDC.ClientID), slog.String("oidc_client_secret", set(c.OIDC.ClientSecret)),
		slog.String("session_key", set(string(c.SessionKey))), slog.String("database_url", set(c.DatabaseURL)),
		slog.String("mail_provider", c.Mail.Provider), slog.String("anthropic_api_key", set(c.AnthropicAPIKey)),
		slog.String("assistant_model", c.AssistantModel), slog.String("version", c.Version),
	)
}
