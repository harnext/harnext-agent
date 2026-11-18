package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type chatLine struct {
	f       ChatFile
	line    string
	tooLong bool
}

type chatRec struct {
	mu     sync.Mutex
	lines  []chatLine
	notify chan struct{}
}

func newChatRec() *chatRec { return &chatRec{notify: make(chan struct{}, 1024)} }

func (r *chatRec) HandleLine(f ChatFile, line []byte, tooLong bool) {
	r.mu.Lock()
	r.lines = append(r.lines, chatLine{f: f, line: string(line), tooLong: tooLong})
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *chatRec) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

func (r *chatRec) all() []chatLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]chatLine(nil), r.lines...)
}

func (r *chatRec) waitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(30 * time.Second) // failure guard only
	for r.count() < n {
		select {
		case <-r.notify:
		case <-deadline:
			t.Fatalf("timeout: got %d lines, want %d", r.count(), n)
		}
	}
}

type chatHarness struct {
	roots []string
	store *MemoryOffsets
	sink  *chatRec
	w     *ChatWatcher
	mu    sync.Mutex
	errs  []error
}

func newChatHarness(t *testing.T, roots []string, mutate ...func(*ChatOptions)) *chatHarness {
	t.Helper()
	h := &chatHarness{roots: roots, store: NewMemoryOffsets(), sink: newChatRec()}
	h.start(t, mutate...)
	return h
}

func (h *chatHarness) start(t *testing.T, mutate ...func(*ChatOptions)) {
	t.Helper()
	opts := ChatOptions{
		Roots: h.roots, Offsets: h.store, Sink: h.sink,
		OnError: func(err error) { h.mu.Lock(); h.errs = append(h.errs, err); h.mu.Unlock() },
	}
	for _, m := range mutate {
		m(&opts)
	}
	w, err := NewChatWatcher(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.w = w
}

func (h *chatHarness) scan(t *testing.T) {
	t.Helper()
	if err := h.w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (h *chatHarness) errors() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.errs...)
}

func (h *chatHarness) run(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}
}

func chatPath(root, ws, name string) string {
	return filepath.Join(root, ws, "chatSessions", name)
}

func TestNewChatWatcherRequiresOptions(t *testing.T) {
	st, sink := NewMemoryOffsets(), newChatRec()
	for name, o := range map[string]ChatOptions{
		"roots":   {Offsets: st, Sink: sink},
		"offsets": {Roots: []string{"x"}, Sink: sink},
		"sink":    {Roots: []string{"x"}, Offsets: st},
	} {
		if _, err := NewChatWatcher(o); err == nil {
			t.Errorf("missing %s: want error", name)
		}
	}
}

func TestDefaultChatRoots(t *testing.T) {
	roots := DefaultChatRoots()
	if len(roots) != 2 || !strings.Contains(roots[0], "workspaceStorage") || !strings.Contains(roots[1], "Code - Insiders") {
		t.Fatalf("roots = %v", roots)
	}
}

func TestChatInitialImportAndAppend(t *testing.T) {
	root := t.TempDir()
	p := chatPath(root, "ws1", "sess-1.jsonl")
	write(t, p, "a\nb\n")
	h := newChatHarness(t, []string{root})
	h.scan(t)

	got := h.sink.all()
	if len(got) != 2 || got[0].line != "a\n" || got[1].line != "b\n" {
		t.Fatalf("lines = %+v", got)
	}
	if got[0].f.SessionID != "sess-1" || got[0].f.WorkspaceDir != filepath.Join(root, "ws1") {
		t.Errorf("file = %+v", got[0].f)
	}
	if got[0].f.ID != (FileID{}) && got[0].f.ID != platformFileID(mustStat(t, p)) {
		t.Errorf("file id mismatch")
	}

	appendTo(t, p, "c\npartial")
	h.scan(t)
	got = h.sink.all()
	if len(got) != 3 || got[2].line != "c\n" {
		t.Fatalf("after append lines = %+v", got)
	}
	if errs := h.errors(); len(errs) != 0 {
		t.Errorf("errors: %v", errs)
	}
}

func TestChatRestartDoesNotRereadLines(t *testing.T) {
	root := t.TempDir()
	p := chatPath(root, "ws1", "s.jsonl")
	write(t, p, "a\nb\n")
	h := newChatHarness(t, []string{root})
	h.scan(t)

	h.sink = newChatRec()
	h.start(t) // same OffsetStore, fresh watcher
	h.w.opts.Sink = h.sink
	h.scan(t)
	if h.sink.count() != 0 {
		t.Fatalf("restart re-read %d lines", h.sink.count())
	}
	appendTo(t, p, "c\n")
	h.scan(t)
	if got := h.sink.all(); len(got) != 1 || got[0].line != "c\n" {
		t.Fatalf("lines = %+v", got)
	}
}

func TestChatIgnoresUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	write(t, chatPath(root, "ws1", "ok.jsonl"), "ok\n")
	write(t, chatPath(root, "ws1", "old.json"), "x\n")
	write(t, chatPath(root, "ws1", "has space.jsonl"), "x\n")
	write(t, chatPath(root, "ws1", "a..b.jsonl"), "x\n")
	write(t, chatPath(root, "ws1", strings.Repeat("a", 201)+".jsonl"), "x\n")
	write(t, chatPath(root, "ws1", ".jsonl"), "x\n")
	write(t, filepath.Join(root, "ws1", "other.jsonl"), "x\n")
	write(t, filepath.Join(root, "ws1", "chatSessions", "deeper", "n.jsonl"), "x\n")
	write(t, filepath.Join(root, "GitHub.copilot-chat", "debug.jsonl"), "x\n")
	write(t, filepath.Join(root, "top.jsonl"), "x\n")
	write(t, filepath.Join(root, "ws1", "other", "chatSessions", "n.jsonl"), "x\n")

	outside := filepath.Join(t.TempDir(), "real.jsonl")
	write(t, outside, "link\n")
	if err := os.Symlink(outside, chatPath(root, "ws1", "link.jsonl")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	realWS := filepath.Join(t.TempDir(), "ws")
	write(t, filepath.Join(realWS, "chatSessions", "viadir.jsonl"), "x\n")
	if err := os.Symlink(realWS, filepath.Join(root, "ws-link")); err != nil {
		t.Skipf("symlink: %v", err)
	}

	h := newChatHarness(t, []string{root})
	h.scan(t)
	got := h.sink.all()
	if len(got) != 1 || got[0].f.SessionID != "ok" {
		t.Fatalf("lines = %+v", got)
	}
	if paths := h.store.Paths(); len(paths) != 1 {
		t.Errorf("offset entries = %d, want 1", len(paths))
	}
}

func TestChatMaxFileAge(t *testing.T) {
	root := t.TempDir()
	oldP := chatPath(root, "ws1", "old.jsonl")
	write(t, oldP, "old\n")
	write(t, chatPath(root, "ws1", "new.jsonl"), "new\n")
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldP, past, past); err != nil {
		t.Fatal(err)
	}
	h := newChatHarness(t, []string{root}, func(o *ChatOptions) { o.MaxFileAge = time.Hour })
	h.scan(t)
	got := h.sink.all()
	if len(got) != 1 || got[0].f.SessionID != "new" {
		t.Fatalf("lines = %+v", got)
	}
	if _, ok := h.store.Get(oldP); ok {
		t.Error("offset recorded for skipped old file")
	}
}

func TestChatTwoRoots(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	write(t, chatPath(a, "wsA", "s1.jsonl"), "1\n")
	write(t, chatPath(b, "wsB", "s2.jsonl"), "2\n")
	h := newChatHarness(t, []string{a, b})
	h.scan(t)
	got := map[string]string{}
	for _, l := range h.sink.all() {
		got[l.f.SessionID] = l.f.WorkspaceDir
	}
	if got["s1"] != filepath.Join(a, "wsA") || got["s2"] != filepath.Join(b, "wsB") || len(got) != 2 {
		t.Fatalf("got = %v", got)
	}
}

func TestChatMissingRootIsSilentAndKeepsState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	h := newChatHarness(t, []string{root})
	h.scan(t)
	if h.sink.count() != 0 || len(h.errors()) != 0 {
		t.Fatalf("lines=%d errs=%v", h.sink.count(), h.errors())
	}
	p := chatPath(root, "ws", "s.jsonl")
	if err := h.store.Set(p, FileState{Offset: 1, Size: 1}); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if _, ok := h.store.Get(p); !ok {
		t.Error("state dropped while root missing")
	}
}

func TestChatDeletedFileOffsetForgotten(t *testing.T) {
	root := t.TempDir()
	p := chatPath(root, "ws1", "s.jsonl")
	write(t, p, "a\n")
	foreign := filepath.Join(t.TempDir(), "claude.jsonl")
	if err := os.WriteFile(foreign, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h := newChatHarness(t, []string{root})
	if err := h.store.Set(foreign, FileState{Offset: 5, Size: 5}); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if _, ok := h.store.Get(p); !ok {
		t.Fatal("offset not recorded")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if _, ok := h.store.Get(p); ok {
		t.Error("offset of deleted file kept")
	}
	if _, ok := h.store.Get(foreign); !ok {
		t.Error("offset outside roots was forgotten")
	}
}

func TestChatTruncateRereads(t *testing.T) {
	root := t.TempDir()
	p := chatPath(root, "ws1", "s.jsonl")
	write(t, p, "aaaa\nbbbb\n")
	h := newChatHarness(t, []string{root})
	h.scan(t)
	write(t, p, "c\n")
	h.scan(t)
	got := h.sink.all()
	if len(got) != 3 || got[2].line != "c\n" {
		t.Fatalf("lines = %+v", got)
	}
}

func TestChatLongLine(t *testing.T) {
	root := t.TempDir()
	write(t, chatPath(root, "ws1", "s.jsonl"), strings.Repeat("x", 100)+"\nok\n")
	h := newChatHarness(t, []string{root}, func(o *ChatOptions) { o.MaxLineBytes = 10 })
	h.scan(t)
	got := h.sink.all()
	if len(got) != 2 || !got[0].tooLong || got[1].tooLong || got[1].line != "ok\n" {
		t.Fatalf("lines = %+v", got)
	}
}

func TestChatRunPicksUpNewWorkspaceAndAppends(t *testing.T) {
	root := t.TempDir()
	write(t, chatPath(root, "existing", "s0.jsonl"), "0\n")
	h := newChatHarness(t, []string{root}, func(o *ChatOptions) { o.ScanInterval = 50 * time.Millisecond })
	stop := h.run(t)
	defer stop()
	<-h.w.Ready()
	h.sink.waitCount(t, 1)

	p := chatPath(root, "fresh", "s1.jsonl")
	write(t, p, "1\n")
	h.sink.waitCount(t, 2)
	appendTo(t, p, "2\n")
	h.sink.waitCount(t, 3)

	got := h.sink.all()
	if got[1].f.SessionID != "s1" || got[1].f.WorkspaceDir != filepath.Join(root, "fresh") || got[2].line != "2\n" {
		t.Fatalf("lines = %+v", got)
	}
	time.Sleep(150 * time.Millisecond)
	if h.sink.count() != 3 {
		t.Errorf("duplicate delivery: %d lines", h.sink.count())
	}
}

func TestChatRunRootCreatedLater(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaceStorage")
	h := newChatHarness(t, []string{root}, func(o *ChatOptions) { o.ScanInterval = 50 * time.Millisecond })
	stop := h.run(t)
	defer stop()
	<-h.w.Ready()

	write(t, chatPath(root, "ws", "s.jsonl"), "x\n")
	h.sink.waitCount(t, 1)
	if errs := h.errors(); len(errs) != 0 {
		t.Errorf("errors: %v", errs)
	}
}

func TestChatRunForgetsRemovedFile(t *testing.T) {
	root := t.TempDir()
	p := chatPath(root, "ws", "s.jsonl")
	write(t, p, "x\n")
	h := newChatHarness(t, []string{root}, func(o *ChatOptions) { o.ScanInterval = time.Hour })
	stop := h.run(t)
	defer stop()
	<-h.w.Ready()
	h.sink.waitCount(t, 1)

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second) // failure guard only
	for {
		if _, ok := h.store.Get(p); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("offset not forgotten after removal")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChatRunStopsOnCancel(t *testing.T) {
	h := newChatHarness(t, []string{t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()
	<-h.w.Ready()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop")
	}
}
