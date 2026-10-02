package keys

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/teb-ooo/playground-go/auth"
	"github.com/teb-ooo/playground-go/ratelimit"
)

type fakePlayd struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	keys       map[string]*ecdsa.PrivateKey
	revoked    []map[string]any
	down       bool
	jwksHits   int
	revHits    int
	notModHits int
}

func newFake(t *testing.T) *fakePlayd {
	f := &fakePlayd{t: t, keys: map[string]*ecdsa.PrivateKey{}}
	f.addKey("k1")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.keys/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.jwksHits++
		if f.down {
			http.Error(w, "down", 503)
			return
		}
		var set jose.JSONWebKeySet
		for kid, k := range f.keys {
			set.Keys = append(set.Keys, jose.JSONWebKey{Key: &k.PublicKey, KeyID: kid, Algorithm: "ES256", Use: "sig"})
		}
		f.serve(w, r, set)
	})
	mux.HandleFunc("GET /.keys/revoked.json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.revHits++
		if f.down {
			http.Error(w, "down", 503)
			return
		}
		f.serve(w, r, map[string]any{"updated_at": time.Now().Format(time.RFC3339), "revoked": f.revoked})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePlayd) serve(w http.ResponseWriter, r *http.Request, v any) {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	etag := `"` + base64.RawURLEncoding.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "max-age=30")
	if r.Header.Get("If-None-Match") == etag {
		f.notModHits++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (f *fakePlayd) addKey(kid string) *ecdsa.PrivateKey {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.mu.Lock()
	f.keys[kid] = k
	f.mu.Unlock()
	return k
}

func (f *fakePlayd) revoke(jti string, exp time.Time) {
	f.mu.Lock()
	f.revoked = append(f.revoked, map[string]any{"jti": jti, "exp": exp.Unix()})
	f.mu.Unlock()
}

func (f *fakePlayd) setDown(d bool) { f.mu.Lock(); f.down = d; f.mu.Unlock() }

func (f *fakePlayd) issuer() string { return f.srv.URL + "/.keys" }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	f   *fakePlayd
	clk *clock
	v   *Verifier
}

func newEnv(t *testing.T) *env {
	f := newFake(t)
	clk := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	v, err := New(Options{App: "notes", Issuer: f.issuer(), Now: clk.now, NoBackground: true,
		FailLimit: ratelimit.Options{Requests: 1000, Per: time.Minute, Burst: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	return &env{f: f, clk: clk, v: v}
}

func (e *env) claims() map[string]any {
	now := e.clk.now()
	return map[string]any{
		"iss": e.f.issuer(), "sub": "u-1", "email": "a@x.org", "preferred_username": "alex",
		"scp": "notes:read notes:write", "aud": []string{"notes", "bd"}, "jti": "j-1",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
}

func (e *env) sign(kid string, claims map[string]any) string {
	e.f.mu.Lock()
	k := e.f.keys[kid]
	e.f.mu.Unlock()
	return signWith(jose.SigningKey{Algorithm: jose.ES256, Key: k}, kid, claims)
}

func signWith(sk jose.SigningKey, kid string, claims map[string]any) string {
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
	s, err := jose.NewSigner(sk, opts)
	if err != nil {
		panic(err)
	}
	b, _ := json.Marshal(claims)
	o, err := s.Sign(b)
	if err != nil {
		panic(err)
	}
	c, _ := o.CompactSerialize()
	return Prefix + c
}

func (e *env) verify(tok string) (auth.User, bool, error) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	return e.v.Verify(r, tok)
}

func (e *env) wantOK(t *testing.T, tok string) auth.User {
	t.Helper()
	u, ok, err := e.verify(tok)
	if !ok || err != nil {
		t.Fatalf("want ok, got ok=%v err=%v", ok, err)
	}
	return u
}

func (e *env) want401(t *testing.T, tok string) {
	t.Helper()
	if _, ok, err := e.verify(tok); ok || err != nil {
		t.Fatalf("want plain 401, got ok=%v err=%v", ok, err)
	}
}

func TestValid(t *testing.T) {
	e := newEnv(t)
	c := e.claims()
	c["groups"] = []string{"admin"}
	u := e.wantOK(t, e.sign("k1", c))
	if u.Subject != "u-1" || u.Email != "a@x.org" || u.Username != "alex" || !u.IsAdmin() ||
		len(u.Scopes) != 2 || !u.HasScope("notes:write") || u.HasScope("notes:admin") {
		t.Fatalf("user = %+v", u)
	}
	// Groups only come from the claim.
	u = e.wantOK(t, e.sign("k1", e.claims()))
	if u.IsAdmin() || len(u.Groups) != 0 {
		t.Fatalf("groups = %v", u.Groups)
	}
	// aud as a plain string
	c = e.claims()
	c["aud"] = "notes"
	e.wantOK(t, e.sign("k1", c))
}

func TestAct(t *testing.T) {
	e := newEnv(t)
	c := e.claims()
	c["act"] = "bd"
	if u := e.wantOK(t, e.sign("k1", c)); u.Agent != "bd" || u.Subject != "u-1" || u.Email != "a@x.org" {
		t.Fatalf("user = %+v", u)
	}
	if u := e.wantOK(t, e.sign("k1", e.claims())); u.Agent != "" {
		t.Fatalf("agent = %q", u.Agent)
	}
	for _, bad := range []string{"BD", "b", "1bd", "bd_x", "bd x", "-bd", "a/b", strings.Repeat("a", 32)} {
		c := e.claims()
		c["act"] = bad
		e.want401(t, e.sign("k1", c))
	}
	c = e.claims()
	c["act"] = 5
	e.want401(t, e.sign("k1", c))
	c = e.claims()
	c["act"] = strings.Repeat("a", 31)
	e.wantOK(t, e.sign("k1", c))
}

func TestRefusals(t *testing.T) {
	e := newEnv(t)
	cases := map[string]func(c map[string]any){
		"wrong aud":     func(c map[string]any) { c["aud"] = []string{"bd"} },
		"no aud":        func(c map[string]any) { delete(c, "aud") },
		"wrong issuer":  func(c map[string]any) { c["iss"] = "https://evil.example/.keys" },
		"expired":       func(c map[string]any) { c["exp"] = e.clk.now().Add(-time.Minute).Unix() },
		"no exp":        func(c map[string]any) { delete(c, "exp") },
		"not yet valid": func(c map[string]any) { c["nbf"] = e.clk.now().Add(time.Hour).Unix() },
		"no subject":    func(c map[string]any) { delete(c, "sub") },
		"no jti":        func(c map[string]any) { delete(c, "jti") },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			c := e.claims()
			mod(c)
			e.want401(t, e.sign("k1", c))
		})
	}
}

func TestLeeway(t *testing.T) {
	e := newEnv(t)
	c := e.claims()
	c["exp"] = e.clk.now().Add(-20 * time.Second).Unix()
	e.wantOK(t, e.sign("k1", c))
	c["exp"] = e.clk.now().Add(-40 * time.Second).Unix()
	e.want401(t, e.sign("k1", c))
	c = e.claims()
	c["nbf"] = e.clk.now().Add(20 * time.Second).Unix()
	e.wantOK(t, e.sign("k1", c))
}

func TestRevoked(t *testing.T) {
	e := newEnv(t)
	tok := e.sign("k1", e.claims())
	e.wantOK(t, tok)
	e.f.revoke("j-1", e.clk.now().Add(time.Hour))
	// Still accepted until the poll sees it (the window of the RFC).
	e.wantOK(t, tok)
	if err := e.v.pollRevoked(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.want401(t, tok)
	// A different key is not affected.
	c := e.claims()
	c["jti"] = "j-2"
	e.wantOK(t, e.sign("k1", c))
}

func TestRevokedETag(t *testing.T) {
	e := newEnv(t)
	e.wantOK(t, e.sign("k1", e.claims()))
	_ = e.v.pollRevoked(t.Context())
	e.f.mu.Lock()
	n := e.f.notModHits
	e.f.mu.Unlock()
	if n != 1 {
		t.Fatalf("304 hits = %d", n)
	}
}

func TestWrongKeyAndTamper(t *testing.T) {
	e := newEnv(t)
	// Signed by a key the issuer does not publish, under a known kid.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	e.want401(t, signWith(jose.SigningKey{Algorithm: jose.ES256, Key: other}, "k1", e.claims()))

	tok := e.sign("k1", e.claims())
	parts := strings.Split(tok, ".")
	c := e.claims()
	c["sub"] = "admin-user"
	c["aud"] = []string{"notes"}
	pb, _ := json.Marshal(c)
	parts[1] = base64.RawURLEncoding.EncodeToString(pb)
	e.want401(t, strings.Join(parts, "."))

	for _, bad := range []string{"", "pk_", "pk_x", "pk_a.b.c", "pat_" + tok[3:], tok[3:], tok + "x", "pk_" + strings.Repeat("a", 5000)} {
		e.want401(t, bad)
	}
}

func TestUnknownKidRefresh(t *testing.T) {
	e := newEnv(t)
	e.wantOK(t, e.sign("k1", e.claims()))
	e.f.addKey("k2")
	tok2 := e.sign("k2", e.claims())
	// Unknown kid right after a load: no refetch yet (at most once a minute).
	e.want401(t, tok2)
	if e.f.jwksHits != 1 {
		t.Fatalf("jwks hits = %d", e.f.jwksHits)
	}
	e.clk.add(61 * time.Second)
	e.v.pollRevoked(t.Context())
	e.wantOK(t, tok2)
	if e.f.jwksHits != 2 {
		t.Fatalf("jwks hits = %d", e.f.jwksHits)
	}
	// Garbage kids cannot make us fetch more than once a minute.
	for i := 0; i < 20; i++ {
		e.want401(t, signWith(jose.SigningKey{Algorithm: jose.ES256, Key: e.f.keys["k1"]}, "nope", e.claims()))
	}
	if e.f.jwksHits != 2 {
		t.Fatalf("jwks hits = %d", e.f.jwksHits)
	}
}

func TestJWKSPeriodicRefreshAndOutage(t *testing.T) {
	e := newEnv(t)
	tok := e.sign("k1", e.claims())
	e.wantOK(t, tok)
	e.clk.add(11 * time.Minute)
	c := e.claims()
	c["exp"] = e.clk.now().Add(time.Hour).Unix()
	tok = e.sign("k1", c)
	e.v.pollRevoked(t.Context())
	e.f.setDown(true)
	// The JWKS refresh fails but the cached keys keep working.
	e.wantOK(t, tok)
	if e.f.jwksHits != 2 {
		t.Fatalf("jwks hits = %d", e.f.jwksHits)
	}
}

func TestJWKSNeverLoaded(t *testing.T) {
	e := newEnv(t)
	e.f.setDown(true)
	_, ok, err := e.verify(e.sign("k1", e.claims()))
	var un *auth.UnavailableError
	if ok || err == nil || !asUnavail(err, &un) {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	e.f.setDown(false)
	e.wantOK(t, e.sign("k1", e.claims()))
}

func asUnavail(err error, t **auth.UnavailableError) bool {
	u, ok := err.(*auth.UnavailableError)
	*t = u
	return ok
}

func TestRevocationNeverLoaded(t *testing.T) {
	e := newEnv(t)
	tok := e.sign("k1", e.claims())
	e.v.o.RevokedURL = e.f.srv.URL + "/missing"
	_, ok, err := e.verify(tok)
	if ok || err != ErrRevocationUnavailable || err.Error() != "keys: revocation list unavailable" {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	e.v.o.RevokedURL = e.f.issuer() + "/revoked.json"
	e.wantOK(t, tok)
}

func TestStaleListToleranceAndCutoff(t *testing.T) {
	e := newEnv(t)
	c := e.claims()
	c["exp"] = e.clk.now().Add(24 * time.Hour).Unix()
	tok := e.sign("k1", c)
	e.wantOK(t, tok)
	e.f.setDown(true)
	e.clk.add(5 * time.Minute)
	_ = e.v.pollRevoked(t.Context()) // fails, keeps the old list
	e.wantOK(t, tok)                 // inside the 10 minute tolerance
	e.clk.add(5*time.Minute + time.Second)
	_, ok, err := e.verify(tok)
	if ok || err != ErrRevocationUnavailable {
		t.Fatalf("after cutoff: ok=%v err=%v", ok, err)
	}
	// The endpoint comes back: service resumes, and a revoked key is refused.
	e.f.setDown(false)
	e.f.revoke("j-1", e.clk.now().Add(time.Hour))
	e.want401(t, tok)
}

func TestAlgConfusion(t *testing.T) {
	e := newEnv(t)
	c := e.claims()
	b64 := func(v any) string { j, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(j) }

	// alg none
	none := Prefix + b64(map[string]any{"alg": "none", "kid": "k1", "typ": "JWT"}) + "." + b64(c) + "."
	e.want401(t, none)

	// HS256 keyed with the public key bytes (classic confusion)
	e.f.mu.Lock()
	pub := e.f.keys["k1"].PublicKey
	e.f.mu.Unlock()
	pubBytes, _ := json.Marshal(jose.JSONWebKey{Key: &pub})
	hs := b64(map[string]any{"alg": "HS256", "kid": "k1", "typ": "JWT"}) + "." + b64(c)
	m := hmac.New(sha256.New, pubBytes)
	m.Write([]byte(hs))
	e.want401(t, Prefix+hs+"."+base64.RawURLEncoding.EncodeToString(m.Sum(nil)))

	// RS256 signed by an attacker's RSA key under the known kid
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	e.want401(t, signWith(jose.SigningKey{Algorithm: jose.RS256, Key: rk}, "k1", c))

	// RS256 header with an ES256-shaped signature
	rs := b64(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + b64(c)
	h := sha256.Sum256([]byte(rs))
	sig, _ := e.f.keys["k1"].Sign(rand.Reader, h[:], crypto.SHA256)
	e.want401(t, Prefix+rs+"."+base64.RawURLEncoding.EncodeToString(sig))

	// No kid at all
	nk := signWith(jose.SigningKey{Algorithm: jose.ES256, Key: e.f.keys["k1"]}, "", c)
	e.want401(t, nk)
}

func TestRateLimitFailures(t *testing.T) {
	e := newEnv(t)
	e.v.fails = ratelimit.New(ratelimit.Options{Requests: 1, Per: time.Hour, Burst: 2, Now: e.clk.now})
	e.want401(t, "pk_garbage")
	e.want401(t, "pk_garbage")
	_, _, err := e.verify("pk_garbage")
	if _, ok := err.(*auth.RateLimitedError); !ok {
		t.Fatalf("err = %v", err)
	}
	// Even a good key from that client is refused while blocked.
	if _, ok, err := e.verify(e.sign("k1", e.claims())); ok || err == nil {
		t.Fatal("blocked client was served")
	}
	// A valid key with a wrong audience is not charged.
	e2 := newEnv(t)
	e2.v.fails = ratelimit.New(ratelimit.Options{Requests: 1, Per: time.Hour, Burst: 1, Now: e2.clk.now})
	c := e2.claims()
	c["aud"] = []string{"bd"}
	for i := 0; i < 5; i++ {
		e2.want401(t, e2.sign("k1", c))
	}
}

func TestNeverLogsOrReturnsToken(t *testing.T) {
	e := newEnv(t)
	tok := e.sign("k1", e.claims())
	e.f.setDown(true)
	e.v.o.RevokedURL = e.f.srv.URL + "/missing"
	_, _, err := e.verify(tok)
	if err != nil && strings.Contains(err.Error(), tok[3:20]) {
		t.Fatal("error contains token")
	}
}

func TestNew(t *testing.T) {
	if _, err := New(Options{Domain: "teb.ooo"}); err == nil {
		t.Fatal("App is required")
	}
	if _, err := New(Options{App: "notes"}); err == nil {
		t.Fatal("Domain or Issuer is required")
	}
	v, err := New(Options{App: "notes", Domain: "teb.ooo"})
	if err != nil || v.o.Issuer != "https://id.teb.ooo/.keys" || v.o.JWKSURL != "https://id.teb.ooo/.keys/jwks.json" ||
		v.o.RevokedURL != "https://id.teb.ooo/.keys/revoked.json" {
		t.Fatalf("%+v %v", v, err)
	}
	v.Close()
}
