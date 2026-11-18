//go:build realdata

package aggregate

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/claudecode"
	"github.com/harnext/harnext-agent/internal/parser/copilotchat"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

func realExtractor(t *testing.T) analyzer.Extractor {
	t.Helper()
	ex, err := analyzer.Compile(&remoteconfig.Config{
		Friction: remoteconfig.Friction{DetectorVersion: 1, RepeatMinTurns: 2, RepeatJaccard: 0.8, ErrorPasteMinShare: 0.6},
		Topics:   remoteconfig.Topics{Labels: []string{"feature", "other"}, MinScore: 1, Fallback: "other"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func histogram(vals []int) string {
	sort.Ints(vals)
	counts := map[int]int{}
	for _, v := range vals {
		counts[min(v, 5)]++
	}
	var parts []string
	for k := 1; k <= 5; k++ {
		label := string(rune('0' + k))
		if k == 5 {
			label = "5+"
		}
		parts = append(parts, label+":"+itoa(counts[k]))
	}
	return strings.Join(parts, " ")
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Reads the developer's own Claude Code transcripts (HARNEXT_REAL_CLAUDE_ROOT, default ~/.claude/projects) and prints numbers only:
// how the requests split over folders, never a path or a name.
// Run: go test -tags realdata -run TestRealClaudeFolders -v ./internal/aggregate
func TestRealClaudeFolders(t *testing.T) {
	root := os.Getenv("HARNEXT_REAL_CLAUDE_ROOT")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".claude", "projects")
	}
	ex := realExtractor(t)
	a := New()
	a.SetAnalyzer(ex)
	p := claudecode.NewParser(ex)
	wantOut := map[string]int64{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 32<<20)
		for sc.Scan() {
			ev := p.ParseLine(sc.Bytes())
			if u, ok := ev.(claudecode.AssistantUsage); ok && u.MessageID != "" && u.RequestID != "" && !strings.HasPrefix(u.Model, "<") && u.Model != "" {
				wantOut[u.MessageID+"|"+u.RequestID] = u.Usage.OutputTokens
			}
			a.Add(ev)
		}
		return nil
	})
	var want int64
	for _, n := range wantOut {
		want += n
	}
	rows := a.Rows()
	var got int64
	sessions := map[string]bool{}
	for _, r := range rows {
		got += r.OutputTokens
		sessions[r.SessionID] = true
	}
	if got != want {
		t.Errorf("Σ folder output tokens %d != independent sum %d", got, want)
	}
	var perSession []int
	var turns, turnsMain, turnsOffCwd, friction, analyzed, frictionMainOnly int64
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.SessionID] {
			continue
		}
		seen[r.SessionID] = true
		perSession = append(perSession, len(r.Parts))
		cwd := CwdFolder(a.resolve, r.Cwd)
		var sTurns, mainTurns int64
		var q analyzer.Quality
		for _, pt := range r.Parts {
			sTurns += pt.Turns
			q.Add(pt.Quality)
			if pt.Path == r.MainFolder {
				mainTurns = pt.Turns
			}
			if pt.Path != cwd {
				turnsOffCwd += pt.Turns
			}
		}
		turns += sTurns
		turnsMain += mainTurns
		friction += q.FrictionExchanges
		analyzed += q.AnalyzedExchanges
		if len(r.Parts) > 1 {
			for _, pt := range r.Parts {
				if pt.Quality.FrictionExchanges > 0 {
					frictionMainOnly++
					break
				}
			}
		}
		assertFoldersSumToSession(t, "real", r.Parts, r.Quality)
	}
	t.Logf("claude: sessions=%d rows=%d requests=%d output_tokens(Σ folders == independent)=%d", len(sessions), len(rows), turns, got)
	t.Logf("claude: folders per session: %s", histogram(perSession))
	blob, _ := json.Marshal(a.Snapshot())
	t.Logf("claude: state JSON %d KB", len(blob)>>10)
	t.Logf("claude: requests in the session's main folder=%d (%.0f%%), requests outside the working directory's folder=%d (%.0f%%)",
		turnsMain, pct100(turnsMain, turns), turnsOffCwd, pct100(turnsOffCwd, turns))
	t.Logf("claude: analyzed exchanges=%d friction exchanges=%d (%.1f%%); multi-folder sessions with friction=%d", analyzed, friction, pct100(friction, analyzed), frictionMainOnly)
}

func pct100(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

// Reads the developer's own VS Code chat session files (history path: credits per request) and prints numbers only.
// Run: go test -tags realdata -run TestRealCopilotFolders -v ./internal/aggregate
func TestRealCopilotFolders(t *testing.T) {
	root := os.Getenv("HARNEXT_REAL_CHAT_ROOT")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, "Library", "Application Support", "Code", "User", "workspaceStorage")
	}
	ex := realExtractor(t)
	a := NewCopilot()
	a.SetAnalyzer(ex)
	files := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") || filepath.Base(filepath.Dir(path)) != "chatSessions" {
			return nil
		}
		id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(path)), "workspace.json")); err == nil {
			var w struct {
				Folder string `json:"folder"`
			}
			if json.Unmarshal(data, &w) == nil {
				if u, err := url.Parse(w.Folder); err == nil && u.Scheme == "file" && strings.HasPrefix(u.Path, "/") {
					a.SetFolder(id, u.Path)
				}
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		files++
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), copilotchat.MaxLineBytes)
		for sc.Scan() {
			copilotchat.Parse(sc.Bytes(), ex, func(ev copilotchat.Event) { a.AddChat(id, ev) })
		}
		return nil
	})
	rows := a.Rows()
	var credits, wantCredits float64
	for _, r := range rows {
		if r.CopilotCredits != nil {
			credits += *r.CopilotCredits
		}
	}
	for _, s := range a.Snapshot().Sessions {
		if len(s.Models) > 0 {
			continue
		}
		for _, r := range s.Requests {
			if r.Hash != 0 && !r.Hidden && r.Model != "" {
				wantCredits += float64(r.NanoAIU) / 1e9
			}
		}
	}
	if d := credits - wantCredits; d > 1e-3 || d < -1e-3 {
		t.Errorf("Σ folder credits %v != Σ request credits %v", credits, wantCredits)
	}
	var perSession []int
	var turns, turnsMain, turnsOffCwd int64
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.SessionID] {
			continue
		}
		seen[r.SessionID] = true
		perSession = append(perSession, len(r.Parts))
		cwd := CwdFolder(a.resolve, r.Cwd)
		for _, pt := range r.Parts {
			turns += pt.Turns
			if pt.Path == r.MainFolder {
				turnsMain += pt.Turns
			}
			if pt.Path != cwd {
				turnsOffCwd += pt.Turns
			}
		}
		assertFoldersSumToSession(t, "real", r.Parts, r.Quality)
	}
	t.Logf("copilot: files=%d conversations with rows=%d requests=%d credits(Σ folders == Σ requests)=%.1f", files, len(seen), turns, credits)
	t.Logf("copilot: folders per conversation: %s", histogram(perSession))
	blob, _ := json.Marshal(a.Snapshot())
	t.Logf("copilot: state JSON %d KB, seen hashes %d", len(blob)>>10, len(a.Snapshot().Seen))
	t.Logf("copilot: requests in the conversation's main folder=%d (%.0f%%), requests outside the workspace folder=%d (%.0f%%)",
		turnsMain, pct100(turnsMain, turns), turnsOffCwd, pct100(turnsOffCwd, turns))
}
