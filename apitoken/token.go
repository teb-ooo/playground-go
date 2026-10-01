package apitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/teb-ooo/playground-go/auth"
)

const (
	// Prefix starts every token (auth.PATPrefix).
	Prefix = auth.PATPrefix
	// PrefixLen is how many leading characters are stored and shown to
	// identify a token ("pat_" plus four characters).
	PrefixLen = 8

	secretBytes = 32
	// tokenLen is len("pat_") + the 43 characters of 32 bytes in unpadded base64url.
	tokenLen = len(Prefix) + 43
)

// Generate returns a new token: "pat_" followed by 32 random bytes in
// unpadded base64url. The caller shows it once and stores only Hash(token).
func Generate() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("apitoken: generating token: %w", err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash returns the SHA-256 of the full token, which is what is stored.
func Hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// DisplayPrefix returns the first PrefixLen characters of a token.
func DisplayPrefix(token string) string {
	if len(token) < PrefixLen {
		return token
	}
	return token[:PrefixLen]
}

// wellFormed reports whether s has the shape Generate produces, so malformed
// input is refused without a store round trip.
func wellFormed(s string) bool {
	if len(s) != tokenLen || !strings.HasPrefix(s, Prefix) {
		return false
	}
	for _, c := range s[len(Prefix):] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
