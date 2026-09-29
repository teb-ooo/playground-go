package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	// CookieName is the session cookie.
	CookieName = "factory_session"
	// LoginCookieName holds the in-flight login state (PKCE verifier, state, nonce).
	LoginCookieName = "factory_login"

	// DefaultSessionTTL is how long a session cookie is valid.
	DefaultSessionTTL = 24 * time.Hour

	keyLen = 32
)

// ParseKey turns the SESSION_KEY value into the 32 byte AES-256 key. It
// accepts 32 raw bytes, 64 hex characters, or base64 (standard or URL
// alphabet, padded or not) that decodes to 32 bytes.
func ParseKey(s string) ([]byte, error) {
	if len(s) == keyLen {
		return []byte(s), nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == keyLen {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == keyLen {
			return b, nil
		}
	}
	return nil, fmt.Errorf("auth: SESSION_KEY must be 32 bytes (raw, 64 hex characters, or base64)")
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("auth: session key must be %d bytes, got %d", keyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("auth: creating cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// seal encrypts and authenticates v under key. The cookie name is bound as
// additional data so a cookie cannot be replayed as a different kind.
func seal(key []byte, name string, v any) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("auth: encoding cookie: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: generating nonce: %w", err)
	}
	out := gcm.Seal(nonce, nonce, plain, []byte(name))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

var errBadCookie = errors.New("auth: invalid cookie")

func open(key []byte, name, value string, v any) error {
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return errBadCookie
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, []byte(name))
	if err != nil {
		return errBadCookie
	}
	if err := json.Unmarshal(plain, v); err != nil {
		return errBadCookie
	}
	return nil
}

type sessionPayload struct {
	Subject  string   `json:"sub"`
	Email    string   `json:"email,omitempty"`
	Username string   `json:"username,omitempty"`
	Picture  string   `json:"picture,omitempty"`
	Groups   []string `json:"groups,omitempty"`
	Expires  int64    `json:"exp"`
}

// NewSessionCookie builds a valid session cookie for user, expiring ttl after
// now (DefaultSessionTTL if ttl is zero). The cookie is host-only (no Domain),
// HttpOnly, Secure and SameSite=Lax. The testkit package wraps this for tests.
func NewSessionCookie(key []byte, u User, ttl time.Duration, now time.Time) (*http.Cookie, error) {
	if u.Subject == "" {
		return nil, errors.New("auth: session user needs a subject")
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	exp := now.Add(ttl)
	v, err := seal(key, CookieName, sessionPayload{
		Subject: u.Subject, Email: u.Email, Username: u.Username, Picture: u.Picture,
		Groups: u.Groups, Expires: exp.Unix(),
	})
	if err != nil {
		return nil, err
	}
	return baseCookie(CookieName, v, "/", exp, int(ttl.Seconds())), nil
}

func baseCookie(name, value, path string, exp time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: path,
		Expires: exp.UTC().Truncate(time.Second), MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}
}

func expiredCookie(name, path string) *http.Cookie {
	return &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

// DecodeSession validates a session cookie value and returns its user.
func DecodeSession(key []byte, value string, now time.Time) (User, error) {
	var p sessionPayload
	if err := open(key, CookieName, value, &p); err != nil {
		return User{}, err
	}
	if p.Subject == "" || now.Unix() >= p.Expires {
		return User{}, errBadCookie
	}
	return User{Subject: p.Subject, Email: p.Email, Username: p.Username, Picture: p.Picture, Groups: p.Groups}, nil
}

type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"next"`
	Expires  int64  `json:"exp"`
}
