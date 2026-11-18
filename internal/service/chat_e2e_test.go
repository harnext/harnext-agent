package service

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/harnext/harnext-agent/internal/watcher"
)

const (
	chatSessionID = "chat-session-0001"
	chatSentinel  = "SENTINEL_PROMPT_TEXT"
	chatNegText   = "fixture-neg-pattern-a"
)

type chatHarness struct {
	paths    Paths
	rec      *ingestRecorder
	chatRoot string
	otel     string
	keys     remoteconfig.TrustedKeys
	at       time.Time
}

func newChatHarness(t *testing.T, signed bool) *chatHarness {
	t.Helper()
	h := &chatHarness{rec: &ingestRecorder{}, keys: remoteconfig.TrustedKeys{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ingest", h.rec.handler)
	if signed {
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
		if h.keys, err = remoteconfig.ParseTrustedKeys(strings.Join(pairs, ",")); err != nil || envelope == nil {
			t.Fatalf("vector unusable: %v", err)
		}
		h.at, _ = time.Parse(time.RFC3339, vec.VerifyAt)
		mux.HandleFunc("/api/v1/agent-config", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("ETag", `"v2"`)
			_, _ = w.Write(envelope)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	h.otel = filepath.Join(t.TempDir(), "telemetry", "otel.jsonl")
	h.chatRoot = filepath.Join(t.TempDir(), "workspaceStorage")
	h.paths = PathsIn(filepath.Join(t.TempDir(), "home"))
	if err := os.MkdirAll(filepath.Dir(h.otel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.paths.SaveSettings(Settings{ServerURL: srv.URL, AllowInsecureLocalhost: true, CopilotOTelPath: h.otel}); err != nil {
		t.Fatal(err)
	}
	if err := h.paths.SaveToken(testToken); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *chatHarness) start(t *testing.T) (stop func() error) {
	t.Helper()
	var clock remoteconfig.Clock
	if !h.at.IsZero() {
		clock = frozenClock{h.at}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var (
		once   sync.Once
		result error
	)
	go func() {
		done <- Run(ctx, RunOptions{
			Paths: h.paths, Version: "0.1.0", Root: filepath.Join(h.chatRoot, "no-claude"), ChatRoots: []string{h.chatRoot},
			FlushInterval: 30 * time.Millisecond, UploadPeriod: 50 * time.Millisecond,
			Keys: h.keys, Clock: clock,
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

func (h *chatHarness) writeFiles(t *testing.T) {
	t.Helper()
	ws := filepath.Join(h.chatRoot, "hash0001")
	if err := os.MkdirAll(filepath.Join(ws, "chatSessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "workspace.json"), []byte(`{"folder":"file:///tmp/demo-project"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	const base = int64(1794258000000)
	req := func(id string, i int, text string) string {
		b, _ := json.Marshal(map[string]any{
			"requestId": id, "timestamp": base + int64(i)*600000, "modelId": "copilot/claude-test-1",
			"message": map[string]any{"text": text},
		})
		return string(b)
	}
	state := func(i, v int) string {
		return fmt.Sprintf(`{"kind":1,"k":["requests",%d,"modelState"],"v":{"value":%d,"completedAt":%d}}`, i, v, base+int64(i)*600000+60000)
	}
	// Patch order as VS Code writes it: each request, then its final state, then the next request.
	// req-2 pushes back on req-1 (negative pattern), req-2 itself is cancelled, req-3 is clean.
	appendReq := func(i int, r string) string { return fmt.Sprintf(`{"kind":2,"k":["requests"],"i":%d,"v":[%s]}`, i, r) }
	lines := []string{
		appendReq(0, req("req-1", 0, "please change the layout of the header "+chatSentinel)),
		state(0, 1),
		appendReq(1, req("req-2", 1, "that is "+chatNegText+", the header still overlaps the menu")),
		state(1, 2),
		appendReq(2, req("req-3", 2, "now add a unit test for the parser module please")),
		state(2, 1),
		`{"kind":1,"k":["requests",2,"result"],"v":{"metadata":{"resolvedModel":"claude-test-1","toolCallRounds":[{"toolCalls":[{"name":"FixtureTool"},{"name":"read_file"}]}]}}}`,
	}
	appendFile(t, filepath.Join(ws, "chatSessions", chatSessionID+".jsonl"), strings.Join(lines, "\n")+"\n")

	span := func(n int, in, out int) string {
		return fmt.Sprintf(`{"traceId":"cccc00000000000000000000000000%02d","spanId":"dddd0000000000%02d","name":"chat claude-test-1","kind":2,`+
			`"startTime":[%d,0],"endTime":[%d,500000000],"attributes":{"gen_ai.operation.name":"chat","gen_ai.request.model":"claude-test-1",`+
			`"gen_ai.response.model":"claude-test-1","gen_ai.conversation.id":"%s","gen_ai.agent.name":"panel/editAgent",`+
			`"gen_ai.usage.input_tokens":%d,"gen_ai.usage.output_tokens":%d},"resource":{"attributes":{"service.version":"0.68.0"}}}`+"\n",
			n, n, 1794258000+n*600, 1794258001+n*600, chatSessionID, in, out)
	}
	appendFile(t, h.otel, span(1, 100, 10)+span(2, 200, 20)+span(3, 300, 30))
}

// latestSession is the newest uploaded vscode_copilot row of the test session.
func latestSession(t *testing.T, rec *ingestRecorder) map[string]any {
	t.Helper()
	var last map[string]any
	for _, b := range rec.all() {
		var env struct {
			Sessions []map[string]any `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(b), &env); err != nil {
			t.Fatal(err)
		}
		for _, s := range env.Sessions {
			if s["tool_name"] == "vscode_copilot" && s["session_external_id"] == chatSessionID {
				last = s
			}
		}
	}
	return last
}

func (h *chatHarness) noLeaks(t *testing.T) {
	t.Helper()
	for _, b := range h.rec.all() {
		for _, bad := range []string{chatSentinel, chatNegText, "that is wrong", testToken, h.otel, h.chatRoot} {
			if strings.Contains(b, bad) {
				t.Fatalf("body leaks %q", bad)
			}
		}
	}
}

func TestChatEndToEnd(t *testing.T) {
	h := newChatHarness(t, true)
	h.writeFiles(t)

	stop := h.start(t)
	eventually(t, "analyzed copilot chat upload", func() bool {
		s := latestSession(t, h.rec)
		if s == nil || inputOf(s) != 600 {
			return false
		}
		q, _ := s["quality"].(map[string]any)
		return q != nil && q["analyzed_exchanges"] == float64(3)
	})
	s := latestSession(t, h.rec)
	q := s["quality"].(map[string]any)
	// Exchange 1: pushback from req-2. Exchange 2: cancelled. Exchange 3: clean.
	if q["exchanges"] != float64(3) || q["friction_exchanges"] != float64(2) || q["fx_pushback"] != float64(1) ||
		q["fx_interrupt"] != float64(1) || q["cancelled_responses"] != float64(1) || q["first_friction_exchange"] != float64(1) ||
		q["detector_version"] != float64(7) {
		t.Fatalf("quality = %v", q)
	}
	if s["project"] != "/tmp/demo-project" {
		t.Fatalf("session = %v", s)
	}
	labels := map[string]bool{"feature": true, "bugfix": true, "refactor": true, "test": true, "docs": true, "devops": true, "data": true, "explore": true, "other": true}
	if topic, _ := s["topic"].(string); !labels[topic] {
		t.Fatalf("topic = %v", s["topic"])
	}
	eventually(t, "chat status", func() bool {
		st, err := h.paths.ReadStatus()
		return err == nil && st.Copilot.ChatRequests == 3 && st.Copilot.ChatSessions == 1 && st.Copilot.ChatSkippedLines == 0
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	h.noLeaks(t)
	want := latestSession(t, h.rec)

	// Restart with the same home: nothing is counted twice.
	stop = h.start(t)
	eventually(t, "restarted chat status", func() bool {
		st, err := h.paths.ReadStatus()
		return err == nil && st.Copilot.ChatRequests == 3 && st.Copilot.ChatSessions == 1
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := latestSession(t, h.rec); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("session changed after restart: %v vs %v", got, want)
	}

	// Lost Copilot state: the chat file and the telemetry are read again with the same result.
	db, err := storage.Open(h.paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveState(copilotStateName, []byte("{nope")); err != nil {
		t.Fatal(err)
	}
	db.Close()
	stop = h.start(t)
	eventually(t, "status after state loss", func() bool {
		st, err := h.paths.ReadStatus()
		return err == nil && st.Copilot.ChatRequests == 3 && st.Copilot.UniqueSpans == 3
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := latestSession(t, h.rec); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("session changed after state loss: %v vs %v", got, want)
	}
	h.noLeaks(t)
}

func TestChatWithoutConfigurationHasNoFriction(t *testing.T) {
	h := newChatHarness(t, false)
	h.writeFiles(t)

	stop := h.start(t)
	defer stop()
	eventually(t, "unscored copilot upload", func() bool {
		s := latestSession(t, h.rec)
		return s != nil && inputOf(s) == 600 && s["project"] == "/tmp/demo-project"
	})
	s := latestSession(t, h.rec)
	if q := s["quality"].(map[string]any); q["analyzed_exchanges"] != float64(0) || q["exchanges"] != float64(3) || q["detector_version"] != float64(0) {
		t.Fatalf("quality = %v", q)
	}
	h.noLeaks(t)
}

func TestFolderFromURI(t *testing.T) {
	for in, want := range map[string]string{
		"file:///tmp/demo-project":         "/tmp/demo-project",
		"file:///tmp/my%20project":         "/tmp/my project",
		"file:///c%3A/Users/dev/x":         "c:/Users/dev/x",
		"file:///C:/Users/dev/x":           "C:/Users/dev/x",
		"vscode-remote://ssh-remote%2Bh/x": "",
		"file://server/share/x":            "",
		"https://example.com/x":            "",
		"file:relative":                    "",
		"/tmp/plain":                       "",
		"":                                 "",
	} {
		if got := folderFromURI(in); got != want {
			t.Errorf("folderFromURI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadWorkspaceFolder(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "w.json")
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if f, def := readWorkspaceFolder(write(`{"folder":"file:///tmp/a"}`)); f != "/tmp/a" || !def {
		t.Fatalf("folder = %q, %v", f, def)
	}
	if f, def := readWorkspaceFolder(write(`{"workspace":"file:///tmp/a.code-workspace"}`)); f != "" || !def {
		t.Fatalf("multi-root = %q, %v", f, def)
	}
	if f, def := readWorkspaceFolder(write(`{nope`)); f != "" || !def {
		t.Fatalf("corrupt = %q, %v", f, def)
	}
	if f, def := readWorkspaceFolder(write(`{"folder":"` + strings.Repeat("a", 5000) + `"}`)); f != "" || !def {
		t.Fatalf("oversized = %q, %v", f, def)
	}
	if f, def := readWorkspaceFolder(filepath.Join(dir, "missing.json")); f != "" || def {
		t.Fatalf("missing = %q, %v", f, def)
	}
}

func TestOffsetStoreDeletePrefix(t *testing.T) {
	db, _ := openDB(t)
	o, _ := LoadOffsets(db)
	_ = o.Set("/chat/a/1.jsonl", watcher.FileState{Offset: 1})
	_ = o.Set("/chat/b/2.jsonl", watcher.FileState{Offset: 2})
	_ = o.Set("/chatter/3.jsonl", watcher.FileState{Offset: 3})
	_ = o.Set("/claude/4.jsonl", watcher.FileState{Offset: 4})
	if err := o.Commit(o.Pending()); err != nil {
		t.Fatal(err)
	}
	if err := o.DeletePrefix("/chat/"); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(o.Pending()); err != nil {
		t.Fatal(err)
	}
	paths, _ := db.OffsetPaths()
	if len(paths) != 2 || len(o.Paths()) != 2 {
		t.Fatalf("db = %v, memory = %v", paths, o.Paths())
	}
	if _, ok := o.Get("/chatter/3.jsonl"); !ok {
		t.Fatal("sibling prefix was deleted")
	}
}

func TestRestoreCopilotDropsChatOffsets(t *testing.T) {
	db, _ := openDB(t)
	o, _ := LoadOffsets(db)
	root := filepath.Join(os.TempDir(), "ws-root")
	chatFile := filepath.Join(root, "h", "chatSessions", "s.jsonl")
	_ = o.Set(chatFile, watcher.FileState{Offset: 9})
	_ = o.Set("/x/claude.jsonl", watcher.FileState{Offset: 5})
	logf := func(string, ...any) {}

	if _, err := restoreCopilot(db, o, "/x/cop.jsonl", logf, root); err != nil {
		t.Fatal(err)
	}
	if _, ok := o.Get(chatFile); ok {
		t.Fatal("chat offset kept without copilot state")
	}
	if _, ok := o.Get("/x/claude.jsonl"); !ok {
		t.Fatal("claude offset dropped")
	}

	blob, _ := json.Marshal(fixtureAggregator(t).Snapshot())
	_ = db.SaveState(copilotStateName, blob)
	_ = o.Set(chatFile, watcher.FileState{Offset: 9})
	if _, err := restoreCopilot(db, o, "/x/cop.jsonl", logf, root); err != nil {
		t.Fatal(err)
	}
	if _, ok := o.Get(chatFile); !ok {
		t.Fatal("chat offset dropped despite valid state")
	}
}

// States saved before the active-minutes bump (Claude 3, Copilot 5) cannot be trusted: they are discarded and the files are read
// again from offset 0, once. The next snapshot carries the new versions.
func TestStatesBeforeActiveMinutesAreDiscardedAndReread(t *testing.T) {
	db, _ := openDB(t)
	o, _ := LoadOffsets(db)
	root := filepath.Join(os.TempDir(), "ws-root")
	chatFile := filepath.Join(root, "h", "chatSessions", "s.jsonl")
	const cop = "/x/cop.jsonl"
	for _, p := range []string{"/x/claude.jsonl", cop, chatFile} {
		_ = o.Set(p, watcher.FileState{Offset: 9})
	}
	_ = db.SaveState(stateName, []byte(`{"version":3,"sessions":{},"seen":[],"counters":{"skipped":{},"ignored":{}}}`))
	_ = db.SaveState(copilotStateName, []byte(`{"version":5,"sessions":{},"seen":[],"counters":{"skipped":{},"ignored":{}}}`))
	logf := func(string, ...any) {}

	claude, err := restoreAggregator(db, o, logf)
	if err != nil || claude.Snapshot().Version != 4 {
		t.Fatalf("claude: err=%v version=%d", err, claude.Snapshot().Version)
	}
	if len(o.Paths()) != 0 {
		t.Fatalf("offsets kept after a version 3 state: %v", o.Paths())
	}
	_ = o.Set("/x/claude.jsonl", watcher.FileState{Offset: 9})
	_ = o.Set(cop, watcher.FileState{Offset: 9})
	_ = o.Set(chatFile, watcher.FileState{Offset: 9})
	cp, err := restoreCopilot(db, o, cop, logf, root)
	if err != nil || cp.Snapshot().Version != 6 {
		t.Fatalf("copilot: err=%v version=%d", err, cp.Snapshot().Version)
	}
	if _, ok := o.Get(cop); ok {
		t.Fatal("copilot offset kept after a version 5 state")
	}
	if _, ok := o.Get(chatFile); ok {
		t.Fatal("chat offset kept after a version 5 state")
	}
	if _, ok := o.Get("/x/claude.jsonl"); !ok {
		t.Fatal("restoreCopilot touched a claude offset")
	}
}

// inputOf sums input tokens over a wire session's usage rows (payload v4).
func inputOf(s map[string]any) int64 {
	var n int64
	use, _ := s["usage"].([]any)
	for _, u := range use {
		if m, ok := u.(map[string]any); ok {
			v, _ := m["input_tokens"].(float64)
			n += int64(v)
		}
	}
	return n
}
