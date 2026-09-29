// Package health serves GET /healthz as a Huma operation that is hidden from
// MCP.
package health

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Pinger is the part of a database pool health needs. *pgxpool.Pool
// implements it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Body is the /healthz response.
type Body struct {
	Version       string  `json:"version" doc:"Application version."`
	Env           string  `json:"env" doc:"staging or production."`
	DB            string  `json:"db" enum:"ok,error" doc:"Database reachability."`
	UptimeSeconds float64 `json:"uptime_seconds" doc:"Seconds since the process started."`
}

// Output is the Huma output of the health operation.
type Output struct {
	Status int
	Body   Body
}

// Option customises Register.
type Option func(*config)

type config struct {
	env   string
	start time.Time
	now   func() time.Time
}

// WithEnv overrides the reported environment (default: the APP_ENV variable).
func WithEnv(env string) Option { return func(c *config) { c.env = env } }

// WithClock replaces the clock and start time, for tests.
func WithClock(start time.Time, now func() time.Time) Option {
	return func(c *config) { c.start, c.now = start, now }
}

// Register adds GET /healthz to api. db may be nil for apps without a
// database (db then reports ok). The response is 200 when everything is fine
// and 503, with the same body, when the database ping fails.
func Register(api huma.API, db Pinger, version string, opts ...Option) {
	cfg := &config{env: os.Getenv("APP_ENV"), start: time.Now(), now: time.Now}
	for _, o := range opts {
		o(cfg)
	}
	huma.Register(api, huma.Operation{
		OperationID: "get-health",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Health check",
		Description: "Reports version, environment, database reachability and uptime. Hidden from MCP.",
		Tags:        []string{"health"},
		Hidden:      true,
	}, func(ctx context.Context, _ *struct{}) (*Output, error) {
		out := &Output{Status: http.StatusOK, Body: Body{
			Version: version, Env: cfg.env, DB: "ok",
			UptimeSeconds: cfg.now().Sub(cfg.start).Seconds(),
		}}
		if db != nil && !isNil(db) {
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if err := db.Ping(pctx); err != nil {
				out.Body.DB = "error"
				out.Status = http.StatusServiceUnavailable
			}
		}
		return out, nil
	})
}
