package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teb-ooo/playground-go/auth"
)

// B8: a hanging issuer must not serialise callers behind one mutex.
func TestDiscoveryDoesNotBlockCallers(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	a, err := auth.New(auth.OIDCConfig{Issuer: srv.URL, ClientID: "app", PublicURL: "https://app.example"}, testKey, auth.WithoutRateLimit())
	if err != nil {
		t.Fatal(err)
	}
	const timeout, retry = 400 * time.Millisecond, 700 * time.Millisecond
	a.SetDiscoveryTimings(timeout, retry)

	login := func() (int, time.Duration) {
		w := httptest.NewRecorder()
		start := time.Now()
		a.ServeLogin(w, httptest.NewRequest("GET", "/auth/login", nil))
		return w.Code, time.Since(start)
	}
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _ := login(); code != http.StatusBadGateway {
				t.Errorf("login = %d", code)
			}
		}()
	}
	wg.Wait()
	if d := time.Since(start); d > timeout+300*time.Millisecond {
		t.Errorf("8 concurrent logins took %v, want about one discovery timeout (%v)", d, timeout)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("discovery requests = %d, want 1 (single in flight)", n)
	}
	// Inside the retry window a call fails at once, without a new discovery.
	if code, d := login(); code != http.StatusBadGateway || d > 100*time.Millisecond {
		t.Errorf("negative cache: %d after %v", code, d)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("discovery requests = %d inside the retry window", n)
	}
	// After the window a new discovery is tried.
	time.Sleep(retry)
	login()
	if n := hits.Load(); n != 2 {
		t.Errorf("discovery requests = %d after the retry window, want 2", n)
	}
}

// A waiter gives up with its own context, not with the discovery's.
func TestDiscoveryWaiterHonoursItsContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	a, _ := auth.New(auth.OIDCConfig{Issuer: srv.URL, ClientID: "app", PublicURL: "https://app.example"}, testKey, auth.WithoutRateLimit())
	a.SetDiscoveryTimings(5*time.Second, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	start := time.Now()
	a.ServeLogin(w, httptest.NewRequest("GET", "/auth/login", nil).WithContext(ctx))
	if d := time.Since(start); d > time.Second || w.Code != http.StatusBadGateway {
		t.Errorf("code=%d after %v", w.Code, d)
	}
}

func TestCloseRunsOnCloseOnce(t *testing.T) {
	n := 0
	a, err := auth.New(auth.OIDCConfig{Issuer: "https://x.example", ClientID: "a", PublicURL: "https://a.example"}, testKey,
		auth.OnClose(func() { n++ }))
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	a.Close()
	if n != 1 {
		t.Fatalf("closer ran %d times", n)
	}
}
