package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/parser/claudecode"
	"github.com/harnext/harnext-agent/internal/service"
	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/svcmgr"
	"github.com/harnext/harnext-agent/internal/uploader"
)

const tok = "hxt_" + "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"

type result struct {
	out, err string
	failure  error
}

func exec(t *testing.T, p service.Paths, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	root := New(Env{Paths: p, Version: "9.9.9", In: strings.NewReader(stdin), Out: &out, Err: &errb, Gate: testGate})
	root.SetArgs(args)
	err := root.Execute()
	return result{out.String(), errb.String(), err}
}

// testGate sends every session as one fixed account to one fixed route; the default gate is fail-closed.
func testGate(aggregate.Row) uploader.Decision {
	return uploader.Decision{Send: true, AccountHash: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90", RouteID: "route-a"}
}

func newPaths(t *testing.T) service.Paths {
	t.Helper()
	return service.PathsIn(filepath.Join(t.TempDir(), "home"))
}

func TestVersion(t *testing.T) {
	r := exec(t, newPaths(t), "", "--version")
	if r.failure != nil || r.out != "harnext-agent 9.9.9\n" {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}
}

func TestLoginAddsAnAccountAndLogoutRemovesIt(t *testing.T) {
	p := newPaths(t)
	second := "hxt_" + strings.Repeat("S", 43)
	if r := exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com"); r.failure != nil {
		t.Fatal(r.failure)
	}
	// "login" is the same command; a second token adds a second account, the first one stays.
	r := exec(t, p, second+"\n", "login", "--token-stdin", "--server", "https://app.example.com")
	if r.failure != nil || !strings.Contains(r.out, "Saved account "+service.AccountID(second)) {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}
	if list, _ := p.LoadAccounts(); len(list) != 2 {
		t.Fatalf("accounts = %+v", list)
	}
	// The same token again changes nothing.
	exec(t, p, second+"\n", "login", "--token-stdin", "--server", "https://app.example.com")
	if list, _ := p.LoadAccounts(); len(list) != 2 {
		t.Fatalf("accounts after a repeat = %+v", list)
	}
	// With two accounts a bare logout is refused; an id prefix removes one.
	if r := exec(t, p, "", "logout"); r.failure == nil || !strings.Contains(r.failure.Error(), "name the one to remove") {
		t.Fatalf("bare logout: %v", r.failure)
	}
	if r := exec(t, p, "", "logout", "nosuchid"); r.failure == nil {
		t.Fatal("unknown id accepted")
	}
	r = exec(t, p, "", "logout", service.AccountID(second)[:5])
	if r.failure != nil || !strings.Contains(r.out, "Removed account "+service.AccountID(second)) {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}
	// The last account goes without an id.
	if r := exec(t, p, "", "logout"); r.failure != nil {
		t.Fatal(r.failure)
	}
	if _, err := p.LoadAccounts(); err != service.ErrNotInitialized {
		t.Fatalf("after logout: %v", err)
	}
	if r := exec(t, p, "", "logout"); r.failure == nil {
		t.Fatal("logout without an account succeeded")
	}
}

func TestInit(t *testing.T) {
	p := newPaths(t)
	r := exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	if r.failure != nil || !strings.Contains(r.out, "Saved") {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}
	if got, err := p.LoadAccounts(); err != nil || len(got) != 1 || got[0].Token != tok || !strings.Contains(r.out, got[0].ID) {
		t.Fatalf("accounts = %+v err=%v out=%q", got, err, r.out)
	}
	if s, err := p.LoadSettings(); err != nil || s.ServerURL != "https://app.example.com" || s.AllowInsecureLocalhost {
		t.Fatalf("settings = %+v err=%v", s, err)
	}
	if strings.Contains(r.out+r.err, tok) {
		t.Fatal("token echoed")
	}
}

func TestInitFromEnvAndDevFlag(t *testing.T) {
	p := newPaths(t)
	t.Setenv("HARNEXT_TOKEN", tok)
	r := exec(t, p, "", "init", "--server", "http://localhost:3000", "--dev-allow-insecure-localhost")
	if r.failure != nil {
		t.Fatalf("err=%v", r.failure)
	}
	if s, _ := p.LoadSettings(); !s.AllowInsecureLocalhost {
		t.Fatalf("settings = %+v", s)
	}
}

func TestInitRejections(t *testing.T) {
	for name, c := range map[string]struct {
		stdin string
		args  []string
		want  string
	}{
		"bad token":    {"nope\n", []string{"init", "--token-stdin", "--server", "https://a.example.com"}, "malformed"},
		"no token":     {"", []string{"init", "--server", "https://a.example.com"}, "malformed"},
		"no server":    {tok + "\n", []string{"init", "--token-stdin"}, "--server is required"},
		"http remote":  {tok + "\n", []string{"init", "--token-stdin", "--server", "http://a.example.com"}, "https"},
		"http no flag": {tok + "\n", []string{"init", "--token-stdin", "--server", "http://localhost:3000"}, "https"},
		"extra arg":    {"", []string{"init", "extra"}, "unknown command"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HARNEXT_TOKEN", "")
			p := newPaths(t)
			r := exec(t, p, c.stdin, c.args...)
			if r.failure == nil || !strings.Contains(r.failure.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", r.failure, c.want)
			}
			if strings.Contains(r.failure.Error(), tok) {
				t.Fatal("error echoes token")
			}
			if _, err := p.LoadAccounts(); err != service.ErrNotInitialized {
				t.Fatalf("token stored despite failure: %v", err)
			}
		})
	}
}

func TestStatusStates(t *testing.T) {
	p := newPaths(t)
	r := exec(t, p, "", "status")
	if r.failure != nil || !strings.Contains(r.out, "not initialized") {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}

	exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	r = exec(t, p, "", "status")
	for _, want := range []string{"Server:    app.example.com", "Token:     1 account stored", "Agent:     not running", "no data yet"} {
		if !strings.Contains(r.out, want) {
			t.Fatalf("status lacks %q:\n%s", want, r.out)
		}
	}

	now := time.Now().UTC()
	if err := p.WriteStatus(service.StatusFile{
		PID: 1, Version: "9.9.9", UpdatedAt: now, Sessions: 3, UniqueResponses: 42, SkippedLines: 1,
		Config: service.ConfigStatus{TrustedKeys: true, Loaded: true, Version: 7, ExpiresAt: now.Add(48 * time.Hour)},
		Upload: service.UploadStatus{LastAttempt: now, LastSuccess: now, Accepted: 2, Unchanged: 1},
	}); err != nil {
		t.Fatal(err)
	}
	r = exec(t, p, "", "status")
	for _, want := range []string{"3 sessions, 42 responses", "configuration v7", "2 sent, 1 unchanged"} {
		if !strings.Contains(r.out, want) {
			t.Fatalf("status lacks %q:\n%s", want, r.out)
		}
	}
	if strings.Contains(r.out, tok) {
		t.Fatal("status prints the token")
	}
}

func TestStatusWithoutTrustedKeysExplainsNullScores(t *testing.T) {
	p := newPaths(t)
	exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	_ = p.WriteStatus(service.StatusFile{UpdatedAt: time.Now()})
	r := exec(t, p, "", "status")
	if !strings.Contains(r.out, "no trusted keys") {
		t.Fatalf("status:\n%s", r.out)
	}
}

func TestStopWhenNotRunning(t *testing.T) {
	r := exec(t, newPaths(t), "", "stop")
	if r.failure != nil || !strings.Contains(r.out, "not running") {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}
}

func asst(session, id, model, cwd string, in, out int64) claudecode.AssistantUsage {
	return claudecode.AssistantUsage{
		SessionID: session, MessageID: id, RequestID: "r" + id, Model: model, Cwd: cwd,
		Timestamp: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		Usage:     claudecode.Usage{InputTokens: in, OutputTokens: out},
	}
}

func TestStats(t *testing.T) {
	p := newPaths(t)
	if r := exec(t, p, "", "stats"); !strings.Contains(r.out, "No data yet") {
		t.Fatalf("empty: %q", r.out)
	}

	agg := aggregate.New()
	agg.Add(asst("s1", "m1", "m", "/work/alpha", 10, 100))
	agg.Add(asst("s1", "m2", "m", "/work/alpha", 5, 50))
	agg.Add(asst("s2", "m3", "m", "/work/beta", 1, 400))
	db, err := storage.Open(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(agg.Snapshot())
	if err := db.SaveState("aggregate", blob); err != nil {
		t.Fatal(err)
	}
	db.Close()

	r := exec(t, p, "", "stats")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	lines := strings.Split(strings.TrimSpace(r.out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "PROJECT") {
		t.Fatalf("output:\n%s", r.out)
	}
	// Sorted by output tokens: beta (400) before alpha (150).
	if !strings.HasPrefix(lines[1], "/work/beta") || !strings.HasPrefix(lines[2], "/work/alpha") {
		t.Fatalf("order:\n%s", r.out)
	}
	if f := strings.Fields(lines[2]); f[1] != "1" || f[2] != "15" || f[3] != "150" {
		t.Fatalf("alpha row = %v", f)
	}
	if f := strings.Fields(lines[3]); f[0] != "TOTAL" || f[1] != "2" || f[2] != "16" || f[3] != "550" {
		t.Fatalf("total row = %v", f)
	}
}

func svcEnv(t *testing.T, p service.Paths, calls *[]string) (func() (svcmgr.Options, error), string) {
	home := t.TempDir()
	return func() (svcmgr.Options, error) {
		return svcmgr.Options{
			GOOS: "linux", Exe: "/usr/local/bin/harnext-agent", UserHome: home,
			Run: func(name string, args ...string) (string, error) {
				*calls = append(*calls, name+" "+strings.Join(args, " "))
				return "", nil
			},
		}, nil
	}, home
}

func execWith(t *testing.T, env Env, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	env.Version, env.Out, env.Err, env.In = "9.9.9", &out, &errb, strings.NewReader("")
	root := New(env)
	root.SetArgs(args)
	err := root.Execute()
	return result{out.String(), errb.String(), err}
}

func TestServiceInstallRequiresInit(t *testing.T) {
	p := newPaths(t)
	var calls []string
	opts, _ := svcEnv(t, p, &calls)
	r := execWith(t, Env{Paths: p, ServiceOptions: opts}, "service", "install")
	if r.failure == nil || !strings.Contains(r.failure.Error(), "not initialized") || len(calls) != 0 {
		t.Fatalf("err=%v calls=%v", r.failure, calls)
	}
}

func TestServiceInstallAndUninstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installs a systemd unit with a Unix binary path; runs on macOS and Linux")
	}
	p := newPaths(t)
	exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	var calls []string
	opts, home := svcEnv(t, p, &calls)
	env := Env{Paths: p, ServiceOptions: opts}

	r := execWith(t, env, "service", "install")
	if r.failure != nil || !strings.Contains(r.out, "Service installed") {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "harnext-agent.service")
	b, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), tok) || strings.Contains(r.out, tok) {
		t.Fatal("token leaked into the service definition or output")
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}

	r = execWith(t, env, "service", "uninstall")
	if r.failure != nil || !strings.Contains(r.out, "kept") {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatal("unit file still exists")
	}
	if _, err := p.LoadAccounts(); err != nil {
		t.Fatalf("uninstall removed the token: %v", err)
	}
}

// fakeRunningAgent holds the agent lock; the stubbed terminate releases it and counts the calls.
func fakeRunningAgent(t *testing.T, p service.Paths, stopErr error) *int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("lock probing differs on Windows")
	}
	if err := os.MkdirAll(filepath.Dir(p.Lock), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := service.AcquireLock(p.Lock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	stops := new(int)
	old := terminateAgent
	terminateAgent = func(int, time.Duration, func() bool) error {
		*stops++
		if stopErr != nil {
			return stopErr
		}
		l.Release()
		return nil
	}
	t.Cleanup(func() { terminateAgent = old })
	return stops
}

func TestServiceInstallRestartsRunningAgent(t *testing.T) {
	p := newPaths(t)
	exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	var calls []string
	opts, _ := svcEnv(t, p, &calls)
	stops := fakeRunningAgent(t, p, nil)
	r := execWith(t, Env{Paths: p, ServiceOptions: opts}, "service", "install")
	if r.failure != nil || strings.TrimSpace(r.out) != "Restarted the agent." || *stops != 1 || len(calls) != 2 {
		t.Fatalf("out=%q err=%v stops=%d calls=%v", r.out, r.failure, *stops, calls)
	}
}

func TestServiceInstallStopFailureDoesNotInstall(t *testing.T) {
	p := newPaths(t)
	exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	var calls []string
	opts, home := svcEnv(t, p, &calls)
	fakeRunningAgent(t, p, errors.New("the agent did not stop in time"))
	r := execWith(t, Env{Paths: p, ServiceOptions: opts}, "service", "install")
	if r.failure == nil || !strings.Contains(r.failure.Error(), "could not stop the running agent") || len(calls) != 0 {
		t.Fatalf("err=%v calls=%v", r.failure, calls)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "harnext-agent.service")); err == nil {
		t.Fatal("unit written despite stop failure")
	}
}

func TestStatusBackfillLine(t *testing.T) {
	p := newPaths(t)
	exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	now := time.Now().UTC()
	_ = p.WriteStatus(service.StatusFile{UpdatedAt: now})
	if r := exec(t, p, "", "status"); strings.Contains(r.out, "Backfill:") {
		t.Fatalf("a backfill line without any history:\n%s", r.out)
	}
	_ = p.WriteStatus(service.StatusFile{UpdatedAt: now, Backfill: service.BackfillStatus{Found: 312, Scanned: 312, Remaining: 140, FirstPacketAt: now}})
	r := exec(t, p, "", "status")
	line := ""
	for _, l := range strings.Split(r.out, "\n") {
		if strings.HasPrefix(l, "Backfill:") {
			line = l
		}
	}
	for _, want := range []string{"312 sessions found (last 30 days)", "first packet sent ", "140 remaining"} {
		if !strings.Contains(line, want) {
			t.Fatalf("backfill line %q lacks %q", line, want)
		}
	}
}
