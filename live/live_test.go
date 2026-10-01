package live

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teb-ooo/playground-go/auth"
)

var (
	ann   = auth.User{Subject: "ann", Email: "ann@example.com"}
	bob   = auth.User{Subject: "bob", Email: "bob@example.com"}
	admin = auth.User{Subject: "root", Email: "root@example.com", Groups: []string{auth.AdminGroup}}
)

func resources(frames []Frame) []string {
	var out []string
	for _, f := range frames {
		if f.Degraded {
			out = append(out, "degraded")
		} else {
			out = append(out, f.Event.Resource)
		}
	}
	return out
}

func sub(t *testing.T, h *Hub, u auth.User) *Subscription {
	t.Helper()
	s, cancel, err := h.Subscribe(u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	return s
}

func TestAudienceFiltering(t *testing.T) {
	h := NewHub(WithProjectAccess(func(u auth.User, p string) bool { return u.Subject == "ann" && p == "alpha" }))
	a, b, r := sub(t, h, ann), sub(t, h, bob), sub(t, h, admin)
	h.Publish("everyone", Everyone())
	h.Publish("ann-only", Subject("ann"))
	h.Publish("nobody-subject", Subject(""))
	h.Publish("admins", Admins())
	h.Publish("alpha", Project("alpha"))
	h.Publish("beta", Project("beta"))
	h.Publish("zero", Audience{})
	for _, c := range []struct {
		name string
		s    *Subscription
		want []string
	}{
		{"ann", a, []string{"everyone", "ann-only", "alpha"}},
		{"bob", b, []string{"everyone"}},
	} {
		if got := resources(c.s.Take()); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s got %v want %v", c.name, got, c.want)
		}
	}
	// root is not ann: a project is only visible through the access function.
	got := resources(r.Take())
	if fmt.Sprint(got) != fmt.Sprint([]string{"everyone", "admins"}) {
		t.Errorf("admin got %v", got)
	}
}

func TestProjectFailsClosedToAdmins(t *testing.T) {
	h := NewHub()
	a, r := sub(t, h, ann), sub(t, h, admin)
	h.Publish("p", Project("alpha"))
	if got := a.Take(); len(got) != 0 {
		t.Errorf("member got %v", got)
	}
	got := r.Take()
	if len(got) != 1 || got[0].Event.Project != "alpha" {
		t.Errorf("admin got %+v", got)
	}
}

func TestCoalescingKeepsLatestAndSet(t *testing.T) {
	h := NewHub()
	s := sub(t, h, ann)
	for i := 0; i < 1000; i++ {
		h.Publish("widgets", Everyone())
	}
	h.Publish("gadgets", Everyone())
	h.Publish("widgets", Everyone(), WithID("7"))
	fr := s.Take()
	if len(fr) != 3 {
		t.Fatalf("want 3 coalesced frames, got %+v", fr)
	}
	if fr[0].Event.Resource != "widgets" || fr[0].Seq != 1000 || fr[1].Event.Resource != "gadgets" || fr[2].Event.ID != "7" {
		t.Errorf("order or seq wrong: %+v", fr)
	}
	if len(s.Take()) != 0 {
		t.Error("Take must empty the queue")
	}
}

func TestPendingIsBounded(t *testing.T) {
	h := NewHub()
	s := sub(t, h, ann)
	for i := 0; i < maxPending*3; i++ {
		h.Publish("w", Everyone(), WithID(fmt.Sprint(i)))
	}
	if n := len(s.Take()); n > maxPending+1 {
		t.Errorf("queue grew to %d", n)
	}
}

func TestPublishNeverBlocksOnSlowSubscriber(t *testing.T) {
	h := NewHub()
	_ = sub(t, h, ann) // never reads
	fast := sub(t, h, bob)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			h.Publish("w", Everyone())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked")
	}
	select {
	case <-fast.Ready():
	default:
		t.Fatal("fast subscriber not signalled")
	}
}

func TestMaxPerUserAndRelease(t *testing.T) {
	h := NewHub(WithMaxPerUser(2))
	_, c1, _ := h.Subscribe(ann)
	_, _, err2 := h.Subscribe(ann)
	if err2 != nil {
		t.Fatal(err2)
	}
	if _, _, err := h.Subscribe(ann); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("want ErrTooManyStreams, got %v", err)
	}
	if _, _, err := h.Subscribe(bob); err != nil {
		t.Fatalf("another person is not limited: %v", err)
	}
	c1()
	c1() // idempotent
	if _, _, err := h.Subscribe(ann); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
}

func TestDegradedOrderingAndClear(t *testing.T) {
	h := NewHub()
	s := sub(t, h, ann)
	h.Degraded("down")
	fr := s.Take()
	if len(fr) != 1 || !fr[0].Degraded || fr[0].Reason != "down" {
		t.Fatalf("got %+v", fr)
	}
	h.Degraded("down")
	h.Publish("w", Everyone())
	if got := resources(s.Take()); fmt.Sprint(got) != "[w]" {
		t.Errorf("a change after degraded replaces the pending notice, got %v", got)
	}
	// A new subscriber learns the hub is degraded; a publish clears that.
	h.Degraded("again")
	late := sub(t, h, bob)
	if fr := late.Take(); len(fr) != 1 || !fr[0].Degraded {
		t.Errorf("late subscriber got %+v", fr)
	}
	h.Publish("w", Everyone())
	if fr := sub(t, h, admin).Take(); len(fr) != 0 {
		t.Errorf("hub still degraded: %+v", fr)
	}
}

// ---- the HTTP stream ----

func userFromHeader(r *http.Request) (auth.User, bool) {
	switch r.Header.Get("X-User") {
	case "ann":
		return ann, true
	case "bob":
		return bob, true
	case "admin":
		return admin, true
	}
	return auth.User{}, false
}

type stream struct {
	t     *testing.T
	resp  *http.Response
	lines chan string
}

func open(t *testing.T, srv *httptest.Server, user string) *stream {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+Path, nil)
	if user != "" {
		req.Header.Set("X-User", user)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{t: t, resp: resp, lines: make(chan string, 64)}
	go func() {
		defer close(s.lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return s
}

// next returns the next non-empty line.
func (s *stream) next(timeout time.Duration) (string, bool) {
	for {
		select {
		case l, ok := <-s.lines:
			if !ok {
				return "", false
			}
			if l != "" {
				return l, true
			}
		case <-time.After(timeout):
			s.t.Fatalf("no line within %s", timeout)
		}
	}
}

func (s *stream) expect(want string) {
	s.t.Helper()
	got, ok := s.next(3 * time.Second)
	if !ok || got != want {
		s.t.Fatalf("got %q (open=%v) want %q", got, ok, want)
	}
}

func newServer(t *testing.T, h *Hub, opts ...Option) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(h, append([]Option{WithUser(userFromHeader)}, opts...)...))
	t.Cleanup(srv.Close)
	return srv
}

func TestStreamHeadersFirstFrameAndChange(t *testing.T) {
	h := NewHub()
	srv := newServer(t, h)
	s := open(t, srv, "ann")
	if s.resp.StatusCode != 200 || s.resp.Header.Get("Content-Type") != "text/event-stream" ||
		s.resp.Header.Get("Cache-Control") != "no-store" || s.resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("bad response: %d %v", s.resp.StatusCode, s.resp.Header)
	}
	s.expect(": live")
	h.Publish("widgets", Everyone(), WithID("42"))
	s.expect("event: change")
	id, _ := s.next(time.Second)
	if !strings.HasPrefix(id, "id: ") {
		t.Fatalf("want an id line, got %q", id)
	}
	data, _ := s.next(time.Second)
	var d map[string]string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &d); err != nil || d["resource"] != "widgets" || d["id"] != "42" || len(d) != 2 {
		t.Fatalf("bad data %q (%v)", data, err)
	}
	// Not for bob's audience.
	b := open(t, srv, "bob")
	b.expect(": live")
	h.Publish("secret", Subject("ann"))
	h.Publish("shared", Everyone())
	b.expect("event: change")
	b.next(time.Second)
	if data, _ := b.next(time.Second); !strings.Contains(data, `"shared"`) {
		t.Fatalf("bob received %q", data)
	}
}

func TestStream401IsJSON(t *testing.T) {
	srv := newServer(t, NewHub())
	resp, err := srv.Client().Get(srv.URL + Path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestDefaultUserIsAuthContext(t *testing.T) {
	h := Handler(NewHub())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", Path, nil))
	if rec.Code != 401 {
		t.Fatalf("signed out: %d", rec.Code)
	}
	ctx, cancel := context.WithCancel(auth.WithUser(context.Background(), ann))
	cancel()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", Path, nil).WithContext(ctx))
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), ": live") {
		t.Fatalf("signed in: %d %q", rec.Code, rec.Body.String())
	}
}

func TestWithAuthWraps(t *testing.T) {
	var called atomic.Bool
	deny := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called.Store(true)
			http.Error(w, "no", http.StatusForbidden)
		})
	}
	srv := httptest.NewServer(Handler(NewHub(), WithAuth(deny), WithUser(userFromHeader)))
	defer srv.Close()
	resp, _ := srv.Client().Get(srv.URL + Path)
	resp.Body.Close()
	if !called.Load() || resp.StatusCode != 403 {
		t.Fatalf("middleware not used: %d", resp.StatusCode)
	}
}

func TestStream429(t *testing.T) {
	h := NewHub(WithMaxPerUser(1))
	srv := newServer(t, h)
	first := open(t, srv, "ann")
	first.expect(": live")
	resp, err := srv.Client().Do(func() *http.Request {
		r, _ := http.NewRequest("GET", srv.URL+Path, nil)
		r.Header.Set("X-User", "ann")
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
}

func TestHeartbeat(t *testing.T) {
	srv := newServer(t, NewHub(), WithHeartbeat(20*time.Millisecond))
	s := open(t, srv, "ann")
	s.expect(": live")
	s.expect(": ping")
	s.expect(": ping")
}

func TestMaxLifetimeClosesStream(t *testing.T) {
	h := NewHub()
	srv := newServer(t, h, WithMaxLifetime(50*time.Millisecond))
	s := open(t, srv, "ann")
	s.expect(": live")
	if _, ok := s.next(3 * time.Second); ok {
		t.Fatal("expected the stream to end")
	}
	waitFor(t, "subscription released", func() bool { return h.Subscribers() == 0 })
}

func TestDegradedOverHTTP(t *testing.T) {
	h := NewHub()
	srv := newServer(t, h)
	s := open(t, srv, "ann")
	s.expect(": live")
	h.Degraded("source down")
	s.expect("event: degraded")
	s.expect(`data: {"reason":"source down"}`)
	h.Publish("w", Everyone())
	s.expect("event: change")
	// A subscriber arriving while degraded is told at once.
	h.Degraded("still down")
	late := open(t, srv, "bob")
	late.expect(": live")
	late.expect("event: degraded")
}

func TestSlowReaderIsDroppedByWriteDeadlineWithoutBlockingOthers(t *testing.T) {
	h := NewHub()
	srv := newServer(t, h, WithWriteTimeout(100*time.Millisecond))
	// A raw client that sends its request and never reads, with a tiny receive buffer.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.(*net.TCPConn).SetReadBuffer(1024)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nX-User: bob\r\n\r\n", Path)
	fast := open(t, srv, "ann")
	fast.expect(": live")
	waitFor(t, "both subscribed", func() bool { return h.Subscribers() == 2 })
	big := strings.Repeat("x", 16<<10)
	deadline := time.Now().Add(10 * time.Second)
	for h.Subscribers() == 2 && time.Now().Before(deadline) {
		start := time.Now()
		for i := 0; i < 50; i++ {
			h.Publish("w", bobOnly(), WithID(fmt.Sprint(i)+big))
		}
		if time.Since(start) > time.Second {
			t.Fatal("publishing blocked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.Subscribers() != 1 {
		t.Fatalf("the slow reader was not dropped (%d subscribers)", h.Subscribers())
	}
	h.Publish("after", Everyone())
	for {
		l, ok := fast.next(3 * time.Second)
		if !ok {
			t.Fatal("the fast reader lost its stream")
		}
		if strings.Contains(l, "after") {
			break
		}
	}
}

// bobOnly is the audience of the slow reader in tests.
func bobOnly() Audience { return Subject("bob") }

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	settle := func() int {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	before := settle()
	h := NewHub()
	srv := httptest.NewServer(Handler(h, WithUser(userFromHeader), WithHeartbeat(10*time.Millisecond)))
	tr := &http.Transport{}
	client := &http.Client{Transport: tr}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+Path, nil)
		req.Header.Set("X-User", "ann")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			bufio.NewReader(resp.Body).ReadString('\n')
			h.Publish("w", Everyone())
			time.Sleep(15 * time.Millisecond)
			cancel()
			resp.Body.Close()
		}()
		if i%6 == 5 { // stay under the per-user cap of 8
			wg.Wait()
			waitFor(t, "subscriptions released", func() bool { return h.Subscribers() == 0 })
		}
	}
	wg.Wait()
	waitFor(t, "all subscriptions released", func() bool { return h.Subscribers() == 0 })
	srv.Close()
	tr.CloseIdleConnections()
	waitFor(t, "goroutines to settle", func() bool { return settle() <= before+1 })
}

// ---- relay ----

type fakeUp struct {
	mu    sync.Mutex
	calls int
	run   func(call int, ctx context.Context, fn func(UpstreamEvent)) error
}

func (f *fakeUp) Stream(ctx context.Context, fn func(UpstreamEvent)) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.run(n, ctx, fn)
}

func TestRelayPublishesDegradesAndRecovers(t *testing.T) {
	h := NewHub()
	s := sub(t, h, ann)
	up := &fakeUp{run: func(call int, ctx context.Context, fn func(UpstreamEvent)) error {
		switch call {
		case 1:
			return errors.New("refused") // down
		case 2:
			fn(UpstreamEvent{Name: Connected})
			fn(UpstreamEvent{Name: "change", Data: `{"project":"alpha"}`})
			fn(UpstreamEvent{Name: "noise"})
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	translate := func(e UpstreamEvent) (string, Audience, bool) {
		if e.Name != "change" {
			return "", Audience{}, false
		}
		var d struct{ Project string }
		if json.Unmarshal([]byte(e.Data), &d) != nil {
			return "", Audience{}, false
		}
		return "beads", Everyone(), true
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Relay(ctx, h, up, translate, WithBackoff(time.Millisecond, 5*time.Millisecond)); close(done) }()

	var got []string
	waitFor(t, "relay frames", func() bool {
		select {
		case <-s.Ready():
		case <-time.After(10 * time.Millisecond):
		}
		got = append(got, resources(s.Take())...)
		return strings.Contains(strings.Join(got, ","), "beads")
	})
	cancel()
	<-done
	joined := strings.Join(got, ",")
	if !strings.HasPrefix(joined, "degraded") {
		t.Errorf("want degraded first, got %s", joined)
	}
	if strings.Contains(joined, "noise") {
		t.Errorf("skipped event published: %s", joined)
	}
}

func TestSSEUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, ": live\n\nevent: change\nid: v1\ndata: {\"a\":1}\n\n: ping\n\nevent: change\ndata: x\ndata: y\n\n")
	}))
	defer srv.Close()
	up := SSEUpstream{Open: func(ctx context.Context) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
		return srv.Client().Do(req)
	}}
	var got []UpstreamEvent
	if err := up.Stream(context.Background(), func(e UpstreamEvent) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	want := []UpstreamEvent{{Name: Connected}, {Name: "change", ID: "v1", Data: `{"a":1}`}, {Name: "change", Data: "x\ny"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	bad := SSEUpstream{Open: func(ctx context.Context) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.WriteHeader(503)
		return rec.Result(), nil
	}}
	if err := bad.Stream(context.Background(), func(UpstreamEvent) {}); err == nil {
		t.Fatal("503 must be an error")
	}
}
