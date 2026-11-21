package identity

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

const (
	DefaultPeriod      = 5 * time.Minute
	defaultJitter      = 0.1
	defaultBaseBackoff = time.Minute
	// etagMinRemaining: a cached document with less validity left is refetched without If-None-Match. The server re-signs
	// every hour, so a document that old can only be matched by a stale cache; a 304 never extends expiry.
	etagMinRemaining = time.Hour
)

// Remote is satisfied by *Client.
type Remote interface {
	Fetch(ctx context.Context, ifNoneMatch string) (*FetchResult, error)
	Report(ctx context.Context, accounts []Account) (*FetchResult, error)
}

// Persister is satisfied by *Store.
type Persister interface {
	Load() (StoredState, bool)
	Save(StoredState) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type ManagerOptions struct {
	Remote Remote                   // required
	Keys   remoteconfig.TrustedKeys // empty: nothing can be verified, no request is made
	Store  Persister                // optional; nil keeps state in memory only
	Clock  remoteconfig.Clock       // default: system clock
	Period time.Duration            // default DefaultPeriod
	// Jitter is the fraction of Period added at random to each wait (default 0.1).
	Jitter float64
	// BaseBackoff is the first retry delay after a failure (default 1 minute); it doubles per consecutive failure up to Period.
	BaseBackoff time.Duration
	Rand        func() float64 // [0,1); default math/rand/v2
	// OnChange runs (outside any lock, in the goroutine that adopted the document) after a document is adopted that makes the
	// document valid again or changes the (tool, hash) -> route set. It does not fire for expiry or at construction.
	OnChange func()
}

// Status is a content-free snapshot for status output.
type Status struct {
	HasDocument     bool // a verified document is held (it may be expired)
	Valid           bool
	IdentityVersion int64
	IssuedAt        time.Time
	ExpiresAt       time.Time
	LastCheck       time.Time
	LastError       Code
	// Bindings is the number of (tool, account) bindings in the held document.
	Bindings int
	// PersistFailed is true when the last accepted document could not be written.
	PersistFailed bool
}

type routeKey struct{ tool, hash string }

// Snapshot is an immutable view of the document at the time of Current. It re-checks expiry on every call, so a snapshot
// that is held past expiry stops resolving. The zero Snapshot is invalid.
type Snapshot struct {
	version   int64
	issuedAt  time.Time
	expiresAt time.Time
	routes    map[routeKey]string
	now       func() time.Time
}

// Valid reports whether a verified document is held and not yet expired.
func (s Snapshot) Valid() bool { return s.now != nil && s.now().Before(s.expiresAt) }

// RouteFor returns the route id bound to (tool, hash); false when there is no binding or the document is not valid.
func (s Snapshot) RouteFor(tool, hash string) (routeID string, ok bool) {
	if !s.Valid() {
		return "", false
	}
	routeID, ok = s.routes[routeKey{tool, hash}]
	return routeID, ok
}

// IdentityVersion is the document's content fingerprint (equality only); 0 without a document.
func (s Snapshot) IdentityVersion() int64 { return s.version }

// IssuedAt is the document's issue time; zero without a document.
func (s Snapshot) IssuedAt() time.Time { return s.issuedAt }

// ExpiresAt is the document's expiry; zero without a document.
func (s Snapshot) ExpiresAt() time.Time { return s.expiresAt }

// Manager keeps the newest verified identity document, refreshes it periodically and is safe for concurrent use. Bindings
// resolve only while now < expires_at; otherwise it fails closed.
type Manager struct {
	opts      ManagerOptions
	clock     remoteconfig.Clock
	refreshMu sync.Mutex // serializes Refresh and Report; network I/O happens outside mu

	mu            sync.RWMutex
	doc           *Document
	env           remoteconfig.Envelope
	etag          string // belongs to env; empty when unknown (after a report response)
	floor         time.Time
	snap          Snapshot
	lastCheck     time.Time
	lastErr       Code
	persistFailed bool
}

// NewManager loads and RE-VERIFIES the stored envelope. A stored file that fails verification or has expired is ignored (no
// document, everything unresolved) and its ETag is dropped; the rollback floor survives either way.
func NewManager(opts ManagerOptions) *Manager {
	m := &Manager{opts: opts, clock: opts.Clock}
	if m.clock == nil {
		m.clock = realClock{}
	}
	if m.opts.Period <= 0 {
		m.opts.Period = DefaultPeriod
	}
	if m.opts.Jitter <= 0 {
		m.opts.Jitter = defaultJitter
	}
	if m.opts.BaseBackoff <= 0 {
		m.opts.BaseBackoff = defaultBaseBackoff
	}
	if m.opts.Rand == nil {
		m.opts.Rand = rand.Float64
	}
	m.snap = Snapshot{now: m.clock.Now}
	if opts.Store == nil {
		return m
	}
	st, ok := opts.Store.Load()
	if !ok {
		return m
	}
	m.floor = st.LastAcceptedIssuedAt
	m.lastCheck = st.LastCheck
	if st.Envelope.Payload == "" {
		return m
	}
	if doc, err := VerifyEnvelope(st.Envelope, opts.Keys, m.clock.Now(), st.LastAcceptedIssuedAt); err == nil {
		m.setLocked(doc, st.Envelope, st.ETag)
	}
	return m
}

func (m *Manager) setLocked(doc Document, env remoteconfig.Envelope, etag string) {
	routes := make(map[routeKey]string, len(doc.Bindings))
	for _, b := range doc.Bindings {
		routes[routeKey{b.Tool, b.Hash}] = b.RouteID
	}
	m.doc, m.env, m.etag = &doc, env, etag
	m.snap = Snapshot{version: doc.IdentityVersion, issuedAt: doc.IssuedAt, expiresAt: doc.ExpiresAt, routes: routes, now: m.clock.Now}
	if doc.IssuedAt.After(m.floor) {
		m.floor = doc.IssuedAt
	}
}

// Current returns the current snapshot. Check Valid (or use RouteFor, which does) on every use.
func (m *Manager) Current() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snap
}

// Status returns a snapshot for status output.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := Status{LastCheck: m.lastCheck, LastError: m.lastErr, PersistFailed: m.persistFailed}
	if m.doc != nil {
		st.HasDocument, st.Valid = true, m.snap.Valid()
		st.IdentityVersion, st.IssuedAt, st.ExpiresAt = m.doc.IdentityVersion, m.doc.IssuedAt, m.doc.ExpiresAt
		st.Bindings = len(m.doc.Bindings)
	}
	return st
}

// Refresh fetches once. On 200 the envelope is verified before anything changes; a failed verification keeps the previous
// document. A 304 only moves the last-check time: it renews nothing, and it is only requested (If-None-Match) while the cached
// document has more than an hour of validity left, so near expiry the full signed document is always fetched again.
func (m *Manager) Refresh(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	if len(m.opts.Keys) == 0 {
		return m.fail(newError(CodeNoTrustedKeys))
	}
	m.mu.RLock()
	inm := ""
	if m.doc != nil && m.doc.ExpiresAt.Sub(m.clock.Now()) > etagMinRemaining {
		inm = m.etag
	}
	m.mu.RUnlock()

	res, err := m.opts.Remote.Fetch(ctx, inm)
	if err != nil {
		return m.fail(err)
	}
	if res.NotModified {
		return m.notModified(inm)
	}
	return m.adopt(res.Envelope, res.ETag)
}

// Report sends the accounts and adopts the verified document from the response. An invalid report is not sent.
func (m *Manager) Report(ctx context.Context, accounts []Account) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	if len(m.opts.Keys) == 0 {
		return m.fail(newError(CodeNoTrustedKeys))
	}
	res, err := m.opts.Remote.Report(ctx, accounts)
	if err != nil {
		return m.fail(err)
	}
	// The response carries no ETag, so the next Refresh downloads the full document.
	return m.adopt(res.Envelope, "")
}

func (m *Manager) notModified(sent string) error {
	now := m.clock.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if sent == "" || m.doc == nil || !now.Before(m.doc.ExpiresAt) {
		m.etag = ""
		m.lastErr = CodeUnexpectedStatus
		return newError(CodeUnexpectedStatus)
	}
	m.lastCheck, m.lastErr = now, ""
	m.persistLocked()
	return nil
}

// adopt verifies env and, only if it passes, makes it current and notifies OnChange.
func (m *Manager) adopt(env remoteconfig.Envelope, etag string) error {
	now := m.clock.Now()
	m.mu.Lock()
	doc, err := VerifyEnvelope(env, m.opts.Keys, now, m.floor)
	if err != nil {
		m.lastErr = CodeOf(err)
		m.mu.Unlock()
		return err
	}
	wasValid := m.snap.Valid()
	old := m.snap.routes
	m.setLocked(doc, env, etag)
	changed := !wasValid || !sameRoutes(old, m.snap.routes)
	m.lastCheck, m.lastErr = now, ""
	m.persistLocked()
	m.mu.Unlock()
	if changed && m.opts.OnChange != nil {
		m.opts.OnChange()
	}
	return nil
}

func sameRoutes(a, b map[routeKey]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func (m *Manager) fail(err error) error {
	if c := CodeOf(err); c != "" {
		m.mu.Lock()
		m.lastErr = c
		m.mu.Unlock()
	}
	return err
}

// persistLocked writes the current state; m.mu must be held. A write failure does not undo the in-memory adoption; it is
// surfaced through Status.
func (m *Manager) persistLocked() {
	if m.opts.Store == nil {
		return
	}
	err := m.opts.Store.Save(StoredState{Envelope: m.env, ETag: m.etag, LastAcceptedIssuedAt: m.floor, LastCheck: m.lastCheck})
	m.persistFailed = err != nil
}

// Run refreshes until ctx ends: at once, then every Period plus jitter. Failures back off exponentially with jitter and honor
// Retry-After; an unauthorized token waits a full period (no hammering). Run returns nil when ctx ends and an error only when
// no trusted keys exist.
func (m *Manager) Run(ctx context.Context) error {
	var delay time.Duration
	failures := 0
	for {
		if delay > 0 {
			if err := m.clock.Sleep(ctx, delay); err != nil {
				return nil
			}
		}
		err := m.Refresh(ctx)
		switch code := CodeOf(err); {
		case err == nil:
			failures = 0
			delay = m.periodWait()
		case ctx.Err() != nil:
			return nil
		case code == CodeNoTrustedKeys:
			return err
		case code == CodeUnauthorized:
			failures++
			delay = m.periodWait()
		default:
			failures++
			retryAfter := 0
			if e, ok := err.(*Error); ok {
				retryAfter = e.RetryAfterSeconds
			}
			delay = m.backoff(failures, retryAfter)
		}
	}
}

func (m *Manager) periodWait() time.Duration {
	p := m.opts.Period
	return p + time.Duration(m.opts.Rand()*m.opts.Jitter*float64(p))
}

func (m *Manager) backoff(failures, retryAfterSeconds int) time.Duration {
	d := m.opts.BaseBackoff
	for i := 1; i < failures && d < m.opts.Period; i++ {
		d *= 2
	}
	if d > m.opts.Period {
		d = m.opts.Period
	}
	d = d/2 + time.Duration(m.opts.Rand()*float64(d/2)) // jitter in [d/2, d)
	if ra := time.Duration(retryAfterSeconds) * time.Second; ra > d {
		d = ra
	}
	if d <= 0 {
		d = m.opts.BaseBackoff
	}
	return d
}
