package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

// The whole path for the request-based folder split: Claude Code transcript lines (with tool_use blocks whose file_path points
// into a git repository) -> parser -> aggregator -> uploader. The upload carries project-relative folders and nothing else of the
// paths: no file name, no absolute folder, no temp directory.
func TestFolderSplitEndToEnd(t *testing.T) {
	h := newChatHarness(t, true)

	repo := filepath.Join(t.TempDir(), "work", "app")
	for _, d := range []string{".git", "apps/web/src", "docs", "services/api/internal"} {
		if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:acme/app.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const sid = "claude-folders-1"
	line := func(i int, typ, content, usage string) string {
		ts := fmt.Sprintf("2026-01-01T10:%02d:00.000Z", i)
		if typ == "user" {
			return fmt.Sprintf(`{"type":"user","sessionId":"%s","uuid":"u%d","cwd":%q,"timestamp":"%s","message":{"role":"user","content":%q}}`, sid, i, repo, ts, content)
		}
		return fmt.Sprintf(`{"type":"assistant","sessionId":"%s","requestId":"r%d","uuid":"a%d","cwd":%q,"timestamp":"%s","message":{"id":"m%d","model":"claude-sonnet-5","content":%s,"usage":%s}}`,
			sid, i, i, repo, ts, i, content, usage)
	}
	edit := func(id, rel string) string {
		return fmt.Sprintf(`[{"type":"tool_use","id":%q,"name":"Edit","input":{"file_path":%q,"old_string":"SECRET-OLD-TEXT","new_string":"SECRET-NEW-TEXT"}}]`, id, filepath.Join(repo, filepath.FromSlash(rel)))
	}
	use := func(out int) string { return fmt.Sprintf(`{"input_tokens":10,"output_tokens":%d}`, out) }
	lines := []string{
		line(1, "user", "add the page", ""),
		line(2, "assistant", edit("t1", "apps/web/src/SECRET-FILE-ONE.ts"), use(100)),
		line(3, "assistant", edit("t2", "apps/web/src/SECRET-FILE-TWO.ts"), use(1)),
		line(4, "user", "now describe it", ""),
		line(5, "assistant", edit("t3", "docs/SECRET-FILE-THREE.md"), use(200)),
		line(6, "user", "and the api", ""),
		line(7, "assistant", edit("t4", "services/api/internal/SECRET-FILE-FOUR.go"), use(300)),
		line(8, "user", "last one in the api", ""),
		line(9, "assistant", edit("t5", "services/api/SECRET-FILE-FIVE.go"), use(400)),
	}
	root := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(root, "proj", sid+".jsonl"), strings.Join(lines, "\n")+"\n")

	var clock remoteconfig.Clock
	if !h.at.IsZero() {
		clock = frozenClock{h.at}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var once sync.Once
	go func() {
		done <- Run(ctx, RunOptions{
			Paths: h.paths, Version: "0.1.0", Root: root, ChatRoots: []string{h.chatRoot},
			FlushInterval: 30 * time.Millisecond, UploadPeriod: 50 * time.Millisecond, Keys: h.keys, Clock: clock,
		})
	}()
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("Run did not stop")
			}
		})
	}
	defer stop()

	var sess map[string]any
	eventually(t, "folder upload", func() bool {
		for _, b := range h.rec.all() {
			var env struct {
				Sessions []map[string]any `json:"sessions"`
			}
			if json.Unmarshal([]byte(b), &env) != nil {
				continue
			}
			for _, s := range env.Sessions {
				if s["session_external_id"] == sid {
					if fs, _ := s["folders"].([]any); len(fs) == 3 {
						sess = s
					}
				}
			}
		}
		return sess != nil
	})
	if sess["project"] != "acme/app" || sess["project_path"] != "services/api" || sess["turn_count"] != float64(4) {
		t.Fatalf("session = project %v path %v turns %v", sess["project"], sess["project_path"], sess["turn_count"])
	}
	type fold struct{ turns, out float64 }
	got := map[string]fold{}
	for _, x := range sess["folders"].([]any) {
		f := x.(map[string]any)
		var out float64
		for _, u := range f["usage"].([]any) {
			out += u.(map[string]any)["output_tokens"].(float64)
		}
		got[f["path"].(string)] = fold{f["turn_count"].(float64), out}
	}
	want := map[string]fold{"apps/web": {1, 101}, "docs": {1, 200}, "services/api": {2, 700}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("folders = %v, want %v", got, want)
	}
	var total float64
	for _, u := range sess["usage"].([]any) {
		total += u.(map[string]any)["output_tokens"].(float64)
	}
	if total != 1001 {
		t.Fatalf("session usage = %v", total)
	}
	// Privacy: only project-relative folders travel.
	stop()
	for _, b := range h.rec.all() {
		for _, bad := range []string{"SECRET-FILE", "SECRET-OLD-TEXT", "SECRET-NEW-TEXT", repo, filepath.Dir(repo), testToken} {
			if strings.Contains(b, bad) {
				t.Fatalf("upload leaks %q:\n%s", bad, b)
			}
		}
	}
}
