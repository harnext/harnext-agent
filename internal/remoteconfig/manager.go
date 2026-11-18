package remoteconfig

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	DefaultPeriod      = 6 * time.Hour
	defaultJitter      = 0.1
	defaultBaseBackoff = time.Minute
)

// Fetcher is satisfied by *Client.
type Fetcher interface {
	Fetch(ctx context.Context, ifNoneMatch string) (*FetchResult, error)
}

// Persister is satisfied by *Store.
type Persister interface {
	Load() (StoredState, bool)
	Save(StoredState) error
}

// Clock makes time injectable for tests.
type Clock interface {
	Now() time.Time
	// Sleep waits for d or until ctx ends, returning ctx.Err() in that case.
	Sleep(ctx context.Context, d time.Duration) error
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
	Fetcher Fetcher     // required
	Keys    TrustedKeys // empty: nothing can be verified, Refresh makes no request
	Store   Persister   // optional; nil keeps state in memory only
	Clock   Clock       // default: system clock
	Period  time.Duration
	// Jitter is the fraction of Period added at random to each wait (default 0.1).
	Jitter float64
	// BaseBackoff is the first retry delay after a transient failure (default 1 minute);
	// it doubles per consecutive failure up to Period.
	BaseBackoff time.Duration
	Rand        func() float64 // [0,1); default math/rand/v2
}

// Status is a content-free snapshot for status output.
type Status struct {
	HasConfig       bool
	ConfigVersion   int64
	ExpiresAt       time.Time
	LastCheck       time.Time
	LastError       Code
	UpgradeRequired bool
	// PersistFailed is true when the last accepted configuration could not be written.
	PersistFailed bool
}

// Manager keeps the newest verified configuration, refreshes it periodically and is safe
// for concurrent use. A configuration is exposed only while now < expires_at.
type Manager struct {
	opts      ManagerOptions
	clock     Clock
	refreshMu sync.Mutex // serializes Refresh; network I/O happens outside mu

	mu              sync.RWMutex
	cfg             *Config
	env             Envelope
	etag            string
	floor           int64 // highest accepted config_version; never lowered
	lastCheck       time.Time
	lastErr         Code
	upgradeRequired bool
	persistFailed   bool
}

// NewManager loads and RE-VERIFIES the stored envelope. A stored file that fails
// verification or has expired is ignored (no configuration, friction stays null) and its
// ETag is dropped so the next refresh downloads a full response.
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
	if opts.Store == nil {
		return m
	}
	st, ok := opts.Store.Load()
	if !ok {
		return m
	}
	m.floor = st.LastAcceptedVersion
	m.lastCheck = st.LastCheck
	if st.Envelope.Payload == "" {
		return m
	}
	if cfg, err := Verify(st.Envelope, opts.Keys, m.clock.Now(), st.LastAcceptedVersion); err == nil {
		m.cfg, m.env, m.etag = cfg, st.Envelope, st.ETag
	}
	return m
}

// Current returns the verified, unexpired configuration or (nil, false). The result is shared
// and must not be modified.
func (m *Manager) Current() (*Config, bool) {
	now := m.clock.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cfg == nil || !now.Before(m.cfg.ExpiresAt) {
		return nil, false
	}
	return m.cfg, true
}

// Status returns a snapshot.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := Status{LastCheck: m.lastCheck, LastError: m.lastErr, UpgradeRequired: m.upgradeRequired, PersistFailed: m.persistFailed}
	if m.cfg != nil && m.clock.Now().Before(m.cfg.ExpiresAt) {
		st.HasConfig, st.ConfigVersion, st.ExpiresAt = true, m.cfg.ConfigVersion, m.cfg.ExpiresAt
	}
	return st
}

// Refresh fetches once. On 200 the envelope is verified before anything changes; a failed
// verification keeps the previous configuration. On 304 only the last-check time moves.
func (m *Manager) Refresh(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	if len(m.opts.Keys) == 0 {
		return m.fail(newError(CodeNoTrustedKeys))
	}

	m.mu.RLock()
	etag, floor := m.etag, m.floor
	haveValid := m.cfg != nil && m.clock.Now().Before(m.cfg.ExpiresAt)
	m.mu.RUnlock()
	inm := ""
	if haveValid {
		inm = etag
	}

	res, err := m.opts.Fetcher.Fetch(ctx, inm)
	if err != nil {
		return m.fail(err)
	}
	now := m.clock.Now()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.upgradeRequired = res.UpgradeRequired

	if res.NotModified {
		if m.cfg == nil || !now.Before(m.cfg.ExpiresAt) {
			// A 304 cannot revive an expired or missing configuration.
			m.etag = ""
			m.lastErr = CodeUnexpectedStatus
			return newError(CodeUnexpectedStatus)
		}
		m.lastCheck, m.lastErr = now, ""
		m.persistLocked()
		return nil
	}

	cfg, err := Verify(res.Envelope, m.opts.Keys, now, floor)
	if err != nil {
		m.lastErr = CodeOf(err)
		return err
	}
	m.cfg, m.env, m.etag = cfg, res.Envelope, res.ETag
	if cfg.ConfigVersion > m.floor {
		m.floor = cfg.ConfigVersion
	}
	m.lastCheck, m.lastErr = now, ""
	m.persistLocked()
	return nil
}

func (m *Manager) fail(err error) error {
	if c := CodeOf(err); c != "" {
		m.mu.Lock()
		m.lastErr = c
		m.mu.Unlock()
	}
	return err
}

// persistLocked writes the current state; m.mu must be held. A write failure does not
// undo the in-memory adoption; it is surfaced through Status.
func (m *Manager) persistLocked() {
	if m.opts.Store == nil {
		return
	}
	err := m.opts.Store.Save(StoredState{Envelope: m.env, ETag: m.etag, LastAcceptedVersion: m.floor, LastCheck: m.lastCheck})
	m.persistFailed = err != nil
}

// Run refreshes until ctx ends: once at start (unless a fresh check is already recorded),
// then every Period plus jitter. Transient failures back off exponentially with jitter and
// honor Retry-After; there is no fast retry loop. Run returns nil when ctx ends and an error
// only when no trusted keys exist (nothing could ever be verified).
func (m *Manager) Run(ctx context.Context) error {
	delay := m.initialDelay()
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
			// The token will not fix itself quickly; do not hammer the server.
			failures++
			delay = m.periodWait()
		default:
			failures++
			var retryAfter int
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

func (m *Manager) initialDelay() time.Duration {
	now := m.clock.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cfg == nil || !now.Before(m.cfg.ExpiresAt) || m.lastCheck.IsZero() {
		return 0
	}
	remaining := m.lastCheck.Add(m.opts.Period).Sub(now)
	if remaining <= 0 {
		return 0
	}
	if remaining > m.opts.Period {
		remaining = m.opts.Period // lastCheck in the future (clock change)
	}
	return remaining + time.Duration(m.opts.Rand()*m.opts.Jitter*float64(m.opts.Period))
}
