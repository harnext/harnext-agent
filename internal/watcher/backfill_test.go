package watcher

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type recProgress struct {
	mu     sync.Mutex
	found  []int
	mtimes []time.Time
}

func (p *recProgress) Found(n int) { p.mu.Lock(); p.found = append(p.found, n); p.mu.Unlock() }
func (p *recProgress) File(m time.Time) {
	p.mu.Lock()
	p.mtimes = append(p.mtimes, m)
	p.mu.Unlock()
}

func ageFile(t *testing.T, path string, age time.Duration) time.Time {
	t.Helper()
	m := time.Now().Add(-age)
	if err := os.Chtimes(path, m, m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestClaudeScanIsNewestFirstAndSkipsOldFiles(t *testing.T) {
	root := t.TempDir()
	// Names sort the other way round than the mtimes, so a lexical walk would fail the test.
	ages := map[string]time.Duration{
		"a-oldest.jsonl":  20 * 24 * time.Hour,
		"b-middle.jsonl":  5 * 24 * time.Hour,
		"c-newest.jsonl":  time.Hour,
		"d-expired.jsonl": 31 * 24 * time.Hour,
	}
	for name, age := range ages {
		p := filepath.Join(root, "proj", name)
		write(t, p, asstLine(name, 5))
		ageFile(t, p, age)
	}
	prog := &recProgress{}
	h := newHarness(t, func(o *Options) {
		o.Root = root
		o.MaxFileAge = 30 * 24 * time.Hour
	})
	if err := h.w.scanOnce(context.Background(), prog); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(prog.found, []int{3}) {
		t.Fatalf("found = %v, want [3]", prog.found)
	}
	if len(prog.mtimes) != 3 {
		t.Fatalf("files reported = %d, want 3", len(prog.mtimes))
	}
	for i := 1; i < len(prog.mtimes); i++ {
		if prog.mtimes[i].After(prog.mtimes[i-1]) {
			t.Fatalf("files were not read newest first: %v", prog.mtimes)
		}
	}
	if n := h.sink.count(); n != 3 {
		t.Fatalf("events = %d, want 3 (the expired file must not be read)", n)
	}
	if _, ok := h.store.Get(filepath.Join(root, "proj", "d-expired.jsonl")); ok {
		t.Error("offset recorded for an expired file")
	}
	if _, ok := h.store.Get(filepath.Join(root, "proj", "c-newest.jsonl")); !ok {
		t.Error("no offset for a file inside the window")
	}
}

// A file that left the window keeps the offset it earned while it was inside it.
func TestClaudeScanKeepsOffsetsOfFilesThatAged(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "proj", "s.jsonl")
	write(t, p, asstLine("m1", 5))
	h := newHarness(t, func(o *Options) { o.Root = root; o.MaxFileAge = 30 * 24 * time.Hour })
	h.scan(t)
	before, ok := h.store.Get(p)
	if !ok {
		t.Fatal("no offset after the first scan")
	}
	ageFile(t, p, 40*24*time.Hour)
	h.sink.take()
	h.scan(t)
	after, ok := h.store.Get(p)
	if !ok || after != before {
		t.Fatalf("offset changed or was dropped for an aged file: %+v -> %+v (%v)", before, after, ok)
	}
	if h.sink.count() != 0 {
		t.Fatal("aged file was read again")
	}
}

func TestClaudeWithoutMaxFileAgeReadsEverything(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "proj", "s.jsonl")
	write(t, p, asstLine("m1", 5))
	ageFile(t, p, 400*24*time.Hour)
	h := newHarness(t, func(o *Options) { o.Root = root })
	h.scan(t)
	if h.sink.count() != 1 {
		t.Fatalf("events = %d, want 1", h.sink.count())
	}
}

func TestChatScanIsNewestFirstAcrossWorkspaces(t *testing.T) {
	root := t.TempDir()
	type f struct {
		ws, name string
		age      time.Duration
	}
	files := []f{
		{"ws1", "old", 25 * 24 * time.Hour},
		{"ws2", "new", time.Hour},
		{"ws1", "mid", 3 * 24 * time.Hour},
		{"ws3", "expired", 31 * 24 * time.Hour},
	}
	for _, x := range files {
		p := chatPath(root, x.ws, x.name+".jsonl")
		write(t, p, "x\n")
		ageFile(t, p, x.age)
	}
	prog := &recProgress{}
	h := newChatHarness(t, []string{root}, func(o *ChatOptions) { o.MaxFileAge = 30 * 24 * time.Hour })
	if err := h.w.scan(context.Background(), nil, prog); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, l := range h.sink.all() {
		order = append(order, l.f.SessionID)
	}
	if want := []string{"new", "mid", "old"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("read order = %v, want %v", order, want)
	}
	if !reflect.DeepEqual(prog.found, []int{3}) || len(prog.mtimes) != 3 {
		t.Fatalf("progress found=%v files=%d", prog.found, len(prog.mtimes))
	}
}

// While a file is being read the gate is read-locked, so a writer waits for the whole file.
func TestFileGateBlocksWriterWhileFileIsRead(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "proj", "s.jsonl"), asstLine("m1", 5)+asstLine("m2", 5))
	var gate sync.RWMutex
	locked := make(chan int, 2)
	h := newHarness(t, func(o *Options) {
		o.Root = root
		o.Gate = &gate
	})
	// Try to take the write lock as soon as the first line arrives; it must only succeed after the file is done.
	h.sink.agg = nil
	go func() {
		for h.sink.count() == 0 {
			time.Sleep(time.Microsecond)
		}
		gate.Lock()
		locked <- h.sink.count()
		gate.Unlock()
	}()
	h.scan(t)
	select {
	case n := <-locked:
		if n != 2 {
			t.Fatalf("writer got the gate after %d of 2 lines", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer never got the gate")
	}
}
