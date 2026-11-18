package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/harnext/harnext-agent/internal/aggregate"
)

var testNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

const (
	fixtureCwd     = "/home/dev/example-project"
	secretPrompt   = "my secret sk-FAKE0123456789abcdef and DB_PASSWORD=hunter2-fake please"
	secretToolText = "API_KEY=fake-api-key-value"
	fakeUser       = "fakeuser-x"
)

// env is an isolated machine: a fake home with two projects, the agent directory and the tools' record folders.
type env struct {
	root, home, agentHome string
	claudeRoot, chatRoot  string
	otel                  string
	gitProject, plainProj string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{root: root, home: filepath.Join(root, "Users", fakeUser)}
	e.agentHome = filepath.Join(root, "agent-home")
	e.claudeRoot = filepath.Join(e.home, ".claude", "projects")
	e.chatRoot = filepath.Join(root, "workspaceStorage")
	e.otel = filepath.Join(root, "otel", "copilot.jsonl")
	e.gitProject = filepath.Join(e.home, "work", "app")
	e.plainProj = filepath.Join(e.home, "work", "plain")
	for _, d := range []string{filepath.Join(e.gitProject, ".git"), e.plainProj, e.agentHome, e.claudeRoot, filepath.Dir(e.otel)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(e.gitProject, ".git", "config"), "[remote \"origin\"]\n\turl = https://github.com/acme/app.git\n")
	t.Setenv("HOME", e.home)
	t.Setenv("USERPROFILE", e.home)
	t.Setenv("HARNEXT_HOME", e.agentHome)
	return e
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

func fixture(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash("../../testdata/" + rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// transcript writes a Claude Code transcript made from the basic fixture: its own session id and day, the given project
// and the fake secrets in prompt and tool result; mtime is set to at.
func (e *env) transcript(t *testing.T, name, sessionNo, day, cwd string, at time.Time) string {
	t.Helper()
	s := fixture(t, "claude-code/session-basic.jsonl")
	s = strings.NewReplacer(
		"2026-01-01", day,
		"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-"+sessionNo,
		"msg_", "msg_"+sessionNo+"_", "req_", "req_"+sessionNo+"_", "u-0", "u-"+sessionNo+"-", "a-0", "a-"+sessionNo+"-",
		fixtureCwd, jsonString(cwd),
		"fixture prompt one", secretPrompt,
		"fixture file content", secretToolText,
	).Replace(s)
	p := filepath.Join(e.claudeRoot, "proj", name)
	write(t, p, s)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return p
}

func fileURI(p string) string {
	if runtime.GOOS == "windows" {
		return "file:///" + filepath.ToSlash(p)
	}
	return "file://" + p
}

// copilotFiles writes the Copilot fixtures: the telemetry sample and the chat session of its first conversation.
func (e *env) copilotFiles(t *testing.T) {
	t.Helper()
	write(t, e.otel, fixture(t, "copilot/otel-sample.jsonl"))
	ws := filepath.Join(e.chatRoot, "hash0001")
	write(t, filepath.Join(ws, "workspace.json"), `{"folder":"`+jsonString(fileURI(e.plainProj))+`"}`)
	write(t, filepath.Join(ws, "chatSessions", "conv-synthetic-0001.jsonl"), fixture(t, "copilot/chat-session.jsonl"))
}

func (e *env) options() Options {
	return Options{Now: testNow, ClaudeRoot: e.claudeRoot, CopilotOTelPath: e.otel, ChatRoots: []string{e.chatRoot}}
}

// snapshot lists every file and directory under root with size; files also carry their modification time.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		// NTFS refreshes directory times lazily, so only files carry a time; an added or removed entry still changes the list.
		mtime := int64(0)
		if !d.IsDir() {
			mtime = info.ModTime().UnixNano()
		}
		lines = append(lines, fmt.Sprintf("%s|%v|%d|%d", rel, d.IsDir(), info.Size(), mtime))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func sessionIDs(rows []aggregate.Row) map[string]bool {
	m := map[string]bool{}
	for _, r := range rows {
		m[r.SessionID] = true
	}
	return m
}

func TestCollectWindowFilters(t *testing.T) {
	e := newEnv(t)
	fresh := testNow.Add(-time.Hour)
	e.transcript(t, "recent.jsonl", "000000000002", "2026-11-10", e.gitProject, fresh)
	// A file older than the window is never read, even though its rows carry recent days.
	e.transcript(t, "stale-file.jsonl", "000000000003", "2026-11-10", e.gitProject, testNow.Add(-40*24*time.Hour))
	// A file written recently that still holds old days: its old rows are dropped.
	e.transcript(t, "long-lived.jsonl", "000000000004", "2026-01-01", e.gitProject, fresh)

	rows, err := Collect(context.Background(), e.options())
	if err != nil {
		t.Fatal(err)
	}
	ids := sessionIDs(rows)
	if len(ids) != 1 || !ids["00000000-0000-4000-8000-000000000002"] {
		t.Fatalf("sessions = %v, want only the recent one", ids)
	}
	for _, r := range rows {
		if r.Day != "2026-11-10" || r.Tool != aggregate.ToolClaudeCode {
			t.Errorf("unexpected row %s %s", r.Tool, r.Day)
		}
	}

	// The same files as seen from a later day: the recent session has aged out as well.
	o := e.options()
	o.Now = time.Date(2026, 12, 31, 9, 0, 0, 0, time.UTC)
	e.copilotFiles(t)
	if rows, err = Collect(context.Background(), o); err != nil || len(rows) != 0 {
		t.Fatalf("rows=%d err=%v, want none after the window moved on", len(rows), err)
	}
}

func TestCollectWindowStartIsInclusive(t *testing.T) {
	e := newEnv(t)
	// 30 days before 2026-11-18 is 2026-10-19: that day is in, the day before is out.
	e.transcript(t, "edge-in.jsonl", "000000000005", "2026-10-19", e.plainProj, testNow)
	e.transcript(t, "edge-out.jsonl", "000000000006", "2026-10-18", e.plainProj, testNow)
	rows, err := Collect(context.Background(), e.options())
	if err != nil {
		t.Fatal(err)
	}
	ids := sessionIDs(rows)
	if len(ids) != 1 || !ids["00000000-0000-4000-8000-000000000005"] {
		t.Fatalf("sessions = %v, want only the 2026-10-19 one", ids)
	}
}

func TestCollectWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.transcript(t, "recent.jsonl", "000000000002", "2026-11-10", e.gitProject, testNow.Add(-time.Hour))
	e.copilotFiles(t)
	before := snapshot(t, e.root)
	rows, err := Collect(context.Background(), e.options())
	if err != nil || len(rows) == 0 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if after := snapshot(t, e.root); after != before {
		t.Errorf("Collect changed the file system:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if entries, _ := os.ReadDir(e.agentHome); len(entries) != 0 {
		t.Errorf("agent directory is not empty: %d entries", len(entries))
	}
}

func TestCollectMissingSourcesAndCancel(t *testing.T) {
	e := newEnv(t)
	o := Options{Now: testNow, ClaudeRoot: filepath.Join(e.root, "none"), CopilotOTelPath: filepath.Join(e.root, "none.jsonl"),
		ChatRoots: []string{filepath.Join(e.root, "none-chat")}}
	if rows, err := Collect(context.Background(), o); err != nil || len(rows) != 0 {
		t.Errorf("missing sources: rows=%d err=%v", len(rows), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Collect(ctx, e.options()); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestCollectDefaultsReadAgentSettings(t *testing.T) {
	e := newEnv(t)
	// No explicit paths: roots come from the defaults; the telemetry file is the agent directory's own file.
	write(t, filepath.Join(e.agentHome, "copilot-otel.jsonl"), fixture(t, "copilot/otel-sample.jsonl"))
	rows, err := Collect(context.Background(), Options{Now: testNow, ClaudeRoot: e.claudeRoot, ChatRoots: []string{e.chatRoot}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("the agent's default telemetry file was not read")
	}
}

func TestPrivacyEndToEnd(t *testing.T) {
	e := newEnv(t)
	e.transcript(t, "git.jsonl", "000000000002", "2026-11-10", e.gitProject, testNow.Add(-time.Hour))
	e.copilotFiles(t)
	rows, err := Collect(context.Background(), e.options())
	if err != nil {
		t.Fatal(err)
	}
	s := Build(rows, testNow, DefaultResolve())

	projects := map[string]bool{}
	for _, p := range s.Projects {
		projects[p.Project] = true
	}
	if !projects["acme/app"] || !projects["~/work/plain"] {
		t.Fatalf("projects = %v, want acme/app and ~/work/plain", projects)
	}
	tools := map[string]bool{}
	for _, g := range s.Usage {
		tools[g.Tool] = true
	}
	if !tools[aggregate.ToolClaudeCode] || !tools[aggregate.ToolVSCodeCopilot] {
		t.Fatalf("tools = %v, want both", tools)
	}

	withFolders, _ := json.Marshal(s.CardPayload("0.3.0", true))
	withoutFolders, _ := json.Marshal(s.CardPayload("0.3.0", false))
	var rendered bytes.Buffer
	Render(&rendered, s)
	if bytes.Contains(withoutFolders, []byte(`"folders"`)) {
		t.Error("folders key present without includeFolders")
	}
	forbidden := []string{
		"hunter2", "sk-FAKE", "DB_PASSWORD", "API_KEY", "fake-api-key-value", "SENTINEL_PROMPT_TEXT", "fixture answer",
		fakeUser, e.root, filepath.ToSlash(e.root), fixtureCwd, "example-project", "main.go", "Users",
	}
	for name, out := range map[string][]byte{"card": withFolders, "card without folders": withoutFolders, "render": rendered.Bytes()} {
		for _, bad := range forbidden {
			if bytes.Contains(out, []byte(bad)) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
	}
	if !bytes.Contains(rendered.Bytes(), []byte("acme/app")) {
		t.Error("render lacks the resolved project name")
	}
}

func row(tool, session, day, model, cwd string, in, out int64) aggregate.Row {
	return aggregate.Row{
		SessionID: session, Tool: tool, Model: model, Day: day, Speed: "standard", PromptFloor: -1, Cwd: cwd,
		Totals: aggregate.Totals{InputTokens: in, OutputTokens: out},
	}
}

func TestBuild(t *testing.T) {
	credits := 1.5
	creditsMore := 2.25
	a := row(aggregate.ToolClaudeCode, "s1", "2026-11-10", "m1", "/p/one", 100, 50)
	a.CacheReadTokens, a.CacheCreationTokens, a.CacheCreation1hTokens, a.WebSearchRequests = 1000, 200, 150, 2
	b := a // another folder of the same session and usage key
	b.Folder = "sub"
	c := row(aggregate.ToolClaudeCode, "s2", "2026-11-11", "m1", "/p/two", 10, 5)
	d := row(aggregate.ToolVSCodeCopilot, "s3", "2026-11-11", "gpt", "/p/one", 400, 100)
	d.Speed, d.CopilotCredits, d.TokensEstimated = "unknown", &credits, true
	e := d
	e.SessionID, e.Day, e.CopilotCredits, e.TokensEstimated = "s4", "2026-11-12", &creditsMore, false
	old := row(aggregate.ToolClaudeCode, "s9", "2026-10-18", "m1", "/p/one", 999, 999)
	future := row(aggregate.ToolClaudeCode, "s9", "2026-11-19", "m1", "/p/one", 999, 999)
	abs := row(aggregate.ToolClaudeCode, "s5", "2026-11-12", "", "/leak/Users/bob/proj", 1, 1)

	resolve := func(cwd string) string {
		switch cwd {
		case "/p/one":
			return "acme/one"
		case "/p/two":
			return "~/two"
		}
		return cwd // an unresolved absolute path
	}
	s := Build([]aggregate.Row{a, b, c, d, e, old, future, abs}, testNow, resolve)

	if s.From != "2026-10-19" || s.To != "2026-11-18" || s.ActiveDays != 3 || s.Sessions != 5 {
		t.Fatalf("period/days/sessions = %s %s %d %d", s.From, s.To, s.ActiveDays, s.Sessions)
	}
	by := map[string]UsageGroup{}
	for _, g := range s.Usage {
		by[g.Tool+"/"+g.Model+"/"+g.Speed] = g
	}
	m1 := by["claude_code/m1/standard"]
	if m1.InputTokens != 210 || m1.OutputTokens != 105 || m1.CacheReadTokens != 2000 || m1.CacheCreationTokens != 400 ||
		m1.CacheCreation1hTokens != 300 || m1.WebSearchRequests != 4 || m1.CopilotCredits != nil || m1.TokensEstimated {
		t.Errorf("m1 = %+v", m1)
	}
	gpt := by["vscode_copilot/gpt/unknown"]
	if gpt.CopilotCredits == nil || *gpt.CopilotCredits != 3.75 || !gpt.TokensEstimated || gpt.InputTokens != 800 {
		t.Errorf("gpt = %+v", gpt)
	}
	if _, ok := by["claude_code/unknown/standard"]; !ok {
		t.Error("empty model is not named unknown")
	}
	// acme/one: a and b (1350 each) plus both Copilot rows (500 each); the unresolved absolute path shrinks to "proj".
	want := []ProjectTotal{{"acme/one", 3700}, {"~/two", 15}, {"proj", 2}}
	if !slices.Equal(s.Projects, want) {
		t.Errorf("projects = %+v, want %+v", s.Projects, want)
	}
	for _, p := range s.Projects {
		if strings.Contains(p.Project, "/leak") || strings.Contains(p.Project, "bob") {
			t.Errorf("project leaks a path: %q", p.Project)
		}
	}

	empty := Build(nil, testNow, resolve)
	if empty.Usage == nil || empty.Projects == nil || empty.Sessions != 0 || empty.From != "2026-10-19" {
		t.Errorf("empty summary = %+v", empty)
	}
	if err := checkAgainstSchema(t, mustMarshal(t, empty.CardPayload("0.3.0", true))); err != nil {
		t.Errorf("empty card: %v", err)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBuildCapsAndClamps(t *testing.T) {
	r := row(aggregate.ToolClaudeCode, "s", "2026-11-10", strings.Repeat("é", 80), "/p", math.MaxInt64, math.MaxInt64)
	r.CacheCreation1hTokens = 5
	s := Build([]aggregate.Row{r}, testNow, func(string) string { return strings.Repeat("x", 300) })
	g := s.Usage[0]
	if len(g.Model) > 100 || !utf8.ValidString(g.Model) || g.InputTokens != maxTokens || g.CacheCreation1hTokens != 0 {
		t.Errorf("group = %+v", g)
	}
	if len(s.Projects[0].Project) != 128 {
		t.Errorf("project length = %d", len(s.Projects[0].Project))
	}
	if err := checkAgainstSchema(t, mustMarshal(t, s.CardPayload("0.3.0", true))); err != nil {
		t.Error(err)
	}
}

func syntheticSummary(n int, modelLen int) Summary {
	s := Summary{From: "2026-10-19", To: "2026-11-18", ActiveDays: 20, Sessions: 30, Projects: []ProjectTotal{}}
	for i := range n {
		model := fmt.Sprintf("m-%03d-", i)
		model += strings.Repeat("x", max(0, modelLen-len(model)))
		s.Usage = append(s.Usage, UsageGroup{Tool: aggregate.ToolClaudeCode, Model: model, Speed: "standard", PromptFloor: -1,
			InputTokens: int64(1000 - i), OutputTokens: 1, CacheReadTokens: 1, CacheCreationTokens: 1})
	}
	for i := range 8 {
		s.Projects = append(s.Projects, ProjectTotal{Project: fmt.Sprintf("acme/p%d", i), Tokens: int64(100 - i)})
	}
	return s
}

func TestCardPayloadDropsSmallestRows(t *testing.T) {
	s := syntheticSummary(150, 10)
	p := s.CardPayload("0.3.0", true)
	if len(p.Usage) != 100 || p.Dropped != 50 {
		t.Fatalf("usage=%d dropped=%d, want 100 and 50", len(p.Usage), p.Dropped)
	}
	if p.Usage[0].Model[:5] != "m-000" || p.Usage[99].Model[:5] != "m-099" {
		t.Errorf("kept rows are not the largest: first %q last %q", p.Usage[0].Model, p.Usage[99].Model)
	}
	if len(p.Folders) != 5 || p.Folders[0].Project != "acme/p0" {
		t.Errorf("folders = %+v", p.Folders)
	}
	body := mustMarshal(t, p)
	if bytes.Contains(body, []byte("Dropped")) || bytes.Contains(body, []byte("dropped")) {
		t.Error("Dropped is part of the wire body")
	}
	if err := checkAgainstSchema(t, body); err != nil {
		t.Error(err)
	}
	if again := mustMarshal(t, s.CardPayload("0.3.0", true)); !bytes.Equal(again, body) {
		t.Error("payload is not deterministic")
	}

	// Long model names push the body over the byte limit before the row limit.
	big := syntheticSummary(100, 100).CardPayload("0.3.0", true)
	if b := mustMarshal(t, big); len(b) > maxCardBytes || big.Dropped == 0 || len(big.Usage)+big.Dropped != 100 {
		t.Errorf("size=%d usage=%d dropped=%d", len(b), len(big.Usage), big.Dropped)
	}

	none := s.CardPayload("0.3.0", false)
	if bytes.Contains(mustMarshal(t, none), []byte(`"folders"`)) {
		t.Error("folders key present")
	}
}

func TestCardPayloadCopilotRules(t *testing.T) {
	c := 4.5
	s := Summary{From: "2026-10-19", To: "2026-11-18", Projects: []ProjectTotal{}, Usage: []UsageGroup{
		{Tool: aggregate.ToolClaudeCode, Model: "m", Speed: "standard", PromptFloor: -1, InputTokens: 10, CacheCreationTokens: 3, CacheCreation1hTokens: 9,
			CopilotCredits: &c, TokensEstimated: true},
		{Tool: aggregate.ToolVSCodeCopilot, Model: "g", Speed: "unknown", PromptFloor: -1, InputTokens: 5, CopilotCredits: &c, TokensEstimated: true},
		{Tool: aggregate.ToolVSCodeCopilot, Model: "h", Speed: "unknown", PromptFloor: -1, InputTokens: 1, TokensEstimated: true},
	}}
	body := mustMarshal(t, s.CardPayload("", false))
	if err := checkAgainstSchema(t, body); err != nil {
		t.Fatal(err)
	}
	var got struct {
		AgentVersion string           `json:"agent_version"`
		Usage        []map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, u := range got.Usage {
		switch u["model"] {
		case "m":
			if _, ok := u["copilot_credits"]; ok || u["tokens_estimated"] != nil || u["cache_creation_1h_tokens"] != float64(3) {
				t.Errorf("claude row = %v", u)
			}
		case "g":
			if u["copilot_credits"] != 4.5 || u["tokens_estimated"] != true {
				t.Errorf("copilot row = %v", u)
			}
		case "h":
			if _, ok := u["copilot_credits"]; ok || u["tokens_estimated"] != nil {
				t.Errorf("copilot row without credits = %v", u)
			}
		}
	}
	if got.AgentVersion != "dev" {
		t.Errorf("agent_version = %q", got.AgentVersion)
	}
}

func TestRenderGolden(t *testing.T) {
	credits := 12.5
	s := Summary{
		From: "2026-10-19", To: "2026-11-18", ActiveDays: 18, Sessions: 42,
		Usage: []UsageGroup{
			{Tool: aggregate.ToolClaudeCode, Model: "claude-sonnet-4", Speed: "standard", PromptFloor: -1,
				InputTokens: 120000, OutputTokens: 450000, CacheReadTokens: 9800000, CacheCreationTokens: 640000},
			{Tool: aggregate.ToolVSCodeCopilot, Model: "gpt-4.1", Speed: "unknown", PromptFloor: -1,
				InputTokens: 310000, OutputTokens: 95000, CopilotCredits: &credits, TokensEstimated: true},
			{Tool: aggregate.ToolClaudeCode, Model: "claude-haiku-4", Speed: "standard", PromptFloor: -1, InputTokens: 1000, OutputTokens: 500},
		},
		Projects: []ProjectTotal{{"acme/app", 8200000}, {"~/work/caf\u00e9", 1900000}, {"unknown", 12}},
	}
	var b bytes.Buffer
	Render(&b, s)
	const want = `Harnext scan: last 30 days
============================================================
Period           2026-10-19 .. 2026-11-18
Active days      18
Sessions         42
Total tokens     ~11.4M
Copilot credits  12.5

Models (top 5 by tokens)
  claude-sonnet-4        Claude   ####################     11.0M
  gpt-4.1                Copilot  #...................   ~405.0K
  claude-haiku-4         Claude   #...................      1.5K

Top projects
  1. acme/app                                      8.2M
  2. ~/work/caf?                                   1.9M
  3. unknown                                         12

~ = estimated token count (Copilot history, scaled from credits)
`
	if got := b.String(); got != want {
		t.Errorf("render mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
	for _, c := range b.Bytes() {
		if c >= 0x80 || (c < 0x20 && c != '\n') {
			t.Fatalf("non-ASCII or control byte %#x in output", c)
		}
	}

	var empty bytes.Buffer
	Render(&empty, Build(nil, testNow, nil))
	if !strings.Contains(empty.String(), "No usage found") {
		t.Errorf("empty render:\n%s", empty.String())
	}
}

func TestHumanTokens(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1.0K", 1234: "1.2K", 999_949: "999.9K", 999_950: "1.0M",
		1_200_000: "1.2M", 2_500_000_000: "2.5B", 100_000_000_000: "100.0B"} {
		if got := humanTokens(n); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

// --- card.v1 schema check: a small reader of the keywords the schema uses (no schema library) ---

func loadJSON(t *testing.T, rel string) any {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func checkAgainstSchema(t *testing.T, body []byte) error {
	t.Helper()
	schema := loadJSON(t, "../../contracts/card.v1.schema.json")
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	var errs []string
	validate(schema, doc, "$", &errs)
	if err := cardRules(doc); err != nil {
		errs = append(errs, err.Error())
	}
	if len(body) > maxCardBytes {
		errs = append(errs, fmt.Sprintf("body is %d bytes", len(body)))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// validate supports type, const, enum, properties, required, additionalProperties:false, minLength, maxLength, pattern,
// minimum, maximum, maxItems, items and anyOf.
func validate(schema, v any, path string, errs *[]string) {
	sc := schema.(map[string]any)
	fail := func(format string, args ...any) { *errs = append(*errs, path+": "+fmt.Sprintf(format, args...)) }
	if alts, ok := sc["anyOf"].([]any); ok {
		for _, a := range alts {
			var sub []string
			validate(a, v, path, &sub)
			if len(sub) == 0 {
				return
			}
		}
		fail("matches none of anyOf")
		return
	}
	if c, ok := sc["const"]; ok && c != v {
		fail("const %v, got %v", c, v)
	}
	if enum, ok := sc["enum"].([]any); ok && !slices.Contains(enum, v) {
		fail("%v not in enum", v)
	}
	switch sc["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			fail("not an object")
			return
		}
		props, _ := sc["properties"].(map[string]any)
		for k, sub := range m {
			if ps, ok := props[k]; ok {
				validate(ps, sub, path+"."+k, errs)
			} else if sc["additionalProperties"] == false {
				fail("unknown property %q", k)
			}
		}
		if req, ok := sc["required"].([]any); ok {
			for _, k := range req {
				if _, ok := m[k.(string)]; !ok {
					fail("missing %q", k)
				}
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			fail("not an array")
			return
		}
		if mx, ok := sc["maxItems"].(float64); ok && float64(len(a)) > mx {
			fail("%d items, max %v", len(a), mx)
		}
		for i, el := range a {
			validate(sc["items"], el, fmt.Sprintf("%s[%d]", path, i), errs)
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			fail("not a string")
			return
		}
		n := utf8.RuneCountInString(s)
		if mn, ok := sc["minLength"].(float64); ok && float64(n) < mn {
			fail("shorter than %v", mn)
		}
		if mx, ok := sc["maxLength"].(float64); ok && float64(n) > mx {
			fail("longer than %v", mx)
		}
		if pat, ok := sc["pattern"].(string); ok && !regexp.MustCompile(pat).MatchString(s) {
			fail("does not match pattern")
		}
	case "integer", "number":
		f, ok := v.(float64)
		if !ok || (sc["type"] == "integer" && f != math.Trunc(f)) {
			fail("not a %v: %v", sc["type"], v)
			return
		}
		if mn, ok := sc["minimum"].(float64); ok && f < mn {
			fail("%v below %v", f, mn)
		}
		if mx, ok := sc["maximum"].(float64); ok && f > mx {
			fail("%v above %v", f, mx)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			fail("not a boolean")
		}
	case "null":
		if v != nil {
			fail("not null")
		}
	}
}

// cardRules are the contract rules JSON Schema cannot express.
func cardRules(doc any) error {
	m := doc.(map[string]any)
	period := m["period"].(map[string]any)
	from, err1 := time.Parse(time.DateOnly, period["from"].(string))
	to, err2 := time.Parse(time.DateOnly, period["to"].(string))
	if err1 != nil || err2 != nil || to.Before(from) || to.Sub(from) > 30*24*time.Hour {
		return errors.New("period must satisfy to >= from and span at most 31 days inclusive")
	}
	seen := map[string]bool{}
	for _, raw := range m["usage"].([]any) {
		u := raw.(map[string]any)
		key := fmt.Sprint(u["tool_name"], "|", u["model"], "|", u["speed"], "|", u["prompt_floor"])
		if seen[key] {
			return fmt.Errorf("duplicate usage key %s", key)
		}
		seen[key] = true
		if _, has := u["day"]; has {
			return errors.New("usage row has a day")
		}
		if num := func(k string) float64 { f, _ := u[k].(float64); return f }; num("cache_creation_1h_tokens") > num("cache_creation_tokens") {
			return errors.New("cache_creation_1h_tokens above cache_creation_tokens")
		}
		_, credits := u["copilot_credits"]
		if (credits || u["tokens_estimated"] == true) && u["tool_name"] != aggregate.ToolVSCodeCopilot {
			return errors.New("credits or estimate on a non-Copilot row")
		}
		if u["tokens_estimated"] == true && !credits {
			return errors.New("estimated tokens without credits")
		}
	}
	return nil
}

func TestSchemaVectorAndChecker(t *testing.T) {
	vec, err := os.ReadFile(filepath.FromSlash("../../contracts/testdata/card.v1.vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAgainstSchema(t, vec); err != nil {
		t.Fatalf("vector: %v", err)
	}

	// The checker must also say no.
	mutate := func(f func(m map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(vec, &m)
		f(m)
		return mustMarshal(t, m)
	}
	usage := func(m map[string]any) map[string]any { return m["usage"].([]any)[0].(map[string]any) }
	bad := map[string][]byte{
		"unknown property": mutate(func(m map[string]any) { m["extra"] = 1 }),
		"missing required": mutate(func(m map[string]any) { delete(m, "sessions") }),
		"day on usage":     mutate(func(m map[string]any) { usage(m)["day"] = "2026-11-01" }),
		"above maximum":    mutate(func(m map[string]any) { usage(m)["input_tokens"] = 1e12 }),
		"bad enum":         mutate(func(m map[string]any) { usage(m)["tool_name"] = "cursor" }),
		"bad date":         mutate(func(m map[string]any) { m["period"].(map[string]any)["from"] = "2026-02-30" }),
		"too many folders": mutate(func(m map[string]any) {
			f := m["folders"].([]any)
			for len(f) < 6 {
				f = append(f, f[0])
			}
			m["folders"] = f
		}),
		"span over 31 days":  mutate(func(m map[string]any) { m["period"].(map[string]any)["from"] = "2026-10-01" }),
		"1h above creation":  mutate(func(m map[string]any) { usage(m)["cache_creation_1h_tokens"] = 1e9 }),
		"wrong version":      mutate(func(m map[string]any) { m["payload_version"] = 2 }),
		"credits on claude":  mutate(func(m map[string]any) { usage(m)["copilot_credits"] = 1 }),
		"duplicate key":      mutate(func(m map[string]any) { u := m["usage"].([]any); m["usage"] = append(u, u[0]) }),
		"fractional integer": mutate(func(m map[string]any) { m["sessions"] = 1.5 }),
	}
	for name, body := range bad {
		if checkAgainstSchema(t, body) == nil {
			t.Errorf("checker accepted: %s", name)
		}
	}
}
