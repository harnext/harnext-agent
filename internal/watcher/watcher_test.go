package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/parser/claudecode"
)

type recSink struct {
	mu     sync.Mutex
	events []claudecode.Event
	ids    []FileID
	dirs   []string
	agg    *aggregate.Aggregator
	notify chan struct{}
}

func newSink(agg *aggregate.Aggregator) *recSink {
	return &recSink{agg: agg, notify: make(chan struct{}, 1024)}
}

func (s *recSink) Handle(file ClaudeFile, ev claudecode.Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.ids = append(s.ids, file.ID)
	s.dirs = append(s.dirs, file.ConfigDir)
	s.mu.Unlock()
	if s.agg != nil {
		s.agg.Add(ev)
	}
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *recSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// take returns and clears the received events.
func (s *recSink) take() []claudecode.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.events
	s.events, s.ids, s.dirs = nil, nil, nil
	return out
}

// rootOf is a root whose config directory is the parent of the transcript tree.
func rootOf(projects string) Root { return Root{Dir: filepath.Dir(projects), Projects: projects} }

func asstLine(msg string, out int) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":"s1","requestId":"r_%s","uuid":"a_%s","message":{"id":"%s","model":"m","usage":{"output_tokens":%d}}}`+"\n", msg, msg, msg, out)
}

func userLine(uuid string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":"s1","uuid":"%s","message":{"content":"fixture prompt"}}`+"\n", uuid)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

type harness struct {
	root   string
	store  *MemoryOffsets
	sink   *recSink
	agg    *aggregate.Aggregator
	w      *Watcher
	errs   []error
	errsMu sync.Mutex
}

func newHarness(t *testing.T, mutate ...func(*Options)) *harness {
	t.Helper()
	h := &harness{root: t.TempDir(), store: NewMemoryOffsets(), agg: aggregate.New()}
	h.sink = newSink(h.agg)
	h.start(t, mutate...)
	return h
}

func (h *harness) start(t *testing.T, mutate ...func(*Options)) {
	t.Helper()
	opts := Options{
		Roots: []Root{rootOf(h.root)}, Offsets: h.store, Sink: h.sink,
		OnError: func(err error) { h.errsMu.Lock(); h.errs = append(h.errs, err); h.errsMu.Unlock() },
	}
	for _, m := range mutate {
		m(&opts)
	}
	w, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.w = w
}

func (h *harness) scan(t *testing.T) {
	t.Helper()
	if err := h.w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) path(rel string) string { return filepath.Join(h.root, rel) }

func (h *harness) state(t *testing.T, rel string) FileState {
	t.Helper()
	st, ok := h.store.Get(h.path(rel))
	if !ok {
		t.Fatalf("no state for %s", rel)
	}
	return st
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func (h *harness) output() int64 {
	var total int64
	for _, r := range h.agg.Rows() {
		total += r.OutputTokens
	}
	return total
}

func messageIDs(events []claudecode.Event) []string {
	var ids []string
	for _, ev := range events {
		switch e := ev.(type) {
		case claudecode.AssistantUsage:
			ids = append(ids, e.MessageID)
		case claudecode.UserTurn:
			ids = append(ids, "user:"+e.UUID)
		case claudecode.Skipped:
			ids = append(ids, "skipped:"+string(e.Reason))
		default:
			ids = append(ids, "other")
		}
	}
	return ids
}

func TestNewRequiresOptions(t *testing.T) {
	for name, opts := range map[string]Options{
		"empty":      {},
		"no root":    {Offsets: NewMemoryOffsets(), Sink: newSink(nil)},
		"no offsets": {Roots: []Root{rootOf("x")}, Sink: newSink(nil)},
		"no sink":    {Roots: []Root{rootOf("x")}, Offsets: NewMemoryOffsets()},
		"no dir":     {Roots: []Root{{Projects: "x"}}, Offsets: NewMemoryOffsets(), Sink: newSink(nil)},
	} {
		if _, err := New(opts); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestInitialImportAndAppend(t *testing.T) {
	h := newHarness(t)
	p := h.path("proj/s1.jsonl")
	write(t, p, userLine("u1")+asstLine("a1", 10))

	h.scan(t)
	if platformFileID(mustStat(t, p)) != (FileID{}) && h.sink.ids[0] == (FileID{}) {
		t.Error("sink received a zero FileID")
	}
	if got := messageIDs(h.sink.take()); !reflect.DeepEqual(got, []string{"user:u1", "a1"}) {
		t.Fatalf("initial events = %v", got)
	}
	if st := h.state(t, "proj/s1.jsonl"); st.Offset != fileSize(t, p) || st.Size != st.Offset {
		t.Errorf("state = %+v, size %d", st, fileSize(t, p))
	}

	h.scan(t)
	if n := h.sink.count(); n != 0 {
		t.Errorf("rescan delivered %d events", n)
	}

	appendTo(t, p, asstLine("a2", 5))
	h.scan(t)
	if got := messageIDs(h.sink.take()); !reflect.DeepEqual(got, []string{"a2"}) {
		t.Errorf("appended events = %v", got)
	}
	if got := h.output(); got != 15 {
		t.Errorf("output = %d, want 15", got)
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func TestPartialLineWaitsForNewline(t *testing.T) {
	h := newHarness(t)
	p := h.path("s1.jsonl")
	first, second := asstLine("a1", 10), asstLine("a2", 20)
	write(t, p, first+second[:25])

	h.scan(t)
	if got := messageIDs(h.sink.take()); !reflect.DeepEqual(got, []string{"a1"}) {
		t.Fatalf("events = %v", got)
	}
	if st := h.state(t, "s1.jsonl"); st.Offset != int64(len(first)) || st.Size != fileSize(t, p) {
		t.Fatalf("state = %+v", st)
	}

	h.scan(t)
	if n := h.sink.count(); n != 0 {
		t.Fatalf("partial line produced %d events", n)
	}

	appendTo(t, p, second[25:])
	h.scan(t)
	if got := messageIDs(h.sink.take()); !reflect.DeepEqual(got, []string{"a2"}) {
		t.Fatalf("completed events = %v", got)
	}
	if got := h.output(); got != 30 {
		t.Errorf("output = %d, want 30", got)
	}
}

func TestTruncateRereadsWithoutDoubleCounting(t *testing.T) {
	h := newHarness(t)
	p := h.path("s1.jsonl")
	first := asstLine("a1", 10)
	write(t, p, first+asstLine("a2", 20)+asstLine("a3", 30))
	h.scan(t)
	h.sink.take()
	before := h.state(t, "s1.jsonl")

	// Same inode, shorter content: the stored offset no longer applies.
	write(t, p, first)
	h.scan(t)
	if got := messageIDs(h.sink.take()); !reflect.DeepEqual(got, []string{"a1"}) {
		t.Fatalf("re-read events = %v", got)
	}
	if st := h.state(t, "s1.jsonl"); st.ID != before.ID || st.Offset != int64(len(first)) {
		t.Errorf("state = %+v", st)
	}
	if got := h.output(); got != 60 {
		t.Errorf("output = %d, want 60 (no double count)", got)
	}
	if c := h.agg.Counters(); c.DuplicateResponses != 1 {
		t.Errorf("DuplicateResponses = %d, want 1", c.DuplicateResponses)
	}

	// New content after truncation is counted.
	write(t, p, asstLine("b1", 7))
	h.scan(t)
	if got := h.output(); got != 67 {
		t.Errorf("output = %d, want 67", got)
	}
}

func TestReplacedFileIsReadFromStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no file identity on Windows (platformFileID): a replaced file is detected by size only")
	}
	h := newHarness(t)
	p := h.path("s1.jsonl")
	write(t, p, asstLine("a1", 10)+asstLine("a2", 20))
	h.scan(t)
	h.sink.take()
	old := h.state(t, "s1.jsonl")

	tmp := h.path("s1.jsonl.tmp")
	write(t, tmp, asstLine("a1", 10)+asstLine("b1", 1)+asstLine("b2", 2))
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if got := messageIDs(h.sink.take()); !reflect.DeepEqual(got, []string{"a1", "b1", "b2"}) {
		t.Fatalf("events = %v", got)
	}
	if st := h.state(t, "s1.jsonl"); st.ID == old.ID {
		t.Skip("filesystem reused the inode")
	}
	if got := h.output(); got != 33 {
		t.Errorf("output = %d, want 33", got)
	}
}

func TestDeletedFileStateIsCleared(t *testing.T) {
	h := newHarness(t)
	keep, drop := h.path("keep.jsonl"), h.path("sub/drop.jsonl")
	write(t, keep, asstLine("a1", 10))
	write(t, drop, asstLine("a2", 20))
	h.scan(t)
	if n := len(h.store.Paths()); n != 2 {
		t.Fatalf("paths = %d, want 2", n)
	}

	if err := os.RemoveAll(h.path("sub")); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if got := h.store.Paths(); !reflect.DeepEqual(got, []string{keep}) {
		t.Errorf("paths = %v", got)
	}
	// Totals already counted stay (transcripts are deleted after 30 days).
	if got := h.output(); got != 30 {
		t.Errorf("output = %d, want 30", got)
	}

	// A recreated file at the same path starts from offset 0.
	write(t, drop, asstLine("a3", 5))
	h.scan(t)
	if got := h.output(); got != 35 {
		t.Errorf("output = %d, want 35", got)
	}
}

func TestMissingRootKeepsState(t *testing.T) {
	h := newHarness(t)
	h.root = filepath.Join(h.root, "does-not-exist")
	h.start(t)
	if err := h.store.Set("/some/file.jsonl", FileState{Offset: 5, Size: 5}); err != nil {
		t.Fatal(err)
	}
	err := h.w.ScanOnce(context.Background())
	if err == nil {
		t.Fatal("expected error for a missing root")
	}
	if strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error leaks the path: %v", err)
	}
	if len(h.store.Paths()) != 1 {
		t.Error("state was purged")
	}
}

func TestDiscoversNestedAndSubagentFiles(t *testing.T) {
	h := newHarness(t)
	write(t, h.path("-proj-a/s1.jsonl"), asstLine("a1", 1))
	write(t, h.path("-proj-a/s1/subagents/agent-1.jsonl"), asstLine("a2", 2)+asstLine("a3", 3))
	write(t, h.path("-proj-b/deep/er/s2.jsonl"), userLine("u1"))
	write(t, h.path("-proj-a/notes.txt"), asstLine("x1", 100))
	write(t, h.path("-proj-a/s1.jsonl.bak"), asstLine("x2", 100))
	if err := os.MkdirAll(h.path("-proj-a/dir.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	write(t, outside, asstLine("x3", 100))
	if err := os.Symlink(outside, h.path("-proj-a/link.jsonl")); err != nil {
		t.Fatal(err)
	}

	h.scan(t)
	if got := h.output(); got != 6 {
		t.Errorf("output = %d, want 6", got)
	}
	if n := len(h.store.Paths()); n != 3 {
		t.Errorf("tracked files = %d, want 3", n)
	}
	if n := h.sink.count(); n != 4 {
		t.Errorf("events = %d, want 4", n)
	}
}

func TestLongLines(t *testing.T) {
	t.Run("over the limit is skipped and reading continues", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.MaxLineBytes = 1024 })
		p := h.path("s1.jsonl")
		long := `{"type":"user","sessionId":"s1","message":{"content":"` + strings.Repeat("x", 5000) + `"}}` + "\n"
		write(t, p, asstLine("a1", 1)+long+asstLine("a2", 2))
		h.scan(t)
		if got := messageIDs(h.sink.take()); !reflect.DeepEqual(got, []string{"a1", "skipped:line_too_long", "a2"}) {
			t.Errorf("events = %v", got)
		}
		if st := h.state(t, "s1.jsonl"); st.Offset != fileSize(t, p) {
			t.Errorf("offset = %d, size %d", st.Offset, fileSize(t, p))
		}
		if cap(h.w.lineBuf) > 4096 {
			t.Errorf("line buffer grew to %d", cap(h.w.lineBuf))
		}
	})

	t.Run("unfinished long line is reported once when completed", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.MaxLineBytes = 1024 })
		p := h.path("s1.jsonl")
		first := asstLine("a1", 1)
		write(t, p, first+strings.Repeat("y", 3000))
		h.scan(t)
		h.scan(t)
		if got := messageIDs(h.sink.take()); !reflect.DeepEqual(got, []string{"a1"}) {
			t.Fatalf("events = %v", got)
		}
		if st := h.state(t, "s1.jsonl"); st.Offset != int64(len(first)) {
			t.Fatalf("offset = %d", st.Offset)
		}
		appendTo(t, p, "\n"+asstLine("a2", 2))
		h.scan(t)
		if got := messageIDs(h.sink.take()); !reflect.DeepEqual(got, []string{"skipped:line_too_long", "a2"}) {
			t.Errorf("events = %v", got)
		}
	})

	t.Run("line spanning several read buffers is parsed", func(t *testing.T) {
		h := newHarness(t)
		big := `{"type":"user","sessionId":"s1","uuid":"u1","message":{"content":"` + strings.Repeat("z", 3*readBufferBytes+17) + `"}}` + "\n"
		write(t, h.path("s1.jsonl"), asstLine("a1", 1)+big+asstLine("a2", 2))
		h.scan(t)
		if got := messageIDs(h.sink.take()); !reflect.DeepEqual(got, []string{"a1", "user:u1", "a2"}) {
			t.Errorf("events = %v", got)
		}
	})
}

type persisted struct {
	Offsets map[string]FileState
	Agg     aggregate.State
}

func persist(t *testing.T, store *MemoryOffsets, agg *aggregate.Aggregator) []byte {
	t.Helper()
	p := persisted{Offsets: map[string]FileState{}, Agg: agg.Snapshot()}
	for _, path := range store.Paths() {
		p.Offsets[path], _ = store.Get(path)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func restore(t *testing.T, raw []byte) (*MemoryOffsets, *aggregate.Aggregator) {
	t.Helper()
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	store, agg := NewMemoryOffsets(), aggregate.New()
	for path, st := range p.Offsets {
		_ = store.Set(path, st)
	}
	if err := agg.Restore(p.Agg); err != nil {
		t.Fatal(err)
	}
	return store, agg
}

func TestRestartDoesNotRecount(t *testing.T) {
	h := newHarness(t)
	p := h.path("s1.jsonl")
	write(t, p, userLine("u1")+asstLine("a1", 10)+asstLine("a2", 20)+asstLine("a1", 10))
	h.scan(t)
	wantRows := h.agg.Rows()
	saved := persist(t, h.store, h.agg)

	t.Run("offsets lost but aggregator restored", func(t *testing.T) {
		_, agg := restore(t, saved)
		r := &harness{root: h.root, store: NewMemoryOffsets(), agg: agg, sink: newSink(agg)}
		r.start(t)
		r.scan(t)
		if n := r.sink.count(); n == 0 {
			t.Error("expected a full re-read")
		}
		if got := agg.Rows(); !reflect.DeepEqual(got, wantRows) {
			t.Errorf("rows changed after re-read:\n%+v\nwant\n%+v", got, wantRows)
		}
	})

	// Runs last: it appends to the file shared with the subtest above.
	t.Run("offsets and aggregator restored", func(t *testing.T) {
		store, agg := restore(t, saved)
		r := &harness{root: h.root, store: store, agg: agg, sink: newSink(agg)}
		r.start(t)
		r.scan(t)
		if n := r.sink.count(); n != 0 {
			t.Errorf("restart re-read %d events", n)
		}
		appendTo(t, p, asstLine("a3", 5))
		r.scan(t)
		if got := messageIDs(r.sink.take()); !reflect.DeepEqual(got, []string{"a3"}) {
			t.Errorf("events = %v", got)
		}
		if got := r.output(); got != 35 {
			t.Errorf("output = %d, want 35", got)
		}
	})
}

func TestErrorsDoNotLeakPathsOrContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0 does not make a file unreadable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	h := newHarness(t)
	secretFile, secretDir := "secret-file-name.jsonl", "secret-dir-name"
	write(t, h.path(secretFile), asstLine("a1", 1))
	write(t, h.path(secretDir+"/x.jsonl"), asstLine("a2", 2))
	h.scan(t)

	if err := os.Chmod(h.path(secretFile), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h.path(secretDir), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.path(secretDir), 0o755) })

	h.scan(t)

	h.errsMu.Lock()
	defer h.errsMu.Unlock()
	if len(h.errs) == 0 {
		t.Fatal("expected reported errors")
	}
	for _, err := range h.errs {
		for _, leak := range []string{h.root, "secret", ".jsonl"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("error %q contains %q", err, leak)
			}
		}
	}
	// The unreadable directory must not cause its file's state to be purged.
	if _, ok := h.store.Get(h.path(secretDir + "/x.jsonl")); !ok {
		t.Error("state of a file in an unreadable directory was purged")
	}
}

func TestRunPicksUpNewSubdirectoryFiles(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ScanInterval = time.Hour })
	write(t, h.path("existing/s0.jsonl"), asstLine("a0", 1))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	waitFor := func(n int) {
		t.Helper()
		deadline := time.After(30 * time.Second) // failure guard only
		for h.sink.count() < n {
			select {
			case <-h.sink.notify:
			case <-deadline:
				t.Fatalf("timed out waiting for %d events, have %d", n, h.sink.count())
			}
		}
	}

	<-h.w.Ready()
	waitFor(1) // initial import

	// The new directory tree did not exist when the watches were installed.
	p := h.path("new-project/session/subagents/agent-1.jsonl")
	write(t, p, asstLine("a1", 10))
	waitFor(2)

	appendTo(t, p, asstLine("a2", 20))
	waitFor(3)
	if got := h.output(); got != 31 {
		t.Errorf("output = %d, want 31", got)
	}
}
