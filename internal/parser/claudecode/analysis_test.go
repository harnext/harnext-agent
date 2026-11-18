package claudecode

import (
	"bytes"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

func analysisExtractor(t testing.TB) analyzer.Extractor {
	t.Helper()
	ex, err := analyzer.Compile(&remoteconfig.Config{
		Friction: remoteconfig.Friction{
			DetectorVersion: 1, RepeatMinTurns: 2, RepeatJaccard: 0.8, ErrorPasteMinShare: 0.6,
			Patterns: []remoteconfig.FeedbackPattern{
				{Category: "error_report", Lang: "en", Pattern: "fixture broken thing"},
				{Category: "error_report", Lang: "en", Pattern: "nope"},
			},
		},
		Topics: remoteconfig.Topics{
			Labels: []string{"feature", "bugfix", "other"}, Fallback: "other", MinScore: 1,
			KeywordWeights: map[string][]remoteconfig.TermWeight{"feature": {{Term: "implement", Weight: 3}}, "bugfix": {{Term: "crash", Weight: 2}}},
			ExtensionSignals: map[string][]remoteconfig.LabelWeight{
				".go": {{Label: "feature", Weight: 1}}, "_test.go": {{Label: "feature", Weight: 1}},
				".tsx": {{Label: "feature", Weight: 1}}, ".test.tsx": {{Label: "feature", Weight: 1}},
				".md": {{Label: "feature", Weight: 1}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func userMsg(content string) string {
	return `{"type":"user","sessionId":"s1","uuid":"u1","message":{"role":"user","content":` + content + `}}`
}

func TestParserUserFeatures(t *testing.T) {
	p := NewParser(analysisExtractor(t))
	tests := []struct {
		name  string
		line  string
		chars int
		neg   bool
		kw    uint32 // feature keyword points in thousandths
	}{
		{"string content", userMsg(`"please implement it, nope"`), 25, true, 3000},
		{"escaped string content", userMsg(`"line one\nnope. \"quoted\""`), 23, true, 0},
		{"text blocks are merged", userMsg(`[{"type":"text","text":"implement"},{"type":"image","source":{"data":"AAAA"}},{"type":"text","text":" fixture BROKEN thing"}]`), 9 + 21, true, 3000},
		{"no match", userMsg(`"all good"`), 8, false, 0},
		{"empty string", userMsg(`""`), 0, false, 0},
		{"null content", userMsg(`null`), 0, false, 0},
		{"numeric content", userMsg(`5`), 0, false, 0},
		{"object content", userMsg(`{"text":"nope"}`), 0, false, 0},
		{"image only", userMsg(`[{"type":"image","source":{}}]`), 0, false, 0},
		{"non-text block with a text key is ignored", userMsg(`[{"type":"thinking","text":"nope"}]`), 0, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, ok := p.ParseLine([]byte(tt.line)).(UserTurn)
			if !ok {
				t.Fatalf("not a UserTurn")
			}
			f := ev.Features
			if !ev.Analyzed || ev.IsToolResult || f.Chars != tt.chars || f.Negative() != tt.neg || f.KeywordScores[analyzer.LabelIndex("feature")] != tt.kw {
				t.Errorf("event = %+v", ev)
			}
			if ev.ResultCount != 0 {
				t.Error("tool results on a user message")
			}
		})
	}
}

func TestParserToolResultErrorHash(t *testing.T) {
	p := NewParser(analysisExtractor(t))
	errLine := func(uuid, out string, isErr bool) []byte {
		return []byte(fmt.Sprintf(`{"type":"user","sessionId":"s1","uuid":"%s","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":%q,"is_error":%v}]}}`, uuid, out, isErr))
	}
	a := p.ParseLine(errLine("a", "Error: build failed at line 12\n", true)).(UserTurn)
	b := p.ParseLine(errLine("b", "error: BUILD failed at line 340", true)).(UserTurn)
	c := p.ParseLine(errLine("c", "Error: something else", true)).(UserTurn)
	ok := p.ParseLine(errLine("d", "Error: build failed at line 12", false)).(UserTurn)
	h := func(u UserTurn) uint64 { return u.Results[0].ErrorHash }
	if !a.IsError || a.ResultCount != 1 || h(a) == 0 || h(a) != h(b) || h(a) == h(c) {
		t.Errorf("hashes: %d %d %d", h(a), h(b), h(c))
	}
	if h(ok) != 0 || ok.IsError || ok.Results[0].IsError || !ok.Analyzed || ok.Features != (analyzer.UserFeatures{}) {
		t.Errorf("non-error tool_result: %+v", ok)
	}
	// Array-form content, error without content, error content in a later block.
	arr := p.ParseLine([]byte(`{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"boom"}],"is_error":true}]}}`)).(UserTurn)
	none := p.ParseLine([]byte(`{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","is_error":true}]}}`)).(UserTurn)
	if h(arr) == 0 || h(none) != 0 || !none.IsError {
		t.Errorf("arr=%d none=%d", h(arr), h(none))
	}
	// Several blocks in one line are reported separately; a permission refusal is flagged.
	multi := p.ParseLine([]byte(`{"type":"user","sessionId":"s1","message":{"content":[` +
		`{"type":"tool_result","content":"ok"},` +
		`{"type":"tool_result","content":"The user doesn't want to proceed with this tool use. The tool use was rejected.","is_error":true},` +
		`{"type":"tool_result","content":"Error: boom","is_error":true}]}}`)).(UserTurn)
	if multi.ResultCount != 3 || multi.Results[0].IsError || !multi.Results[1].Denied || multi.Results[1].ErrorHash != 0 ||
		multi.Results[2].Denied || h(multi) != 0 || multi.Results[2].ErrorHash == 0 {
		t.Errorf("multi = %+v", multi.Results[:multi.ResultCount])
	}
	// Tool results never produce user-message features.
	if a.Features != (analyzer.UserFeatures{}) {
		t.Errorf("features on tool_result: %+v", a.Features)
	}
}

func TestParserSidechainIsNotAnalyzed(t *testing.T) {
	p := NewParser(analysisExtractor(t))
	ev := p.ParseLine([]byte(`{"type":"user","isSidechain":true,"sessionId":"s1","message":{"content":"nope"}}`)).(UserTurn)
	if ev.Analyzed || ev.Features != (analyzer.UserFeatures{}) || !ev.IsSidechain {
		t.Errorf("event = %+v", ev)
	}
	tr := p.ParseLine([]byte(`{"type":"user","isSidechain":true,"sessionId":"s1","message":{"content":[{"type":"tool_result","is_error":true,"content":"x"}]}}`)).(UserTurn)
	if tr.Analyzed || tr.ResultCount != 1 || tr.Results[0].ErrorHash != 0 || !tr.Results[0].IsError {
		t.Errorf("sidechain tool_result analyzed: %+v", tr)
	}
}

func TestParserWithoutExtractorEqualsParseLine(t *testing.T) {
	lines := []string{
		assistantLine, userStringLine, userMsg(`"nope"`),
		`{"type":"assistant","sessionId":"s1","message":{"id":"m","content":[{"type":"tool_use","id":"t","name":"Edit","input":{"file_path":"/x/y.go"}}],"usage":{"output_tokens":1}}}`,
		`{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","is_error":true,"content":"x"}]}}`,
		`garbage`,
	}
	var nilParser *Parser
	for _, l := range lines {
		want := ParseLine([]byte(l))
		if got := nilParser.ParseLine([]byte(l)); got != want {
			t.Errorf("nil parser differs for %.40s", l)
		}
		if got := NewParser(nil).ParseLine([]byte(l)); got != want {
			t.Errorf("parser without extractor differs for %.40s", l)
		}
	}
	if u := ParseLine([]byte(userMsg(`"nope"`))).(UserTurn); u.Analyzed {
		t.Error("ParseLine analyzed content")
	}
	if a := ParseLine([]byte(lines[3])).(AssistantUsage); a.ToolCount != 0 {
		t.Error("ParseLine extracted tool uses")
	}
}

func TestParserSetExtractorSwitchesAnalysis(t *testing.T) {
	p := NewParser(nil)
	line := []byte(userMsg(`"nope"`))
	if p.ParseLine(line).(UserTurn).Analyzed {
		t.Fatal("analyzed without extractor")
	}
	p.SetExtractor(analysisExtractor(t))
	if u := p.ParseLine(line).(UserTurn); !u.Analyzed || !u.Features.Negative() {
		t.Errorf("not analyzed after SetExtractor: %+v", u)
	}
	p.SetExtractor(nil)
	if p.ParseLine(line).(UserTurn).Analyzed {
		t.Error("still analyzing after SetExtractor(nil)")
	}
}

func toolLine(blocks string) []byte {
	return []byte(`{"type":"assistant","sessionId":"s1","requestId":"r","message":{"id":"m","model":"x","content":` + blocks + `,"usage":{"output_tokens":1}}}`)
}

func TestParserToolUses(t *testing.T) {
	p := NewParser(analysisExtractor(t))
	tests := []struct {
		name   string
		blocks string
		want   []ToolUse
	}{
		{"edit with go test file", `[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/home/dev/proj/pkg/foo_test.go","old_string":"x","new_string":"y"}}]`,
			[]ToolUse{{ID: "t1", Name: "Edit", Ext: ".go", CompoundExt: "_test.go", Dir: "/home/dev/proj/pkg"}}},
		{"bash keeps only the name", `[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"rm -rf /tmp/x","description":"d"}}]`,
			[]ToolUse{{ID: "t2", Name: "Bash"}}},
		{"typescript test file", `[{"type":"tool_use","id":"t3","name":"Write","input":{"file_path":"src/App.test.TSX","content":"c"}}]`,
			[]ToolUse{{ID: "t3", Name: "Write", Ext: ".tsx", CompoundExt: ".test.tsx"}}}, // relative path: no Dir,
		{"plain extension", `[{"type":"tool_use","id":"t4","name":"Read","input":{"file_path":"README.md"}}]`,
			[]ToolUse{{ID: "t4", Name: "Read", Ext: ".md"}}},
		{"windows path", `[{"type":"tool_use","id":"t5","name":"Read","input":{"file_path":"C:\\dev\\My.Project\\Main.GO"}}]`,
			[]ToolUse{{ID: "t5", Name: "Read", Ext: ".go", Dir: `C:\dev\My.Project`}}},
		{"extension without a configured signal stays empty", `[{"type":"tool_use","id":"t13","name":"Read","input":{"file_path":"/x/secret_notes.rs"}}]`,
			[]ToolUse{{ID: "t13", Name: "Read", Dir: "/x"}}},
		{"only the configured part of a suffix pair is kept", `[{"type":"tool_use","id":"t14","name":"Read","input":{"file_path":"/x/a_spec.go"}}]`,
			[]ToolUse{{ID: "t14", Name: "Read", Ext: ".go", Dir: "/x"}}},
		{"mcp style tool name", `[{"type":"tool_use","id":"t6","name":"mcp__server__tool","input":{}}]`,
			[]ToolUse{{ID: "t6", Name: "mcp__server__tool"}}},
		{"text and thinking blocks are ignored", `[{"type":"text","text":"hello"},{"type":"thinking","thinking":"hm"},{"type":"tool_use","id":"t7","name":"Grep","input":{"pattern":"secret"}}]`,
			[]ToolUse{{ID: "t7", Name: "Grep"}}},
		{"invalid tool name is skipped", `[{"type":"tool_use","id":"t8","name":"Bad Name!","input":{}},{"type":"tool_use","id":"t9","name":"","input":{}}]`, nil},
		{"file_path of wrong type", `[{"type":"tool_use","id":"t10","name":"Read","input":{"file_path":42}}]`, []ToolUse{{ID: "t10", Name: "Read"}}},
		{"input of wrong type", `[{"type":"tool_use","id":"t11","name":"Read","input":"x"}]`, []ToolUse{{ID: "t11", Name: "Read"}}},
		{"missing id is kept for the aggregator to drop", `[{"type":"tool_use","name":"Read","input":{}}]`, []ToolUse{{Name: "Read"}}},
		{"string content", `"just text"`, nil},
		{"null content", `null`, nil},
		{"notebook edit reads notebook_path", `[{"type":"tool_use","id":"t20","name":"NotebookEdit","input":{"notebook_path":"/home/dev/proj/nb/a.ipynb","new_source":"x"}}]`,
			[]ToolUse{{ID: "t20", Name: "NotebookEdit", Dir: "/home/dev/proj/nb"}}},
		{"file at the filesystem root", `[{"type":"tool_use","id":"t21","name":"Read","input":{"file_path":"/f.go"}}]`,
			[]ToolUse{{ID: "t21", Name: "Read", Ext: ".go", Dir: "/"}}},
		{"unc and mixed separators", `[{"type":"tool_use","id":"t22","name":"Edit","input":{"file_path":"\\\\srv\\share/p\\a.go"}}]`,
			[]ToolUse{{ID: "t22", Name: "Edit", Ext: ".go", Dir: `\\srv\share/p`}}},
		{"control characters drop the folder", `[{"type":"tool_use","id":"t23","name":"Edit","input":{"file_path":"/a\u0000b/c.go"}}]`,
			[]ToolUse{{ID: "t23", Name: "Edit", Ext: ".go"}}},
		{"mixed garbage elements", `[1,null,"x",{"type":"tool_use","id":"t12","name":"Glob","input":{"path":"/secret/dir"}}]`, []ToolUse{{ID: "t12", Name: "Glob"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, ok := p.ParseLine(toolLine(tt.blocks)).(AssistantUsage)
			if !ok {
				t.Fatal("not an AssistantUsage")
			}
			if ev.ToolCount != len(tt.want) {
				t.Fatalf("ToolCount = %d, want %d (%+v)", ev.ToolCount, len(tt.want), ev.Tools)
			}
			for i, w := range tt.want {
				if ev.Tools[i] != w {
					t.Errorf("tool %d = %+v, want %+v", i, ev.Tools[i], w)
				}
			}
			for i := ev.ToolCount; i < len(ev.Tools); i++ {
				if ev.Tools[i] != (ToolUse{}) {
					t.Errorf("slot %d beyond ToolCount is set", i)
				}
			}
		})
	}

	t.Run("capped per line", func(t *testing.T) {
		var blocks []string
		for i := 0; i < 20; i++ {
			blocks = append(blocks, fmt.Sprintf(`{"type":"tool_use","id":"t%d","name":"Read","input":{}}`, i))
		}
		ev := p.ParseLine(toolLine("[" + strings.Join(blocks, ",") + "]")).(AssistantUsage)
		if ev.ToolCount != MaxToolUsesPerLine || ev.Tools[MaxToolUsesPerLine-1].ID != "t7" {
			t.Errorf("ToolCount = %d", ev.ToolCount)
		}
	})
}

func TestPathSuffixes(t *testing.T) {
	tests := []struct{ path, ext, compound string }{
		{"/a/b/main.go", ".go", ""},
		{"/a/b/main_test.go", ".go", "_test.go"},
		{"a.test.ts", ".ts", ".test.ts"},
		{"a.spec.ts", ".ts", ".spec.ts"},
		{"archive.tar.gz", ".gz", ".tar.gz"},
		{"UPPER.GO", ".go", ""},
		{"README", "", ""},
		{".gitignore", "", ""},
		{".test.ts", ".ts", ".test.ts"},
		{"noext.", "", ""},
		{"dir.with.dots/file", "", ""},
		{"dir.with.dots/", "", ""},
		{"", "", ""},
		{"x.abcdefghi", "", ""},         // 9-letter extension is too long
		{"x.abcdefgh", ".abcdefgh", ""}, // 8 is the limit
		{"x.g o", "", ""},
		{"x.é", "", ""},
		{"a_b-c.go", ".go", "_b-c.go"},
		{"a." + strings.Repeat("z", 30) + ".go", ".go", ""}, // compound part too long
		{strings.Repeat("a", 300) + ".go", "", ""},
		{`C:\x\y.PY`, ".py", ""},
	}
	for _, tt := range tests {
		ext, compound := pathSuffixes(tt.path)
		if ext != tt.ext || compound != tt.compound {
			t.Errorf("pathSuffixes(%q) = %q, %q; want %q, %q", tt.path, ext, compound, tt.ext, tt.compound)
		}
	}
}

// Text, commands, file names and tool output must not appear in any event. The folder part of a file path is
// the one allowed exception (ToolUse.Dir, product-owner decision of 2026-11-15) and is tested separately.
func TestAnalysisDoesNotLeakContentIntoEvents(t *testing.T) {
	const s = "SENTINEL-CONTENT-8842"
	ex, err := analyzer.Compile(&remoteconfig.Config{
		Friction: remoteconfig.Friction{DetectorVersion: 1, RepeatMinTurns: 2, RepeatJaccard: 0.8, ErrorPasteMinShare: 0.6,
			Patterns: []remoteconfig.FeedbackPattern{{Category: "error_report", Lang: "en", Pattern: "sentinel-content"}}},
		Topics: remoteconfig.Topics{Labels: []string{"other"}, Fallback: "other",
			KeywordWeights: map[string][]remoteconfig.TermWeight{"other": {{Term: "sentinel", Weight: 1}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := NewParser(ex)
	lines := []string{
		userMsg(`"` + s + `"`),
		userMsg(`[{"type":"text","text":"` + s + `"}]`),
		`{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":"` + s + `","is_error":true}]},"toolUseResult":{"stdout":"` + s + `"}}`,
		`{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"` + s + `"}],"is_error":true}]}}`,
		string(toolLine(`[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"echo ` + s + `","file_path":"/allowed/dir/` + s + `.go"}},{"type":"text","text":"` + s + `"}]`)),
		`{"type":"assistant","sessionId":"s1","message":{"usage":"` + s + `"}}`,
		`{"type":"user","sessionId":"s1","message":` + s, // truncated: Skipped
	}
	for _, l := range lines {
		out := fmt.Sprintf("%+v %#v", p.ParseLine([]byte(l)), p.ParseLine([]byte(l)))
		if strings.Contains(strings.ToLower(out), strings.ToLower(s)) {
			t.Errorf("event leaks content for line %.60s: %s", l, out)
		}
	}
	// The pattern matched, so the test above really exercised analysis.
	if !p.ParseLine([]byte(lines[0])).(UserTurn).Features.Negative() {
		t.Error("sentinel pattern did not match")
	}
}

// Assistant lines, tool results and non-user lines must stay independent of content size
// even with analysis on; only real user messages may use temporary memory.
func TestAnalyzedParserMemoryDoesNotScaleWithContent(t *testing.T) {
	p := NewParser(analysisExtractor(t))
	big := strings.Repeat("y", 2<<20)
	for name, line := range map[string]string{
		"assistant text":     `{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"` + big + `"}],"usage":{"output_tokens":1}}}`,
		"assistant tool_use": `{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"tool_use","id":"t","name":"Write","input":{"file_path":"/p/a.go","content":"` + big + `"}}],"usage":{"output_tokens":1}}}`,
		"tool result":        `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":"` + big + `","is_error":false}]},"toolUseResult":{"stdout":"` + big + `"}}`,
		"tool result error":  `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":"` + big + `","is_error":true}]},"toolUseResult":{"stdout":"` + big + `"}}`,
		"tool result blocks": `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"` + big + `"}],"is_error":true}]}}`,
		"ignored":            `{"type":"attachment","x":"` + big + `"}`,
		"sidechain user":     `{"type":"user","isSidechain":true,"sessionId":"s1","message":{"content":"` + big + `"}}`,
	} {
		b := []byte(line)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		ev := p.ParseLine(b)
		runtime.ReadMemStats(&after)
		if grown := after.TotalAlloc - before.TotalAlloc; grown > 64<<10 {
			t.Errorf("%s: allocated %d bytes for a %d byte line", name, grown, len(b))
		}
		runtime.KeepAlive(ev)
	}
}

// Real user messages decode their text temporarily: bounded by a small multiple of the line
// size, and nothing is retained once the event is built.
func TestUserTextMemoryIsTemporary(t *testing.T) {
	p := NewParser(analysisExtractor(t))
	big := strings.Repeat("y", 2<<20)
	line := []byte(userMsg(`"` + big + `"`))

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	ev := p.ParseLine(line)
	runtime.ReadMemStats(&after)
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 8*uint64(len(line)) {
		t.Errorf("allocated %d bytes for a %d byte message", grown, len(line))
	}
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained > 1<<20 {
		t.Errorf("%d bytes still live after the call although the event is held", retained)
	}
	runtime.KeepAlive(ev)
}

func TestAnalyzedParseLineNeverPanics(t *testing.T) {
	p := NewParser(analysisExtractor(t))
	seeds := []string{
		userMsg(`"nope implement"`),
		userMsg(`[{"type":"text","text":"nope"},{"type":"image"}]`),
		`{"type":"user","sessionId":"s","message":{"content":[{"type":"tool_result","is_error":true,"content":"x"}]},"toolUseResult":{"interrupted":true}}`,
		string(toolLine(`[{"type":"tool_use","id":"t","name":"Edit","input":{"file_path":"/a/b_test.go"}}]`)),
	}
	rng := rand.New(rand.NewSource(2))
	for _, seed := range seeds {
		raw := []byte(seed)
		for i := 0; i <= len(raw); i++ {
			p.ParseLine(raw[:i])
			p.ParseLine(raw[i:])
		}
		for n := 0; n < 3000; n++ {
			mutated := bytes.Clone(raw)
			for k := rng.Intn(4) + 1; k > 0; k-- {
				mutated[rng.Intn(len(mutated))] = byte(rng.Intn(256))
			}
			p.ParseLine(mutated)
		}
	}
}

func FuzzParseLineAnalyzed(f *testing.F) {
	p := NewParser(analysisExtractor(f))
	for _, s := range []string{
		assistantLine, userStringLine, ``, `{`, `[[[`,
		userMsg(`"nope"`), userMsg(`[{"type":"text","text":"x"}]`),
		string(toolLine(`[{"type":"tool_use","id":"t","name":"Edit","input":{"file_path":"/a/b.go"}}]`)),
		`{"type":"user","sessionId":"s","message":{"content":[{"type":"tool_result","is_error":true,"content":"x"}]}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		if _, ok := p.ParseLine(line).(interface{ isEvent() }); !ok {
			t.Fatal("ParseLine returned nil event")
		}
	})
}

// The folder part of a file path is kept in ToolUse.Dir and nowhere else; the file name and the tool input
// around it (command, content) never appear in the event.
func TestToolUseKeepsOnlyFolderOfPath(t *testing.T) {
	const name, content, cmd = "FILENAME-SENTINEL-71", "CONTENT-SENTINEL-72", "COMMAND-SENTINEL-73"
	p := NewParser(analysisExtractor(t))
	line := toolLine(`[{"type":"tool_use","id":"t1","name":"Write","input":{"file_path":"/work/FOLDER-SENTINEL-70/` + name + `.go","content":"` + content + `","command":"` + cmd + `"}}]`)
	ev := p.ParseLine(line).(AssistantUsage)
	if ev.Tools[0].Dir != "/work/FOLDER-SENTINEL-70" {
		t.Fatalf("Dir = %q", ev.Tools[0].Dir)
	}
	// Everything except Dir must be free of the folder name too, so it cannot leak through another field.
	probe := ev.Tools[0]
	probe.Dir = ""
	out := strings.ToLower(fmt.Sprintf("%+v %#v %+v", ev, ev, probe))
	out = strings.ReplaceAll(out, strings.ToLower("/work/FOLDER-SENTINEL-70"), "<dir>")
	for _, s := range []string{name, content, cmd, "FOLDER-SENTINEL-70"} {
		if strings.Contains(out, strings.ToLower(s)) {
			t.Errorf("event leaks %q outside Dir: %s", s, out)
		}
	}
}
