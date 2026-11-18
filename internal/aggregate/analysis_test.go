package aggregate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/claudecode"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

func testExtractor(t *testing.T) analyzer.Extractor {
	t.Helper()
	ex, err := analyzer.Compile(&remoteconfig.Config{
		Friction: remoteconfig.Friction{
			DetectorVersion: 1, RepeatMinTurns: 2, RepeatJaccard: 0.8, ErrorPasteMinShare: 0.6,
			Patterns: []remoteconfig.FeedbackPattern{{Category: "error_report", Lang: "en", Pattern: "fixture broken thing"}},
		},
		Topics: remoteconfig.Topics{
			Labels: []string{"feature", "bugfix", "test", "other"}, MinScore: 1, Fallback: "other",
			ExtensionSignals: map[string][]remoteconfig.LabelWeight{".go": {{Label: "feature", Weight: 1}}},
			ToolSignals:      map[string][]remoteconfig.LabelWeight{"Edit": {{Label: "feature", Weight: 1}}},
			KeywordWeights:   map[string][]remoteconfig.TermWeight{"bugfix": {{Term: "crash", Weight: 2}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func withTool(id, name, ext string) func(*claudecode.AssistantUsage) {
	return func(e *claudecode.AssistantUsage) {
		e.Tools[0], e.ToolCount = claudecode.ToolUse{ID: id, Name: name, Ext: ext}, 1
	}
}

func atTime(s string) func(*claudecode.AssistantUsage) {
	return func(e *claudecode.AssistantUsage) { e.Timestamp = at(s) }
}

func analyzedUser(ex analyzer.Extractor, uuid, ts, text string) claudecode.UserTurn {
	return claudecode.UserTurn{SessionID: "s", UUID: uuid, Timestamp: at(ts), Analyzed: true, Features: ex.UserFeatures(text)}
}

func toolResult(uuid string, analyzed, isErr bool, hash uint64, interrupted bool) claudecode.UserTurn {
	u := claudecode.UserTurn{SessionID: "s", UUID: uuid, IsToolResult: true, Analyzed: analyzed, IsError: isErr, Interrupted: interrupted, ResultCount: 1}
	u.Results[0] = claudecode.ToolResult{IsError: isErr, ErrorHash: hash}
	return u
}

// analysisEvents: 2 analyzed user messages (the first reports an error but has nothing to push
// back on), 2 responses and 2 identical tool errors in the second exchange (a tool loop).
func analysisEvents(ex analyzer.Extractor) []claudecode.Event {
	return []claudecode.Event{
		usage("s", "m", "m1", "r1", 1, atTime("2026-01-01T10:00:00Z"), withTool("t1", "Edit", ".go")),
		analyzedUser(ex, "u1", "2026-01-01T10:00:05Z", "this is a fixture broken thing"),
		analyzedUser(ex, "u2", "2026-01-01T10:00:30Z", "all good"),
		usage("s", "m", "m2", "r2", 1, atTime("2026-01-01T10:01:00Z")),
		toolResult("tr1", true, true, 7, false),
		toolResult("tr2", true, true, 7, false),
	}
}

func TestRowsCarryFrictionFirstPassAndTopic(t *testing.T) {
	ex := testExtractor(t)
	a := New()
	a.SetAnalyzer(ex)
	for _, e := range analysisEvents(ex) {
		a.Add(e)
	}
	// Replayed lines (content blocks repeat the same response and tool_use) change nothing.
	a.Add(usage("s", "m", "m1", "r1", 1, atTime("2026-01-01T10:00:00Z"), withTool("t1", "Edit", ".go")))
	a.Add(toolResult("tr1", true, true, 7, false))

	rows := a.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	want := analyzer.Quality{
		Exchanges: 2, AnalyzedExchanges: 2, FrictionExchanges: 1, FxToolLoop: 1, FxPrimaryToolLoop: 1,
		SigErrorReport: 1, ToolCalls: 2, ToolErrors: 2, RepeatedErrorRuns: 1, RepeatedErrorCalls: 2,
		Responses: 2, FirstFrictionExchange: 2,
	}
	if r.Quality != want || r.DetectorVersion != 1 || r.SignalCoverage != ClaudeCodeCoverage {
		t.Errorf("quality = %+v (detector %d, coverage %d)\nwant %+v", r.Quality, r.DetectorVersion, r.SignalCoverage, want)
	}
	if r.Topic != "feature" { // Edit tool (1) + .go suffix (1) once each
		t.Errorf("topic = %q", r.Topic)
	}
	an := a.Snapshot().Sessions["s"].Analysis
	if an.Topic.Tool.Count(analyzer.SignalKey("Edit")) != 1 {
		t.Error("tool_use counted twice")
	}

	if c := a.Counters(); c.AnalyzedUserTurns != 2 || c.UnanalyzedUserTurns != 0 || c.DuplicateToolResults != 1 || c.ToolUses != 1 {
		t.Errorf("counters = %+v", c)
	}
}

func TestNoAnalyzerMeansNoScoresAndStateSurvivesExpiry(t *testing.T) {
	ex := testExtractor(t)

	// A parser without an Extractor yields unanalyzed turns: nothing can be scored.
	plain := New()
	for _, e := range analysisEvents(ex) {
		if u, ok := e.(claudecode.UserTurn); ok {
			u.Analyzed, u.Features = false, analyzer.UserFeatures{}
			u.Results[0].ErrorHash = 0
			e = u
		}
		plain.Add(e)
	}
	if r := plain.Rows()[0]; r.Quality.AnalyzedExchanges != 0 || r.Quality.Exchanges != 2 || r.Quality.FrictionExchanges != 0 || r.DetectorVersion != 0 || r.Topic != "" {
		t.Errorf("without analyzer: %+v %d %q", r.Quality, r.DetectorVersion, r.Topic)
	}

	// Thresholds are applied when events arrive, so the analyzer is set before feeding.
	a := New()
	a.SetAnalyzer(ex)
	for _, e := range analysisEvents(ex) {
		a.Add(e)
	}
	withAn := a.Rows()[0]
	if withAn.Quality.FrictionExchanges != 1 || withAn.DetectorVersion != 1 {
		t.Fatalf("with analyzer: %+v", withAn.Quality)
	}
	a.SetAnalyzer(nil) // configuration expired: the detector version and topic go, counters stay
	if r := a.Rows()[0]; r.Quality != withAn.Quality || r.DetectorVersion != 0 || r.Topic != "" {
		t.Errorf("expired: %+v %d %q", r.Quality, r.DetectorVersion, r.Topic)
	}
	a.SetAnalyzer(ex)
	if r := a.Rows()[0]; r.Quality != withAn.Quality || r.DetectorVersion != 1 {
		t.Errorf("renewed: %+v", r.Quality)
	}
}

func TestUnanalyzedMessagesStayOutOfTheDenominator(t *testing.T) {
	ex := testExtractor(t)
	a := New()
	a.SetAnalyzer(ex)
	a.Add(claudecode.UserTurn{SessionID: "s", UUID: "u1"}) // read while no configuration existed
	a.Add(usage("s", "m", "m1", "r1", 1))
	r := a.Rows()[0]
	if r.Quality.Exchanges != 1 || r.Quality.AnalyzedExchanges != 0 || r.Quality.FrictionExchanges != 0 {
		t.Errorf("quality = %+v; an unanalyzed exchange must not enter the denominator", r.Quality)
	}
	if r.Topic != "other" {
		t.Errorf("topic = %q, want the fallback", r.Topic)
	}
	if c := a.Counters(); c.UnanalyzedUserTurns != 1 || c.AnalyzedUserTurns != 0 {
		t.Errorf("counters = %+v", c)
	}
	if got := a.Snapshot().Sessions["s"].Analysis.Stats.UnanalyzedMessages; got != 1 {
		t.Errorf("UnanalyzedMessages = %d", got)
	}
}

func TestSidechainAndInterruption(t *testing.T) {
	ex := testExtractor(t)
	a := New()
	a.SetAnalyzer(ex)
	a.Add(usage("s", "m", "m1", "r1", 1, atTime("2026-01-01T10:00:00Z")))
	a.Add(usage("s", "m", "m2", "r2", 1, func(e *claudecode.AssistantUsage) { e.IsSidechain = true }, withTool("t9", "Edit", ".go")))
	a.Add(analyzedUser(ex, "u1", "2026-01-01T10:05:00Z", "fine"))
	a.Add(claudecode.UserTurn{SessionID: "s", UUID: "x", IsSidechain: true, IsToolResult: true, IsError: true, Interrupted: true})
	// Interruptions are structural: counted even when the result itself was not analyzed.
	a.Add(toolResult("tr1", false, false, 0, true))

	q := a.Rows()[0].Quality
	// The sidechain response and tool result do not count; the interrupted result marks the
	// open (analyzed) exchange.
	if q.Responses != 1 || q.InterruptedTools != 1 || q.ToolCalls != 1 || q.FxInterrupt != 1 || q.FrictionExchanges != 1 || q.FirstFrictionExchange != 1 {
		t.Errorf("quality = %+v", q)
	}
}

// Replaying lines over a restored snapshot (offset lost) must reproduce the same analysis.
func TestSnapshotRestoreWithAnalysisDoesNotDoubleCount(t *testing.T) {
	ex := testExtractor(t)
	events := analysisEvents(ex)

	ref := New()
	ref.SetAnalyzer(ex)
	for _, e := range events {
		ref.Add(e)
	}
	// Counters legitimately differ after a replay (duplicates are counted), so compare the
	// per-session state and the dedup keys instead.
	sessionState := func(a *Aggregator) string {
		st := a.Snapshot()
		return mustJSON(t, []any{st.Sessions, st.Seen})
	}
	wantRows, wantState := ref.Rows(), sessionState(ref)

	for split := 0; split <= len(events); split++ {
		first := New()
		first.SetAnalyzer(ex)
		for _, e := range events[:split] {
			first.Add(e)
		}
		var st State
		if err := json.Unmarshal([]byte(mustJSON(t, first.Snapshot())), &st); err != nil {
			t.Fatal(err)
		}
		second := New()
		second.SetAnalyzer(ex)
		if err := second.Restore(st); err != nil {
			t.Fatal(err)
		}
		for _, e := range events { // full replay from line 0
			second.Add(e)
		}
		if got := second.Rows(); !reflect.DeepEqual(got, wantRows) {
			t.Fatalf("split %d: rows =\n%+v\nwant\n%+v", split, got, wantRows)
		}
		if got := sessionState(second); got != wantState {
			t.Fatalf("split %d: state differs:\n%s\nwant\n%s", split, got, wantState)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// legacy2aSnapshot is a snapshot as written by phase 2a, before analysis existed: no
// "analysis" per session and none of the analysis counters. It is written by hand on purpose;
// it must keep loading as long as stateVersion is 1. The "seen" values are the FNV-1a keys of
// response (msg-legacy-1, req-legacy-1) and user turn (uuid-legacy-1).
const legacy2aSnapshot = `{
  "version": 1,
  "sessions": {
    "legacy-session": {
      "pending_turns": 1,
      "last_model": "claude-legacy",
      "started_at": "2026-01-01T10:00:00Z",
      "ended_at": "2026-01-01T10:05:00Z",
      "meta_at": "2026-01-01T10:05:00Z",
      "cwd": "/home/dev/example",
      "git_branch": "main",
      "tool_version": "2.0.0",
      "models": {
        "claude-legacy": {
          "input_tokens": 10,
          "output_tokens": 200,
          "cache_read_tokens": 3000,
          "cache_creation_tokens": 400,
          "cache_creation_1h_tokens": 100,
          "turn_count": 2
        }
      }
    }
  },
  "seen": [4292026569651696125, 16841896283675297466],
  "counters": {
    "unique_responses": 5,
    "duplicate_responses": 2,
    "sidechain_responses": 0,
    "synthetic_responses": 0,
    "missing_ids": 0,
    "missing_model": 0,
    "user_turns": 3,
    "tool_results": 4,
    "sidechain_user_turns": 0,
    "duplicate_user_turns": 0,
    "skipped": {"invalid_json": 1},
    "ignored": {"queue-operation": 2}
  }
}`

// Snapshots written before usage keys (state version 1) are rejected; the service then discards
// them and re-reads transcripts, so totals are rebuilt with day, speed and prompt-floor keys.
func TestRestoreRejectsPreUsageKeySnapshot(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(legacy2aSnapshot), &st); err != nil {
		t.Fatal(err)
	}
	a := New()
	if err := a.Restore(st); err == nil {
		t.Fatal("a version 1 snapshot must not load")
	}
	if rows := a.Rows(); len(rows) != 0 {
		t.Errorf("rejected snapshot left rows: %+v", rows)
	}
}

// walkTypes fails on any type that could hold text: strings, byte slices, interfaces, pointers
// to such, maps with string keys or values. time.Time is the one allowed opaque leaf.
func walkTypes(t *testing.T, ty reflect.Type, path string, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[ty] || ty == reflect.TypeOf(time.Time{}) {
		return
	}
	seen[ty] = true
	switch ty.Kind() {
	case reflect.String, reflect.Interface, reflect.UnsafePointer, reflect.Func, reflect.Chan:
		t.Errorf("%s has forbidden kind %s", path, ty.Kind())
	case reflect.Slice:
		if ty.Elem().Kind() == reflect.Uint8 {
			t.Errorf("%s is a byte slice", path)
			return
		}
		walkTypes(t, ty.Elem(), path+"[]", seen)
	case reflect.Array, reflect.Pointer:
		walkTypes(t, ty.Elem(), path+"[]", seen)
	case reflect.Map:
		walkTypes(t, ty.Key(), path+"{key}", seen)
		walkTypes(t, ty.Elem(), path+"{value}", seen)
	case reflect.Struct:
		for i := 0; i < ty.NumField(); i++ {
			walkTypes(t, ty.Field(i).Type, path+"."+ty.Field(i).Name, seen)
		}
	}
}

// Analysis is the only part of the state derived from message content.
func TestAnalysisStateHoldsNoText(t *testing.T) {
	walkTypes(t, reflect.TypeOf(Analysis{}), "Analysis", map[reflect.Type]bool{})
}

// End to end: transcript text, commands, paths and tool output must not reach the aggregator
// state, its JSON snapshot or the rows, while the derived numbers are still produced.
func TestTextFromTranscriptNeverReachesState(t *testing.T) {
	ex := testExtractor(t)
	p := claudecode.NewParser(ex)
	a := New()
	a.SetAnalyzer(ex)

	lines := []string{
		`{"type":"assistant","sessionId":"s","requestId":"r1","timestamp":"2026-01-01T10:00:00Z","message":{"id":"m1","model":"claude-x","usage":{"input_tokens":1,"output_tokens":2},"content":[{"type":"text","text":"SENTINEL-ASSISTANT-TEXT"},{"type":"tool_use","id":"toolu_1","name":"Edit","input":{"file_path":"/home/SENTINEL-DIR/SENTINEL-FILE.go","command":"SENTINEL-COMMAND"}}]}}`,
		`{"type":"user","sessionId":"s","uuid":"u1","timestamp":"2026-01-01T10:00:04Z","message":{"role":"user","content":"SENTINEL-PROMPT fixture broken thing crash"}}`,
		`{"type":"user","sessionId":"s","uuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"SENTINEL-ERROR-OUTPUT"}]},"toolUseResult":{"interrupted":false,"stdout":"SENTINEL-STDOUT"}}`,
	}
	for _, l := range lines {
		a.Add(p.ParseLine([]byte(l)))
	}

	r := a.Rows()[0]
	if r.Quality.AnalyzedExchanges != 1 || r.Topic != "feature" && r.Topic != "bugfix" {
		t.Fatalf("analysis missing: quality=%+v topic=%q", r.Quality, r.Topic)
	}
	if q := r.Quality; q.SigErrorReport != 1 || q.ToolCalls != 1 || q.ToolErrors != 1 {
		t.Errorf("quality = %+v", q)
	}
	for name, out := range map[string]string{
		"snapshot json": mustJSON(t, a.Snapshot()),
		"rows":          fmt.Sprintf("%+v %#v", a.Rows(), a.Rows()),
		"counters":      fmt.Sprintf("%+v", a.Counters()),
	} {
		if strings.Contains(strings.ToLower(out), "sentinel") {
			t.Errorf("%s leaks transcript content: %.200s", name, out)
		}
	}
}
