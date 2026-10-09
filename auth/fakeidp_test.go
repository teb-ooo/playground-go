package auth_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeIDP is a minimal OIDC provider: discovery, JWKS, token and userinfo.
type fakeIDP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]codeInfo // authorization code -> info
	// claims returned in ID tokens and userinfo, by subject
	claims map[string]map[string]any
	// userinfoCalls counts userinfo requests.
	userinfoCalls int
	failUserinfo  bool
	// opaque maps an opaque access token to its userinfo claims (with "sub").
	opaque map[string]map[string]any
}

type codeInfo struct {
	challenge string
	nonce     string
	sub       string
	clientID  string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIDP{t: t, key: key, codes: map[string]codeInfo{}, claims: map[string]map[string]any{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/jwks",
			"userinfo_endpoint":                     f.srv.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(f.key.N.Bytes()), "e": b64(big.NewInt(int64(f.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/userinfo", f.userinfo)
	return f
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeIDP) sign(claims map[string]any) string {
	hdr, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	pl, _ := json.Marshal(claims)
	in := b64(hdr) + "." + b64(pl)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return in + "." + b64(sig)
}

func (f *fakeIDP) idToken(sub, aud, nonce string) string {
	c := map[string]any{"iss": f.srv.URL, "sub": sub, "aud": aud,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	if nonce != "" {
		c["nonce"] = nonce
	}
	for k, v := range f.claims[sub] {
		c[k] = v
	}
	return f.sign(c)
}

// accessToken mints a JWT access token that carries no user claims, like Hydra's.
func (f *fakeIDP) accessToken(sub string, exp time.Time) string {
	return f.sign(map[string]any{"iss": f.srv.URL, "sub": sub, "aud": []string{"some-api"},
		"iat": time.Now().Unix(), "exp": exp.Unix(), "scp": []string{"openid"}})
}

// accessTokenFor mints a JWT access token with the given scp and aud.
func (f *fakeIDP) accessTokenFor(sub string, scp, aud []string) string {
	return f.sign(map[string]any{"iss": f.srv.URL, "sub": sub, "aud": aud,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "scp": scp})
}

func (f *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ParseForm()
	ci, ok := f.codes[r.Form.Get("code")]
	if !ok {
		http.Error(w, `{"error":"invalid_grant"}`, 400)
		return
	}
	delete(f.codes, r.Form.Get("code"))
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if b64(sum[:]) != ci.challenge {
		http.Error(w, `{"error":"invalid_grant","error_description":"pkce"}`, 400)
		return
	}
	id, secret, _ := r.BasicAuth()
	if id == "" {
		id, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
	}
	if id != ci.clientID || secret != "shh" {
		http.Error(w, `{"error":"invalid_client"}`, 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": f.accessToken(ci.sub, time.Now().Add(time.Hour)), "token_type": "bearer", "expires_in": 3600,
		"id_token": f.idToken(ci.sub, ci.clientID, ci.nonce),
	})
}

func (f *fakeIDP) userinfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.userinfoCalls++
	fail := f.failUserinfo
	f.mu.Unlock()
	tok := r.Header.Get("Authorization")
	if fail || len(tok) < 8 {
		http.Error(w, "nope", 401)
		return
	}
	// Decode the subject out of the JWT payload (already signature-checked by
	// the client path in the tests that matter).
	parts := splitDots(tok[7:])
	f.mu.Lock()
	oc, isOpaque := f.opaque[tok[7:]]
	f.mu.Unlock()
	if isOpaque {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(oc)
		return
	}
	if len(parts) != 3 {
		http.Error(w, "opaque tokens are not known to the fake", 401)
		return
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	json.Unmarshal(raw, &c)
	sub, _ := c["sub"].(string)
	out := map[string]any{"sub": sub}
	for k, v := range f.claims[sub] {
		out[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func splitDots(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
