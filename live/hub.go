// Package live is the server half of rule WEB-50: one server-sent-events stream per app, GET /api/live, that tells
// every open screen "this resource changed" so it refetches through the normal API.
//
// An event carries no data, only a resource name (and optionally a project and an id that narrow a detail query):
// authorisation stays the API's. A Hub fans events out; an Audience decides who may be told; Handler is the stream
// (a plain http.Handler, never a Huma operation, so the OpenAPI, MCP and parity checks do not see it); Mount
// registers it on a mux; Relay feeds the hub from an upstream stream such as playd's /v1/work/events.
//
//	hub := live.NewHub()
//	live.Mount(mux, hub)
//	hub.Publish("widgets", live.Everyone()) // after a successful create, update or delete
//
// The wire format is what @teb-ooo/web's useLive() reads: `event: change`, `id: <counter>`,
// `data: {"resource":"widgets"}`; `event: degraded` with `data: {"reason":"..."}`; `: ping` comments.
package live

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/teb-ooo/playground-go/auth"
)

// DefaultMaxPerUser is how many streams one person may hold open at once (browser tabs).
const DefaultMaxPerUser = 8

// maxPending is how many distinct events a slow subscriber may have queued before they collapse into one event that
// names no resource (the client then refreshes everything): memory per subscriber stays bounded.
const maxPending = 256

// ErrTooManyStreams is returned by Subscribe when the person already holds the maximum number of streams.
var ErrTooManyStreams = errors.New("live: too many streams open for this person")

// Audience says who is told about an event. Build one with Everyone, Subject, Admins or Project.
type Audience struct {
	kind    audienceKind
	subject string
	project string
}

type audienceKind uint8

const (
	audNobody audienceKind = iota // the zero Audience reaches no one
	audEveryone
	audSubject
	audAdmins
	audProject
)

// Everyone reaches every signed-in person.
func Everyone() Audience { return Audience{kind: audEveryone} }

// Subject reaches the person with this identity subject (all their tabs). An empty subject reaches no one.
func Subject(sub string) Audience { return Audience{kind: audSubject, subject: sub} }

// Admins reaches administrators (auth.User.IsAdmin).
func Admins() Audience { return Audience{kind: audAdmins} }

// Project reaches the people who may see the named project, as decided by the hub's WithProjectAccess function
// (administrators when the hub has none: it fails closed). The name is also carried in the event as "project", so a
// client can narrow a detail query.
func Project(name string) Audience { return Audience{kind: audProject, project: name} }

// includes reports whether u is in the audience.
func (a Audience) includes(u auth.User, access func(auth.User, string) bool) bool {
	switch a.kind {
	case audEveryone:
		return true
	case audSubject:
		return a.subject != "" && u.Subject == a.subject
	case audAdmins:
		return u.IsAdmin()
	case audProject:
		if a.project == "" {
			return false
		}
		if access == nil {
			return u.IsAdmin()
		}
		return access(u, a.project)
	}
	return false
}

// Event is one change as the subscriber receives it.
type Event struct {
	// Resource is the resource name ("widgets"); empty means "something changed": refresh everything.
	Resource string
	// Project and ID optionally narrow a detail query.
	Project string
	ID      string
}

// Frame is one thing to write to a stream: a change (Degraded false) or a degraded notice carrying Reason.
// Seq is the hub's counter at the time, used as the SSE id of a change.
type Frame struct {
	Seq      uint64
	Event    Event
	Degraded bool
	Reason   string
}

// PublishOption narrows an event.
type PublishOption func(*Event)

// WithID names the record that changed, for a detail query. Opaque to the hub.
func WithID(id string) PublishOption { return func(e *Event) { e.ID = id } }

// HubOption configures NewHub.
type HubOption func(*Hub)

// WithMaxPerUser sets the number of simultaneous streams one person may hold (default DefaultMaxPerUser).
func WithMaxPerUser(n int) HubOption { return func(h *Hub) { h.maxPerUser = n } }

// WithProjectAccess decides who may see a project, for Project audiences. Without it Project events reach
// administrators only.
func WithProjectAccess(f func(u auth.User, project string) bool) HubOption {
	return func(h *Hub) { h.access = f }
}

// Hub fans change events out to subscribers. Publish and Degraded never block. Each subscriber keeps only the set
// of resources that changed since it last read (like playd's watcher), so a slow reader costs memory for that set and
// nothing for anyone else. The zero value is not usable; use NewHub.
type Hub struct {
	maxPerUser int
	access     func(auth.User, string) bool

	mu       sync.Mutex
	seq      uint64
	subs     map[*Subscription]struct{}
	perUser  map[string]int
	degraded bool
	reason   string
}

// NewHub returns an empty hub.
func NewHub(opts ...HubOption) *Hub {
	h := &Hub{maxPerUser: DefaultMaxPerUser, subs: map[*Subscription]struct{}{}, perUser: map[string]int{}}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Publish tells everyone in aud that resource changed. Call it after a successful write. An empty resource means
// "something changed, refresh everything". It never blocks and is safe for concurrent use. A published change also
// ends a degraded state.
func (h *Hub) Publish(resource string, aud Audience, opts ...PublishOption) {
	e := Event{Resource: resource}
	if aud.kind == audProject {
		e.Project = aud.project
	}
	for _, o := range opts {
		o(&e)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	h.degraded, h.reason = false, ""
	for s := range h.subs {
		if aud.includes(s.user, h.access) {
			s.addEvent(h.seq, e)
		}
	}
}

// Degraded tells every subscriber that the source of changes is down (event "degraded"); live updates may be late
// until the next Publish, which clears it. New subscribers are told too. reason is shown to people: keep it short and
// free of internals.
func (h *Hub) Degraded(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	h.degraded, h.reason = true, reason
	for s := range h.subs {
		s.setDegraded(h.seq, reason)
	}
}

// Subscribe registers a stream for u. cancel must be called when the stream ends. It returns ErrTooManyStreams when
// u already holds the maximum.
func (h *Hub) Subscribe(u auth.User) (*Subscription, func(), error) {
	key := userKey(u)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maxPerUser > 0 && h.perUser[key] >= h.maxPerUser {
		return nil, nil, ErrTooManyStreams
	}
	s := &Subscription{user: u, ready: make(chan struct{}, 1), pending: map[Event]uint64{}}
	if h.degraded {
		s.setDegraded(h.seq, h.reason)
	}
	h.subs[s] = struct{}{}
	h.perUser[key]++
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.subs, s)
			if h.perUser[key]--; h.perUser[key] <= 0 {
				delete(h.perUser, key)
			}
		})
	}
	return s, cancel, nil
}

// Subscribers is the number of open subscriptions (for tests and metrics).
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func userKey(u auth.User) string {
	if u.Subject != "" {
		return u.Subject
	}
	return "email:" + strings.ToLower(u.Email)
}

// Subscription is one open stream's queue. Wait on Ready, then call Take.
type Subscription struct {
	user  auth.User
	ready chan struct{}

	mu        sync.Mutex
	pending   map[Event]uint64 // event -> latest seq
	degSeq    uint64           // non-zero: a degraded notice is pending
	degReason string
}

// Ready is signalled (never blocks the sender) when Take has something.
func (s *Subscription) Ready() <-chan struct{} { return s.ready }

func (s *Subscription) notify() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

func (s *Subscription) addEvent(seq uint64, e Event) {
	s.mu.Lock()
	s.degSeq = 0 // an event reaches the client after it and clears degraded there
	if _, ok := s.pending[e]; !ok && len(s.pending) >= maxPending {
		clear(s.pending)
		e = Event{} // too many to list: refresh everything
	}
	s.pending[e] = seq
	s.mu.Unlock()
	s.notify()
}

func (s *Subscription) setDegraded(seq uint64, reason string) {
	s.mu.Lock()
	s.degSeq, s.degReason = seq, reason
	s.mu.Unlock()
	s.notify()
}

// Take returns what changed since the last call, oldest first, and empties the queue. An event that happened several
// times appears once, with its latest Seq.
func (s *Subscription) Take() []Frame {
	s.mu.Lock()
	out := make([]Frame, 0, len(s.pending)+1)
	for e, seq := range s.pending {
		out = append(out, Frame{Seq: seq, Event: e})
	}
	if s.degSeq != 0 {
		out = append(out, Frame{Seq: s.degSeq, Degraded: true, Reason: s.degReason})
	}
	clear(s.pending)
	s.degSeq = 0
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}
