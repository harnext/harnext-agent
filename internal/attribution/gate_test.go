package attribution

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/account"
	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/copilotid"
	"github.com/harnext/harnext-agent/internal/identity"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
	"github.com/harnext/harnext-agent/internal/watcher"
)

const (
	home       = "/home/test"
	dirDefault = "/home/test/.claude"
	dirWork    = "/home/test/.claude-work"
	emailWork  = "jane.doe@corp.example.test"
	emailHome  = "alice.smith@personal.example.test"
	emailOther = "bob.jones@other.example.test"
	routeA     = "acct-a"
	routeB     = "acct-b"
	labelWork  = "jan***@corp.example.test"
	labelHome  = "ali***@personal.example.test"
	labelOther = "bob***@other.example.test"
	testKID    = "k-test"
	claude     = aggregate.ToolClaudeCode
	copilot    = aggregate.ToolVSCodeCopilot
	hour       = time.Hour
)

var (
	t0         = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	hashWork   = account.Hash(emailWork)
	hashHome   = account.Hash(emailHome)
	hashOther  = account.Hash(emailOther)
	allEmails  = []string{emailWork, emailHome, emailOther}
	hex64      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	allClaudes = []string{dirDefault, dirWork}
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Sleep(context.Context, time.Duration) error { return nil }

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeRemote struct {
	mu  sync.Mutex
	env remoteconfig.Envelope
}

func (f *fakeRemote) Fetch(context.Context, string) (*identity.FetchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &identity.FetchResult{Envelope: f.env}, nil
}

func (f *fakeRemote) Report(context.Context, identity.Report) (*identity.FetchResult, error) {
	return f.Fetch(context.Background(), "")
}

// idFixture is a real identity.Manager fed with documents signed by a throwaway key.
type idFixture struct {
	t      *testing.T
	clk    *testClock
	remote *fakeRemote
	priv   ed25519.PrivateKey
	m      *identity.Manager
	ver    int
}

func newIDFixture(t *testing.T, clk *testClock) *idFixture {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &idFixture{t: t, clk: clk, remote: &fakeRemote{}, priv: priv}
	f.m = identity.NewManager(identity.ManagerOptions{
		Remote: f.remote, Keys: remoteconfig.TrustedKeys{testKID: pub}, Clock: clk,
	})
	return f
}

func (f *idFixture) publish(ttl time.Duration, bs ...identity.Binding) {
	f.t.Helper()
	f.ver++
	type jb struct {
		Tool string `json:"tool"`
		Hash string `json:"hash"`
	}
	list := make([]jb, 0, len(bs))
	for _, b := range bs {
		list = append(list, jb(b))
	}
	issued := f.clk.Now()
	layout := "2006-01-02T15:04:05.000Z"
	payload, _ := json.Marshal(map[string]any{
		"payload_version": 2, "identity_version": f.ver,
		"issued_at": issued.UTC().Format(layout), "expires_at": issued.Add(ttl).UTC().Format(layout), "bindings": list,
	})
	f.remote.mu.Lock()
	f.remote.env = remoteconfig.Envelope{
		Payload: base64.RawURLEncoding.EncodeToString(payload),
		Sig:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.priv, payload)),
		KID:     testKID,
	}
	f.remote.mu.Unlock()
	if err := f.m.Refresh(context.Background()); err != nil {
		f.t.Fatalf("publish: %v", err)
	}
}

type fakeEmails struct {
	mu    sync.Mutex
	byDir map[string]string
	reads int
}

func (f *fakeEmails) read(dir, _ string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	e, ok := f.byDir[dir]
	return e, ok
}

func (f *fakeEmails) set(dir, email string) {
	f.mu.Lock()
	f.byDir[dir] = email
	f.mu.Unlock()
}

func (f *fakeEmails) unset(dir string) {
	f.mu.Lock()
	delete(f.byDir, dir)
	f.mu.Unlock()
}

func (f *fakeEmails) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// fakeCopilot is the editor's state database: what Read returns is set by the test.
type fakeCopilot struct {
	mu sync.Mutex
	st copilotid.State
	ok bool
}

func (f *fakeCopilot) Read() (copilotid.State, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st, f.ok
}

func (f *fakeCopilot) set(st copilotid.State) {
	f.mu.Lock()
	f.st, f.ok = st, true
	f.mu.Unlock()
}

type fakeStat struct {
	mu sync.Mutex
	st stamp
	ok bool
}

func (f *fakeStat) stat(string) (int64, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st.size, f.st.mod, f.ok
}

func (f *fakeStat) set(size int64, mod time.Time) {
	f.mu.Lock()
	f.st, f.ok = stamp{size, mod}, true
	f.mu.Unlock()
}

// env is a machine with two Harnext accounts: A (routeA) and B (routeB). Each holds its own identity document.
type env struct {
	t      *testing.T
	clk    *testClock
	id     *idFixture // Harnext account A
	id2    *idFixture // Harnext account B
	cop    *fakeCopilot
	emails *fakeEmails
	stat   *fakeStat
	g      *Gate
}

func newEnv(t *testing.T) *env {
	clk := &testClock{t: t0}
	e := &env{
		t: t, clk: clk, id: newIDFixture(t, clk), id2: newIDFixture(t, clk),
		cop: &fakeCopilot{}, emails: &fakeEmails{byDir: map[string]string{}}, stat: &fakeStat{},
	}
	e.g = New(Options{
		Home: home,
		Roots: func() []watcher.Root {
			var rs []watcher.Root
			for _, d := range allClaudes {
				rs = append(rs, watcher.ClaudeRoot(d))
			}
			return rs
		},
		Accounts: []Account{{ID: routeA, Identity: e.id.m}, {ID: routeB, Identity: e.id2.m}},
		Copilot:  e.cop, Now: clk.Now, ReadClaudeEmail: e.emails.read, Stat: e.stat.stat,
	})
	return e
}

// pub publishes the documents: account A holds the Claude Code hash a, account B the hash b (empty: no binding).
func (e *env) pub(ttl time.Duration, a, b string) {
	e.t.Helper()
	var as, bs []identity.Binding
	if a != "" {
		as = append(as, bind(claude, a))
	}
	if b != "" {
		bs = append(bs, bind(claude, b))
	}
	e.id.publish(ttl, as...)
	e.id2.publish(ttl, bs...)
}

func claudeRow(dir, sid string, first time.Time) aggregate.Row {
	return aggregate.Row{Tool: claude, SessionID: sid, Source: account.DirID(dir), FirstAt: first, StartedAt: first, Cwd: "/work/claude-project"}
}

func copilotRow(sid string, first time.Time) aggregate.Row {
	return aggregate.Row{Tool: copilot, SessionID: sid, FirstAt: first, StartedAt: first, Cwd: "/work/copilot-project"}
}

func bind(tool, hash string) identity.Binding { return identity.Binding{Tool: tool, Hash: hash} }

func TestTimelineBehaviour(t *testing.T) {
	t.Run("unknown timeline fails closed", func(t *testing.T) {
		e := newEnv(t)
		e.emails.set(dirDefault, emailHome)
		e.g.ObserveAccounts()
		e.clk.Advance(2 * time.Hour)
		e.emails.set(dirDefault, emailWork)
		e.g.ObserveAccounts()
		e.pub(hour*24, hashWork, hashHome)
		if got := e.g.Decide(claudeRow(dirDefault, "old", t0.Add(-time.Hour))); got.Reason != ReasonNoAccount || got.Send {
			t.Errorf("row older than the first observation of a two-account dir = %+v", got)
		}
	})

	t.Run("single account is assumed for older rows", func(t *testing.T) {
		e := newEnv(t)
		e.emails.set(dirDefault, emailWork)
		e.g.ObserveAccounts()
		e.pub(hour, hashWork, "")
		got := e.g.Decide(claudeRow(dirDefault, "old", t0.Add(-48*time.Hour)))
		if !got.Send || got.AccountHash != hashWork || got.RouteID != routeA {
			t.Errorf("older row = %+v", got)
		}
	})

	t.Run("account switch inside one directory", func(t *testing.T) {
		e := newEnv(t)
		e.emails.set(dirDefault, emailHome)
		e.g.ObserveAccounts()
		e.clk.Advance(2 * time.Hour)
		e.emails.set(dirDefault, emailWork)
		e.g.ObserveAccounts()
		e.pub(hour*24, hashWork, hashHome)
		before := e.g.Decide(claudeRow(dirDefault, "s-before", t0.Add(time.Hour)))
		after := e.g.Decide(claudeRow(dirDefault, "s-after", t0.Add(3*time.Hour)))
		if !before.Send || before.AccountHash != hashHome || before.RouteID != routeB {
			t.Errorf("row before the switch = %+v", before)
		}
		if !after.Send || after.AccountHash != hashWork || after.RouteID != routeA {
			t.Errorf("row after the switch = %+v", after)
		}
	})

	t.Run("two directories keep their own accounts", func(t *testing.T) {
		e := newEnv(t)
		e.emails.set(dirDefault, emailHome)
		e.emails.set(dirWork, emailWork)
		e.g.ObserveAccounts()
		e.pub(hour, hashWork, hashHome)
		if got := e.g.Decide(claudeRow(dirDefault, "d", t0)); got.AccountHash != hashHome {
			t.Errorf("default dir = %+v", got)
		}
		if got := e.g.Decide(claudeRow(dirWork, "w", t0)); got.AccountHash != hashWork {
			t.Errorf("work dir = %+v", got)
		}
	})

	t.Run("logout opens a gap that stays closed after a different login", func(t *testing.T) {
		e := newEnv(t)
		e.emails.set(dirDefault, emailHome)
		e.g.ObserveAccounts()
		e.clk.Advance(time.Hour)
		e.emails.unset(dirDefault)
		e.g.ObserveAccounts()
		e.pub(hour*24, hashWork, hashHome)

		if got := e.g.Decide(claudeRow(dirDefault, "before", t0.Add(30*time.Minute))); got.AccountHash != hashHome {
			t.Errorf("row before the logout = %+v", got)
		}
		if got := e.g.Decide(claudeRow(dirDefault, "during", t0.Add(90*time.Minute))); got.Reason != ReasonNoAccount {
			t.Errorf("row during the logout = %+v", got)
		}
		e.clk.Advance(time.Hour)
		e.emails.set(dirDefault, emailWork)
		e.g.ObserveAccounts()
		if got := e.g.Decide(claudeRow(dirDefault, "during2", t0.Add(90*time.Minute))); got.Reason != ReasonNoAccount {
			t.Errorf("gap row after the next login = %+v", got)
		}
		if got := e.g.Decide(claudeRow(dirDefault, "after", t0.Add(150*time.Minute))); got.AccountHash != hashWork {
			t.Errorf("row after the next login = %+v", got)
		}
	})

	t.Run("short read failure with the same account is not a gap", func(t *testing.T) {
		e := newEnv(t)
		e.emails.set(dirDefault, emailWork)
		e.g.ObserveAccounts()
		e.clk.Advance(30 * time.Second)
		e.emails.unset(dirDefault)
		e.g.ObserveAccounts()
		e.clk.Advance(30 * time.Second)
		e.emails.set(dirDefault, emailWork)
		e.g.ObserveAccounts()
		e.pub(hour, hashWork, "")
		if got := e.g.Decide(claudeRow(dirDefault, "glitch", t0.Add(45*time.Second))); !got.Send || got.AccountHash != hashWork {
			t.Errorf("row inside a read glitch = %+v", got)
		}
	})

	t.Run("a directory that lost its account blocks new rows", func(t *testing.T) {
		e := newEnv(t)
		e.emails.set(dirDefault, emailWork)
		e.g.ObserveAccounts()
		e.clk.Advance(time.Hour)
		e.emails.unset(dirDefault)
		e.g.ObserveAccounts()
		e.pub(hour, hashWork, "")
		if got := e.g.Decide(claudeRow(dirDefault, "now", e.clk.Now())); got.Reason != ReasonNoAccount {
			t.Errorf("row after the account disappeared = %+v", got)
		}
	})
}

func TestDecideRoutesToTheAccountThatOwnsTheClaudeAccount(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailHome)
	e.emails.set(dirWork, emailWork)
	e.g.ObserveAccounts()
	e.pub(hour, hashWork, hashHome) // A is the work account, B the personal one
	if got := e.g.Decide(claudeRow(dirWork, "w", t0)); !got.Send || got.RouteID != routeA || got.AccountHash != hashWork {
		t.Errorf("work session = %+v", got)
	}
	if got := e.g.Decide(claudeRow(dirDefault, "h", t0)); !got.Send || got.RouteID != routeB || got.AccountHash != hashHome {
		t.Errorf("personal session = %+v", got)
	}
}

func TestDecideKeepsUnmatchedAccountOnTheMachine(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailOther)
	e.g.ObserveAccounts()
	e.pub(hour, hashWork, hashHome)
	got := e.g.Decide(claudeRow(dirDefault, "x", t0))
	if got.Send || got.Reason != ReasonNoMatch || got.RouteID != "" {
		t.Errorf("unmatched account = %+v", got)
	}
	if st := e.g.Stats(); st.Skipped[ReasonNoMatch] != 1 || st.Bound != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestDecideWithoutAnyValidIdentityIsPending(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailWork)
	e.g.ObserveAccounts()
	if got := e.g.Decide(claudeRow(dirDefault, "x", t0)); got.Send || got.Reason != ReasonIdentityPending {
		t.Errorf("no document = %+v", got)
	}
	// One valid document is enough to tell "no match" from "pending".
	e.id2.publish(hour, bind(claude, hashHome))
	if got := e.g.Decide(claudeRow(dirDefault, "y", t0)); got.Send || got.Reason != ReasonNoMatch {
		t.Errorf("one valid document = %+v", got)
	}
	// Expiry closes the gate again.
	e.clk.Advance(2 * hour)
	if got := e.g.Decide(claudeRow(dirDefault, "z", t0)); got.Send || got.Reason != ReasonIdentityPending {
		t.Errorf("expired document = %+v", got)
	}
}

func TestCopilotSessionGoesToTheSelectedGitHubAccount(t *testing.T) {
	e := newEnv(t)
	e.cop.set(copilotid.State{Active: "alice", ActiveID: "11", Plan: "pro", Known: []string{"alice", "bob"}})
	e.g.ObserveAccounts()
	e.clk.Advance(2 * hour)
	e.cop.set(copilotid.State{Active: "bob", ActiveID: "22", Plan: "business", Known: []string{"alice", "bob"}})
	e.g.ObserveAccounts()
	// Account A selected bob, account B selected alice.
	e.id.publish(hour*24, bind(copilot, copilotid.IDHash("22")))
	e.id2.publish(hour*24, bind(copilot, copilotid.IDHash("11")))

	if got := e.g.Decide(copilotRow("early", t0.Add(30*time.Minute))); !got.Send || got.RouteID != routeB || got.AccountHash != copilotid.IDHash("11") {
		t.Errorf("session while alice was active = %+v", got)
	}
	if got := e.g.Decide(copilotRow("late", t0.Add(3*hour))); !got.Send || got.RouteID != routeA || got.AccountHash != copilotid.IDHash("22") {
		t.Errorf("session while bob was active = %+v", got)
	}
	// Before the first observation two accounts exist: the session is not attributed.
	if got := e.g.Decide(copilotRow("ancient", t0.Add(-hour))); got.Send || got.Reason != ReasonCopilotUnknown {
		t.Errorf("session before the first observation = %+v", got)
	}
}

func TestCopilotWithoutSelectionOrWithoutEditorState(t *testing.T) {
	e := newEnv(t)
	if got := e.g.Decide(copilotRow("none", t0)); got.Send || got.Reason != ReasonCopilotUnknown {
		t.Errorf("no editor state = %+v", got)
	}
	e.cop.set(copilotid.State{Active: "alice", ActiveID: "11", Known: []string{"alice"}})
	e.g.ObserveAccounts()
	e.pub(hour, hashWork, "") // valid documents, but no Copilot account selected anywhere
	if got := e.g.Decide(copilotRow("unselected", t0.Add(time.Minute))); got.Send || got.Reason != ReasonNoMatch {
		t.Errorf("nothing selected = %+v", got)
	}
}

func TestReportForCountsUnmatchedAccountsAndListsGitHubAccounts(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailHome)
	e.emails.set(dirWork, emailWork)
	e.cop.set(copilotid.State{Active: "alice", ActiveID: "11", Plan: "pro", Known: []string{"alice", "bob"}})
	e.g.ObserveAccounts()
	a, b := Account{ID: routeA, Identity: e.id.m}, Account{ID: routeB, Identity: e.id2.m}

	if _, ok := e.g.ReportFor(a); ok {
		t.Error("a report was built without a valid document")
	}
	e.pub(hour, hashWork, hashOther) // B's Claude account is not signed in here
	repA, ok := e.g.ReportFor(a)
	if !ok || repA.Claude.Found != 2 || !repA.Claude.Matched || repA.Claude.Label != labelWork {
		t.Fatalf("report A = %+v %v", repA, ok)
	}
	repB, _ := e.g.ReportFor(b)
	if repB.Claude.Found != 2 || repB.Claude.Matched || repB.Claude.Label != "" {
		t.Fatalf("report B = %+v", repB)
	}
	if len(repA.Copilot.Logins) != 2 || repA.Copilot.Logins[0].Login != "alice" || repA.Copilot.Logins[0].IDHash != copilotid.IDHash("11") || repA.Copilot.Logins[0].Plan != "pro" {
		t.Fatalf("logins = %+v", repA.Copilot.Logins)
	}
	if err := identity.ValidateReport(repA); err != nil {
		t.Errorf("report A is not valid: %v", err)
	}
	// Once alice is selected, only she is reported (name and plan stay current), bob is not.
	e.id.publish(hour, bind(claude, hashWork), bind(copilot, copilotid.IDHash("11")))
	repA, _ = e.g.ReportFor(a)
	if len(repA.Copilot.Logins) != 1 || repA.Copilot.Logins[0].Login != "alice" {
		t.Fatalf("after selection: %+v", repA.Copilot.Logins)
	}
}

func TestStatsAndFound(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailHome)
	e.emails.set(dirWork, emailWork)
	e.g.ObserveAccounts()
	if n := e.g.ClaudeFound(); n != 2 {
		t.Errorf("found = %d", n)
	}
	e.emails.unset(dirDefault)
	e.clk.Advance(time.Minute)
	e.stat.set(1, e.clk.Now()) // forces a re-read
	e.g.ObserveAccounts()
	if n := e.g.ClaudeFound(); n != 1 {
		t.Errorf("found after logout = %d", n)
	}
	e.pub(hour, hashWork, "")
	e.g.Decide(claudeRow(dirWork, "ok", t0))
	e.g.Decide(aggregate.Row{Tool: "other", SessionID: "u"})
	st := e.g.Stats()
	if st.Bound != 1 || st.Skipped[ReasonUnsupportedTool] != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestNilDependenciesFailClosed(t *testing.T) {
	g := New(Options{Home: home})
	g.ObserveAccounts() // no roots, no editor source: nothing happens
	if got := g.Decide(claudeRow(dirDefault, "x", t0)); got.Send {
		t.Errorf("nothing configured = %+v", got)
	}
	if got := g.Decide(copilotRow("x", t0)); got.Send {
		t.Errorf("nothing configured = %+v", got)
	}
}

func TestPrivacyNoClearEmailOrGitHubIDAnywhere(t *testing.T) {
	e := newEnv(t)
	for _, d := range allClaudes {
		e.emails.set(d, emailWork)
	}
	e.cop.set(copilotid.State{Active: "alice", ActiveID: "987654321", Known: []string{"alice"}})
	e.g.ObserveAccounts()
	e.pub(hour, hashWork, "")
	e.g.Decide(claudeRow(dirDefault, "s", t0))
	rep, _ := e.g.ReportFor(Account{ID: routeA, Identity: e.id.m})
	blob, _ := json.Marshal(rep)
	all := string(blob) + string(e.g.TimelineSnapshot()) + string(e.g.CopilotSnapshot())
	for _, secret := range append(allEmails, "987654321") {
		if strings.Contains(all, secret) {
			t.Errorf("%q leaked into the report or a snapshot", secret)
		}
	}
	if !hex64.MatchString(copilotid.IDHash("987654321")) {
		t.Error("id hash is not 64 hex")
	}
}

func TestConcurrentUse(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailWork)
	e.cop.set(copilotid.State{Active: "alice", ActiveID: "11", Known: []string{"alice"}})
	e.g.ObserveAccounts()
	e.pub(hour, hashWork, "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				e.g.ObserveAccounts()
				e.g.Decide(claudeRow(dirDefault, fmt.Sprintf("s%d-%d", i, n), t0))
				e.g.ReportFor(Account{ID: routeA, Identity: e.id.m})
				_ = e.g.Stats()
				_ = e.g.ClaudeFound()
			}
		}(i)
	}
	wg.Wait()
}
