package identity

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	hashC  = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	routeC = "33333333-3333-4333-8333-333333333333"
)

type rig struct {
	t       *testing.T
	s       signer
	f       *fakeServer
	clk     *fakeClock
	changes atomic.Int32
	store   string
	m       *Manager
}

func newRig(t *testing.T, mod ...func(*ManagerOptions)) *rig {
	t.Helper()
	s := newSigner(t, "k1")
	now := t0.Add(10 * time.Minute)
	r := &rig{t: t, s: s, f: newFakeServer(t, s, now), clk: &fakeClock{now: now}, store: filepath.Join(t.TempDir(), "agent-identity.json")}
	r.m = r.newManager(mod...)
	return r
}

// newManager builds another manager on the same store, server and clock.
func (r *rig) newManager(mod ...func(*ManagerOptions)) *Manager {
	opts := ManagerOptions{
		Remote: r.f.client(r.t), Keys: r.s.keys(), Store: NewStore(r.store), Clock: r.clk,
		Rand: func() float64 { return 0 }, OnChange: func() { r.changes.Add(1) },
	}
	for _, m := range mod {
		m(&opts)
	}
	return NewManager(opts)
}

func (r *rig) setTime(now time.Time) {
	r.clk.mu.Lock()
	r.clk.now = now
	r.clk.mu.Unlock()
	r.f.set(func(f *fakeServer) { f.now = now })
}

func (r *rig) advance(d time.Duration) { r.setTime(r.clk.Now().Add(d)) }

func (r *rig) mustRefresh() {
	r.t.Helper()
	if err := r.m.Refresh(context.Background()); err != nil {
		r.t.Fatalf("refresh: %v", err)
	}
}

func lastINM(f *fakeServer) string {
	v := f.view().ifNoneMatch
	return v[len(v)-1]
}

func TestFailClosedWithoutDocument(t *testing.T) {
	r := newRig(t)
	if snap := r.m.Current(); snap.Valid() || snap.IdentityVersion() != 0 || !snap.IssuedAt().IsZero() {
		t.Fatalf("fresh manager is not closed: %+v", snap)
	}
	if _, ok := r.m.Current().RouteFor(ToolClaudeCode, hashA); ok {
		t.Fatal("route resolved without a document")
	}
	if _, ok := (Snapshot{}).RouteFor(ToolClaudeCode, hashA); ok || (Snapshot{}).Valid() {
		t.Fatal("zero snapshot resolves")
	}
	r.f.set(func(f *fakeServer) { f.failStatus = 500 })
	if err := r.m.Refresh(context.Background()); CodeOf(err) != CodeTransient {
		t.Fatalf("err = %v", err)
	}
	if r.m.Current().Valid() || r.m.Status().LastError != CodeTransient || r.changes.Load() != 0 {
		t.Fatal("a failed fetch opened the gate")
	}
}

func TestRefreshAndLookup(t *testing.T) {
	r := newRig(t)
	r.mustRefresh()
	snap := r.m.Current()
	if !snap.Valid() {
		t.Fatal("not valid")
	}
	if id, ok := snap.RouteFor(ToolClaudeCode, hashA); !ok || id != routeA {
		t.Errorf("claude route = %q %v", id, ok)
	}
	if id, ok := snap.RouteFor(ToolVSCodeCopilot, hashB); !ok || id != routeB {
		t.Errorf("copilot route = %q %v", id, ok)
	}
	for _, c := range [][2]string{{ToolVSCodeCopilot, hashA}, {ToolClaudeCode, hashC}, {"", ""}, {ToolClaudeCode, strings.ToUpper(hashA)}} {
		if id, ok := snap.RouteFor(c[0], c[1]); ok || id != "" {
			t.Errorf("RouteFor%v = %q %v", c, id, ok)
		}
	}
	if snap.IdentityVersion() != fingerprint(defaultBindings()) || !snap.IssuedAt().Equal(t0) || !snap.ExpiresAt().Equal(t0.Add(24*time.Hour)) {
		t.Errorf("version %d issued %v expires %v", snap.IdentityVersion(), snap.IssuedAt(), snap.ExpiresAt())
	}
	if r.changes.Load() != 1 {
		t.Errorf("OnChange fired %d times", r.changes.Load())
	}
	st := r.m.Status()
	if !st.HasDocument || !st.Valid || st.LastError != "" || st.PersistFailed || !st.LastCheck.Equal(r.clk.Now()) {
		t.Errorf("status = %+v", st)
	}
}

func TestNotModifiedKeepsDocumentAndRenewsNothing(t *testing.T) {
	r := newRig(t)
	r.mustRefresh()
	before := r.m.Current()
	r.advance(5 * time.Minute)
	r.mustRefresh()
	if lastINM(r.f) == "" {
		t.Fatal("second refresh did not send If-None-Match")
	}
	after := r.m.Current()
	if !after.ExpiresAt().Equal(before.ExpiresAt()) || !after.IssuedAt().Equal(before.IssuedAt()) {
		t.Errorf("304 changed validity: %v -> %v", before.ExpiresAt(), after.ExpiresAt())
	}
	if r.changes.Load() != 1 {
		t.Errorf("OnChange fired on a 304: %d", r.changes.Load())
	}
	if !r.m.Status().LastCheck.Equal(r.clk.Now()) {
		t.Error("last check not moved")
	}

	// The hour rolls over: a new signed document with the same bindings extends validity, without OnChange.
	r.advance(time.Hour)
	r.mustRefresh()
	if !r.m.Current().IssuedAt().Equal(t0.Add(time.Hour)) || !r.m.Current().ExpiresAt().Equal(t0.Add(25*time.Hour)) {
		t.Errorf("hourly re-sign not adopted: %v", r.m.Current().IssuedAt())
	}
	if r.changes.Load() != 1 {
		t.Errorf("OnChange fired for identical bindings: %d", r.changes.Load())
	}
}

func TestIfNoneMatchOnlyWhileMoreThanAnHourRemains(t *testing.T) {
	r := newRig(t)
	r.mustRefresh() // document expires at t0+24h

	r.clk.mu.Lock()
	r.clk.now = t0.Add(24*time.Hour - 61*time.Minute)
	r.clk.mu.Unlock()
	r.mustRefresh()
	if lastINM(r.f) == "" {
		t.Error("ETag withheld with 61 minutes left")
	}
	r.clk.mu.Lock()
	r.clk.now = t0.Add(24*time.Hour - 59*time.Minute)
	r.clk.mu.Unlock()
	r.mustRefresh()
	if lastINM(r.f) != "" {
		t.Error("ETag sent with 59 minutes left")
	}
	if !r.m.Current().Valid() {
		t.Error("document lost")
	}
}

func TestExpiryFailsClosedAndRecovers(t *testing.T) {
	r := newRig(t)
	r.mustRefresh()
	held := r.m.Current()
	if !held.Valid() {
		t.Fatal("not valid")
	}
	r.clk.mu.Lock()
	r.clk.now = t0.Add(24 * time.Hour) // now == expires_at
	r.clk.mu.Unlock()
	if held.Valid() {
		t.Error("held snapshot still valid at expiry")
	}
	if _, ok := held.RouteFor(ToolClaudeCode, hashA); ok {
		t.Error("held snapshot still resolves after expiry")
	}
	if _, ok := r.m.Current().RouteFor(ToolClaudeCode, hashA); ok || r.m.Current().Valid() || r.m.Status().Valid {
		t.Error("manager resolves after expiry")
	}
	if !r.m.Status().HasDocument {
		t.Error("status lost the expired document")
	}

	r.f.set(func(f *fakeServer) { f.failStatus = 500 })
	if err := r.m.Refresh(context.Background()); CodeOf(err) != CodeTransient || r.m.Current().Valid() {
		t.Fatalf("outage after expiry: err=%v valid=%v", err, r.m.Current().Valid())
	}
	if lastINM(r.f) != "" {
		t.Error("expired document sent an ETag")
	}

	r.f.set(func(f *fakeServer) { f.failStatus = 0 })
	r.setTime(t0.Add(25 * time.Hour))
	r.mustRefresh()
	if !r.m.Current().Valid() {
		t.Fatal("did not recover")
	}
	if r.changes.Load() != 2 {
		t.Errorf("OnChange after recovery: %d, want 2", r.changes.Load())
	}
}

func TestOnChangeOnBindingChange(t *testing.T) {
	r := newRig(t)
	r.mustRefresh()
	r.f.set(func(f *fakeServer) { f.bindings = []Binding{{ToolClaudeCode, hashA, routeC}} })
	r.advance(time.Minute)
	r.mustRefresh()
	if id, _ := r.m.Current().RouteFor(ToolClaudeCode, hashA); id != routeC {
		t.Errorf("route = %q", id)
	}
	if _, ok := r.m.Current().RouteFor(ToolVSCodeCopilot, hashB); ok {
		t.Error("removed binding still resolves")
	}
	if r.changes.Load() != 2 {
		t.Errorf("OnChange = %d, want 2", r.changes.Load())
	}
	// Empty list is a valid document; it changes the set and resolves nothing.
	r.f.set(func(f *fakeServer) { f.bindings = nil })
	r.advance(time.Minute)
	r.mustRefresh()
	if !r.m.Current().Valid() || r.changes.Load() != 3 {
		t.Errorf("valid=%v changes=%d", r.m.Current().Valid(), r.changes.Load())
	}
	if _, ok := r.m.Current().RouteFor(ToolClaudeCode, hashA); ok {
		t.Error("empty document resolves")
	}
}

func TestRollbackKeepsCurrentDocument(t *testing.T) {
	r := newRig(t)
	r.mustRefresh() // floor = t0
	r.f.set(func(f *fakeServer) { f.now = t0.Add(-time.Hour) })
	err := r.m.Refresh(context.Background())
	if CodeOf(err) != CodeRollback {
		t.Fatalf("err = %v", err)
	}
	if !r.m.Current().Valid() || !r.m.Current().IssuedAt().Equal(t0) {
		t.Error("rollback replaced or dropped the document")
	}
	if r.m.Status().LastError != CodeRollback {
		t.Errorf("last error = %q", r.m.Status().LastError)
	}
}

func TestBadSignatureAndWrongKeyAreRejected(t *testing.T) {
	r := newRig(t)
	other := newSigner(t, "k1")
	r.f.set(func(f *fakeServer) { f.s = other })
	if err := r.m.Refresh(context.Background()); CodeOf(err) != CodeBadSignature || r.m.Current().Valid() {
		t.Fatalf("wrong key: err=%v valid=%v", err, r.m.Current().Valid())
	}
	r.f.set(func(f *fakeServer) { f.s = newSigner(t, "k2") })
	if err := r.m.Refresh(context.Background()); CodeOf(err) != CodeUnknownKID || r.m.Current().Valid() {
		t.Fatalf("unknown kid: err=%v", err)
	}
	// A good document, then a forged one: the good one stays.
	r.f.set(func(f *fakeServer) { f.s = r.s })
	r.mustRefresh()
	r.f.set(func(f *fakeServer) { f.s = other; f.bindings = []Binding{{ToolClaudeCode, hashA, routeC}} })
	r.advance(time.Minute)
	if err := r.m.Refresh(context.Background()); CodeOf(err) != CodeBadSignature {
		t.Fatalf("forged: %v", err)
	}
	if id, _ := r.m.Current().RouteFor(ToolClaudeCode, hashA); id != routeA {
		t.Errorf("forged binding adopted: %q", id)
	}
}

func TestGarbageResponseKeepsCurrentDocument(t *testing.T) {
	r := newRig(t)
	r.mustRefresh()
	r.f.set(func(f *fakeServer) { f.rawResponse = []byte(`{"payload":"!!","sig":"x","kid":"k1"}`); f.bindings = nil })
	r.advance(time.Minute)
	if err := r.m.Refresh(context.Background()); CodeOf(err) != CodeMalformed {
		t.Fatalf("err = %v", err)
	}
	if _, ok := r.m.Current().RouteFor(ToolClaudeCode, hashA); !ok {
		t.Error("document dropped")
	}
}

func TestCacheReload(t *testing.T) {
	r := newRig(t)
	r.mustRefresh()

	// A restart with the server down still resolves from the verified cache.
	r.f.set(func(f *fakeServer) { f.failStatus = 500 })
	m2 := r.newManager()
	if !m2.Current().Valid() {
		t.Fatal("cache not loaded")
	}
	if id, ok := m2.Current().RouteFor(ToolVSCodeCopilot, hashB); !ok || id != routeB {
		t.Errorf("route = %q %v", id, ok)
	}
	if r.changes.Load() != 1 {
		t.Errorf("construction fired OnChange: %d", r.changes.Load())
	}

	// The cached ETag is reused, and a 304 keeps the cached document.
	r.f.set(func(f *fakeServer) { f.failStatus = 0 })
	if err := m2.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lastINM(r.f) == "" || !m2.Current().Valid() {
		t.Errorf("cached ETag not sent or document lost (inm %q)", lastINM(r.f))
	}
}

func TestCacheIsReverified(t *testing.T) {
	r := newRig(t)
	r.mustRefresh()
	r.f.set(func(f *fakeServer) { f.failStatus = 500 })

	// Expired cache: ignored.
	r.clk.advance(24 * time.Hour)
	if m := r.newManager(); m.Current().Valid() {
		t.Error("expired cache accepted")
	}
	r.clk.advance(-24 * time.Hour)

	// Different trusted key: ignored.
	if m := r.newManager(func(o *ManagerOptions) { o.Keys = newSigner(t, "k1").keys() }); m.Current().Valid() {
		t.Error("cache accepted under a different key")
	}
	if m := r.newManager(func(o *ManagerOptions) { o.Keys = nil }); m.Current().Valid() {
		t.Error("cache accepted without keys")
	}

	// Edited cache: the payload of another signed document under this document's signature.
	st, _ := NewStore(r.store).Load()
	st.Envelope.Payload = r.s.envelopeAt(t0, []Binding{{ToolClaudeCode, hashC, routeC}}).Payload
	if err := NewStore(r.store).Save(st); err != nil {
		t.Fatal(err)
	}
	if m := r.newManager(); m.Current().Valid() {
		t.Error("edited cache accepted")
	}
}

func TestRollbackFloorSurvivesRestart(t *testing.T) {
	r := newRig(t)
	r.advance(2 * time.Hour)
	r.mustRefresh() // floor = t0+2h
	st, ok := NewStore(r.store).Load()
	if !ok || !st.LastAcceptedIssuedAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("stored floor = %v", st.LastAcceptedIssuedAt)
	}
	// Lose the cached document but not the floor, then let the server replay an older document.
	st.Envelope.Payload = ""
	if err := NewStore(r.store).Save(st); err != nil {
		t.Fatal(err)
	}
	m2 := r.newManager()
	if m2.Current().Valid() {
		t.Fatal("document without payload accepted")
	}
	r.f.set(func(f *fakeServer) { f.now = t0.Add(time.Hour) })
	if err := m2.Refresh(context.Background()); CodeOf(err) != CodeRollback || m2.Current().Valid() {
		t.Fatalf("replay after restart: err=%v", err)
	}
	r.f.set(func(f *fakeServer) { f.now = t0.Add(2 * time.Hour) })
	if err := m2.Refresh(context.Background()); err != nil {
		t.Fatalf("current document after restart: %v", err)
	}
}

type failingStore struct{ saves atomic.Int32 }

func (*failingStore) Load() (StoredState, bool) { return StoredState{}, false }
func (s *failingStore) Save(StoredState) error  { s.saves.Add(1); return errors.New("disk full") }

func TestPersistFailureDoesNotUndoAdoption(t *testing.T) {
	fs := &failingStore{}
	r := newRig(t, func(o *ManagerOptions) { o.Store = fs })
	r.mustRefresh()
	if !r.m.Current().Valid() || !r.m.Status().PersistFailed || fs.saves.Load() == 0 {
		t.Errorf("valid=%v status=%+v", r.m.Current().Valid(), r.m.Status())
	}
}

func TestReportAdoptsResponse(t *testing.T) {
	r := newRig(t)
	r.mustRefresh()
	r.f.set(func(f *fakeServer) {
		f.onReport = func(acc []Account) []Binding {
			return append(defaultBindings(), Binding{acc[0].Tool, acc[0].Hash, routeC})
		}
	})
	r.advance(time.Minute)
	if err := r.m.Report(context.Background(), []Account{{ToolClaudeCode, hashC, "w***@corp.example", 4}}); err != nil {
		t.Fatal(err)
	}
	if id, ok := r.m.Current().RouteFor(ToolClaudeCode, hashC); !ok || id != routeC {
		t.Errorf("route = %q %v", id, ok)
	}
	if r.changes.Load() != 2 {
		t.Errorf("OnChange = %d, want 2", r.changes.Load())
	}
	if st, _ := NewStore(r.store).Load(); st.ETag != "" || st.Envelope.Payload == "" {
		t.Errorf("stored state after report: etag %q", st.ETag)
	}
	// The response has no ETag, so the next refresh downloads the full document, and the one after that can be a 304.
	r.mustRefresh()
	if lastINM(r.f) != "" {
		t.Error("ETag sent for a document received from a report")
	}
	r.mustRefresh()
	if lastINM(r.f) == "" {
		t.Error("ETag not sent after the full download")
	}
}

func TestReportFailuresChangeNothing(t *testing.T) {
	r := newRig(t)
	r.mustRefresh()
	good := []Account{{ToolClaudeCode, hashA, "c***@gmail.com", 1}}

	if err := r.m.Report(context.Background(), []Account{{ToolClaudeCode, hashA, "c@gmail.com", 1}}); CodeOf(err) != CodeInvalidReport {
		t.Errorf("plain e-mail: %v", err)
	}
	if v := r.f.view(); v.posts != 0 {
		t.Errorf("%d posts for an invalid report", v.posts)
	}
	r.f.set(func(f *fakeServer) { f.failStatus = 503 })
	if err := r.m.Report(context.Background(), good); CodeOf(err) != CodeTransient {
		t.Errorf("503: %v", err)
	}
	r.f.set(func(f *fakeServer) { f.failStatus = 0; f.s = newSigner(t, "k1"); f.bindings = nil })
	if err := r.m.Report(context.Background(), good); CodeOf(err) != CodeBadSignature {
		t.Errorf("forged response: %v", err)
	}
	if _, ok := r.m.Current().RouteFor(ToolClaudeCode, hashA); !ok || r.changes.Load() != 1 {
		t.Error("state changed by a failed report")
	}
}

func TestNoTrustedKeysMakesNoRequest(t *testing.T) {
	r := newRig(t, func(o *ManagerOptions) { o.Keys = nil })
	if err := r.m.Refresh(context.Background()); CodeOf(err) != CodeNoTrustedKeys {
		t.Errorf("refresh: %v", err)
	}
	if err := r.m.Report(context.Background(), nil); CodeOf(err) != CodeNoTrustedKeys {
		t.Errorf("report: %v", err)
	}
	if err := r.m.Run(context.Background()); CodeOf(err) != CodeNoTrustedKeys {
		t.Errorf("run: %v", err)
	}
	if v := r.f.view(); len(v.auth) != 0 {
		t.Error("a request was made")
	}
}

// runFor runs the manager until onSleep(n) cancels the context.
func runFor(r *rig, stopAt int, each func(n int)) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.clk.onSleep = func(n int) error {
		if each != nil {
			each(n)
		}
		if n >= stopAt {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if err := r.m.Run(ctx); err != nil {
		r.t.Fatalf("run: %v", err)
	}
}

func TestRunRefreshesPeriodically(t *testing.T) {
	r := newRig(t, func(o *ManagerOptions) { o.Period = 5 * time.Minute })
	runFor(r, 3, nil)
	for _, d := range r.clk.sleepLog() {
		if d != 5*time.Minute {
			t.Errorf("sleep = %v", d)
		}
	}
	if v := r.f.view(); v.gets != 3 || !r.m.Current().Valid() {
		t.Errorf("gets = %d valid = %v", v.gets, r.m.Current().Valid())
	}
}

func TestRunDefaultsAndJitter(t *testing.T) {
	r := newRig(t, func(o *ManagerOptions) { o.Rand = func() float64 { return 0.5 } })
	if r.m.opts.Period != 5*time.Minute {
		t.Errorf("default period = %v", r.m.opts.Period)
	}
	runFor(r, 1, nil)
	if got := r.clk.sleepLog()[0]; got != 5*time.Minute+15*time.Second {
		t.Errorf("sleep with jitter = %v", got)
	}
}

func TestRunBacksOffOnErrorsAndRecovers(t *testing.T) {
	r := newRig(t, func(o *ManagerOptions) { o.Period = 10 * time.Minute; o.BaseBackoff = time.Minute })
	r.f.set(func(f *fakeServer) { f.failStatus = 500 })
	runFor(r, 5, func(n int) {
		if n == 4 {
			r.f.set(func(f *fakeServer) { f.failStatus = 0 })
		}
	})
	got := r.clk.sleepLog()
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("sleep %d = %v, want %v", i, got[i], w)
		}
	}
	if got[4] != 10*time.Minute {
		t.Errorf("after recovery sleep = %v, want the period", got[4])
	}
	if !r.m.Current().Valid() {
		t.Error("did not recover")
	}
}

func TestRunHonorsRetryAfterAndUnauthorized(t *testing.T) {
	r := newRig(t, func(o *ManagerOptions) { o.Period = 10 * time.Minute })
	r.f.set(func(f *fakeServer) { f.failStatus, f.retryAfter = 429, "900" })
	runFor(r, 1, nil)
	if got := r.clk.sleepLog()[0]; got != 15*time.Minute {
		t.Errorf("retry-after sleep = %v", got)
	}

	r2 := newRig(t, func(o *ManagerOptions) { o.Period = 10 * time.Minute })
	r2.f.set(func(f *fakeServer) { f.failStatus = 401 })
	runFor(r2, 2, nil)
	for _, d := range r2.clk.sleepLog() {
		if d != 10*time.Minute {
			t.Errorf("unauthorized sleep = %v, want the period", d)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	r := newRig(t)
	var wg sync.WaitGroup
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = r.m.Refresh(ctx)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s := r.m.Current()
				_, _ = s.RouteFor(ToolClaudeCode, hashA)
				_ = r.m.Status()
			}
		}()
	}
	wg.Wait()
	if !r.m.Current().Valid() {
		t.Error("not valid after concurrent refreshes")
	}
}
