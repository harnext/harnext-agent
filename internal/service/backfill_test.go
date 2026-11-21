package service

import (
	"context"
	"encoding/json"
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

	"github.com/harnext/harnext-agent/internal/remoteconfig"
	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/uploader"
)

// ---- tracker -------------------------------------------------------------------------

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestFirstPacketReadyAtSessionLimit(t *testing.T) {
	now := time.Now()
	b := newBackfill(func() time.Time { return now }, nil, 3, 7*24*time.Hour)
	src, other := b.source(), b.source()
	src.Found(10)
	for i := 0; i < 2; i++ {
		src.File(now.Add(-time.Hour))
	}
	if closed(b.FirstPacketReady()) {
		t.Fatal("ready after 2 of 3 sessions while another source is still running")
	}
	src.File(now.Add(-time.Hour))
	if !closed(b.FirstPacketReady()) {
		t.Fatal("not ready at the session limit")
	}
	_ = other
}

func TestFirstPacketReadyWhenEverySourceCrossedTheWindow(t *testing.T) {
	now := time.Now()
	b := newBackfill(func() time.Time { return now }, nil, 400, 7*24*time.Hour)
	claude, chat, tail := b.source(), b.source(), b.source()
	claude.Found(100)
	chat.Found(100)
	claude.File(now.Add(-24 * time.Hour))
	claude.File(now.Add(-8 * 24 * time.Hour)) // first file outside the window: claude is past it
	if closed(b.FirstPacketReady()) {
		t.Fatal("ready while chat and the telemetry file are still inside the window")
	}
	tail.Done()
	chat.File(now.Add(-9 * 24 * time.Hour))
	if !closed(b.FirstPacketReady()) {
		t.Fatal("not ready although every source passed the window")
	}
	if b.Status().Scanned != 3 || b.Status().Found != 200 {
		t.Fatalf("status = %+v", b.Status())
	}
}

func TestFirstPacketReadyWhenNothingToRead(t *testing.T) {
	b := newBackfill(nil, nil, 400, 7*24*time.Hour)
	a, c := b.source(), b.source()
	a.Done()
	if closed(b.FirstPacketReady()) {
		t.Fatal("ready before the second source reported")
	}
	c.Found(0)
	if !closed(b.FirstPacketReady()) {
		t.Fatal("not ready with nothing to read")
	}
}

func TestBackfillStatusLineAndCompletion(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	b := newBackfill(nil, logf, 400, 7*24*time.Hour)
	s := b.source()
	s.Found(5)
	for i := 0; i < 5; i++ {
		s.File(time.Now())
	}
	s.Done()
	if st := b.Status(); st.Remaining != 0 || st.Scanned != 5 || !st.FirstPacketAt.IsZero() {
		t.Fatalf("status = %+v", st)
	}
	if !strings.Contains(b.Status().Line(), "first packet not sent yet") {
		t.Fatalf("line = %q", b.Status().Line())
	}

	_ = b.Catchup() // the pass starts after every scan finished
	b.Batch(uploader.Result{Accepted: 3, Remaining: 2})
	if len(logs) != 1 || !strings.Contains(logs[0], "5 sessions found") || !strings.Contains(logs[0], "2 remaining") || !strings.Contains(logs[0], "first packet sent") {
		t.Fatalf("logs = %q", logs)
	}
	if st := b.Status(); st.Remaining != 2 || st.FirstPacketAt.IsZero() || st.Complete {
		t.Fatalf("status after batch = %+v", st)
	}
	b.Batch(uploader.Result{Accepted: 5, Remaining: 0})
	b.Pass(uploader.Result{Accepted: 5}, nil)
	if st := b.Status(); st.Remaining != 0 || !st.Complete {
		t.Fatalf("status after pass = %+v", st)
	}
	if len(logs) != 2 {
		t.Fatalf("expected one line at the first packet and one at completion, got %q", logs)
	}
}

// A pass that began while files were still being read does not complete the backfill.
func TestBackfillNotCompleteWhenPassStartedEarly(t *testing.T) {
	b := newBackfill(nil, nil, 400, 7*24*time.Hour)
	s := b.source()
	s.Found(2)
	s.File(time.Now())
	if !b.Catchup() {
		t.Fatal("catch-up not reported while a file is unread")
	}
	s.File(time.Now())
	s.Done()
	b.Pass(uploader.Result{}, nil)
	if b.Status().Complete {
		t.Fatal("complete although the pass started before the last file was read")
	}
	if b.Catchup() {
		t.Fatal("catch-up still reported after the scan finished")
	}
	b.Pass(uploader.Result{}, nil)
	if !b.Status().Complete {
		t.Fatal("not complete after a pass that started after the scan")
	}
}

// Sessions the gate keeps back are not backlog, and a pass that only held sessions has not delivered the first packet; when
// the identity makes them sendable the pass queues them and the first packet is recorded as usual.
func TestBackfillIgnoresGateHeldSessions(t *testing.T) {
	b := newBackfill(nil, nil, 400, 7*24*time.Hour)
	s := b.source()
	s.Found(3)
	for i := 0; i < 3; i++ {
		s.File(time.Now())
	}
	s.Done()
	_ = b.Catchup()
	b.Queued(0)
	b.Pass(uploader.Result{Skipped: 3}, nil)
	if st := b.Status(); st.Remaining != 0 || !st.FirstPacketAt.IsZero() {
		t.Fatalf("status with every session held = %+v", st)
	}
	_ = b.Catchup()
	b.Queued(3)
	if st := b.Status(); st.Remaining != 3 {
		t.Fatalf("remaining after the identity arrived = %d, want 3", st.Remaining)
	}
	b.Batch(uploader.Result{Accepted: 2, Remaining: 1})
	if st := b.Status(); st.Remaining != 1 || st.FirstPacketAt.IsZero() {
		t.Fatalf("status after the first batch = %+v", st)
	}
	b.Pass(uploader.Result{Accepted: 3}, nil)
	if st := b.Status(); st.Remaining != 0 || !st.Complete {
		t.Fatalf("status after the pass = %+v", st)
	}
}

func TestBackfillLineHasNoPathsOrContent(t *testing.T) {
	line := BackfillStatus{Found: 12, Remaining: 3, FirstPacketAt: time.Now()}.Line()
	for _, bad := range []string{"/", "\\", ".jsonl"} {
		if strings.Contains(line, bad) {
			t.Fatalf("line %q contains %q", line, bad)
		}
	}
}

// ---- acceptance -----------------------------------------------------------------------

type signedConfig struct {
	keys     remoteconfig.TrustedKeys
	envelope json.RawMessage
	at       time.Time
}

func loadSignedConfig(t *testing.T) signedConfig {
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
	var sc signedConfig
	for _, c := range vec.Cases {
		if c.Name == "valid" {
			sc.envelope = c.Envelope
		}
	}
	var pairs []string
	for kid, k := range vec.TrustedKeys {
		pairs = append(pairs, kid+":"+k)
	}
	if sc.keys, err = remoteconfig.ParseTrustedKeys(strings.Join(pairs, ",")); err != nil || sc.envelope == nil {
		t.Fatalf("vector unusable: %v", err)
	}
	sc.at, _ = time.Parse(time.RFC3339, vec.VerifyAt)
	return sc
}

// timedRecorder remembers when each ingest request arrived.
type timedRecorder struct {
	mu     sync.Mutex
	at     []time.Time
	bodies []map[string]any
	raw    []string
}

func (r *timedRecorder) handler(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	r.mu.Lock()
	r.at = append(r.at, time.Now())
	r.bodies = append(r.bodies, body)
	r.raw = append(r.raw, string(b))
	r.mu.Unlock()
	n := 0
	if s, ok := body["sessions"].([]any); ok {
		n = len(s)
	}
	fmt.Fprintf(w, `{"accepted":%d,"rejected":0,"errors":[]}`, n)
}

func (r *timedRecorder) snapshot() ([]time.Time, []map[string]any, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.at...), append([]map[string]any(nil), r.bodies...), append([]string(nil), r.raw...)
}

func (r *timedRecorder) sessionIDs() []string {
	_, bodies, _ := r.snapshot()
	var ids []string
	for _, b := range bodies {
		ids = append(ids, batchIDs(b)...)
	}
	return ids
}

func batchIDs(body map[string]any) []string {
	var ids []string
	if s, ok := body["sessions"].([]any); ok {
		for _, x := range s {
			ids = append(ids, x.(map[string]any)["session_external_id"].(string))
		}
	}
	return ids
}

const (
	syntheticFiles   = 500
	syntheticExpired = 12
)

// writeSyntheticHistory creates syntheticFiles transcripts spread evenly over the last 30
// days (index 0 is the newest) and syntheticExpired older than the window. Each file is one
// session with a user turn and two responses; the event time equals the file's mtime.
func writeSyntheticHistory(t *testing.T, root string) {
	t.Helper()
	write := func(name string, age time.Duration) {
		p := filepath.Join(root, "proj-"+name[:2], name+".jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(-age).UTC()
		stamp := func(d time.Duration) string { return ts.Add(d).Format("2006-01-02T15:04:05.000Z") }
		var sb strings.Builder
		fmt.Fprintf(&sb, `{"type":"user","sessionId":"%[1]s","uuid":"u_%[1]s","cwd":"/home/dev/app","timestamp":"%[2]s","message":{"content":"please fix the build"}}`+"\n", name, stamp(-2*time.Minute))
		for i := 0; i < 2; i++ {
			fmt.Fprintf(&sb, `{"type":"assistant","sessionId":"%[1]s","requestId":"req_%[1]s_%[3]d","uuid":"a_%[1]s_%[3]d","cwd":"/home/dev/app","timestamp":"%[2]s","message":{"id":"msg_%[1]s_%[3]d","model":"claude-sonnet-5","content":[{"type":"text","text":"SECRET-ANSWER-TEXT"}],"usage":{"input_tokens":10,"output_tokens":%[4]d}}}`+"\n",
				name, stamp(time.Duration(i)*time.Minute), i, 20+i)
		}
		if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		m := time.Now().Add(-age)
		if err := os.Chtimes(p, m, m); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < syntheticFiles; i++ {
		write(fmt.Sprintf("s%04d", i), time.Duration(float64(i)/float64(syntheticFiles)*29.5*24*float64(time.Hour))+time.Hour)
	}
	for i := 0; i < syntheticExpired; i++ {
		write(fmt.Sprintf("x%04d", i), time.Duration(32+i)*24*time.Hour)
	}
}

type backfillEnv struct {
	paths Paths
	root  string
	rec   *timedRecorder
	srv   *httptest.Server
}

// newBackfillEnv starts a server whose agent-config handler is cfg (nil = no config route).
func newBackfillEnv(t *testing.T, cfg http.HandlerFunc) *backfillEnv {
	t.Helper()
	e := &backfillEnv{rec: &timedRecorder{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ingest", e.rec.handler)
	if cfg != nil {
		mux.HandleFunc("/api/v1/agent-config", cfg)
	}
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	e.paths = PathsIn(filepath.Join(t.TempDir(), "home"))
	if err := e.paths.SaveSettings(Settings{ServerURL: e.srv.URL, AllowInsecureLocalhost: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.paths.SaveToken(testToken); err != nil {
		t.Fatal(err)
	}
	e.root = filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(e.root, 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

// run starts the agent and returns when the process "started" (the instant before Run).
func (e *backfillEnv) run(t *testing.T, sc signedConfig, mutate func(*RunOptions)) (start time.Time, stop func() error) {
	t.Helper()
	opts := RunOptions{Gate: testGate,
		Paths: e.paths, Version: "0.2.0", Root: e.root, ChatRoots: []string{filepath.Join(e.root, "chat")},
		FlushInterval: 50 * time.Millisecond, UploadPeriod: 50 * time.Millisecond, BatchPause: 20 * time.Millisecond,
		Keys: sc.keys, Clock: frozenClock{sc.at},
	}
	if mutate != nil {
		mutate(&opts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start = time.Now()
	go func() { done <- Run(ctx, opts) }()
	var once sync.Once
	var result error
	stop = func() error {
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
	t.Cleanup(func() { _ = stop() })
	return start, stop
}

func signedConfigHandler(sc signedConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"v2"`)
		_, _ = w.Write(sc.envelope)
	}
}

// acceptanceBudget is deliberately wide for CI machines; a healthy run needs well under a second.
const acceptanceBudget = 10 * time.Second

// Acceptance (P2): with 30 days of history and a fast configuration server, the first accepted
// upload leaves within the budget, the newest sessions go first, nothing older than 30 days is
// read, and the whole history arrives without duplicates.
func TestBackfillFirstPacketWithinBudget(t *testing.T) {
	sc := loadSignedConfig(t)
	e := newBackfillEnv(t, signedConfigHandler(sc))
	writeSyntheticHistory(t, e.root)

	start, stop := e.run(t, sc, nil)
	eventually(t, "first upload", func() bool { at, _, _ := e.rec.snapshot(); return len(at) > 0 })
	at, bodies, raw := e.rec.snapshot()
	first := at[0].Sub(start)
	t.Logf("process start -> first accepted upload: %s (budget %s)", first.Round(time.Millisecond), acceptanceBudget)
	if first > acceptanceBudget {
		t.Fatalf("first upload after %s, want <= %s", first, acceptanceBudget)
	}

	// Newest sessions first: the first request holds the newest sessions that were read, in order.
	ids := batchIDs(bodies[0])
	if len(ids) == 0 {
		t.Fatal("empty first batch")
	}
	for i, id := range ids {
		if want := fmt.Sprintf("s%04d", i); id != want {
			t.Fatalf("first batch[%d] = %s, want %s (newest first)", i, id, want)
		}
	}
	if !strings.Contains(raw[0], `"config_version":2`) {
		t.Errorf("fast configuration was not used for the first packet: %.200s", raw[0])
	}

	// Everything inside the window arrives once; nothing older than 30 days does.
	eventually(t, "whole history uploaded", func() bool { return len(e.rec.sessionIDs()) >= syntheticFiles })
	seen := map[string]int{}
	for _, id := range e.rec.sessionIDs() {
		seen[id]++
	}
	for id, n := range seen {
		if strings.HasPrefix(id, "x") {
			t.Errorf("session %s is older than 30 days but was uploaded", id)
		}
		if n != 1 {
			t.Errorf("session %s uploaded %d times", id, n)
		}
	}
	if len(seen) != syntheticFiles {
		t.Errorf("distinct sessions uploaded = %d, want %d", len(seen), syntheticFiles)
	}
	for _, b := range raw {
		if strings.Contains(b, "SECRET-ANSWER-TEXT") || strings.Contains(b, "please fix the build") || strings.Contains(b, testToken) {
			t.Fatal("upload leaks content or token")
		}
	}

	// The status line: found, first packet time, remaining.
	eventually(t, "backfill complete in status", func() bool {
		s, err := e.paths.ReadStatus()
		return err == nil && s.Backfill.Complete
	})
	s, _ := e.paths.ReadStatus()
	if s.Backfill.Found != syntheticFiles || s.Backfill.Scanned != syntheticFiles || s.Backfill.Remaining != 0 || s.Backfill.FirstPacketAt.IsZero() {
		t.Fatalf("backfill status = %+v", s.Backfill)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(e.paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if paths, _ := db.OffsetPaths(); len(paths) != syntheticFiles {
		t.Fatalf("offsets = %d, want %d (expired files must not get one)", len(paths), syntheticFiles)
	}
}

// Config timeout path: the configuration server never answers. The first upload must still
// leave within the budget (after the short grace), without a configuration, and the 20 s
// fetch timeout must not matter.
func TestBackfillFirstPacketWhenConfigNeverArrives(t *testing.T) {
	sc := loadSignedConfig(t)
	release := make(chan struct{})
	e := newBackfillEnv(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	writeSyntheticHistory(t, e.root)

	start, stop := e.run(t, sc, nil) // default grace
	eventually(t, "first upload", func() bool { at, _, _ := e.rec.snapshot(); return len(at) > 0 })
	at, _, raw := e.rec.snapshot()
	first := at[0].Sub(start)
	t.Logf("config unreachable: process start -> first accepted upload: %s (grace %s, budget %s)", first.Round(time.Millisecond), configGrace, acceptanceBudget)
	if first > acceptanceBudget {
		t.Fatalf("first upload after %s, want <= %s", first, acceptanceBudget)
	}
	if first < configGrace-100*time.Millisecond {
		t.Errorf("first upload after %s: the grace period was not honored", first)
	}
	if strings.Contains(raw[0], `"config_version"`) {
		t.Errorf("first packet carries a config version although none arrived: %.200s", raw[0])
	}
	// Text read without a configuration is not analyzed: behavior kept from before P2 (P4 changes it).
	if !strings.Contains(raw[0], `"first_friction_exchange":null`) {
		t.Errorf("friction should stay null without a configuration: %.300s", raw[0])
	}
	eventually(t, "whole history uploaded", func() bool { return len(e.rec.sessionIDs()) >= syntheticFiles })
	eventually(t, "status shows the config error", func() bool {
		s, err := e.paths.ReadStatus()
		return err == nil && !s.Config.Loaded && s.Backfill.Complete
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// A configuration that arrives after the grace is picked up in the background; the first
// packet has gone out without it.
func TestBackfillLateConfigIsAppliedInTheBackground(t *testing.T) {
	sc := loadSignedConfig(t)
	slow := signedConfigHandler(sc)
	e := newBackfillEnv(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(700 * time.Millisecond):
			slow(w, r)
		case <-r.Context().Done():
		}
	})
	writeSyntheticHistory(t, e.root)

	_, stop := e.run(t, sc, func(o *RunOptions) { o.ConfigGrace = 100 * time.Millisecond })
	eventually(t, "first upload", func() bool { at, _, _ := e.rec.snapshot(); return len(at) > 0 })
	_, _, raw := e.rec.snapshot()
	if strings.Contains(raw[0], `"config_version"`) {
		t.Errorf("first packet should predate the late configuration: %.200s", raw[0])
	}
	eventually(t, "late configuration loaded", func() bool {
		s, err := e.paths.ReadStatus()
		return err == nil && s.Config.Loaded && s.Config.Version == 2
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}
