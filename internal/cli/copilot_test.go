package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/harnext/harnext-agent/internal/service"
	"github.com/harnext/harnext-agent/internal/vscodecfg"
)

func initForCopilot(t *testing.T) service.Paths {
	t.Helper()
	p := newPaths(t)
	if r := exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com"); r.failure != nil {
		t.Fatal(r.failure)
	}
	return p
}

// parseBlock extracts the three setting lines and parses them as a JSON object.
func parseBlock(t *testing.T, out string) map[string]any {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, `  "github.copilot.chat.otel.`) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 3 {
		t.Fatalf("want 3 setting lines, got %d:\n%s", len(lines), out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte("{"+strings.Join(lines, "\n")+"}"), &m); err != nil {
		t.Fatalf("block is not valid JSON: %v\n%s", err, out)
	}
	return m
}

func TestCopilotDefaultPath(t *testing.T) {
	p := initForCopilot(t)
	r := exec(t, p, "", "copilot")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	m := parseBlock(t, r.out)
	want := filepath.Join(p.Dir, "copilot-otel.jsonl")
	if m["github.copilot.chat.otel.outfile"] != want || m["github.copilot.chat.otel.enabled"] != true ||
		m["github.copilot.chat.otel.exporterType"] != "file" {
		t.Fatalf("block = %v", m)
	}
	if !strings.Contains(r.out, "captureContent") || strings.Contains(r.out, tok) || strings.Contains(r.out, "example.com") {
		t.Fatalf("unexpected output:\n%s", r.out)
	}
}

func TestCopilotWithoutInit(t *testing.T) {
	p := newPaths(t)
	r := exec(t, p, "", "copilot")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	parseBlock(t, r.out)
	if r := exec(t, p, "", "copilot", "--path", filepath.Join(t.TempDir(), "x.jsonl")); r.failure == nil || !strings.Contains(r.failure.Error(), "not initialized") {
		t.Fatalf("err = %v", r.failure)
	}
}

func TestCopilotSetPathAndDefault(t *testing.T) {
	p := initForCopilot(t)
	custom := filepath.Join(t.TempDir(), "otel.jsonl")
	r := exec(t, p, "", "copilot", "--path", custom)
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if parseBlock(t, r.out)["github.copilot.chat.otel.outfile"] != custom {
		t.Fatalf("out:\n%s", r.out)
	}
	s, _ := p.LoadSettings()
	if s.CopilotOTelPath != custom || s.ServerURL != "https://app.example.com" {
		t.Fatalf("settings = %+v", s)
	}
	r = exec(t, p, "", "copilot")
	if parseBlock(t, r.out)["github.copilot.chat.otel.outfile"] != custom {
		t.Fatalf("out:\n%s", r.out)
	}

	r = exec(t, p, "", "copilot", "--default")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if s, _ := p.LoadSettings(); s.CopilotOTelPath != "" || s.ServerURL != "https://app.example.com" {
		t.Fatalf("settings = %+v", s)
	}
	if parseBlock(t, r.out)["github.copilot.chat.otel.outfile"] != filepath.Join(p.Dir, "copilot-otel.jsonl") {
		t.Fatalf("out:\n%s", r.out)
	}
	if r := exec(t, p, "", "copilot", "--default", "--path", custom); r.failure == nil {
		t.Fatal("both flags accepted")
	}
}

func TestCopilotRejectsRelativePath(t *testing.T) {
	p := initForCopilot(t)
	before, _ := os.ReadFile(p.Settings)
	for _, bad := range []string{"relative/otel.jsonl", "", "/tmp/a\nb"} {
		r := exec(t, p, "", "copilot", "--path", bad)
		if r.failure == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	after, _ := os.ReadFile(p.Settings)
	if string(before) != string(after) {
		t.Fatalf("settings changed: %s -> %s", before, after)
	}
}

func TestCopilotPathNeedsJSONEscaping(t *testing.T) {
	p := initForCopilot(t)
	odd := filepath.Join(t.TempDir(), `we"ird\dir`, "otel.jsonl")
	r := exec(t, p, "", "copilot", "--path", odd)
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if parseBlock(t, r.out)["github.copilot.chat.otel.outfile"] != odd {
		t.Fatalf("out:\n%s", r.out)
	}
}

func TestStatusCopilotSection(t *testing.T) {
	p := initForCopilot(t)
	// Old status file without the copilot key: section is skipped.
	if err := os.WriteFile(p.Status, []byte(`{"pid":1,"sessions":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := exec(t, p, "", "status"); r.failure != nil || strings.Contains(r.out, "Copilot:") {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}

	if err := p.WriteStatus(service.StatusFile{}); err != nil {
		t.Fatal(err)
	}
	if r := exec(t, p, "", "status"); !strings.Contains(r.out, "telemetry file not found yet") || !strings.Contains(r.out, "harnext-agent copilot") {
		t.Fatalf("out=%q", r.out)
	}

	st := service.StatusFile{Copilot: service.CopilotStatus{Sessions: 3, UniqueSpans: 40, SkippedLines: 2, BackgroundSpans: 5, File: true}}
	if err := p.WriteStatus(st); err != nil {
		t.Fatal(err)
	}
	r := exec(t, p, "", "status")
	if !strings.Contains(r.out, "Copilot:   3 sessions, 40 spans, 5 background spans, 2 unreadable lines skipped") {
		t.Fatalf("out=%q", r.out)
	}
}

func init() { vscodeRoots = func() []string { return nil } }

// fakeVSCode points the command at a temp "Code/User" dir (Insiders is absent on purpose).
func fakeVSCode(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	user := filepath.Join(base, "Code", "User")
	if err := os.MkdirAll(user, 0o755); err != nil {
		t.Fatal(err)
	}
	old := vscodeRoots
	vscodeRoots = func() []string {
		return []string{user, filepath.Join(base, "Code - Insiders", "User")}
	}
	t.Cleanup(func() { vscodeRoots = old })
	return filepath.Join(user, "settings.json")
}

func fakeTTY(t *testing.T) {
	t.Helper()
	old := isTerminal
	isTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { isTerminal = old })
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSONC(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	m, err := vscodecfg.Decode(string(b))
	if err != nil {
		t.Fatalf("not valid JSONC: %v\n%s", err, b)
	}
	return m
}

const (
	kEnabled  = "github.copilot.chat.otel.enabled"
	kExporter = "github.copilot.chat.otel.exporterType"
	kOutfile  = "github.copilot.chat.otel.outfile"
	kCapture  = "github.copilot.chat.otel.captureContent"
)

func TestApplyNoTTYNoYesWritesNothing(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	orig := "{\n  \"a\": 1\n}\n"
	writeFile(t, sp, orig)
	r := exec(t, p, "", "copilot", "--apply")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if b, _ := os.ReadFile(sp); string(b) != orig {
		t.Fatalf("file changed:\n%s", b)
	}
	if _, err := os.Stat(sp + vscodecfg.BackupSuffix); err == nil {
		t.Fatal("backup created")
	}
	if !strings.Contains(r.out, "re-run with --apply --yes") {
		t.Fatalf("out:\n%s", r.out)
	}
	parseBlock(t, r.out)
}

func TestApplyYesCreatesFileAndIsIdempotent(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	r := exec(t, p, "", "copilot", "--apply", "--yes")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	m := readJSONC(t, sp)
	if m[kEnabled] != true || m[kExporter] != "file" || m[kOutfile] != filepath.Join(p.Dir, "copilot-otel.jsonl") {
		t.Fatalf("m = %v", m)
	}
	if _, ok := m[kCapture]; ok {
		t.Fatal("captureContent written")
	}
	if !strings.Contains(r.out, "Created "+sp) || !strings.Contains(r.out, "Reload the VS Code window") {
		t.Fatalf("out:\n%s", r.out)
	}
	first, _ := os.ReadFile(sp)
	r = exec(t, p, "", "copilot", "--apply", "--yes")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	second, _ := os.ReadFile(sp)
	if string(first) != string(second) || !strings.Contains(r.out, "Already configured") || strings.Contains(r.out, "Reload") {
		t.Fatalf("out:\n%s", r.out)
	}
}

func TestApplyUpdatesWithBackupAndKeepsComments(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	orig := "{\n  // mine\n  \"editor.fontSize\": 13,\n}\n"
	writeFile(t, sp, orig)
	r := exec(t, p, "", "copilot", "--apply", "--yes")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if b, _ := os.ReadFile(sp + vscodecfg.BackupSuffix); string(b) != orig {
		t.Fatalf("backup = %q", b)
	}
	if b, _ := os.ReadFile(sp); !strings.Contains(string(b), "// mine") {
		t.Fatalf("comment lost:\n%s", b)
	}
	if !strings.Contains(r.out, "Updated "+sp+" (backup: "+sp+vscodecfg.BackupSuffix+")") {
		t.Fatalf("out:\n%s", r.out)
	}
	readJSONC(t, sp)
}

func TestApplyPrompt(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	fakeTTY(t)
	r := exec(t, p, "n\n", "copilot", "--apply")
	if _, err := os.Stat(sp); err == nil || !strings.Contains(r.out, "Update VS Code settings at "+sp+"? [Y/n]") {
		t.Fatalf("declined prompt wrote the file (err=%v):\n%s", err, r.out)
	}
	r = exec(t, p, "\n", "copilot", "--apply")
	if r.failure != nil || readJSONC(t, sp)[kEnabled] != true {
		t.Fatalf("out:\n%s", r.out)
	}
}

func TestApplyAdoptsUserOutfile(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	custom := filepath.Join(t.TempDir(), "mine.jsonl")
	writeFile(t, sp, "{\n  \""+kOutfile+"\": "+strconv.Quote(custom)+"\n}\n")
	r := exec(t, p, "", "copilot", "--apply", "--yes")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if readJSONC(t, sp)[kOutfile] != custom {
		t.Fatal("user outfile changed")
	}
	if s, _ := p.LoadSettings(); s.CopilotOTelPath != custom {
		t.Fatalf("settings = %+v", s)
	}
}

func TestApplyExporterTypeWarningAndForce(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	writeFile(t, sp, `{"`+kExporter+`": "otlp-http"}`)
	r := exec(t, p, "", "copilot", "--apply", "--yes")
	if r.failure != nil || readJSONC(t, sp)[kExporter] != "otlp-http" || !strings.Contains(r.out, "sends telemetry to a collector") {
		t.Fatalf("out:\n%s", r.out)
	}
	if r = exec(t, p, "", "copilot", "--apply", "--yes", "--force"); r.failure != nil || readJSONC(t, sp)[kExporter] != "file" {
		t.Fatalf("out:\n%s", r.out)
	}
}

func TestCaptureContentWarning(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	writeFile(t, sp, `{"`+kCapture+`": true}`)
	for _, args := range [][]string{{"copilot"}, {"copilot", "--apply", "--yes"}} {
		r := exec(t, p, "", args...)
		if r.failure != nil || !strings.Contains(r.out, "captureContent enabled") {
			t.Fatalf("%v: out:\n%s", args, r.out)
		}
		if readJSONC(t, sp)[kCapture] != true {
			t.Fatal("captureContent changed")
		}
	}
	if r := exec(t, p, "", "copilot"); !strings.Contains(r.out, "copilot --apply` to configure") {
		t.Fatalf("hint missing:\n%s", r.out)
	}
}

func TestApplyUnsafeFileUntouched(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	orig := "{\n  \"a\": 1\n"
	writeFile(t, sp, orig)
	r := exec(t, p, "", "copilot", "--apply", "--yes")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if b, _ := os.ReadFile(sp); string(b) != orig {
		t.Fatalf("file changed: %q", b)
	}
	if !strings.Contains(r.out, "untouched") {
		t.Fatalf("out:\n%s", r.out)
	}
	parseBlock(t, r.out)
}

func TestRevert(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	orig := "{\n  \"a\": 1\n}\n"
	writeFile(t, sp, orig)
	if r := exec(t, p, "", "copilot", "--apply", "--yes"); r.failure != nil {
		t.Fatal(r.failure)
	}
	backup, _ := os.ReadFile(sp + vscodecfg.BackupSuffix)
	r := exec(t, p, "", "copilot", "--revert")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if b, _ := os.ReadFile(sp); string(b) != orig {
		t.Fatalf("after revert:\n%s", b)
	}
	if b, _ := os.ReadFile(sp + vscodecfg.BackupSuffix); string(b) != string(backup) {
		t.Fatal("backup touched")
	}
	if r = exec(t, p, "", "copilot", "--revert"); !strings.Contains(r.out, "Nothing to revert") {
		t.Fatalf("out:\n%s", r.out)
	}
}

func TestRevertKeepsForeignValues(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	writeFile(t, sp, `{"`+kEnabled+`": true, "`+kExporter+`": "otlp-http", "`+kOutfile+`": "/theirs.jsonl"}`)
	r := exec(t, p, "", "copilot", "--revert")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	m := readJSONC(t, sp)
	if _, ok := m[kEnabled]; ok || m[kExporter] != "otlp-http" || m[kOutfile] != "/theirs.jsonl" {
		t.Fatalf("m = %v", m)
	}
	if strings.Count(r.out, "Kept ") != 2 {
		t.Fatalf("out:\n%s", r.out)
	}
}

func TestApplyRevertTogetherFails(t *testing.T) {
	p := initForCopilot(t)
	fakeVSCode(t)
	if r := exec(t, p, "", "copilot", "--apply", "--revert"); r.failure == nil {
		t.Fatal("accepted")
	}
}

const restartHint = "run `harnext-agent service install` to restart it."

func installedUnit(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "harnext-agent.service"), "x")
}

func TestApplyWarnsWhenSettingChangedAndAgentRunning(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	custom := filepath.Join(t.TempDir(), "mine.jsonl")
	writeFile(t, sp, "{\n  \""+kOutfile+"\": "+strconv.Quote(custom)+"\n}\n")
	stops := fakeRunningAgent(t, p, nil)
	r := exec(t, p, "", "copilot", "--apply", "--yes")
	if r.failure != nil || !strings.Contains(r.out, restartHint) || *stops != 0 {
		t.Fatalf("out=%q err=%v stops=%d", r.out, r.failure, *stops)
	}
	// Second run: setting already adopted, nothing changed.
	r = exec(t, p, "", "copilot", "--apply", "--yes")
	if r.failure != nil || strings.Contains(r.out, restartHint) {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}
}

func TestApplyNoWarningWhenAgentNotRunning(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	custom := filepath.Join(t.TempDir(), "mine.jsonl")
	writeFile(t, sp, "{\n  \""+kOutfile+"\": "+strconv.Quote(custom)+"\n}\n")
	r := exec(t, p, "", "copilot", "--apply", "--yes")
	if r.failure != nil || strings.Contains(r.out, restartHint) {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}
}

func TestApplyRestartRestartsThroughService(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	custom := filepath.Join(t.TempDir(), "mine.jsonl")
	writeFile(t, sp, "{\n  \""+kOutfile+"\": "+strconv.Quote(custom)+"\n}\n")
	var calls []string
	opts, home := svcEnv(t, p, &calls)
	installedUnit(t, home)
	stops := fakeRunningAgent(t, p, nil)
	r := execWith(t, Env{Paths: p, ServiceOptions: opts}, "copilot", "--apply", "--yes", "--restart")
	if r.failure != nil || !strings.Contains(r.out, "Restarted the agent.") || *stops != 1 || len(calls) != 2 || strings.Contains(r.out, restartHint) {
		t.Fatalf("out=%q err=%v stops=%d calls=%v", r.out, r.failure, *stops, calls)
	}
}

func TestApplyRestartWithoutServiceHintsOnly(t *testing.T) {
	p := initForCopilot(t)
	sp := fakeVSCode(t)
	custom := filepath.Join(t.TempDir(), "mine.jsonl")
	writeFile(t, sp, "{\n  \""+kOutfile+"\": "+strconv.Quote(custom)+"\n}\n")
	var calls []string
	opts, _ := svcEnv(t, p, &calls)
	stops := fakeRunningAgent(t, p, nil)
	r := execWith(t, Env{Paths: p, ServiceOptions: opts}, "copilot", "--apply", "--yes", "--restart")
	if r.failure != nil || !strings.Contains(r.out, "No service is installed") || *stops != 0 || len(calls) != 0 {
		t.Fatalf("out=%q err=%v stops=%d calls=%v", r.out, r.failure, *stops, calls)
	}
}

func TestPathChangeWarnsWhenAgentRunning(t *testing.T) {
	p := initForCopilot(t)
	fakeRunningAgent(t, p, nil)
	custom := filepath.Join(t.TempDir(), "otel.jsonl")
	r := exec(t, p, "", "copilot", "--path", custom)
	if r.failure != nil || !strings.Contains(r.out, restartHint) {
		t.Fatalf("out=%q err=%v", r.out, r.failure)
	}
	r = exec(t, p, "", "copilot", "--path", custom)
	if strings.Contains(r.out, restartHint) {
		t.Fatalf("warned without a change:\n%s", r.out)
	}
}

func TestApplyYesWithoutVSCodeSkipsQuietly(t *testing.T) {
	p := initForCopilot(t)
	r := exec(t, p, "", "copilot", "--apply", "--yes")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if !strings.Contains(r.out, "VS Code not found, so Copilot tracking was skipped") || strings.Contains(r.out, "add the lines manually") {
		t.Fatalf("out:\n%s", r.out)
	}
}
