// Package log provides structured JSON logging to stdout with secret
// redaction, and HTTP request logging middleware.
//
// Redaction: the values of every environment variable whose name ends in
// _KEY, _SECRET, _TOKEN or _PASSWORD are collected when a logger is created,
// and any occurrence of such a value in a log message or attribute is
// replaced with [REDACTED]. Values shorter than MinSecretLen are ignored so
// that a flag like FOO_TOKEN=1 cannot blank out ordinary text.
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
)

// MinSecretLen is the shortest environment value treated as a secret.
const MinSecretLen = 6

// Redacted replaces secret values in log output.
const Redacted = "[REDACTED]"

var secretSuffixes = []string{"_KEY", "_SECRET", "_TOKEN", "_PASSWORD"}

// SecretsFromEnv returns the values of the secret-looking environment
// variables in environ (as returned by os.Environ), longest first so that a
// secret containing another is replaced whole.
func SecretsFromEnv(environ []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, kv := range environ {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || len(val) < MinSecretLen {
			continue
		}
		for _, suf := range secretSuffixes {
			if strings.HasSuffix(name, suf) {
				if !seen[val] {
					seen[val] = true
					out = append(out, val)
				}
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// New returns a JSON logger writing to w (os.Stdout if nil) that redacts the
// current environment's secrets.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return NewWithSecrets(w, level, SecretsFromEnv(os.Environ()))
}

// NewWithSecrets is New with an explicit secret list.
func NewWithSecrets(w io.Writer, level slog.Leveler, secrets []string) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	if level == nil {
		level = slog.LevelInfo
	}
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(&redactor{next: base, secrets: secrets})
}

// LevelFromEnv returns the level named by the environment variable name (debug, info, warn or error, any case; "warning" is
// accepted too), or def when it is unset or not a level. An app reads LOG_LEVEL with it: `log.Setup(log.LevelFromEnv("LOG_LEVEL",
// slog.LevelInfo))`.
func LevelFromEnv(name string, def slog.Level) slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return def
}

// Setup installs New(os.Stdout, level) as the default slog logger and returns it.
func Setup(level slog.Leveler) *slog.Logger {
	l := New(os.Stdout, level)
	slog.SetDefault(l)
	return l
}

type redactor struct {
	next    slog.Handler
	secrets []string
}

func (r *redactor) scrub(s string) string {
	for _, sec := range r.secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	return s
}

func (r *redactor) attr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(r.scrub(a.Value.String()))
	case slog.KindGroup:
		g := a.Value.Group()
		out := make([]slog.Attr, len(g))
		for i, ga := range g {
			out[i] = r.attr(ga)
		}
		a.Value = slog.GroupValue(out...)
	case slog.KindAny:
		// Errors, Stringers and arbitrary structs: render, and only replace the
		// value if it actually contains a secret so typed values stay typed.
		rendered := fmt.Sprintf("%+v", a.Value.Any())
		if scrubbed := r.scrub(rendered); scrubbed != rendered {
			a.Value = slog.StringValue(scrubbed)
		}
	}
	return a
}

func (r *redactor) Enabled(ctx context.Context, l slog.Level) bool { return r.next.Enabled(ctx, l) }

func (r *redactor) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, r.scrub(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(r.attr(a))
		return true
	})
	return r.next.Handle(ctx, out)
}

func (r *redactor) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = r.attr(a)
	}
	return &redactor{next: r.next.WithAttrs(scrubbed), secrets: r.secrets}
}

func (r *redactor) WithGroup(name string) slog.Handler {
	return &redactor{next: r.next.WithGroup(name), secrets: r.secrets}
}
