package attribution

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/account"
	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/identity"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
	"github.com/harnext/harnext-agent/internal/uploader"
	"github.com/harnext/harnext-agent/internal/watcher"
)

const (
	home       = "/home/test"
	dirDefault = "/home/test/.claude"
	dirWork    = "/home/test/.claude-work"
	cwdA       = "/work/repo-a"
	cwdB       = "/work/repo-b"
	cwdC       = "/work/repo-c"
	emailWork  = "jane.doe@corp.example.test"
	emailHome  = "alice.smith@personal.example.test"
	emailOther = "bob.jones@other.example.test"
	routeA     = "11111111-1111-4111-8111-111111111111"
	routeB     = "22222222-2222-4222-8222-222222222222"
	labelWork  = "j***@corp.example.test"
	labelHome  = "a***@personal.example.test"
	labelOther = "b***@other.example.test"
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

func (f *fakeRemote) Report(context.Context, []identity.Account) (*identity.FetchResult, error) {
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
		Tool    string `json:"tool"`
		Hash    string `json:"hash"`
		RouteID string `json:"route_id"`
	}
	list := make([]jb, 0, len(bs))
	for _, b := range bs {
		list = append(list, jb(b))
	}
	issued := f.clk.Now()
	layout := "2006-01-02T15:04:05.000Z"
	payload, _ := json.Marshal(map[string]any{
		"payload_version": 1, "identity_version": f.ver,
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

type fakeGit struct {
	mu    sync.Mutex
	byCwd map[string]string
	calls int
}

func (f *fakeGit) Email(dir string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	e, ok := f.byCwd[dir]
	return e, ok
}

func (f *fakeGit) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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

type env struct {
	t      *testing.T
	clk    *testClock
	id     *idFixture
	git    *fakeGit
	emails *fakeEmails
	stat   *fakeStat
	g      *Gate
}

func newEnv(t *testing.T) *env {
	clk := &testClock{t: t0}
	e := &env{
		t: t, clk: clk, id: newIDFixture(t, clk),
		git: &fakeGit{byCwd: map[string]string{}}, emails: &fakeEmails{byDir: map[string]string{}}, stat: &fakeStat{},
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
		Identity: e.id.m, Git: e.git, Now: clk.Now, ReadClaudeEmail: e.emails.read, Stat: e.stat.stat,
	})
	return e
}

func claudeRow(dir, sid string, first time.Time) aggregate.Row {
	return aggregate.Row{Tool: claude, SessionID: sid, Source: account.DirID(dir), FirstAt: first, StartedAt: first, Cwd: "/work/claude-project"}
}

func copilotRow(cwd, sid string) aggregate.Row {
	return aggregate.Row{Tool: copilot, SessionID: sid, Cwd: cwd}
}

func bind(tool, hash, route string) identity.Binding {
	return identity.Binding{Tool: tool, Hash: hash, RouteID: route}
}

func labelsOf(as []identity.Account) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Label)
	}
	return out
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(e *env)
		row        aggregate.Row
		want       uploader.Decision
		wantLabels []string
	}{
		{
			name: "claude bound",
			setup: func(e *env) {
				e.emails.set(dirDefault, emailWork)
				e.g.ObserveAccounts()
				e.id.publish(hour, bind(claude, hashWork, routeA))
			},
			row:  claudeRow(dirDefault, "s1", t0),
			want: uploader.Decision{Send: true, AccountHash: hashWork, RouteID: routeA},
		},
		{
			name: "claude unbound becomes a candidate with a masked label",
			setup: func(e *env) {
				e.emails.set(dirDefault, emailWork)
				e.g.ObserveAccounts()
				e.id.publish(hour)
			},
			row:        claudeRow(dirDefault, "s1", t0),
			want:       uploader.Decision{Reason: ReasonAccountUnbound},
			wantLabels: []string{labelWork},
		},
		{
			name: "claude without account",
			setup: func(e *env) {
				e.g.ObserveAccounts()
				e.id.publish(hour, bind(claude, hashWork, routeA))
			},
			row:  claudeRow(dirDefault, "s1", t0),
			want: uploader.Decision{Reason: ReasonNoAccount},
		},
		{
			name: "claude row from an unknown config directory",
			setup: func(e *env) {
				e.emails.set(dirDefault, emailWork)
				e.g.ObserveAccounts()
				e.id.publish(hour, bind(claude, hashWork, routeA))
			},
			row:  claudeRow("/home/test/.claude-gone", "s1", t0),
			want: uploader.Decision{Reason: ReasonNoAccount},
		},
		{
			name: "claude row without source",
			setup: func(e *env) {
				e.emails.set(dirDefault, emailWork)
				e.g.ObserveAccounts()
				e.id.publish(hour, bind(claude, hashWork, routeA))
			},
			row:  aggregate.Row{Tool: claude, SessionID: "s1", FirstAt: t0},
			want: uploader.Decision{Reason: ReasonNoAccount},
		},
		{
			name: "account reasons come before identity",
			setup: func(e *env) {
				e.g.ObserveAccounts()
			},
			row:  claudeRow(dirDefault, "s1", t0),
			want: uploader.Decision{Reason: ReasonNoAccount},
		},
		{
			name: "copilot bound through git e-mail",
			setup: func(e *env) {
				e.git.byCwd[cwdA] = emailHome
				e.id.publish(hour, bind(copilot, hashHome, routeB))
			},
			row:  copilotRow(cwdA, "c1"),
			want: uploader.Decision{Send: true, AccountHash: hashHome, RouteID: routeB},
		},
		{
			name: "copilot unbound",
			setup: func(e *env) {
				e.git.byCwd[cwdA] = emailHome
				e.id.publish(hour)
			},
			row:        copilotRow(cwdA, "c1"),
			want:       uploader.Decision{Reason: ReasonAccountUnbound},
			wantLabels: []string{labelHome},
		},
		{
			name: "copilot without git e-mail",
			setup: func(e *env) {
				e.id.publish(hour, bind(copilot, hashHome, routeB))
			},
			row:  copilotRow(cwdA, "c1"),
			want: uploader.Decision{Reason: ReasonGitEmailMissing},
		},
		{
			name: "binding of the other tool does not count",
			setup: func(e *env) {
				e.git.byCwd[cwdA] = emailHome
				e.id.publish(hour, bind(claude, hashHome, routeA))
			},
			row:        copilotRow(cwdA, "c1"),
			want:       uploader.Decision{Reason: ReasonAccountUnbound},
			wantLabels: []string{labelHome},
		},
		{
			name:  "no identity document yet",
			setup: func(e *env) { e.git.byCwd[cwdA] = emailHome },
			row:   copilotRow(cwdA, "c1"),
			want:  uploader.Decision{Reason: ReasonIdentityPending},
		},
		{
			name:  "unsupported tool",
			setup: func(e *env) { e.id.publish(hour) },
			row:   aggregate.Row{Tool: "cursor", SessionID: "x1"},
			want:  uploader.Decision{Reason: ReasonUnsupportedTool},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.setup(e)
			got := e.g.Decide(tc.row)
			if got != tc.want {
				t.Fatalf("Decide = %+v, want %+v", got, tc.want)
			}
			if got.Send && !hex64.MatchString(got.AccountHash) {
				t.Errorf("account hash is not 64 lowercase hex: %q", got.AccountHash)
			}
			if gotLabels := labelsOf(e.g.Candidates(20)); !slices.Equal(gotLabels, tc.wantLabels) {
				t.Errorf("candidate labels = %v, want %v", gotLabels, tc.wantLabels)
			}
		})
	}
}

func TestTimelineBehaviour(t *testing.T) {
	t.Run("unknown timeline fails closed", func(t *testing.T) {
		e := newEnv(t)
		e.emails.set(dirDefault, emailHome)
		e.g.ObserveAccounts()
		e.clk.Advance(2 * time.Hour)
		e.emails.set(dirDefault, emailWork)
		e.g.ObserveAccounts()
		e.id.publish(hour*24, bind(claude, hashHome, routeB), bind(claude, hashWork, routeA))
		if got := e.g.Decide(claudeRow(dirDefault, "old", t0.Add(-time.Hour))); got.Reason != ReasonNoAccount || got.Send {
			t.Errorf("row older than the first observation of a two-account dir = %+v", got)
		}
	})

	t.Run("single account is assumed for older rows", func(t *testing.T) {
		e := newEnv(t)
		e.emails.set(dirDefault, emailWork)
		e.g.ObserveAccounts()
		e.id.publish(hour, bind(claude, hashWork, routeA))
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
		e.id.publish(hour*24, bind(claude, hashHome, routeB), bind(claude, hashWork, routeA))
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
		e.id.publish(hour, bind(claude, hashHome, routeB), bind(claude, hashWork, routeA))
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
		e.id.publish(hour*24, bind(claude, hashHome, routeB), bind(claude, hashWork, routeA))

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
		e.id.publish(hour, bind(claude, hashWork, routeA))
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
		e.id.publish(hour, bind(claude, hashWork, routeA))
		if got := e.g.Decide(claudeRow(dirDefault, "now", e.clk.Now())); got.Reason != ReasonNoAccount {
			t.Errorf("row after the account disappeared = %+v", got)
		}
	})
}

func TestIdentityNotUsableRecordsNothing(t *testing.T) {
	setups := map[string]func(e *env){
		"never fetched": func(e *env) {},
		"expired": func(e *env) {
			e.id.publish(time.Hour)
			e.clk.Advance(2 * time.Hour)
		},
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.emails.set(dirDefault, emailWork)
			e.git.byCwd[cwdA] = emailHome
			e.g.ObserveAccounts()
			setup(e)
			for _, row := range []aggregate.Row{claudeRow(dirDefault, "s1", e.clk.Now()), copilotRow(cwdA, "c1")} {
				if got := e.g.Decide(row); got != (uploader.Decision{Reason: ReasonIdentityPending}) {
					t.Errorf("%s: Decide = %+v", row.Tool, got)
				}
			}
			if c := e.g.Candidates(20); len(c) != 0 {
				t.Errorf("candidates recorded without a valid identity: %v", labelsOf(c))
			}
			if e.g.CandidatesChanged() {
				t.Error("CandidatesChanged is true although nothing was recorded")
			}
			if st := e.g.Stats(); st.Pending != 0 || st.Skipped[ReasonIdentityPending] != 2 {
				t.Errorf("Stats = %+v", st)
			}
		})
	}
}

func TestCandidatesOrderingAndCounting(t *testing.T) {
	e := newEnv(t)
	e.git.byCwd[cwdA] = emailHome  // a***
	e.git.byCwd[cwdB] = emailOther // b***
	e.git.byCwd[cwdC] = emailWork  // j***
	e.id.publish(hour * 24)
	decide := func(cwd, sid string, times int) {
		for i := 0; i < times; i++ {
			e.g.Decide(copilotRow(cwd, sid))
		}
	}
	decide(cwdC, "c-1", 1)
	decide(cwdC, "c-2", 3) // repeated session counts once
	decide(cwdC, "c-3", 1)
	decide(cwdA, "a-1", 2)
	decide(cwdA, "a-2", 1)
	decide(cwdA, "a-3", 1)
	decide(cwdB, "b-1", 5)

	got := e.g.Candidates(20)
	want := []identity.Account{
		{Tool: copilot, Hash: hashHome, Label: labelHome, Sessions30d: 3},
		{Tool: copilot, Hash: hashWork, Label: labelWork, Sessions30d: 3},
		{Tool: copilot, Hash: hashOther, Label: labelOther, Sessions30d: 1},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Candidates = %+v\nwant %+v", got, want)
	}
	if got := e.g.Candidates(2); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Candidates(2) = %+v", got)
	}
	if err := identity.ValidateAccounts(e.g.Candidates(20)); err != nil {
		t.Errorf("candidates are not a valid report: %v", err)
	}
}

func TestCandidatesLimitAndBounds(t *testing.T) {
	t.Run("at most 20 are returned", func(t *testing.T) {
		e := newEnv(t)
		e.id.publish(hour * 24)
		for i := 0; i < 25; i++ {
			cwd := fmt.Sprintf("/work/r%02d", i)
			e.git.byCwd[cwd] = fmt.Sprintf("user%02d@acme%02d.example.test", i, i)
			for s := 0; s <= i%4; s++ {
				e.g.Decide(copilotRow(cwd, fmt.Sprintf("s-%d-%d", i, s)))
			}
		}
		for _, limit := range []int{0, -1, 20, 100} {
			got := e.g.Candidates(limit)
			if len(got) != 20 {
				t.Fatalf("Candidates(%d) returned %d", limit, len(got))
			}
			for i := 1; i < len(got); i++ {
				a, b := got[i-1], got[i]
				if a.Sessions30d < b.Sessions30d || (a.Sessions30d == b.Sessions30d && a.Label > b.Label) {
					t.Fatalf("order broken at %d: %+v then %+v", i, a, b)
				}
			}
		}
		if got := e.g.Candidates(5); len(got) != 5 {
			t.Errorf("Candidates(5) returned %d", len(got))
		}
		if st := e.g.Stats(); st.Pending != 25 || len(st.Labels) != 20 {
			t.Errorf("Stats pending=%d labels=%d", st.Pending, len(st.Labels))
		}
	})

	t.Run("memory is bounded", func(t *testing.T) {
		e := newEnv(t)
		e.id.publish(hour * 24)
		e.git.byCwd[cwdA] = emailHome
		for i := 0; i < maxCandidateSessions+50; i++ {
			e.g.Decide(copilotRow(cwdA, fmt.Sprintf("s%d", i)))
		}
		if got := e.g.Candidates(1); len(got) != 1 || got[0].Sessions30d != maxCandidateSessions {
			t.Errorf("sessions = %+v, want %d", got, maxCandidateSessions)
		}
		for i := 0; i < maxCandidates+10; i++ {
			cwd := fmt.Sprintf("/work/m%02d", i)
			e.git.byCwd[cwd] = fmt.Sprintf("user%02d@bound%02d.example.test", i, i)
			e.g.Decide(copilotRow(cwd, "only"))
		}
		if st := e.g.Stats(); st.Pending > maxCandidates {
			t.Errorf("pending candidates = %d, cap %d", st.Pending, maxCandidates)
		}
	})
}

func TestCandidatesPruning(t *testing.T) {
	e := newEnv(t)
	e.git.byCwd[cwdA] = emailHome
	e.git.byCwd[cwdB] = emailOther
	e.id.publish(hour * 24)
	e.g.Decide(copilotRow(cwdA, "a1"))
	e.g.Decide(copilotRow(cwdA, "a2"))
	e.g.Decide(copilotRow(cwdB, "b1"))

	e.clk.Advance(20 * 24 * hour)
	e.id.publish(hour * 24)
	e.g.Decide(copilotRow(cwdA, "a3"))
	if got := e.g.Candidates(20); len(got) != 2 || got[0].Sessions30d != 3 {
		t.Fatalf("after 20 days: %+v", got)
	}
	e.g.CandidatesChanged()

	e.clk.Advance(11 * 24 * hour)
	e.id.publish(hour * 24)
	got := e.g.Candidates(20)
	if len(got) != 1 || got[0].Hash != hashHome || got[0].Sessions30d != 1 {
		t.Fatalf("after 31 days: %+v", got)
	}
	if !e.g.CandidatesChanged() {
		t.Error("pruning did not set CandidatesChanged")
	}
}

func TestCandidatesChangedSemantics(t *testing.T) {
	e := newEnv(t)
	e.git.byCwd[cwdA] = emailHome
	e.id.publish(hour * 24)
	step := func(name string, want bool) {
		t.Helper()
		if got := e.g.CandidatesChanged(); got != want {
			t.Errorf("%s: CandidatesChanged = %v, want %v", name, got, want)
		}
	}
	step("fresh gate", false)
	e.g.Decide(copilotRow(cwdA, "s1"))
	step("new candidate", true)
	step("read twice", false)
	e.g.Decide(copilotRow(cwdA, "s1"))
	step("same session again", false)
	e.g.Decide(copilotRow(cwdA, "s2"))
	step("new session", true)
	step("read again", false)

	e.id.publish(hour*24, bind(copilot, hashHome, routeB))
	step("binding published but nothing decided", false)
	if got := e.g.Candidates(20); len(got) != 0 {
		t.Errorf("bound candidate still listed: %+v", got)
	}
	step("candidate dropped because it became bound", true)

	e.id.publish(hour * 24)
	e.g.Decide(copilotRow(cwdA, "s3"))
	step("unbound again", true)
	e.id.publish(hour*24, bind(copilot, hashHome, routeB))
	if got := e.g.Decide(copilotRow(cwdA, "s3")); !got.Send {
		t.Fatalf("Decide = %+v, want send", got)
	}
	step("Decide of a bound account removes the candidate", true)
	if got := e.g.Candidates(20); len(got) != 0 {
		t.Errorf("candidates after binding: %+v", got)
	}
}

func TestStats(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailWork)
	e.git.byCwd[cwdA] = emailHome
	e.git.byCwd[cwdB] = emailOther
	e.g.ObserveAccounts()
	e.g.Decide(copilotRow(cwdA, "c1")) // pending: no document
	e.id.publish(hour*24, bind(claude, hashWork, routeA), bind(copilot, hashHome, routeB))

	e.g.Decide(claudeRow(dirDefault, "k1", t0))
	e.g.Decide(claudeRow(dirDefault, "k1", t0))
	e.g.Decide(claudeRow(dirDefault, "k2", t0))
	e.g.Decide(copilotRow(cwdA, "c1"))
	e.g.Decide(copilotRow(cwdB, "c2"))
	e.g.Decide(copilotRow(cwdB, "c3"))
	e.g.Decide(copilotRow("/work/none", "c4"))
	e.g.Decide(claudeRow(dirWork, "k3", t0))

	st := e.g.Stats()
	// c1 was held while the document was missing and is sent now, so it no longer counts as identity_pending.
	wantSkipped := map[string]int{ReasonAccountUnbound: 2, ReasonGitEmailMissing: 1, ReasonNoAccount: 1}
	if !reflect.DeepEqual(st.Skipped, wantSkipped) {
		t.Errorf("Skipped = %v, want %v", st.Skipped, wantSkipped)
	}
	if st.Bound != 3 || st.Pending != 1 || !slices.Equal(st.Labels, []string{labelOther}) {
		t.Errorf("Stats = %+v", st)
	}
}

func TestObserveSkipsUnchangedConfig(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailWork)
	e.emails.set(dirWork, emailHome)
	e.stat.set(10, t0)
	e.g.ObserveAccounts()
	if n := e.emails.readCount(); n != 2 {
		t.Fatalf("first observation read %d configs, want 2", n)
	}
	for i := 0; i < 3; i++ {
		e.clk.Advance(time.Minute)
		e.g.ObserveAccounts()
	}
	if n := e.emails.readCount(); n != 2 {
		t.Errorf("unchanged configs were read again: %d reads", n)
	}
	e.stat.set(11, t0.Add(time.Minute))
	e.g.ObserveAccounts()
	if n := e.emails.readCount(); n != 4 {
		t.Errorf("changed configs: %d reads, want 4", n)
	}
	e.clk.Advance(rereadAfter + time.Second)
	e.g.ObserveAccounts()
	if n := e.emails.readCount(); n != 6 {
		t.Errorf("stale stamp: %d reads, want 6", n)
	}
}

func TestObserveWithoutStatAlwaysReads(t *testing.T) {
	e := newEnv(t) // fakeStat reports ok=false
	e.emails.set(dirDefault, emailWork)
	e.g.ObserveAccounts()
	e.g.ObserveAccounts()
	if n := e.emails.readCount(); n != 4 {
		t.Errorf("reads = %d, want 4 (two roots, two passes)", n)
	}
}

func TestCopilotCache(t *testing.T) {
	e := newEnv(t)
	e.git.byCwd[cwdA] = emailHome
	e.id.publish(hour*24, bind(copilot, hashHome, routeB))
	for i := 0; i < 5; i++ {
		e.g.Decide(copilotRow(cwdA, fmt.Sprintf("s%d", i)))
		e.g.Decide(copilotRow(cwdB, fmt.Sprintf("m%d", i))) // negative answers are cached too
	}
	if n := e.git.callCount(); n != 2 {
		t.Fatalf("git lookups = %d, want 2", n)
	}
	e.clk.Advance(cacheTTL + time.Second)
	e.id.publish(hour*24, bind(copilot, hashHome, routeB))
	e.git.mu.Lock()
	e.git.byCwd[cwdB] = emailOther
	e.git.mu.Unlock()
	e.g.Decide(copilotRow(cwdA, "again"))
	if got := e.g.Decide(copilotRow(cwdB, "again")); got.Reason != ReasonAccountUnbound {
		t.Errorf("after the cache expired: %+v", got)
	}
	if n := e.git.callCount(); n != 4 {
		t.Errorf("git lookups after expiry = %d, want 4", n)
	}
}

func TestTimelinePersistence(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailWork)
	e.g.ObserveAccounts()
	snap := e.g.TimelineSnapshot()

	e2 := newEnv(t)
	if err := e2.g.RestoreTimeline(snap); err != nil {
		t.Fatalf("RestoreTimeline: %v", err)
	}
	e2.id.publish(hour, bind(claude, hashWork, routeA))
	if got := e2.g.Decide(claudeRow(dirDefault, "s1", t0)); !got.Send || got.AccountHash != hashWork {
		t.Errorf("restored gate decided %+v", got)
	}

	e3 := newEnv(t)
	e3.id.publish(hour)
	if err := e3.g.RestoreTimeline(snap); err != nil {
		t.Fatal(err)
	}
	// The label is memory only: without a fresh observation an unbound account yields no candidate.
	if got := e3.g.Decide(claudeRow(dirDefault, "s1", t0)); got.Reason != ReasonAccountUnbound {
		t.Errorf("Decide = %+v", got)
	}
	if c := e3.g.Candidates(20); len(c) != 0 {
		t.Errorf("candidate without a label: %+v", c)
	}
	if err := e3.g.RestoreTimeline([]byte("not json")); err == nil {
		t.Error("RestoreTimeline accepted garbage")
	}
	if got := e3.g.Decide(claudeRow(dirDefault, "s2", t0)); got.Reason != ReasonNoAccount {
		t.Errorf("after a failed restore: %+v", got)
	}
}

func TestNilDependenciesFailClosed(t *testing.T) {
	g := New(Options{Home: home})
	g.ObserveAccounts()
	for _, row := range []aggregate.Row{claudeRow(dirDefault, "s", t0), copilotRow(cwdA, "c")} {
		if got := g.Decide(row); got.Send {
			t.Errorf("sent without dependencies: %+v", got)
		}
	}
	if g.CandidatesChanged() || len(g.Candidates(20)) != 0 {
		t.Error("candidates without dependencies")
	}
}

// walkStrings collects every string reachable from v, including unexported fields; it never calls Interface.
func walkStrings(v reflect.Value, seen map[uintptr]bool, out *[]string) {
	switch v.Kind() {
	case reflect.String:
		*out = append(*out, v.String())
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return
		}
		seen[v.Pointer()] = true
		walkStrings(v.Elem(), seen, out)
	case reflect.Interface:
		if !v.IsNil() {
			walkStrings(v.Elem(), seen, out)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			walkStrings(v.Field(i), seen, out)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			*out = append(*out, string(v.Bytes()))
			return
		}
		for i := 0; i < v.Len(); i++ {
			walkStrings(v.Index(i), seen, out)
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkStrings(v.Index(i), seen, out)
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			walkStrings(it.Key(), seen, out)
			walkStrings(it.Value(), seen, out)
		}
	}
}

func TestPrivacyNoClearEmailAnywhere(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailWork)
	e.emails.set(dirWork, emailHome)
	e.git.byCwd[cwdA] = emailOther
	e.git.byCwd[cwdB] = emailHome
	e.g.ObserveAccounts()
	e.id.publish(hour*24, bind(claude, hashWork, routeA), bind(copilot, hashHome, routeB))

	var decisions []uploader.Decision
	for i, row := range []aggregate.Row{
		claudeRow(dirDefault, "k1", t0), claudeRow(dirWork, "k2", t0), // work bound, home unbound for claude
		copilotRow(cwdA, "c1"), copilotRow(cwdB, "c2"), copilotRow("/work/none", "c3"),
	} {
		d := e.g.Decide(row)
		decisions = append(decisions, d)
		if d.Send && !hex64.MatchString(d.AccountHash) {
			t.Errorf("row %d: hash %q is not 64 lowercase hex", i, d.AccountHash)
		}
	}

	cands := e.g.Candidates(20)
	if len(cands) != 2 {
		t.Fatalf("candidates = %+v", cands)
	}
	stats := e.g.Stats()
	for _, c := range cands {
		if !hex64.MatchString(c.Hash) {
			t.Errorf("candidate hash %q is not 64 lowercase hex", c.Hash)
		}
		for _, em := range allEmails {
			if c.Label == em {
				t.Errorf("label equals the e-mail address")
			}
		}
		if !strings.Contains(c.Label, "***") {
			t.Errorf("label %q is not masked", c.Label)
		}
	}

	js, _ := json.Marshal(cands)
	var outputs []string
	outputs = append(outputs,
		fmt.Sprintf("%v %+v %#v", stats, stats, stats),
		fmt.Sprintf("%v %+v %#v", cands, cands, cands),
		fmt.Sprintf("%v %+v %#v", decisions, decisions, decisions),
		string(js), string(e.g.TimelineSnapshot()),
	)
	g := reflect.ValueOf(e.g).Elem()
	for i := 0; i < g.NumField(); i++ {
		if g.Type().Field(i).Name == "opts" { // holds the test fakes, which know the addresses
			continue
		}
		walkStrings(g.Field(i), map[uintptr]bool{}, &outputs)
	}
	if len(outputs) < 10 {
		t.Fatalf("scan looked at only %d strings", len(outputs))
	}
	for _, em := range allEmails {
		local := em[:strings.Index(em, "@")]
		for _, s := range outputs {
			if strings.Contains(s, em) || strings.Contains(s, local) {
				t.Fatalf("clear e-mail data (%q) found in %q", local, s)
			}
		}
	}
}

func TestConcurrency(t *testing.T) {
	e := newEnv(t)
	e.emails.set(dirDefault, emailWork)
	e.emails.set(dirWork, emailHome)
	e.git.byCwd[cwdA] = emailOther
	e.id.publish(hour*24, bind(claude, hashWork, routeA))
	e.g.ObserveAccounts()

	var wg sync.WaitGroup
	run := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				fn(i)
			}
		}()
	}
	for w := 0; w < 3; w++ {
		run(func(i int) {
			e.g.Decide(claudeRow(dirDefault, fmt.Sprintf("k%d", i), t0))
			e.g.Decide(claudeRow(dirWork, fmt.Sprintf("w%d", i), t0))
			e.g.Decide(copilotRow(cwdA, fmt.Sprintf("c%d", i)))
			e.g.Decide(copilotRow(cwdB, fmt.Sprintf("x%d", i)))
		})
	}
	run(func(i int) {
		if i%2 == 0 {
			e.emails.set(dirWork, emailHome)
		} else {
			e.emails.unset(dirWork)
		}
		e.stat.set(int64(i), t0.Add(time.Duration(i)*time.Second))
		e.g.ObserveAccounts()
	})
	run(func(i int) {
		e.g.Candidates(20)
		e.g.CandidatesChanged()
		e.g.Stats()
	})
	run(func(i int) {
		_ = e.g.RestoreTimeline(e.g.TimelineSnapshot())
		e.clk.Advance(time.Second)
	})
	wg.Wait()
}
