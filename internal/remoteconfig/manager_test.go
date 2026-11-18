package remoteconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreRoundTripAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	s := NewStore(filepath.Join(dir, "agent-config.json"))
	if _, ok := s.Load(); ok {
		t.Fatal("missing file reported as loaded")
	}
	want := StoredState{
		Envelope: Envelope{"p", "s", "k"}, ETag: `"e"`, LastAcceptedVersion: 7,
		LastCheck: time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Load()
	if !ok || got != want {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(s.path)
		di, _ := os.Stat(dir)
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Errorf("file %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover temporary files: %d entries", len(entries))
	}
	// Overwrite keeps working and stays atomic (no partial file visible).
	want.LastAcceptedVersion = 8
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(); got.LastAcceptedVersion != 8 {
		t.Errorf("overwrite: %+v", got)
	}
}

func TestStoreCorruptFilesMeanNoConfig(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"empty":         {},
		"garbage":       []byte("\x00\x01\x02 not json"),
		"wrong version": []byte(`{"v":99,"envelope":{"payload":"a","sig":"b","kid":"c"}}`),
		"negative":      []byte(`{"v":1,"last_accepted_version":-1}`),
		"oversized":     []byte(`{"v":1,"etag":"` + strings.Repeat("a", maxStoreBytes) + `"}`),
		"truncated":     []byte(`{"v":1,"envelope":{"payload":"a"`),
		"array":         []byte(`[]`),
	}
	for name, content := range cases {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if st, ok := NewStore(path).Load(); ok || st != (StoredState{}) {
			t.Errorf("%s: loaded %+v", name, st)
		}
	}
	if _, ok := NewStore(dir).Load(); ok { // a directory is unreadable as a file
		t.Error("directory loaded")
	}
}

func TestStoreSaveFailureIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission model differs")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent "directory" is a regular file.
	err := NewStore(filepath.Join(blocker, "sub", "cfg.json")).Save(StoredState{})
	if err == nil || strings.Contains(err.Error(), blocker) {
		t.Errorf("err = %v", err)
	}
}

// fakeClock advances only when Sleep is called.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	onSleep func(n int)
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	n := len(c.sleeps)
	cb := c.onSleep
	c.mu.Unlock()
	if cb != nil {
		cb(n)
	}
	return ctx.Err()
}

type harness struct {
	t     *testing.T
	sc    *scenario
	srv   *httptest.Server
	keys  TrustedKeys
	clock *fakeClock
	path  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	_, keys, at := loadVector(t)
	sc := &scenario{status: 200, env: vectorEnvelope(t, "valid"), etag: `"v2-day1"`}
	srv := httptest.NewServer(sc)
	t.Cleanup(srv.Close)
	return &harness{t: t, sc: sc, srv: srv, keys: keys, clock: newFakeClock(at), path: filepath.Join(t.TempDir(), "cfg", "agent-config.json")}
}

func (h *harness) manager() *Manager {
	return NewManager(ManagerOptions{
		Fetcher: newTestClient(h.t, h.srv.URL), Keys: h.keys, Store: NewStore(h.path), Clock: h.clock,
		Rand: func() float64 { return 0 },
	})
}

func (h *harness) offlineManager() *Manager {
	return NewManager(ManagerOptions{Fetcher: failingFetcher{h.t}, Keys: h.keys, Store: NewStore(h.path), Clock: h.clock})
}

func TestManagerAcceptAndRestart(t *testing.T) {
	h := newHarness(t)
	m := h.manager()
	if _, ok := m.Current(); ok {
		t.Fatal("config before first refresh")
	}
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg, ok := m.Current()
	if !ok || cfg.ConfigVersion != 2 {
		t.Fatalf("current = %+v %v", cfg, ok)
	}
	if st := m.Status(); !st.HasConfig || st.ConfigVersion != 2 || st.LastCheck != h.clock.Now() || st.LastError != "" || st.PersistFailed {
		t.Errorf("status = %+v", st)
	}

	// Restart: loaded from the store and re-verified, no network needed.
	m2 := h.offlineManager()
	cfg2, ok := m2.Current()
	if !ok || cfg2.ConfigVersion != 2 {
		t.Fatalf("after restart: %+v %v", cfg2, ok)
	}
}

func TestManagerNotModifiedOnlyUpdatesLastCheck(t *testing.T) {
	h := newHarness(t)
	m := h.manager()
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := h.clock.Now()
	h.clock.set(first.Add(24 * time.Hour))
	h.sc.set(func(s *scenario) { s.status = 304 })
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, inm, _, _ := h.sc.snapshot(); inm != `"v2-day1"` {
		t.Errorf("If-None-Match = %q", inm)
	}
	cfg, ok := m.Current()
	if !ok || cfg.ConfigVersion != 2 || cfg.ExpiresAt != time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) {
		t.Errorf("config changed on 304: %+v", cfg)
	}
	if st := m.Status(); st.LastCheck != first.Add(24*time.Hour) {
		t.Errorf("last check = %v", st.LastCheck)
	}
	if st, _ := NewStore(h.path).Load(); st.LastCheck != first.Add(24*time.Hour) || st.ETag != `"v2-day1"` || st.LastAcceptedVersion != 2 {
		t.Errorf("stored = %+v", st)
	}
}

func TestManagerFailuresKeepPreviousConfig(t *testing.T) {
	tests := []struct {
		name string
		set  func(*scenario, *harness)
		want Code
	}{
		{"unauthorized", func(s *scenario, h *harness) { s.status = 401 }, CodeUnauthorized},
		{"rate limited", func(s *scenario, h *harness) { s.status = 429; s.retryAfter = "60" }, CodeTransient},
		{"server error", func(s *scenario, h *harness) { s.status = 500 }, CodeTransient},
		{"bad signature", func(s *scenario, h *harness) { s.env = vectorEnvelope(h.t, "sig_tampered") }, CodeBadSignature},
		{"tampered payload", func(s *scenario, h *harness) { s.env = vectorEnvelope(h.t, "payload_tampered") }, CodeBadSignature},
		{"wrong key", func(s *scenario, h *harness) { s.env = vectorEnvelope(h.t, "wrong_key") }, CodeBadSignature},
		{"unknown kid", func(s *scenario, h *harness) { s.env = vectorEnvelope(h.t, "unknown_kid") }, CodeUnknownKID},
		{"rollback", func(s *scenario, h *harness) { s.env = vectorEnvelope(h.t, "rollback_v1_rejected") }, CodeRollback},
		{"expired", func(s *scenario, h *harness) { s.env = vectorEnvelope(h.t, "expired") }, CodeExpired},
		{"malformed", func(s *scenario, h *harness) { s.env = vectorEnvelope(h.t, "malformed_padding") }, CodeMalformed},
		{"garbage body", func(s *scenario, h *harness) { s.body = "garbage" }, CodeMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			m := h.manager()
			if err := m.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			before, _ := NewStore(h.path).Load()

			h.sc.set(func(s *scenario) { tt.set(s, h) })
			err := m.Refresh(context.Background())
			if CodeOf(err) != tt.want {
				t.Fatalf("code = %q (%v), want %q", CodeOf(err), err, tt.want)
			}
			cfg, ok := m.Current()
			if !ok || cfg.ConfigVersion != 2 {
				t.Errorf("previous config lost: %+v %v", cfg, ok)
			}
			if st := m.Status(); st.LastError != tt.want {
				t.Errorf("status error = %q", st.LastError)
			}
			// Nothing was written: the store (including the rollback floor) is untouched.
			if after, _ := NewStore(h.path).Load(); after != before {
				t.Errorf("store changed: %+v -> %+v", before, after)
			}
		})
	}
}

func TestManagerRollbackNeverLowersFloor(t *testing.T) {
	h := newHarness(t)
	m := h.manager()
	if err := m.Refresh(context.Background()); err != nil { // v2
		t.Fatal(err)
	}
	h.sc.set(func(s *scenario) { s.env = vectorEnvelope(t, "rollback_v1_rejected"); s.etag = `"v1"` })
	if err := m.Refresh(context.Background()); CodeOf(err) != CodeRollback {
		t.Fatalf("got %v", err)
	}
	st, _ := NewStore(h.path).Load()
	if st.LastAcceptedVersion != 2 || st.Envelope != vectorEnvelope(t, "valid") || st.ETag != `"v2-day1"` {
		t.Errorf("rollback altered the store: %+v", st)
	}
	// Even a fresh manager (restart) still refuses v1.
	m2 := h.manager()
	if err := m2.Refresh(context.Background()); CodeOf(err) != CodeRollback {
		t.Errorf("after restart: %v", err)
	}
	if cfg, ok := m2.Current(); !ok || cfg.ConfigVersion != 2 {
		t.Errorf("after restart: %+v %v", cfg, ok)
	}
}

func TestManagerFloorSurvivesExpiredEnvelope(t *testing.T) {
	s := newTestSigner(t, "k1")
	clock := newFakeClock(testNow)
	path := filepath.Join(t.TempDir(), "cfg.json")
	var gotINM []string
	serve := func(env Envelope, etag string) Fetcher {
		return fetchFunc(func(_ context.Context, inm string) (*FetchResult, error) {
			gotINM = append(gotINM, inm)
			return &FetchResult{Envelope: env, ETag: etag}, nil
		})
	}
	opts := func(f Fetcher) ManagerOptions {
		return ManagerOptions{Fetcher: f, Keys: s.keys, Store: NewStore(path), Clock: clock}
	}

	m := NewManager(opts(serve(s.signJSON(t, validPayload(5)), `"v5"`)))
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// After expiry the stored v5 is unusable, but the rollback floor (5) must remain.
	clock.set(time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC))
	newer := validPayload(3)
	newer["issued_at"], newer["expires_at"] = "2026-12-01T00:00:00.000Z", "2026-12-31T00:00:00.000Z"
	m2 := NewManager(opts(serve(s.signJSON(t, newer), `"v3"`)))
	if _, ok := m2.Current(); ok {
		t.Fatal("expired config served after restart")
	}
	if err := m2.Refresh(context.Background()); CodeOf(err) != CodeRollback {
		t.Errorf("v3 after v5: %v", err)
	}
	if gotINM[len(gotINM)-1] != "" {
		t.Errorf("sent If-None-Match %q although no valid config exists", gotINM[len(gotINM)-1])
	}
	newer["config_version"] = 5
	m3 := NewManager(opts(serve(s.signJSON(t, newer), `"v5b"`)))
	if err := m3.Refresh(context.Background()); err != nil {
		t.Errorf("re-issue of the same version: %v", err)
	}
}

func TestManagerOfflineUsesConfigUntilExpiry(t *testing.T) {
	h := newHarness(t)
	m := h.manager()
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.srv.Close()
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), true},
		{time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), false},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), false},
	} {
		h.clock.set(tc.at)
		if _, ok := m.Current(); ok != tc.want {
			t.Errorf("at %v: ok=%v want %v", tc.at, ok, tc.want)
		}
		if st := m.Status(); st.HasConfig != tc.want {
			t.Errorf("at %v: status.HasConfig=%v", tc.at, st.HasConfig)
		}
	}
	// A failed refresh while offline does not disturb a still valid config.
	h.clock.set(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	if err := m.Refresh(context.Background()); CodeOf(err) != CodeTransient {
		t.Errorf("offline refresh: %v", err)
	}
	if _, ok := m.Current(); !ok {
		t.Error("config dropped after failed refresh")
	}
}

func TestManagerRejectsTamperedOrExpiredStoreFile(t *testing.T) {
	h := newHarness(t)
	if err := h.manager().Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("tampered payload", func(t *testing.T) {
		var f map[string]any
		_ = json.Unmarshal(raw, &f)
		env := f["envelope"].(map[string]any)
		env["payload"] = vectorEnvelope(t, "payload_tampered").Payload
		b, _ := json.Marshal(f)
		if err := os.WriteFile(h.path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		m := h.offlineManager()
		if _, ok := m.Current(); ok {
			t.Error("tampered store accepted")
		}
	})
	t.Run("wrong key id", func(t *testing.T) {
		var f map[string]any
		_ = json.Unmarshal(raw, &f)
		f["envelope"].(map[string]any)["kid"] = "other-key"
		b, _ := json.Marshal(f)
		_ = os.WriteFile(h.path, b, 0o600)
		if _, ok := h.offlineManager().Current(); ok {
			t.Error("unknown kid accepted from store")
		}
	})
	t.Run("expired", func(t *testing.T) {
		_ = os.WriteFile(h.path, raw, 0o600)
		h.clock.set(time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC))
		if _, ok := h.offlineManager().Current(); ok {
			t.Error("expired store accepted")
		}
	})
	t.Run("keys removed from build", func(t *testing.T) {
		_ = os.WriteFile(h.path, raw, 0o600)
		h.clock.set(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC))
		m := NewManager(ManagerOptions{Fetcher: failingFetcher{t}, Keys: TrustedKeys{}, Store: NewStore(h.path), Clock: h.clock})
		if _, ok := m.Current(); ok {
			t.Error("config served without trusted keys")
		}
	})
	t.Run("corrupt file", func(t *testing.T) {
		_ = os.WriteFile(h.path, []byte("{{{"), 0o600)
		if _, ok := h.offlineManager().Current(); ok {
			t.Error("corrupt store accepted")
		}
	})
}

func TestManagerNotModifiedCannotReviveExpiredConfig(t *testing.T) {
	h := newHarness(t)
	m := h.manager()
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.clock.set(time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC))
	h.sc.set(func(s *scenario) { s.status = 304 })
	if err := m.Refresh(context.Background()); err == nil {
		t.Error("304 accepted for an expired config")
	}
	if _, ok := m.Current(); ok {
		t.Error("expired config served")
	}
}

func TestManagerUpgradeFlag(t *testing.T) {
	h := newHarness(t)
	h.sc.set(func(s *scenario) { s.upgrade = true })
	m := h.manager()
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.Status().UpgradeRequired {
		t.Error("upgrade flag not surfaced")
	}
	h.sc.set(func(s *scenario) { s.upgrade = false; s.status = 304 })
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Status().UpgradeRequired {
		t.Error("upgrade flag stuck")
	}
}

func TestManagerPersistFailureDoesNotLoseConfig(t *testing.T) {
	h := newHarness(t)
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	m := NewManager(ManagerOptions{Fetcher: newTestClient(t, h.srv.URL), Keys: h.keys, Store: NewStore(filepath.Join(blocker, "x", "c.json")), Clock: h.clock})
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Current(); !ok || !m.Status().PersistFailed {
		t.Errorf("ok=%v status=%+v", ok, m.Status())
	}
}

func TestManagerWithoutStore(t *testing.T) {
	h := newHarness(t)
	m := NewManager(ManagerOptions{Fetcher: newTestClient(t, h.srv.URL), Keys: h.keys, Clock: h.clock})
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Current(); !ok {
		t.Error("no config")
	}
}

func TestManagerConcurrentUse(t *testing.T) {
	h := newHarness(t)
	m := h.manager()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = m.Refresh(context.Background())
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.Current()
				m.Status()
			}
		}()
	}
	wg.Wait()
	if _, ok := m.Current(); !ok {
		t.Error("no config after concurrent refreshes")
	}
}

type fetchFunc func(ctx context.Context, inm string) (*FetchResult, error)

func (f fetchFunc) Fetch(ctx context.Context, inm string) (*FetchResult, error) { return f(ctx, inm) }

func TestManagerRunSchedule(t *testing.T) {
	_, keys, at := loadVector(t)
	valid := vectorEnvelope(t, "valid")
	clock := newFakeClock(at)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Script: transient x3, rate limit with Retry-After, unauthorized, success, success, then stop.
	script := []func() (*FetchResult, error){
		func() (*FetchResult, error) { return nil, newError(CodeTransient) },
		func() (*FetchResult, error) { return nil, newError(CodeTransient) },
		func() (*FetchResult, error) { return nil, newError(CodeTransient) },
		func() (*FetchResult, error) { return nil, &Error{Code: CodeTransient, RetryAfterSeconds: 5000} },
		func() (*FetchResult, error) { return nil, newError(CodeUnauthorized) },
		func() (*FetchResult, error) { return &FetchResult{Envelope: valid, ETag: `"e"`}, nil },
		func() (*FetchResult, error) { return &FetchResult{NotModified: true}, nil },
	}
	var calls int
	var inms []string
	f := fetchFunc(func(_ context.Context, inm string) (*FetchResult, error) {
		inms = append(inms, inm)
		r := script[calls]
		calls++
		return r()
	})
	clock.onSleep = func(n int) {
		if n >= len(script) {
			cancel()
		}
	}
	m := NewManager(ManagerOptions{Fetcher: f, Keys: keys, Clock: clock, Period: 6 * time.Hour, BaseBackoff: time.Minute, Rand: func() float64 { return 0 }})
	if err := m.Run(ctx); err != nil {
		t.Fatalf("Run = %v", err)
	}

	// Rand=0 gives the lower jitter bound: backoff d/2, period wait = Period.
	want := []time.Duration{
		0,                                                  // placeholder for "no initial sleep"
		30 * time.Second, 1 * time.Minute, 2 * time.Minute, // exponential backoff
		5000 * time.Second,                          // Retry-After beats backoff
		6 * time.Hour, 6 * time.Hour, 6 * time.Hour, // unauthorized waits a full period; then periodic
	}
	got := clock.sleeps
	if len(got) != len(want)-1 {
		t.Fatalf("sleeps = %v", got)
	}
	for i, w := range want[1:] {
		if got[i] != w {
			t.Errorf("sleep %d = %v, want %v (all: %v)", i, got[i], w, got)
		}
	}
	if calls != len(script) {
		t.Errorf("calls = %d", calls)
	}
	// After the success the next request is conditional.
	if inms[len(inms)-1] != `"e"` || inms[0] != "" {
		t.Errorf("If-None-Match sequence = %q", inms)
	}
}

func TestManagerRunBackoffIsCappedAndNeverZero(t *testing.T) {
	_, keys, at := loadVector(t)
	clock := newFakeClock(at)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := fetchFunc(func(context.Context, string) (*FetchResult, error) { return nil, newError(CodeTransient) })
	clock.onSleep = func(n int) {
		if n >= 30 {
			cancel()
		}
	}
	m := NewManager(ManagerOptions{Fetcher: f, Keys: keys, Clock: clock, Period: time.Hour, BaseBackoff: time.Minute, Rand: func() float64 { return 0.999 }})
	_ = m.Run(ctx)
	if len(clock.sleeps) < 10 {
		t.Fatalf("sleeps = %v", clock.sleeps)
	}
	for i, d := range clock.sleeps {
		if d < 30*time.Second || d > time.Hour {
			t.Errorf("sleep %d = %v out of [30s, 1h]", i, d)
		}
	}
	if last := clock.sleeps[len(clock.sleeps)-1]; last < 30*time.Minute {
		t.Errorf("backoff did not grow: last = %v", last)
	}
}

func TestManagerRunSkipsImmediateFetchAfterRecentCheck(t *testing.T) {
	h := newHarness(t)
	m := h.manager()
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Restart one hour later: the stored last check is recent, so Run waits instead of fetching.
	h.clock.set(h.clock.Now().Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	h.clock.onSleep = func(int) { cancel() }
	m2 := NewManager(ManagerOptions{Fetcher: failingFetcher{t}, Keys: h.keys, Store: NewStore(h.path), Clock: h.clock, Period: 6 * time.Hour, Rand: func() float64 { return 0 }})
	if err := m2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.clock.sleeps) != 1 || h.clock.sleeps[0] != 5*time.Hour {
		t.Errorf("sleeps = %v", h.clock.sleeps)
	}
}

func TestManagerRunStopsWithoutKeys(t *testing.T) {
	m := NewManager(ManagerOptions{Fetcher: failingFetcher{t}, Keys: nil, Clock: newFakeClock(testNow)})
	done := make(chan error, 1)
	go func() { done <- m.Run(context.Background()) }()
	select {
	case err := <-done:
		if CodeOf(err) != CodeNoTrustedKeys {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

var _ http.Handler = (*scenario)(nil)
