package copilotchat

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
)

const sentinel = "SENTINEL_PROMPT_TEXT"

type fakeExtractor struct{ calls int }

func (f *fakeExtractor) UserFeatures(text string) analyzer.UserFeatures {
	f.calls++
	return analyzer.UserFeatures{Chars: len([]rune(text)), ErrorReport: strings.Contains(text, "wrong")}
}

func (f *fakeExtractor) Settings() analyzer.Settings { return analyzer.Settings{} }

func collect(line string, ex analyzer.Extractor) []Event {
	var evs []Event
	Parse([]byte(line), ex, func(e Event) { evs = append(evs, e) })
	return evs
}

func fixtureLines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/copilot/chat-session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func ms(n int64) time.Time { return time.UnixMilli(n).UTC() }

func TestFixtureLines(t *testing.T) {
	lines := fixtureLines(t)
	if len(lines) != 10 {
		t.Fatalf("fixture has %d lines, want 10", len(lines))
	}
	ex := &fakeExtractor{}
	tools := func(names ...string) (a [MaxTools]string) { copy(a[:], names); return }

	tests := []struct {
		name string
		line int
		want []Event
	}{
		{"empty snapshot", 0, []Event{Ignored{"snapshot_meta"}}},
		{"two requests appended", 1, []Event{
			RequestStart{At: -1, RequestID: "request_a1", Timestamp: ms(1790000001000), Model: "claude-test-1",
				Analyzed: true, Features: analyzer.UserFeatures{Chars: len([]rune("Please refactor this " + sentinel + " helper"))}},
			RequestStart{At: -1, RequestID: "request_a2", Timestamp: ms(1790000060000), Model: "claude-test-1",
				Analyzed: true, Features: analyzer.UserFeatures{Chars: len([]rune("Bu yanlış, that is wrong again " + sentinel)), ErrorReport: true}},
		}},
		{"completed", 2, []Event{RequestState{Index: 0, State: StateComplete, CompletedAt: ms(1790000030000)}}},
		{"cancelled", 3, []Event{RequestState{Index: 1, State: StateCancelled, CompletedAt: ms(1790000090000)}}},
		{"tools", 4, []Event{RequestTools{Index: 0, Model: "claude-test-1-resolved", Tools: tools("read_file", "grep_search", "read_file"), ToolCount: 3, Rounds: 2}}},
		{"hidden", 5, []Event{RequestStart{At: 2, RequestID: "request_h1", Timestamp: ms(1790000100000), Model: "claude-test-1", Hidden: true}}},
		{"truncate and append", 6, []Event{
			RequestStart{At: 1, RequestID: "request_b1", Timestamp: ms(1790000200000), Model: "claude-test-2",
				HasState: true, State: StateComplete, CompletedAt: ms(1790000230000),
				Analyzed: true, Features: analyzer.UserFeatures{Chars: len([]rune("fresh start " + sentinel))}},
		}},
		{"unrelated append path", 7, []Event{Ignored{"other_path"}}},
		{"unrelated set path", 8, []Event{Ignored{"other_path"}}},
		{"truncated json", 9, []Event{Skipped{ReasonInvalidJSON}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collect(lines[tc.line], ex)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestParseInline(t *testing.T) {
	ex := &fakeExtractor{}
	tests := []struct {
		name string
		line string
		ex   analyzer.Extractor
		want []Event
	}{
		{"empty line", "  ", ex, []Event{Skipped{ReasonEmpty}}},
		{"not an object", `[1]`, ex, []Event{Skipped{ReasonBadShape}}},
		{"missing kind", `{"v":{}}`, ex, []Event{Skipped{ReasonBadShape}}},
		{"unknown kind", `{"kind":7}`, ex, []Event{Skipped{ReasonBadShape}}},
		{"snapshot not object", `{"kind":0,"v":[]}`, ex, []Event{Skipped{ReasonBadShape}}},
		{"compacted snapshot", `{"kind":0,"v":{"requests":[{"requestId":"r1","modelId":"copilot/m","message":{"text":"wrong"},"modelState":{"value":2,"completedAt":1790000000000}},"junk"]}}`, ex, []Event{
			RequestStart{At: -1, RequestID: "r1", Model: "m", Analyzed: true, Features: analyzer.UserFeatures{Chars: 5, ErrorReport: true},
				HasState: true, State: StateCancelled, CompletedAt: ms(1790000000000)},
			RequestStart{At: -1},
		}},
		{"nil extractor ignores corrupt text", `{"kind":2,"k":["requests"],"v":[{"requestId":"r1","message":{"text":12345}}]}`, nil, []Event{RequestStart{At: -1, RequestID: "r1"}}},
		{"non-string text is not analyzed", `{"kind":2,"k":["requests"],"v":[{"requestId":"r1","message":{"text":12345}}]}`, ex, []Event{RequestStart{At: -1, RequestID: "r1"}}},
		{"append without i", `{"kind":2,"k":["requests"],"v":[{"requestId":"r1"},{"requestId":"r2"}]}`, nil, []Event{RequestStart{At: -1, RequestID: "r1"}, RequestStart{At: -1, RequestID: "r2"}}},
		{"append with i", `{"kind":2,"k":["requests"],"i":4,"v":[{"requestId":"r1"},{"requestId":"r2"}]}`, nil, []Event{RequestStart{At: 4, RequestID: "r1"}, RequestStart{At: 5, RequestID: "r2"}}},
		{"append value not array", `{"kind":2,"k":["requests"],"v":{}}`, nil, []Event{Skipped{ReasonBadShape}}},
		{"bad state value", `{"kind":1,"k":["requests",0,"modelState"],"v":{"value":9}}`, nil, []Event{Skipped{ReasonBadShape}}},
		{"bad index", `{"kind":1,"k":["requests","x","modelState"],"v":{"value":1}}`, nil, []Event{Skipped{ReasonBadShape}}},
		{"negative index", `{"kind":1,"k":["requests",-1,"result"],"v":{}}`, nil, []Event{Skipped{ReasonBadShape}}},
		{"pending state", `{"kind":1,"k":["requests",3,"modelState"],"v":{"value":0}}`, nil, []Event{RequestState{Index: 3, State: StatePending}}},
		{"bad ids dropped", `{"kind":2,"k":["requests"],"v":[{"requestId":"a b","modelId":"copilot/bad model","timestamp":"x"}]}`, nil, []Event{RequestStart{At: -1}}},
		{"bad tool names dropped", `{"kind":1,"k":["requests",0,"result"],"v":{"metadata":{"toolCallRounds":[{"toolCalls":[{"name":"a b"},{"name":""},{"name":5},{"name":"ok.tool-1"}]}]}}}`, nil,
			[]Event{RequestTools{Index: 0, Tools: [MaxTools]string{"ok.tool-1"}, ToolCount: 1, Rounds: 1}}},
		{"result not object", `{"kind":1,"k":["requests",0,"result"],"v":null}`, nil, []Event{RequestTools{Index: 0}}},
		{"line too long", `{"kind":0,"v":{"pad":"` + strings.Repeat("x", MaxLineBytes) + `"}}`, nil, []Event{Skipped{ReasonLineTooLong}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collect(tc.line, tc.ex)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestToolCap(t *testing.T) {
	var calls []string
	for i := 0; i < MaxTools+4; i++ {
		calls = append(calls, `{"name":"t"}`)
	}
	line := `{"kind":1,"k":["requests",0,"result"],"v":{"metadata":{"toolCallRounds":[{"toolCalls":[` + strings.Join(calls, ",") + `]}]}}}`
	got := collect(line, nil)
	rt, ok := got[0].(RequestTools)
	if !ok || rt.ToolCount != MaxTools {
		t.Fatalf("got %+v, want ToolCount %d", got, MaxTools)
	}
}

func TestExtractorNotCalledWithoutNeed(t *testing.T) {
	ex := &fakeExtractor{}
	collect(`{"kind":2,"k":["requests"],"v":[{"hiddenFromTranscript":true,"message":{"text":"x"}}]}`, ex)
	collect(`{"kind":2,"k":["requests"],"v":[{"message":{"text":"x"}}]}`, nil)
	if ex.calls != 0 {
		t.Errorf("extractor called %d times for hidden request", ex.calls)
	}
}

func TestPanicGuard(t *testing.T) {
	got := collect(`{"kind":2,"k":["requests"],"v":[{"message":{"text":"x"}}]}`, panicExtractor{})
	if !reflect.DeepEqual(got, []Event{Skipped{ReasonInternal}}) {
		t.Errorf("got %+v", got)
	}
}

type panicExtractor struct{}

func (panicExtractor) UserFeatures(string) analyzer.UserFeatures { panic("boom") }
func (panicExtractor) Settings() analyzer.Settings               { return analyzer.Settings{} }

func TestNoFreeTextFields(t *testing.T) {
	// Dir is the one path-derived string, allowed only on FileRef: the FOLDER part of a file path (product-owner decision of
	// 2026-11-15); the file name never appears in an event (TestFileRefsKeepOnlyTheFolder).
	allowed := map[string]bool{"RequestID": true, "Model": true, "Type": true, "Reason": true, "ToolID": true}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(RequestStart{}), reflect.TypeOf(RequestState{}), reflect.TypeOf(RequestTools{}),
		reflect.TypeOf(RequestToolCalls{}), reflect.TypeOf(RequestFiles{}), reflect.TypeOf(RequestMetric{}), reflect.TypeOf(ToolCall{}),
		reflect.TypeOf(FileRef{}), reflect.TypeOf(Ignored{}), reflect.TypeOf(Skipped{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			switch f.Type.Kind() {
			case reflect.String:
				if !allowed[f.Name] && !(typ == reflect.TypeOf(FileRef{}) && f.Name == "Dir") {
					t.Errorf("%s.%s: unexpected string field", typ.Name(), f.Name)
				}
			case reflect.Array:
				if f.Name != "Tools" || f.Type.Elem().Kind() != reflect.String {
					if f.Name != "KeywordScores" && !(f.Name == "Calls" && f.Type.Elem() == reflect.TypeOf(ToolCall{})) &&
						!(f.Name == "Files" && f.Type.Elem() == reflect.TypeOf(FileRef{})) {
						t.Errorf("%s.%s: unexpected array field", typ.Name(), f.Name)
					}
				}
			case reflect.Int, reflect.Int64, reflect.Bool, reflect.Struct, reflect.Uint64, reflect.Float64:
			default:
				t.Errorf("%s.%s: unexpected kind %s", typ.Name(), f.Name, f.Type.Kind())
			}
		}
	}

	for _, ex := range []analyzer.Extractor{nil, &fakeExtractor{}} {
		for i, line := range fixtureLines(t) {
			for _, e := range collect(line, ex) {
				if got := fmt.Sprintf("%+v", e); strings.Contains(got, sentinel) {
					t.Errorf("line %d leaked sentinel: %s", i+1, got)
				}
			}
		}
	}
}

func TestNilExtractorNeverAnalyzes(t *testing.T) {
	for i, line := range fixtureLines(t) {
		for _, e := range collect(line, nil) {
			if rs, ok := e.(RequestStart); ok && (rs.Analyzed || rs.Features != (analyzer.UserFeatures{})) {
				t.Errorf("line %d: analyzed without extractor: %+v", i+1, rs)
			}
		}
	}
}

func TestToolInvocationParts(t *testing.T) {
	line := `{"kind":2,"k":["requests",3,"response"],"v":[` +
		`{"kind":"markdownContent","content":{"value":"SENTINEL_PROMPT_TEXT"}},` +
		`{"kind":"toolInvocationSerialized","toolCallId":"call_a","toolId":"copilot_runInTerminal","isComplete":true,` +
		`"invocationMessage":"SENTINEL_PROMPT_TEXT","toolSpecificData":{"kind":"terminal","terminalCommandState":{"exitCode":127}}},` +
		`{"kind":"toolInvocationSerialized","toolCallId":"call_b","toolId":"copilot_replaceString","isComplete":true,` +
		`"resultError":"Error: SENTINEL_PROMPT_TEXT could not be applied","resultDetails":{"isError":true}},` +
		`{"kind":"toolInvocationSerialized","toolCallId":"call_c","toolId":"copilot_readFile","isComplete":false},` +
		`{"kind":"toolInvocationSerialized","toolCallId":"call_d","toolId":"copilot_readFile","isComplete":true,"resultError":false}],"i":4}`
	evs := collect(line, &fakeExtractor{})
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	e, ok := evs[0].(RequestToolCalls)
	if !ok || e.Index != 3 || e.N != 3 {
		t.Fatalf("event = %+v", evs[0])
	}
	term, edit, read := e.Calls[0], e.Calls[1], e.Calls[2]
	if !term.HasExit || term.ExitCode != 127 || term.IsError || term.ToolID != "copilot_runInTerminal" || term.IDHash == 0 {
		t.Errorf("terminal = %+v", term)
	}
	if !edit.IsError || edit.ErrorHash == 0 || edit.HasExit {
		t.Errorf("edit = %+v", edit)
	}
	if read.IsError || read.ErrorHash != 0 {
		t.Errorf("read = %+v", read)
	}
	if strings.Contains(fmt.Sprintf("%+v", e), sentinel) {
		t.Error("tool call event leaked text")
	}
	// Without an extractor the error text is not fingerprinted.
	plain := collect(line, nil)[0].(RequestToolCalls)
	if plain.Calls[1].ErrorHash != 0 || !plain.Calls[1].IsError {
		t.Errorf("no-extractor edit = %+v", plain.Calls[1])
	}
}

func TestMetricsPatchAndAppend(t *testing.T) {
	got := collect(`{"kind":1,"k":["requests",3,"copilotCredits"],"v":23.01549}`, nil)
	if !reflect.DeepEqual(got, []Event{RequestMetric{Index: 3, Kind: MetricCredits, Value: 23.01549}}) {
		t.Errorf("credits patch: %+v", got)
	}
	got = collect(`{"kind":1,"k":["requests",1,"promptTokens"],"v":57204}`, nil)
	if !reflect.DeepEqual(got, []Event{RequestMetric{Index: 1, Kind: MetricPrompt, Value: 57204}}) {
		t.Errorf("prompt patch: %+v", got)
	}
	got = collect(`{"kind":1,"k":["requests",1,"completionTokens"],"v":139}`, nil)
	if !reflect.DeepEqual(got, []Event{RequestMetric{Index: 1, Kind: MetricCompletion, Value: 139}}) {
		t.Errorf("completion patch: %+v", got)
	}
	got = collect(`{"kind":2,"k":["requests"],"v":[{"requestId":"r1","timestamp":1790000000000,"modelId":"copilot/gpt-5.1","copilotCredits":0.5,"promptTokens":1000,"completionTokens":20,"result":{"metadata":{"toolCallRounds":[{},{}]}}}]}`, nil)
	rs, ok := got[0].(RequestStart)
	if len(got) != 1 || !ok || !rs.HasCredits || rs.Credits != 0.5 || !rs.HasPrompt || rs.PromptTokens != 1000 || !rs.HasCompletion || rs.CompletionTokens != 20 || rs.Rounds != 2 || rs.Model != "gpt-5.1" {
		t.Errorf("append: %+v", got)
	}
	got = collect(`{"kind":2,"k":["requests"],"v":[{"requestId":"r2","copilotCredits":-1,"promptTokens":1e300}]}`, nil)
	if rs, ok := got[0].(RequestStart); !ok || rs.HasCredits || rs.HasPrompt {
		t.Errorf("implausible values must be ignored: %+v", got)
	}
	for _, line := range []string{
		`{"kind":1,"k":["requests",0,"copilotCredits"],"v":-3}`,
		`{"kind":1,"k":["requests",0,"promptTokens"],"v":"12"}`,
		`{"kind":1,"k":["requests",0,"completionTokens"],"v":1e300}`,
	} {
		if got := collect(line, nil); !reflect.DeepEqual(got, []Event{Skipped{ReasonBadShape}}) {
			t.Errorf("%s: %+v", line, got)
		}
	}
}

// Only the folder part of a file path is read: the file name, the text edits and everything else in the part never reach an event.
func TestFileRefsKeepOnlyTheFolder(t *testing.T) {
	const name = "SECRET-FILE-NAME"
	uri := func(dir string) string {
		return `{"$mid":1,"fsPath":"` + dir + `/` + name + `.ts","external":"file:///x","path":"` + dir + `/` + name + `.ts","scheme":"file"}`
	}
	line := `{"kind":2,"k":["requests",3,"response"],"v":[` +
		`{"kind":"markdownContent","content":{"value":"SENTINEL_PROMPT_TEXT"}},` +
		`{"kind":"textEditGroup","uri":` + uri("/work/proj/apps/web") + `,"edits":[[{"text":"SENTINEL_PROMPT_TEXT","range":{}}]],"done":true},` +
		`{"kind":"textEditGroup","uri":` + uri("/work/proj/apps/web") + `,"edits":[],"done":true},` + // same file again
		`{"kind":"codeblockUri","uri":` + uri("/work/proj/docs") + `,"isEdit":true},` +
		`{"kind":"codeblockUri","uri":` + uri("/work/proj/lib") + `},` +
		`{"kind":"inlineReference","inlineReference":` + uri("/work/proj/cmd/agent") + `},` +
		`{"kind":"inlineReference","inlineReference":{"name":"symbol","location":{}}},` +
		`{"kind":"textEditGroup","uri":{"scheme":"vscode-remote","path":"/remote/x/` + name + `.ts"}},` +
		`{"kind":"textEditGroup","uri":{"scheme":"file","fsPath":"relative/` + name + `.ts"}},` +
		`{"kind":"textEditGroup","uri":{"scheme":"file","path":"/c:/dev/proj/` + name + `.ts"}}],"i":4}`
	evs := collect(line, &fakeExtractor{})
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	e, ok := evs[0].(RequestFiles)
	if !ok || e.Index != 3 || e.N != 5 {
		t.Fatalf("event = %+v", evs[0])
	}
	var got []string
	for _, f := range e.Files[:e.N] {
		kind := "read"
		if f.Edit {
			kind = "edit"
		}
		got = append(got, kind+" "+f.Dir)
		if f.Hash == 0 {
			t.Errorf("file in %s has no hash", f.Dir)
		}
	}
	want := []string{"edit /work/proj/apps/web", "edit /work/proj/docs", "read /work/proj/lib", "read /work/proj/cmd/agent", "edit c:/dev/proj"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %q\nwant %q", got, want)
	}
	// Two groups for one file collapse; the hash differs per file, and nothing but the folder carries a name.
	out := fmt.Sprintf("%+v %#v", e, e)
	for _, leak := range []string{name, sentinel} {
		if strings.Contains(out, leak) {
			t.Errorf("event leaks %q: %s", leak, out)
		}
	}
}

// Request-level fields carry files too: a request object (snapshot or append) with editedFileEvents and contentReferences, and a
// patch that sets contentReferences.
func TestFileRefsFromRequestFieldsAndPatches(t *testing.T) {
	uri := `{"scheme":"file","fsPath":"/work/proj/services/api/handler.go"}`
	req := `{"kind":2,"k":["requests"],"v":[{"requestId":"r1","timestamp":1790000000000,"message":{"text":"hi"},` +
		`"editedFileEvents":[{"uri":` + uri + `,"eventKind":1}],"contentReferences":[{"kind":"reference","reference":{"scheme":"file","fsPath":"/work/proj/docs/a.md"}}],` +
		`"response":[{"kind":"textEditGroup","uri":` + uri + `}]}],"i":0}`
	rs, ok := collect(req, nil)[0].(RequestStart)
	// The same edited file reached through two fields counts once.
	if !ok || rs.FileCount != 2 {
		t.Fatalf("FileCount = %d", rs.FileCount)
	}
	if rs.Files[0].Dir != "/work/proj/services/api" || !rs.Files[0].Edit || rs.Files[1].Dir != "/work/proj/docs" || rs.Files[1].Edit {
		t.Errorf("files = %+v", rs.Files[:rs.FileCount])
	}
	patch := `{"kind":1,"k":["requests",2,"contentReferences"],"v":[{"kind":"reference","reference":{"scheme":"file","fsPath":"/work/proj/lib/x.go"}}]}`
	pe, ok := collect(patch, nil)[0].(RequestFiles)
	if !ok || pe.Index != 2 || pe.N != 1 || pe.Files[0].Dir != "/work/proj/lib" || pe.Files[0].Edit {
		t.Fatalf("patch event = %+v", pe)
	}
	if got := collect(`{"kind":1,"k":["requests",2,"contentReferences"],"v":[]}`, nil); !reflect.DeepEqual(got, []Event{Ignored{"other_path"}}) {
		t.Errorf("empty references = %+v", got)
	}
}
