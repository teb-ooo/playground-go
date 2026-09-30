package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// KratosOption customises the Kratos helpers.
type KratosOption func(*kratosConfig)

type kratosConfig struct {
	publicURL string
	client    *http.Client
}

// WithKratosPublicURL sets the base URL of Kratos's public API. Default: the
// KRATOS_PUBLIC_URL environment variable, else the admin URL with port 4434
// replaced by 4433 (the playground's compose layout).
func WithKratosPublicURL(u string) KratosOption { return func(c *kratosConfig) { c.publicURL = u } }

// WithKratosHTTPClient sets the HTTP client.
func WithKratosHTTPClient(cl *http.Client) KratosOption {
	return func(c *kratosConfig) { c.client = cl }
}

func newKratosConfig(adminURL string, opts []KratosOption) (*kratosConfig, error) {
	if err := Guard(); err != nil {
		return nil, err
	}
	c := &kratosConfig{}
	for _, o := range opts {
		o(c)
	}
	if c.client == nil {
		c.client = &http.Client{Timeout: 15 * time.Second}
	}
	if c.publicURL == "" {
		c.publicURL = os.Getenv("KRATOS_PUBLIC_URL")
	}
	if c.publicURL == "" {
		u, err := url.Parse(adminURL)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("testkit: kratos admin URL %q is not a URL", adminURL)
		}
		if u.Port() == "4434" {
			u.Host = strings.TrimSuffix(u.Host, ":4434") + ":4433"
		}
		c.publicURL = u.String()
	}
	c.publicURL = strings.TrimRight(c.publicURL, "/")
	return c, nil
}

// MintKratosSession creates a Kratos session token for the identity and
// returns it, for use as the X-Session-Token header (or Authorization: Bearer)
// against Kratos's public API.
//
// Kratos v26.2.0 has no "create session" admin endpoint (its admin API can
// only list and delete sessions), so this uses the supported equivalent: the
// admin recovery-code endpoint (POST /admin/recovery/code with flow_type
// "api") followed by submitting that code to the public recovery flow, which
// issues a session and returns its token in continue_with. That response is
// only produced when Kratos runs with the code recovery strategy enabled
// (selfservice.methods.code.enabled: true, and the admin recovery-code
// endpoint) and feature_flags.use_continue_with_transitions: true; otherwise
// an error says which is missing. The link strategy cannot issue a token.
// See MintKratosSessionCookie for a browser cookie, which needs neither.
func MintKratosSession(ctx context.Context, kratosAdminURL, identityID string, opts ...KratosOption) (sessionToken string, err error) {
	cfg, err := newKratosConfig(kratosAdminURL, opts)
	if err != nil {
		return "", err
	}
	flowID, code, err := createRecovery(ctx, cfg, kratosAdminURL, identityID, "code", "api")
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"method": "code", "code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cfg.publicURL+"/self-service/recovery?flow="+url.QueryEscape(flowID), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("testkit: building recovery request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	status, rb, _, err := do(cfg.client, req)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		if bytes.Contains(rb, []byte("browser_location_change_required")) {
			return "", errors.New("testkit: Kratos did not return a session token: enable feature_flags.use_continue_with_transitions in kratos.yaml (or use MintKratosSessionCookie)")
		}
		return "", fmt.Errorf("testkit: submitting recovery code: status %d: %s", status, trunc(rb))
	}
	var resp struct {
		ContinueWith []struct {
			Action string `json:"action"`
			Token  string `json:"ory_session_token"`
		} `json:"continue_with"`
	}
	if err := json.Unmarshal(rb, &resp); err != nil {
		return "", fmt.Errorf("testkit: decoding recovery response: %w", err)
	}
	for _, c := range resp.ContinueWith {
		if c.Action == "set_ory_session_token" && c.Token != "" {
			return c.Token, nil
		}
	}
	return "", fmt.Errorf("testkit: recovery response carried no session token: %s", trunc(rb))
}

// MintKratosSessionCookie is MintKratosSession for browsers: it runs a
// browser-type recovery flow and returns the ory_kratos_session cookie Kratos
// issues (name, value and attributes as Kratos set them), which can be set in
// agent-browser for the id app's origin. It prefers the admin recovery link
// (the playground's configuration, recovery.use: link) and falls back to the
// recovery code when the link strategy is not enabled, so it works with either
// and does not need feature_flags.use_continue_with_transitions.
func MintKratosSessionCookie(ctx context.Context, kratosAdminURL, identityID string, opts ...KratosOption) (*http.Cookie, error) {
	cfg, err := newKratosConfig(kratosAdminURL, opts)
	if err != nil {
		return nil, err
	}
	// Do not follow the redirect to the settings UI: the cookie is on the 303.
	noFollow := *cfg.client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.client = &noFollow

	var req *http.Request
	flowID, token, linkErr := createRecovery(ctx, cfg, kratosAdminURL, identityID, "link", "")
	if linkErr == nil {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet,
			cfg.publicURL+"/self-service/recovery?flow="+url.QueryEscape(flowID)+"&token="+url.QueryEscape(token), nil)
	} else {
		var code string
		flowID, code, err = createRecovery(ctx, cfg, kratosAdminURL, identityID, "code", "")
		if err != nil {
			return nil, fmt.Errorf("%w (recovery link attempt: %v)", err, linkErr)
		}
		form := url.Values{"method": {"code"}, "code": {code}}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost,
			cfg.publicURL+"/self-service/recovery?flow="+url.QueryEscape(flowID), strings.NewReader(form.Encode()))
		if req != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("testkit: building recovery request: %w", err)
	}
	req.Header.Set("Accept", "text/html")
	status, rb, hdr, err := do(cfg.client, req)
	if err != nil {
		return nil, err
	}
	resp := http.Response{Header: hdr}
	for _, c := range resp.Cookies() {
		if c.Name == "ory_kratos_session" && c.Value != "" {
			return c, nil
		}
	}
	return nil, fmt.Errorf("testkit: recovery submission (status %d) set no ory_kratos_session cookie: %s", status, trunc(rb))
}

// createRecovery asks the admin API for a recovery of the given kind ("code" or
// "link") and returns the flow id and the secret (code or token).
func createRecovery(ctx context.Context, cfg *kratosConfig, adminURL, identityID, kind, flowType string) (flowID, secret string, err error) {
	payload := map[string]string{"identity_id": identityID, "expires_in": "5m"}
	if flowType != "" {
		payload["flow_type"] = flowType
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(adminURL, "/")+"/admin/recovery/"+kind, bytes.NewReader(b))
	if err != nil {
		return "", "", fmt.Errorf("testkit: building admin request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	status, rb, _, err := do(cfg.client, req)
	if err != nil {
		return "", "", err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", "", fmt.Errorf("testkit: creating recovery %s: status %d: %s", kind, status, trunc(rb))
	}
	var resp struct {
		RecoveryLink string `json:"recovery_link"`
		RecoveryCode string `json:"recovery_code"`
	}
	if err := json.Unmarshal(rb, &resp); err != nil {
		return "", "", fmt.Errorf("testkit: decoding recovery %s response: %w", kind, err)
	}
	u, err := url.Parse(resp.RecoveryLink)
	if err != nil || u.Query().Get("flow") == "" {
		return "", "", fmt.Errorf("testkit: recovery %s response has no flow id: %s", kind, trunc(rb))
	}
	secret = resp.RecoveryCode
	if kind == "link" {
		secret = u.Query().Get("token")
	}
	if secret == "" {
		return "", "", fmt.Errorf("testkit: recovery %s response has no %s: %s", kind, map[string]string{"code": "code", "link": "token"}[kind], trunc(rb))
	}
	return u.Query().Get("flow"), secret, nil
}

func do(c *http.Client, req *http.Request) (int, []byte, http.Header, error) {
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("testkit: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, resp.Header, fmt.Errorf("testkit: reading response: %w", err)
	}
	return resp.StatusCode, b, resp.Header, nil
}

func trunc(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
