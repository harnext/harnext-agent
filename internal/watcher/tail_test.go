package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/parser/copilot"
)

type copilotRec struct {
	mu     sync.Mutex
	events []copilot.Event
	notify chan struct{}
}

func newCopilotRec() *copilotRec { return &copilotRec{notify: make(chan struct{}, 1024)} }

func (r *copilotRec) Handle(_ FileID, ev copilot.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *copilotRec) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *copilotRec) take() []copilot.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

func (r *copilotRec) waitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for r.count() < n {
		select {
		case <-r.notify:
		case <-deadline:
			t.Fatalf("timeout: got %d events, want %d", r.count(), n)
		}
	}
}

type tailHarness struct {
	path  string
	store *MemoryOffsets
	sink  *copilotRec
	tl    *FileTailer
	errs  []error
	mu    sync.Mutex
}

func newTailHarness(t *testing.T, path string, mutate ...func(*TailOptions)) *tailHarness {
	t.Helper()
	h := &tailHarness{path: path, store: NewMemoryOffsets(), sink: newCopilotRec()}
	h.start(t, mutate...)
	return h
}

func (h *tailHarness) start(t *testing.T, mutate ...func(*TailOptions)) {
	t.Helper()
	opts := TailOptions{
		Path: h.path, Offsets: h.store, Sink: h.sink,
		OnError: func(err error) { h.mu.Lock(); h.errs = append(h.errs, err); h.mu.Unlock() },
	}
	for _, m := range mutate {
		m(&opts)
	}
	tl, err := NewFileTailer(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.tl = tl
}

func (h *tailHarness) scan(t *testing.T) {
	t.Helper()
	if err := h.tl.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (h *tailHarness) errors() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.errs...)
}

// fixtureLines returns the non-empty lines of the synthetic OTel sample, each ending in '\n'.
func fixtureLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "copilot", "otel-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l+"\n")
		}
	}
	if len(out) < 3 {
		t.Fatalf("fixture has %d lines, need at least 3", len(out))
	}
	return out
}

func TestTailMissingFileIsNotAnError(t *testing.T) {
	h := newTailHarness(t, filepath.Join(t.TempDir(), "sub", "otel.jsonl"))
	h.scan(t)
	if h.sink.count() != 0 || len(h.errors()) != 0 {
		t.Fatalf("events=%d errs=%v", h.sink.count(), h.errors())
	}
}

func TestTailAppendAndNoDuplicates(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"))
	write(t, h.path, lines[0])
	h.scan(t)
	if got := h.sink.count(); got != 1 {
		t.Fatalf("first scan: %d events, want 1", got)
	}
	h.scan(t)
	if got := h.sink.count(); got != 1 {
		t.Fatalf("rescan duplicated: %d events", got)
	}
	appendTo(t, h.path, lines[1]+lines[2])
	h.scan(t)
	if got := h.sink.count(); got != 3 {
		t.Fatalf("after append: %d events, want 3", got)
	}
}

func TestTailRestartDoesNotRecount(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"))
	write(t, h.path, lines[0]+lines[1])
	h.scan(t)
	h.sink.take()

	h.start(t)
	h.scan(t)
	if got := h.sink.count(); got != 0 {
		t.Fatalf("restart re-delivered %d events", got)
	}
	appendTo(t, h.path, lines[2])
	h.scan(t)
	if got := h.sink.count(); got != 1 {
		t.Fatalf("after restart append: %d events, want 1", got)
	}
}

func TestTailPartialLineDeliveredOnce(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"))
	whole := strings.TrimSuffix(lines[0], "\n")
	write(t, h.path, whole[:len(whole)/2])
	h.scan(t)
	if got := h.sink.count(); got != 0 {
		t.Fatalf("partial line delivered: %d", got)
	}
	appendTo(t, h.path, whole[len(whole)/2:])
	h.scan(t)
	if got := h.sink.count(); got != 0 {
		t.Fatalf("line without newline delivered: %d", got)
	}
	appendTo(t, h.path, "\n")
	h.scan(t)
	h.scan(t)
	evs := h.sink.take()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if _, ok := evs[0].(copilot.ChatUsage); !ok {
		t.Fatalf("first fixture line parsed as %T", evs[0])
	}
}

func TestTailReplacedFileReadsFromStart(t *testing.T) {
	lines := fixtureLines(t)
	dir := t.TempDir()
	h := newTailHarness(t, filepath.Join(dir, "otel.jsonl"))
	write(t, h.path, lines[0]+lines[1])
	h.scan(t)
	h.sink.take()

	tmp := filepath.Join(dir, "new.tmp")
	write(t, tmp, lines[2])
	if err := os.Rename(tmp, h.path); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if got := h.sink.count(); got != 1 {
		t.Fatalf("after replace: %d events, want 1", got)
	}
}

func TestTailTruncationReadsFromStart(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"))
	write(t, h.path, lines[0]+lines[1])
	h.scan(t)
	h.sink.take()

	write(t, h.path, lines[0]) // same inode, smaller size
	h.scan(t)
	if got := h.sink.count(); got != 1 {
		t.Fatalf("after truncate: %d events, want 1", got)
	}
}

func TestTailSymlinkNotRead(t *testing.T) {
	lines := fixtureLines(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "real.jsonl")
	write(t, target, lines[0])
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	h := newTailHarness(t, link)
	h.scan(t)
	if h.sink.count() != 0 || len(h.errors()) != 0 {
		t.Fatalf("events=%d errs=%v", h.sink.count(), h.errors())
	}
	if _, ok := h.store.Get(link); ok {
		t.Fatal("offset stored for symlink")
	}
}

func TestTailDirectoryPathSkipped(t *testing.T) {
	dir := t.TempDir()
	h := newTailHarness(t, dir)
	h.scan(t)
	if h.sink.count() != 0 || len(h.errors()) != 0 {
		t.Fatalf("events=%d errs=%v", h.sink.count(), h.errors())
	}
}

func TestTailDeletedFileForgetsState(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"))
	write(t, h.path, lines[0])
	h.scan(t)
	if _, ok := h.store.Get(h.path); !ok {
		t.Fatal("no offset stored")
	}
	if err := os.Remove(h.path); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if _, ok := h.store.Get(h.path); ok {
		t.Fatal("offset kept after delete")
	}
}

func TestTailTooLongLineSkipped(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"), func(o *TailOptions) { o.MaxLineBytes = 256 })
	write(t, h.path, strings.Repeat("x", 1000)+"\n"+lines[0])
	h.scan(t)
	evs := h.sink.take()
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if s, ok := evs[0].(copilot.Skipped); !ok || s.Reason != copilot.ReasonLineTooLong {
		t.Fatalf("first event = %#v", evs[0])
	}
}

func TestTailSharedOffsetStoreWithWatcher(t *testing.T) {
	lines := fixtureLines(t)
	root := t.TempDir()
	store := NewMemoryOffsets()

	claudeSink := newSink(nil)
	w, err := New(Options{Root: root, Offsets: store, Sink: claudeSink})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "p", "s.jsonl"), asstLine("m1", 5))

	otel := filepath.Join(t.TempDir(), "otel.jsonl")
	write(t, otel, lines[0])
	tl, err := NewFileTailer(TailOptions{Path: otel, Offsets: store, Sink: newCopilotRec()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := w.ScanOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err := tl.ScanOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := claudeSink.count(); got != 1 {
		t.Fatalf("claude events = %d, want 1", got)
	}
	if got := len(store.Paths()); got != 2 {
		t.Fatalf("store has %d paths, want 2", got)
	}
}

func TestTailRunPicksUpFileCreatedLater(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"), func(o *TailOptions) { o.ScanInterval = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.tl.Run(ctx) }()
	select {
	case <-h.tl.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	write(t, h.path, lines[0])
	h.sink.waitCount(t, 1)
	appendTo(t, h.path, lines[1])
	h.sink.waitCount(t, 2)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if got := h.sink.count(); got != 2 {
		t.Fatalf("events = %d, want 2", got)
	}
	if len(h.errors()) != 0 {
		t.Fatalf("errors: %v", h.errors())
	}
}

func TestTailRunParentDirCreatedLaterUsesPeriodicScan(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "later", "otel.jsonl"), func(o *TailOptions) { o.ScanInterval = 20 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.tl.Run(ctx) }()
	<-h.tl.Ready()
	write(t, h.path, lines[0])
	h.sink.waitCount(t, 1)
}

func TestNewFileTailerValidatesOptions(t *testing.T) {
	if _, err := NewFileTailer(TailOptions{}); err == nil {
		t.Fatal("expected error")
	}
}

func tailSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func TestTailTrimsOwnFileOnceFullyProcessed(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"), func(o *TailOptions) { o.TrimOver = 10 })
	write(t, h.path, lines[0]+lines[1])
	h.scan(t)
	if got := h.sink.count(); got != 2 {
		t.Fatalf("events = %d, want 2", got)
	}
	if size := tailSize(t, h.path); size != 0 {
		t.Fatalf("file not trimmed: %d bytes", size)
	}
	// After the trim, new lines are read once from the start and nothing is counted twice.
	appendTo(t, h.path, lines[2])
	h.scan(t)
	h.scan(t)
	if got := h.sink.count(); got != 3 {
		t.Fatalf("after trim and append: %d events, want 3", got)
	}
}

func TestTailNeverTrimsAPartialLineOrBelowTheLimit(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"), func(o *TailOptions) { o.TrimOver = 10 })
	partial := strings.TrimSuffix(lines[1], "\n")
	write(t, h.path, lines[0]+partial)
	h.scan(t)
	if size := tailSize(t, h.path); size != int64(len(lines[0]+partial)) {
		t.Fatalf("file with an unprocessed line was trimmed: %d bytes", size)
	}

	big := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"), func(o *TailOptions) { o.TrimOver = 1 << 30 })
	write(t, big.path, lines[0])
	big.scan(t)
	if size := tailSize(t, big.path); size != int64(len(lines[0])) {
		t.Fatalf("file below the limit was trimmed: %d bytes", size)
	}
}

func TestTailWithoutTrimLeavesTheFile(t *testing.T) {
	lines := fixtureLines(t)
	h := newTailHarness(t, filepath.Join(t.TempDir(), "otel.jsonl"))
	write(t, h.path, lines[0]+lines[1])
	h.scan(t)
	if size := tailSize(t, h.path); size != int64(len(lines[0]+lines[1])) {
		t.Fatalf("user file was written to: %d bytes", size)
	}
}
