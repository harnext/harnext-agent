package watcher

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/parser/claudecode"
)

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverClaudeRoots(t *testing.T) {
	type tc struct {
		name  string
		setup func(t *testing.T, home string)
		env   func(home string) string
		want  []string // config directories relative to home
	}
	cases := []tc{
		{name: "default only", setup: func(t *testing.T, h string) {
			mkdirs(t, filepath.Join(h, ".claude", "projects"))
		}, want: []string{".claude"}},
		{name: "extra directories sorted", setup: func(t *testing.T, h string) {
			mkdirs(t, filepath.Join(h, ".claude-work", "projects"), filepath.Join(h, ".claude", "projects"), filepath.Join(h, ".claude-personal", "projects"))
		}, want: []string{".claude", ".claude-personal", ".claude-work"}},
		{name: "files are ignored", setup: func(t *testing.T, h string) {
			mkdirs(t, filepath.Join(h, ".claude", "projects"))
			write(t, filepath.Join(h, ".claude.json"), "{}")
			write(t, filepath.Join(h, ".claude.json.backup"), "{}")
			write(t, filepath.Join(h, ".claude-notes"), "x")
		}, want: []string{".claude"}},
		{name: "no projects directory", setup: func(t *testing.T, h string) {
			mkdirs(t, filepath.Join(h, ".claude"), filepath.Join(h, ".claude-work"), filepath.Join(h, ".claude-empty", "sessions"))
			write(t, filepath.Join(h, ".claude-file", "projects"), "a file, not a directory")
		}, want: nil},
		{name: "other dot directories are ignored", setup: func(t *testing.T, h string) {
			mkdirs(t, filepath.Join(h, ".config", "projects"), filepath.Join(h, "claude-work", "projects"), filepath.Join(h, ".claude-work", "projects"))
		}, want: []string{".claude-work"}},
		{name: "symlinked directory is ignored", setup: func(t *testing.T, h string) {
			real := filepath.Join(t.TempDir(), "elsewhere")
			mkdirs(t, filepath.Join(real, "projects"), filepath.Join(h, ".claude", "projects"))
			if err := os.Symlink(real, filepath.Join(h, ".claude-link")); err != nil {
				t.Skipf("symlinks not available: %v", err)
			}
		}, want: []string{".claude"}},
		{name: "symlinked projects is ignored", setup: func(t *testing.T, h string) {
			real := filepath.Join(t.TempDir(), "elsewhere")
			mkdirs(t, real, filepath.Join(h, ".claude-work"))
			if err := os.Symlink(real, filepath.Join(h, ".claude-work", "projects")); err != nil {
				t.Skipf("symlinks not available: %v", err)
			}
		}, want: nil},
		{name: "env var adds a directory outside home", setup: func(t *testing.T, h string) {
			mkdirs(t, filepath.Join(h, ".claude", "projects"))
		}, env: func(string) string {
			d := filepath.Join(t.TempDir(), "custom-config")
			mkdirs(t, filepath.Join(d, "projects"))
			return d
		}, want: []string{".claude"}},
		{name: "env var equal to a discovered directory is listed once", setup: func(t *testing.T, h string) {
			mkdirs(t, filepath.Join(h, ".claude", "projects"))
		}, env: func(h string) string { return filepath.Join(h, ".claude") + string(filepath.Separator) }, want: []string{".claude"}},
		{name: "env var without projects is ignored", setup: func(t *testing.T, h string) {
			mkdirs(t, filepath.Join(h, ".claude", "projects"))
		}, env: func(string) string { return t.TempDir() }, want: []string{".claude"}},
		{name: "relative env var is ignored", setup: func(t *testing.T, h string) {
			mkdirs(t, filepath.Join(h, ".claude", "projects"))
		}, env: func(string) string { return "relative-dir" }, want: []string{".claude"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			c.setup(t, home)
			env := ""
			if c.env != nil {
				env = c.env(home)
			}
			t.Setenv("CLAUDE_CONFIG_DIR", env)

			got := DiscoverClaudeRoots(home)
			var want []Root
			for _, rel := range c.want {
				want = append(want, ClaudeRoot(filepath.Join(home, rel)))
			}
			if c.name == "env var adds a directory outside home" {
				want = append(want, ClaudeRoot(env))
				if want[0].Dir > want[1].Dir {
					want[0], want[1] = want[1], want[0]
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("roots = %+v, want %+v", got, want)
			}
		})
	}
}

func TestDiscoverClaudeRootsMissingHome(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := DiscoverClaudeRoots(filepath.Join(t.TempDir(), "nope")); len(got) != 0 {
		t.Errorf("roots = %+v", got)
	}
	if got := DiscoverClaudeRoots(""); len(got) != 0 {
		t.Errorf("roots = %+v", got)
	}
}

// eventDirs maps each assistant message id to the config directory it was delivered with.
func (s *recSink) eventDirs() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for i, ev := range s.events {
		if a, ok := ev.(claudecode.AssistantUsage); ok {
			out[a.MessageID] = s.dirs[i]
		}
	}
	return out
}

func twoRoots(t *testing.T) (a, b Root) {
	t.Helper()
	base := t.TempDir()
	a, b = ClaudeRoot(filepath.Join(base, ".claude")), ClaudeRoot(filepath.Join(base, ".claude-work"))
	mkdirs(t, a.Projects, b.Projects)
	return a, b
}

func TestTwoRootsTagEventsAndKeepOffsetsApart(t *testing.T) {
	a, b := twoRoots(t)
	h := newHarness(t, func(o *Options) { o.Roots = []Root{a, b} })
	// Same relative path in both roots: only the full path keys the offsets.
	pa, pb := filepath.Join(a.Projects, "proj", "s1.jsonl"), filepath.Join(b.Projects, "proj", "s1.jsonl")
	write(t, pa, asstLine("a1", 10))
	write(t, pb, asstLine("b1", 20)+asstLine("b2", 30))

	h.scan(t)
	if got, want := h.sink.eventDirs(), map[string]string{"a1": a.Dir, "b1": b.Dir, "b2": b.Dir}; !reflect.DeepEqual(got, want) {
		t.Fatalf("event dirs = %v, want %v", got, want)
	}
	got := h.store.Paths()
	if want := []string{pa, pb}; !reflect.DeepEqual(sortedCopy(got), sortedCopy(want)) {
		t.Fatalf("offset paths = %v", got)
	}
	sa, _ := h.store.Get(pa)
	sb, _ := h.store.Get(pb)
	if sa.Offset != fileSize(t, pa) || sb.Offset != fileSize(t, pb) || sa.Offset == sb.Offset {
		t.Fatalf("offsets collide or are wrong: %+v %+v", sa, sb)
	}

	h.sink.take()
	h.scan(t)
	if n := h.sink.count(); n != 0 {
		t.Fatalf("rescan delivered %d events", n)
	}
	appendTo(t, pb, asstLine("b3", 1))
	h.scan(t)
	if got, want := h.sink.eventDirs(), map[string]string{"b3": b.Dir}; !reflect.DeepEqual(got, want) {
		t.Fatalf("append dirs = %v, want %v", got, want)
	}

	// A deletion in one root does not touch the other root's state.
	if err := os.Remove(pa); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if got := h.store.Paths(); !reflect.DeepEqual(got, []string{pb}) {
		t.Fatalf("paths after delete = %v", got)
	}
}

func TestProgressCoversAllRoots(t *testing.T) {
	a, b := twoRoots(t)
	for i, r := range []Root{a, b} {
		p := filepath.Join(r.Projects, "proj", "s.jsonl")
		write(t, p, asstLine("m"+string(rune('a'+i)), 1))
		ageFile(t, p, time.Duration(i+1)*time.Hour)
	}
	prog := &recProgress{}
	h := newHarness(t, func(o *Options) { o.Roots = []Root{a, b} })
	if err := h.w.scanOnce(context.Background(), prog); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prog.found, []int{2}) || len(prog.mtimes) != 2 {
		t.Fatalf("found = %v, files = %d, want one Found(2) and two files", prog.found, len(prog.mtimes))
	}
	if prog.mtimes[0].Before(prog.mtimes[1]) {
		t.Errorf("files of both roots were not read newest first: %v", prog.mtimes)
	}
}

func TestOneBrokenRootDoesNotStopTheOthers(t *testing.T) {
	a, _ := twoRoots(t)
	missing := ClaudeRoot(filepath.Join(t.TempDir(), ".claude-gone"))
	h := newHarness(t, func(o *Options) { o.Roots = []Root{missing, a} })
	write(t, filepath.Join(a.Projects, "p", "s.jsonl"), asstLine("a1", 1))
	if err := h.w.ScanOnce(context.Background()); err == nil {
		t.Error("expected an error for the missing root")
	}
	if got := h.sink.eventDirs(); !reflect.DeepEqual(got, map[string]string{"a1": a.Dir}) {
		t.Errorf("event dirs = %v", got)
	}
}

func TestRunPicksUpRootCreatedAfterStartAndSurvivesItsRemoval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	def, work := ClaudeRoot(filepath.Join(home, ".claude")), ClaudeRoot(filepath.Join(home, ".claude-work"))
	mkdirs(t, def.Projects)
	write(t, filepath.Join(def.Projects, "p", "s0.jsonl"), asstLine("d1", 1))

	h := newHarness(t, func(o *Options) {
		o.Roots = nil
		o.RootsFunc = func() []Root { return DiscoverClaudeRoots(home) }
		o.ScanInterval = 20 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitForEvents := func(n int) {
		t.Helper()
		waitUntil(t, h.sink, func() bool { return h.sink.count() >= n })
	}

	<-h.w.Ready()
	waitForEvents(1)

	pw := filepath.Join(work.Projects, "proj", "s1.jsonl")
	write(t, pw, asstLine("w1", 5))
	waitForEvents(2)
	appendTo(t, pw, asstLine("w2", 5)) // the new root is also followed live
	waitForEvents(3)
	if got, want := h.sink.eventDirs(), map[string]string{"d1": def.Dir, "w1": work.Dir, "w2": work.Dir}; !reflect.DeepEqual(got, want) {
		t.Fatalf("event dirs = %v, want %v", got, want)
	}

	if err := os.RemoveAll(work.Dir); err != nil {
		t.Fatal(err)
	}
	appendTo(t, filepath.Join(def.Projects, "p", "s0.jsonl"), asstLine("d2", 1))
	waitForEvents(4)
	if h.sink.eventDirs()["d2"] != def.Dir {
		t.Errorf("remaining root stopped working: %v", h.sink.eventDirs())
	}
	// A reappearing root is followed again.
	write(t, pw, asstLine("w1", 5)+asstLine("w2", 5)+asstLine("w3", 5))
	waitUntil(t, h.sink, func() bool { return h.sink.eventDirs()["w3"] == work.Dir })
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

func waitUntil(t *testing.T, s *recSink, cond func() bool) {
	t.Helper()
	deadline := time.After(30 * time.Second) // failure guard only
	for !cond() {
		select {
		case <-s.notify:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for the watcher")
		}
	}
}
