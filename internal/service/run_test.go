package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/account"
	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
	"github.com/harnext/harnext-agent/internal/selfupdate"
	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/uploader"
	"github.com/harnext/harnext-agent/internal/watcher"
)

const testToken = "hxt_" + "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"

func assistant(id string, out int) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":"s1","requestId":"req_%s","uuid":"u_%s","cwd":"/home/dev/example-project",`+
		`"gitBranch":"main","version":"2.1.237","timestamp":"2026-01-01T10:00:05.500Z","message":{"id":"%s","model":"claude-sonnet-5",`+
		`"content":[{"type":"text","text":"SECRET-ANSWER-TEXT"}],"usage":{"input_tokens":10,"output_tokens":%d}}}`+"\n", id, id, id, out)
}

type ingestRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *ingestRecorder) handler(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, string(b))
	r.mu.Unlock()
	var env struct{ Sessions []json.RawMessage }
	_ = json.Unmarshal(b, &env)
	fmt.Fprintf(w, `{"accepted":%d,"rejected":0,"errors":[]}`, len(env.Sessions))
}

func (r *ingestRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

type harness struct {
	paths Paths
	root  string
	rec   *ingestRecorder
	srv   *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	rec := &ingestRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ingest", rec.handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := PathsIn(filepath.Join(t.TempDir(), "home"))
	if err := p.SaveSettings(Settings{ServerURL: srv.URL, AllowInsecureLocalhost: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveToken(testToken); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &harness{paths: p, root: root, rec: rec, srv: srv}
}

func (h *harness) start(t *testing.T) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var (
		once   sync.Once
		result error
	)
	go func() {
		done <- Run(ctx, RunOptions{Gate: testGate,
			Paths: h.paths, Version: "0.1.0", Root: h.root, ChatRoots: []string{filepath.Join(h.root, "chat")},
			FlushInterval: 30 * time.Millisecond, UploadPeriod: 50 * time.Millisecond,
			Keys: remoteconfig.TrustedKeys{},
		})
	}()
	return func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				t.Error("Run did not stop")
			}
		})
		return result
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestRunUploadsPersistsAndResumes(t *testing.T) {
	h := newHarness(t)
	file := filepath.Join(h.root, "proj", "s1.jsonl")
	appendFile(t, file, assistant("m1", 100))

	stop := h.start(t)
	eventually(t, "first upload", func() bool { return len(h.rec.all()) >= 1 })
	body := h.rec.all()[0]
	for _, want := range []string{`"output_tokens":100`, `"input_tokens":10`, `"project":"/home/dev/example-project"`, `"quality":{`, `"first_friction_exchange":null`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "SECRET-ANSWER-TEXT") || strings.Contains(body, testToken) {
		t.Fatalf("body leaks content or token: %s", body)
	}
	eventually(t, "status file", func() bool {
		s, err := h.paths.ReadStatus()
		return err == nil && s.Upload.Accepted == 1 && s.UniqueResponses == 1 && !s.Config.TrustedKeys
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// Offsets and snapshot reached the database.
	db, err := storage.Open(h.paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.LoadState(stateName); !ok {
		t.Fatal("aggregator snapshot not saved")
	}
	if paths, _ := db.OffsetPaths(); len(paths) != 1 {
		t.Fatalf("offsets = %v", paths)
	}
	db.Close()

	// Restart: nothing is re-read or re-sent; a new line produces exactly one more upload
	// whose totals include the old and the new response.
	before := len(h.rec.all())
	stop = h.start(t)
	appendFile(t, file, assistant("m2", 50))
	eventually(t, "upload after append", func() bool { return len(h.rec.all()) > before })
	last := h.rec.all()[len(h.rec.all())-1]
	if !strings.Contains(last, `"output_tokens":150`) || !strings.Contains(last, `"input_tokens":20`) {
		t.Fatalf("totals after resume wrong: %s", last)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if n := len(h.rec.all()) - before; n != 1 {
		t.Fatalf("uploads after restart = %d, want 1", n)
	}
}

func TestRunRefusesWithoutInit(t *testing.T) {
	p := PathsIn(filepath.Join(t.TempDir(), "home"))
	err := Run(context.Background(), RunOptions{Paths: p, Version: "0.1.0", Keys: remoteconfig.TrustedKeys{}})
	if err != ErrNotInitialized {
		t.Fatalf("err = %v, want ErrNotInitialized", err)
	}
}

func TestSecondInstanceIsRefused(t *testing.T) {
	h := newHarness(t)
	stop := h.start(t)
	defer stop()
	eventually(t, "first instance running", func() bool {
		_, running := RunningPID(h.paths.Lock)
		return running
	})
	if pid, running := RunningPID(h.paths.Lock); !running || pid != os.Getpid() {
		t.Fatalf("RunningPID = %d, %v", pid, running)
	}
	err := Run(context.Background(), RunOptions{Paths: h.paths, Version: "0.1.0", Root: h.root, Keys: remoteconfig.TrustedKeys{}})
	if err != ErrAlreadyRunning {
		t.Fatalf("err = %v, want ErrAlreadyRunning", err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, running := RunningPID(h.paths.Lock); running {
		t.Fatal("lock still held after stop")
	}
}

func TestOffsetsWithoutSnapshotAreDiscarded(t *testing.T) {
	db, _ := openDB(t)
	o, _ := LoadOffsets(db)
	_ = o.Set("/a", watcher.FileState{Offset: 99, Size: 99})
	if err := o.Commit(o.Pending()); err != nil {
		t.Fatal(err)
	}
	var msgs []string
	logf := func(f string, a ...any) { msgs = append(msgs, fmt.Sprintf(f, a...)) }

	// No snapshot at all.
	if _, err := restoreAggregator(db, o, logf); err != nil {
		t.Fatal(err)
	}
	if len(o.Paths()) != 0 {
		t.Fatal("offsets kept although no snapshot exists")
	}

	// Corrupt snapshot.
	_ = o.Set("/b", watcher.FileState{Offset: 1})
	_ = db.SaveState(stateName, []byte("{not json"))
	agg, err := restoreAggregator(db, o, logf)
	if err != nil || len(agg.Rows()) != 0 || len(o.Paths()) != 0 || len(msgs) == 0 {
		t.Fatalf("corrupt snapshot: err=%v paths=%v msgs=%v", err, o.Paths(), msgs)
	}

	// Valid snapshot keeps the offsets.
	good := aggregate.New()
	blob, _ := json.Marshal(good.Snapshot())
	_ = db.SaveState(stateName, blob)
	_ = o.Set("/c", watcher.FileState{Offset: 1})
	if _, err := restoreAggregator(db, o, logf); err != nil || len(o.Paths()) != 1 {
		t.Fatalf("valid snapshot: err=%v paths=%v", err, o.Paths())
	}
}

type frozenClock struct{ now time.Time }

func (c frozenClock) Now() time.Time { return c.now }

func (c frozenClock) Sleep(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

// A verified signed configuration makes the upload carry its version, and status reports it.
func TestRunSendsConfigVersion(t *testing.T) {
	data, err := os.ReadFile(filepath.FromSlash("../../contracts/testdata/agent-config.v1.vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vec struct {
		VerifyAt    string            `json:"verify_at"`
		TrustedKeys map[string]string `json:"trusted_keys"`
		Cases       []struct {
			Name     string          `json:"name"`
			Envelope json.RawMessage `json:"envelope"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &vec); err != nil {
		t.Fatal(err)
	}
	var envelope json.RawMessage
	for _, c := range vec.Cases {
		if c.Name == "valid" {
			envelope = c.Envelope
		}
	}
	var pairs []string
	for kid, k := range vec.TrustedKeys {
		pairs = append(pairs, kid+":"+k)
	}
	keys, err := remoteconfig.ParseTrustedKeys(strings.Join(pairs, ","))
	if err != nil || envelope == nil {
		t.Fatalf("vector unusable: %v", err)
	}
	at, _ := time.Parse(time.RFC3339, vec.VerifyAt)

	rec := &ingestRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ingest", rec.handler)
	mux.HandleFunc("/api/v1/agent-config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"v2"`)
		_, _ = w.Write(envelope)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := PathsIn(filepath.Join(t.TempDir(), "home"))
	if err := p.SaveSettings(Settings{ServerURL: srv.URL, AllowInsecureLocalhost: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveToken(testToken); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "projects")
	_ = os.MkdirAll(filepath.Join(root, "proj"), 0o755)
	appendFile(t, filepath.Join(root, "proj", "s1.jsonl"), assistant("m1", 100))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Gate: testGate, Paths: p, Version: "0.1.0", Root: root, ChatRoots: []string{filepath.Join(root, "chat")}, FlushInterval: 30 * time.Millisecond,
			UploadPeriod: 50 * time.Millisecond, Keys: keys, Clock: frozenClock{at}})
	}()
	eventually(t, "upload with config_version", func() bool {
		for _, b := range rec.all() {
			if strings.Contains(b, `"config_version":2`) {
				return true
			}
		}
		return false
	})
	eventually(t, "status reports the configuration", func() bool {
		s, err := p.ReadStatus()
		return err == nil && s.Config.Loaded && s.Config.Version == 2 && s.Config.TrustedKeys
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func vectorConfig(t *testing.T) (remoteconfig.TrustedKeys, json.RawMessage, time.Time) {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash("../../contracts/testdata/agent-config.v1.vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vec struct {
		VerifyAt    string            `json:"verify_at"`
		TrustedKeys map[string]string `json:"trusted_keys"`
		Cases       []struct {
			Name     string          `json:"name"`
			Envelope json.RawMessage `json:"envelope"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &vec); err != nil {
		t.Fatal(err)
	}
	var envelope json.RawMessage
	for _, c := range vec.Cases {
		if c.Name == "valid" {
			envelope = c.Envelope
		}
	}
	var pairs []string
	for kid, k := range vec.TrustedKeys {
		pairs = append(pairs, kid+":"+k)
	}
	keys, err := remoteconfig.ParseTrustedKeys(strings.Join(pairs, ","))
	if err != nil || envelope == nil {
		t.Fatalf("vector unusable: %v", err)
	}
	at, _ := time.Parse(time.RFC3339, vec.VerifyAt)
	return keys, envelope, at
}

// P4: a configuration that arrives after the history was read (it missed the grace period) makes the agent read that
// history again, so the sessions it sent without friction are re-scored and sent once more.
func TestConfigArrivingLateRereadsHistoryWithAnalysis(t *testing.T) {
	keys, envelope, at := vectorConfig(t)
	release := make(chan struct{})
	rec := &ingestRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ingest", rec.handler)
	mux.HandleFunc("/api/v1/agent-config", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("ETag", `"v2"`)
		_, _ = w.Write(envelope)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := PathsIn(filepath.Join(t.TempDir(), "home"))
	if err := p.SaveSettings(Settings{ServerURL: srv.URL, AllowInsecureLocalhost: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveToken(testToken); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "projects")
	_ = os.MkdirAll(filepath.Join(root, "proj"), 0o755)
	user := `{"type":"user","sessionId":"s1","uuid":"uu1","timestamp":"2026-01-01T10:00:01.000Z","cwd":"/home/dev/example-project","message":{"role":"user","content":"please fix the failing test"}}` + "\n"
	appendFile(t, filepath.Join(root, "proj", "s1.jsonl"), user+assistant("m1", 100))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Gate: testGate, Paths: p, Version: "0.1.0", Root: root, ChatRoots: []string{filepath.Join(root, "chat")}, FlushInterval: 30 * time.Millisecond,
			UploadPeriod: 50 * time.Millisecond, Keys: keys, Clock: frozenClock{at}, ConfigGrace: 30 * time.Millisecond})
	}()
	has := func(sub string) bool {
		for _, b := range rec.all() {
			if strings.Contains(b, sub) {
				return true
			}
		}
		return false
	}
	eventually(t, "history sent without the configuration (nothing analyzed)", func() bool { return has(`"analyzed_exchanges":0`) })
	if has(`"analyzed_exchanges":1`) {
		t.Fatal("nothing can be analyzed before the configuration exists")
	}
	close(release)
	eventually(t, "history re-read and re-scored with the configuration", func() bool { return has(`"analyzed_exchanges":1`) })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A release check that installs a newer agent makes Run flush and return ErrRestartForUpdate (the caller restarts into it).
func TestRunRestartsAfterAnUpdate(t *testing.T) {
	h := newHarness(t)
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Gate: testGate,
			Paths: h.paths, Version: "0.2.1", Root: h.root, ChatRoots: []string{filepath.Join(h.root, "chat")},
			FlushInterval: 30 * time.Millisecond, UploadPeriod: 50 * time.Millisecond, Keys: remoteconfig.TrustedKeys{},
			UpdateFirstDelay: 20 * time.Millisecond, UpdateInterval: 20 * time.Millisecond,
			UpdateCheck: func(context.Context) (string, error) {
				calls++
				if calls < 3 {
					return "", selfupdate.ErrNoUpdate // the first checks find nothing; the loop keeps asking
				}
				return "9.9.9", nil
			},
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartForUpdate) || calls < 3 {
			t.Fatalf("err = %v after %d checks", err, calls)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the update")
	}
	// The agent can start again (the instance lock was released).
	stop := h.start(t)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestRunWithoutUpdateCheckNeverAsksTheServer(t *testing.T) {
	var asked int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent-release", func(http.ResponseWriter, *http.Request) { asked++ })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("HARNEXT_NO_AUTO_UPDATE", "1")
	keys, _, _ := vectorConfig(t)
	p := PathsIn(filepath.Join(t.TempDir(), "home"))
	if err := p.SaveSettings(Settings{ServerURL: srv.URL, AllowInsecureLocalhost: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveToken(testToken); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "projects")
	_ = os.MkdirAll(root, 0o755)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Gate: testGate, Paths: p, Version: "0.2.1", Root: root, ChatRoots: []string{filepath.Join(root, "chat")},
			FlushInterval: 30 * time.Millisecond, UploadPeriod: 50 * time.Millisecond, Keys: keys, UpdateFirstDelay: 10 * time.Millisecond})
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	if err := <-done; err != nil || asked != 0 {
		t.Fatalf("err %v, asked %d", err, asked)
	}
}

// testGate sends every session as one fixed account to one fixed route; the production default is fail-closed.
func testGate(aggregate.Row) uploader.Decision {
	return uploader.Decision{Send: true, AccountHash: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90", RouteID: "route-a"}
}

// The sink tags the session with the digest of its config directory (here the parent of Root) and the gate sees it with FirstAt.
func TestGateReceivesSourceAndFirstAt(t *testing.T) {
	h := newHarness(t)
	appendFile(t, filepath.Join(h.root, "proj", "s1.jsonl"), assistant("m1", 100))
	var mu sync.Mutex
	var seen []aggregate.Row
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Paths: h.paths, Version: "0.1.0", Root: h.root, ChatRoots: []string{filepath.Join(h.root, "chat")},
			FlushInterval: 30 * time.Millisecond, UploadPeriod: 50 * time.Millisecond, Keys: remoteconfig.TrustedKeys{},
			Gate: func(r aggregate.Row) uploader.Decision {
				mu.Lock()
				seen = append(seen, r)
				mu.Unlock()
				return testGate(r)
			},
		})
	}()
	defer func() { cancel(); <-done }()
	eventually(t, "upload", func() bool { return len(h.rec.all()) >= 1 })
	mu.Lock()
	defer mu.Unlock()
	want := account.DirID(filepath.Dir(h.root))
	if len(seen) == 0 || seen[0].Source != want || seen[0].FirstAt.IsZero() {
		t.Fatalf("gate rows = %+v, want Source %s", seen, want)
	}
	if !strings.Contains(h.rec.all()[0], `"account_hash":"a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"`) {
		t.Fatalf("body lacks account_hash: %s", h.rec.all()[0])
	}
}

// Without a Gate (the default until accounts are bound) nothing leaves the machine.
func TestNilGateSendsNothing(t *testing.T) {
	h := newHarness(t)
	appendFile(t, filepath.Join(h.root, "proj", "s1.jsonl"), assistant("m1", 100))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Paths: h.paths, Version: "0.1.0", Root: h.root, ChatRoots: []string{filepath.Join(h.root, "chat")},
			FlushInterval: 30 * time.Millisecond, UploadPeriod: 50 * time.Millisecond, Keys: remoteconfig.TrustedKeys{},
		})
	}()
	defer func() { cancel(); <-done }()
	eventually(t, "responses read", func() bool {
		s, err := h.paths.ReadStatus()
		return err == nil && s.UniqueResponses == 1
	})
	time.Sleep(300 * time.Millisecond)
	if n := len(h.rec.all()); n != 0 {
		t.Fatalf("%d uploads with no gate", n)
	}
}
