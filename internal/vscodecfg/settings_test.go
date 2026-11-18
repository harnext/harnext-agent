package vscodecfg

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// An absolute path on every OS; Apply keeps an existing outfile only if it is absolute for the host.
var ours = func() string {
	if runtime.GOOS == "windows" {
		return "C:/home/u/.harnext/copilot-otel.jsonl"
	}
	return "/home/u/.harnext/copilot-otel.jsonl"
}()

func doc(text string) Doc {
	return Doc{Path: "settings.json", Exists: true, Raw: []byte(text), Text: text, Mode: 0o644}
}

func mustApply(t *testing.T, d Doc, w Want) (string, []Change) {
	t.Helper()
	if w.Outfile == "" {
		w.Outfile = ours
	}
	out, ch, err := Apply(d, w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(out); err != nil {
		t.Fatalf("result is not valid JSONC: %v\n%s", err, out)
	}
	return out, ch
}

func actions(ch []Change) string {
	var s []string
	for _, c := range ch {
		s = append(s, c.Key[strings.LastIndex(c.Key, ".")+1:]+":"+c.Action)
	}
	return strings.Join(s, ",")
}

func TestApplyMissingFile(t *testing.T) {
	out, ch := mustApply(t, Doc{Path: "x"}, Want{})
	m, _ := Decode(out)
	if m[KeyEnabled] != true || m[KeyExporter] != "file" || m[KeyOutfile] != ours || len(m) != 3 || !strings.HasSuffix(out, "\n}\n") {
		t.Fatalf("out:\n%s", out)
	}
	if actions(ch) != "enabled:added,exporterType:added,outfile:added" {
		t.Fatal(actions(ch))
	}
}

func TestApplyEmptyFile(t *testing.T) {
	for _, in := range []string{"", "  \n", "// nothing\n"} {
		out, _ := mustApply(t, doc(in), Want{})
		if m, _ := Decode(out); m[KeyOutfile] != ours {
			t.Fatalf("%q -> %s", in, out)
		}
		if strings.HasPrefix(in, "//") && !strings.HasPrefix(out, "// nothing") {
			t.Fatalf("comment lost: %s", out)
		}
	}
}

func TestApplyKeepsCommentsIndentAndTrailingComma(t *testing.T) {
	in := "// top\n{\n\t/* block */\n\t\"editor.fontSize\": 14, // inline\n\t\"files.exclude\": {\"a\": true,},\n}\n"
	out, _ := mustApply(t, doc(in), Want{})
	for _, keep := range []string{"// top", "/* block */", "// inline", `"files.exclude": {"a": true,},`} {
		if !strings.Contains(out, keep) {
			t.Fatalf("lost %q:\n%s", keep, out)
		}
	}
	if !strings.Contains(out, "\t\""+KeyEnabled+"\": true,\n") || !strings.HasSuffix(out, ",\n}\n") {
		t.Fatalf("style not followed:\n%s", out)
	}
	if strings.Index(out, "editor.fontSize") > strings.Index(out, KeyEnabled) {
		t.Fatal("key order changed")
	}
}

func TestApplyAddsCommaWithoutTrailingComma(t *testing.T) {
	in := "{\n  \"a\": 1 // note\n}"
	out, _ := mustApply(t, doc(in), Want{})
	if !strings.Contains(out, "\"a\": 1, // note") || strings.Contains(out, ",\n}") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestApplyCompactAndCRLF(t *testing.T) {
	out, _ := mustApply(t, doc(`{"a":1}`), Want{})
	if m, _ := Decode(out); m["a"] != 1.0 || m[KeyEnabled] != true {
		t.Fatal(out)
	}
	out, _ = mustApply(t, doc("{\r\n  \"a\": 1\r\n}\r\n"), Want{})
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Fatalf("mixed line endings: %q", out)
	}
}

func TestApplyBOM(t *testing.T) {
	d := doc("{}")
	d.BOM = true
	out, _ := mustApply(t, d, Want{})
	if !strings.HasPrefix(out, "\ufeff{") {
		t.Fatalf("BOM lost: %q", out)
	}
}

func TestReadBOMAndMissing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if d, err := Read(p); err != nil || d.Exists {
		t.Fatalf("%+v %v", d, err)
	}
	if err := os.WriteFile(p, []byte("\ufeff{\"a\":1}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := Read(p)
	if err != nil || !d.BOM || d.Text != `{"a":1}` {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestApplyIdempotent(t *testing.T) {
	first, _ := mustApply(t, doc("{\n  // c\n  \"a\": 1\n}\n"), Want{})
	second, ch := mustApply(t, doc(first), Want{})
	if second != first || Changed(ch) {
		t.Fatalf("second run changed the file: %v\n%s", actions(ch), second)
	}
}

func TestApplyEnabledFalseUpdatedInPlace(t *testing.T) {
	in := "{\n  \"github.copilot.chat.otel.enabled\": false, // keep me\n  \"github.copilot.chat.otel.exporterType\": \"file\",\n  \"github.copilot.chat.otel.outfile\": \"/x/y.jsonl\"\n}\n"
	out, ch := mustApply(t, doc(in), Want{})
	if !strings.Contains(out, "true, // keep me") || actions(ch) != "enabled:updated,outfile:kept" {
		t.Fatalf("%s\n%s", actions(ch), out)
	}
}

func TestApplyAdoptsExistingOutfile(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "mine.jsonl")
	in := `{"github.copilot.chat.otel.outfile": ` + enc(custom) + "}"
	out, ch := mustApply(t, doc(in), Want{})
	m, _ := Decode(out)
	if m[KeyOutfile] != custom {
		t.Fatalf("outfile replaced: %v", m[KeyOutfile])
	}
	for _, c := range ch {
		if c.Key == KeyOutfile && (c.Action != "kept" || c.Old != custom) {
			t.Fatalf("%+v", c)
		}
	}
	// A rejected value is replaced.
	out, _ = mustApply(t, doc(`{"github.copilot.chat.otel.outfile": "relative.jsonl"}`), Want{})
	if m, _ := Decode(out); m[KeyOutfile] != ours {
		t.Fatalf("relative outfile kept: %v", m[KeyOutfile])
	}
}

func TestApplyExporterTypeNeedsForce(t *testing.T) {
	in := `{"github.copilot.chat.otel.exporterType": "otlp-http"}`
	out, ch := mustApply(t, doc(in), Want{})
	if m, _ := Decode(out); m[KeyExporter] != "otlp-http" {
		t.Fatal("exporterType overwritten without force")
	}
	var kept bool
	for _, c := range ch {
		kept = kept || c.Key == KeyExporter && c.Action == "kept"
	}
	if !kept {
		t.Fatal(actions(ch))
	}
	out, _ = mustApply(t, doc(in), Want{Force: true})
	if m, _ := Decode(out); m[KeyExporter] != "file" {
		t.Fatal("force did not fix exporterType")
	}
}

func TestApplyUnsafeLeavesInputAlone(t *testing.T) {
	for _, in := range []string{`[1,2]`, `{"a":1`, `{"a": 1 "b": 2}`, `"str"`, `{"a": /* open`, `{"a": 1} trailing`, `{"a": "x\n}`} {
		if _, _, err := Apply(doc(in), Want{Outfile: ours}); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("%q: err = %v", in, err)
		}
	}
}

func TestWindowsPathIsEscaped(t *testing.T) {
	win := `C:\Users\me\AppData\"odd"\copilot-otel.jsonl`
	out, _ := mustApply(t, doc("{\n}\n"), Want{Outfile: win, AcceptOutfile: func(string) bool { return false }})
	m, _ := Decode(out)
	if m[KeyOutfile] != win || !strings.Contains(out, `C:\\Users`) {
		t.Fatalf("out:\n%s", out)
	}
}

func TestRemoveOnlyOurValues(t *testing.T) {
	in := "{\n  \"a\": 1,\n  // keep\n  \"github.copilot.chat.otel.enabled\": true,\n  \"github.copilot.chat.otel.exporterType\": \"otlp-http\",\n  \"github.copilot.chat.otel.outfile\": \"" + ours + "\"\n}\n"
	out, ch, err := Remove(doc(in), RemoveWant{Outfile: ours})
	if err != nil {
		t.Fatal(err)
	}
	m, err := Decode(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := m[KeyEnabled]; ok || m[KeyExporter] != "otlp-http" || m["a"] != 1.0 || len(m) != 2 {
		t.Fatalf("m = %v\n%s", m, out)
	}
	if !strings.Contains(out, "// keep") || actions(ch) != "enabled:removed,exporterType:kept,outfile:removed" {
		t.Fatalf("%s\n%s", actions(ch), out)
	}
	// A different outfile stays.
	out, ch, _ = Remove(doc(`{"github.copilot.chat.otel.outfile": "/other"}`), RemoveWant{Outfile: ours})
	if Changed(ch) || !strings.Contains(out, "/other") {
		t.Fatal(actions(ch))
	}
}

func TestRemoveRoundTrip(t *testing.T) {
	in := "{\n  \"a\": 1\n}\n"
	applied, _ := mustApply(t, doc(in), Want{})
	back, _, err := Remove(doc(applied), RemoveWant{Outfile: ours})
	if err != nil {
		t.Fatal(err)
	}
	if back != in {
		t.Fatalf("round trip differs:\n%q\n%q", in, back)
	}
}

func TestWriteBackupAtomicAndMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	orig := "{\n  \"a\": 1\n}\n"
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	d, _ := Read(p)
	out, _ := mustApply(t, d, Want{})
	b, err := Write(d, out)
	if err != nil || b != p+BackupSuffix {
		t.Fatalf("%q %v", b, err)
	}
	if got, _ := os.ReadFile(b); string(got) != orig {
		t.Fatal("backup differs from the original")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", fi.Mode())
		}
	}
	// Second write must not overwrite the first backup.
	d2, _ := Read(p)
	out2, _, _ := Remove(d2, RemoveWant{Outfile: ours})
	if _, err := Write(d2, out2); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(b); string(got) != orig {
		t.Fatal("backup overwritten")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".settings.json.harnext-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestWriteNewFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	d, _ := Read(p)
	out, _ := mustApply(t, d, Want{})
	if b, err := Write(d, out); err != nil || b != "" {
		t.Fatalf("%q %v", b, err)
	}
	if got, _ := os.ReadFile(p); string(got) != out {
		t.Fatal("content mismatch")
	}
}

func TestReadTooLarge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte("{\"a\":\""+strings.Repeat("x", MaxSize)+"\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(p); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestTargetsSkipsMissingEditions(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "Code", "User"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := Targets([]string{filepath.Join(base, "Code", "User"), filepath.Join(base, "Code - Insiders", "User")})
	if len(got) != 1 || got[0] != filepath.Join(base, "Code", "User", "settings.json") {
		t.Fatalf("%v", got)
	}
}

func TestInspectCaptureContent(t *testing.T) {
	m, err := Inspect(doc(`{"github.copilot.chat.otel.captureContent": true, // x
}`))
	if err != nil || m[KeyCapture] != true {
		t.Fatalf("%v %v", m, err)
	}
}
