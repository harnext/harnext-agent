package aggregate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/copilot"
	"github.com/harnext/harnext-agent/internal/parser/copilotchat"
)

var cpT0 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

func span(conv, trace, span, model string, start, end time.Duration, in, out, cache int64) copilot.ChatUsage {
	u := copilot.ChatUsage{
		ConversationID: conv, TraceID: trace, SpanID: span, Model: model,
		Usage:       copilot.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: cache},
		ToolVersion: "0.68.0",
	}
	if start >= 0 {
		u.Start = cpT0.Add(start)
		u.End = cpT0.Add(end)
	}
	return u
}

// cpKey is the usage key of a span starting on cpT0's day (Copilot: unknown speed, no bands).
func cpKey(model string) string {
	return UsageKey{Model: model, Day: "2026-11-01", Speed: "unknown", PromptFloor: -1}.String()
}

func feedCopilot(a *CopilotAggregator, evs ...copilot.Event) {
	for _, e := range evs {
		a.Add(e)
	}
}

func TestCopilotRows(t *testing.T) {
	sec := time.Second
	tests := []struct {
		name string
		evs  []copilot.Event
		want []Row
		// turns is the session's request count (the sum over its folders); rows carry none.
		turns map[string]int64
	}{
		{
			name: "one conversation two models, tokens and times",
			evs: []copilot.Event{
				span("c1", "t1", "s1", "gpt-x", 10*sec, 12*sec, 100, 10, 50),
				span("c1", "t1", "s2", "claude-y", 13*sec, 15*sec, 200, 20, 0),
				span("c1", "t1", "s3", "gpt-x", 16*sec, 20*sec, 1, 2, 3),
			},
			want: []Row{
				{SessionID: "c1", Tool: ToolVSCodeCopilot, Model: "claude-y", Totals: Totals{InputTokens: 200, OutputTokens: 20},
					StartedAt: cpT0.Add(10 * sec), EndedAt: cpT0.Add(20 * sec), ToolVersion: "0.68.0"},
				{SessionID: "c1", Tool: ToolVSCodeCopilot, Model: "gpt-x", Totals: Totals{InputTokens: 101, OutputTokens: 12, CacheReadTokens: 53},
					StartedAt: cpT0.Add(10 * sec), EndedAt: cpT0.Add(20 * sec), ToolVersion: "0.68.0"},
			},
			turns: map[string]int64{"c1": 1},
		},
		{
			name: "two traces count two turns, same trace once",
			evs: []copilot.Event{
				span("c1", "t1", "s1", "m", 0, sec, 1, 1, 0),
				span("c1", "t1", "s2", "m", sec, 2*sec, 1, 1, 0),
				span("c1", "t2", "s3", "m", 3*sec, 4*sec, 1, 1, 0),
			},
			want: []Row{{SessionID: "c1", Tool: ToolVSCodeCopilot, Model: "m", Totals: Totals{InputTokens: 3, OutputTokens: 3},
				StartedAt: cpT0, EndedAt: cpT0.Add(4 * sec), ToolVersion: "0.68.0"}},
			turns: map[string]int64{"c1": 2},
		},
		{
			name: "same trace id in two conversations counts per conversation",
			evs: []copilot.Event{
				span("c1", "t1", "s1", "m", 0, sec, 1, 1, 0),
				span("c2", "t1", "s2", "m", 0, sec, 1, 1, 0),
			},
			want: []Row{
				{SessionID: "c1", Tool: ToolVSCodeCopilot, Model: "m", Totals: Totals{InputTokens: 1, OutputTokens: 1}, StartedAt: cpT0, EndedAt: cpT0.Add(sec), ToolVersion: "0.68.0"},
				{SessionID: "c2", Tool: ToolVSCodeCopilot, Model: "m", Totals: Totals{InputTokens: 1, OutputTokens: 1}, StartedAt: cpT0, EndedAt: cpT0.Add(sec), ToolVersion: "0.68.0"},
			},
			turns: map[string]int64{"c1": 1, "c2": 1},
		},
		{
			name: "duplicate span is not counted twice",
			evs: []copilot.Event{
				span("c1", "t1", "s1", "m", 0, sec, 5, 5, 5),
				span("c1", "t1", "s1", "m", 0, sec, 5, 5, 5),
			},
			want: []Row{{SessionID: "c1", Tool: ToolVSCodeCopilot, Model: "m", Totals: Totals{InputTokens: 5, OutputTokens: 5, CacheReadTokens: 5},
				StartedAt: cpT0, EndedAt: cpT0.Add(sec), ToolVersion: "0.68.0"}},
			turns: map[string]int64{"c1": 1},
		},
		{
			name: "zero start still counts but leaves times unset",
			evs:  []copilot.Event{span("c1", "t1", "s1", "m", -1, 0, 7, 1, 0)},
			want: []Row{{SessionID: "c1", Tool: ToolVSCodeCopilot, Model: "m", Totals: Totals{InputTokens: 7, OutputTokens: 1},
				ToolVersion: "0.68.0"}},
			turns: map[string]int64{"c1": 1},
		},
		{
			name: "zero start keeps earlier valid range; zero end falls back to start",
			evs: []copilot.Event{
				span("c1", "t1", "s1", "m", 5*sec, 6*sec, 1, 1, 0),
				span("c1", "t2", "s2", "m", -1, 0, 1, 1, 0),
				func() copilot.Event {
					e := span("c1", "t3", "s3", "m", 9*sec, 0, 1, 1, 0)
					e.End = time.Time{}
					return e
				}(),
			},
			want: []Row{{SessionID: "c1", Tool: ToolVSCodeCopilot, Model: "m", Totals: Totals{InputTokens: 3, OutputTokens: 3},
				StartedAt: cpT0.Add(5 * sec), EndedAt: cpT0.Add(9 * sec), ToolVersion: "0.68.0"}},
			turns: map[string]int64{"c1": 3},
		},
		{
			name: "missing ids or model are not counted",
			evs: []copilot.Event{
				span("", "t1", "s1", "m", 0, sec, 1, 1, 0),
				span("c1", "", "s1", "m", 0, sec, 1, 1, 0),
				span("c1", "t1", "", "m", 0, sec, 1, 1, 0),
				span("c1", "t1", "s1", "", 0, sec, 1, 1, 0),
			},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCopilot()
			feedCopilot(a, tc.evs...)
			got := a.Rows()
			for i := range got {
				if got[i].CopilotCredits == nil || *got[i].CopilotCredits != 0 {
					t.Fatalf("row %d credits = %v, want 0", i, got[i].CopilotCredits)
				}
				got[i].CopilotCredits = nil
				wantDay := "2026-11-01"
				if got[i].StartedAt.IsZero() {
					wantDay = ""
				}
				if got[i].Day != wantDay || got[i].Speed != "unknown" || got[i].PromptFloor != -1 {
					t.Fatalf("row %d usage key = %q %q %d", i, got[i].Day, got[i].Speed, got[i].PromptFloor)
				}
				got[i].Day, got[i].Speed, got[i].PromptFloor = "", "", 0
			}
			if gotTurns := turnsBySession(got); !reflect.DeepEqual(gotTurns, orEmpty(tc.turns)) {
				t.Fatalf("turns: got %v want %v", gotTurns, tc.turns)
			}
			if !reflect.DeepEqual(tokensOnly(got), tc.want) {
				t.Fatalf("rows:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func nanoSpan(conv, trace, sp, model string, nano int64) copilot.ChatUsage {
	e := span(conv, trace, sp, model, 0, time.Second, 1, 1, 0)
	e.NanoAIU = nano
	return e
}

func creditsByModel(a *CopilotAggregator) map[string]float64 {
	out := map[string]float64{}
	for _, r := range a.Rows() {
		if r.CopilotCredits == nil {
			out[r.Model] = -1
			continue
		}
		out[r.Model] = *r.CopilotCredits
	}
	return out
}

func TestCopilotCredits(t *testing.T) {
	a := NewCopilot()
	feedCopilot(a,
		nanoSpan("c1", "t1", "s1", "m1", 2_010_160_000),
		nanoSpan("c1", "t1", "s2", "m1", 1_000),
		nanoSpan("c1", "t1", "s3", "m2", 500_000_000),
		nanoSpan("c1", "t1", "s4", "free", 0),
		nanoSpan("c1", "t1", "s1", "m1", 2_010_160_000), // replay
	)
	got := creditsByModel(a)
	want := map[string]float64{"m1": 2.010161, "m2": 0.5, "free": 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("credits %v, want %v", got, want)
	}
	one := NewCopilot()
	one.Add(nanoSpan("c1", "t1", "s1", "m", 2_010_160_000))
	if v := creditsByModel(one)["m"]; v != 2.01016 {
		t.Fatalf("single span credits = %v", v)
	}
}

func TestCopilotCreditsSaturate(t *testing.T) {
	a := NewCopilot()
	feedCopilot(a,
		nanoSpan("c1", "t1", "s1", "m", maxNanoAIU-1),
		nanoSpan("c1", "t1", "s2", "m", 5),
		nanoSpan("c1", "t1", "s3", "m", -7),
	)
	if got := a.Snapshot().Sessions["c1"].Models[cpKey("m")].NanoAIU; got != maxNanoAIU {
		t.Fatalf("nano = %d, want saturated %d", got, maxNanoAIU)
	}
	if v := creditsByModel(a)["m"]; v != 1e9 {
		t.Fatalf("credits = %v", v)
	}
}

func TestCopilotCreditsSnapshotRestoreContinues(t *testing.T) {
	first := []copilot.Event{nanoSpan("c1", "t1", "s1", "m", 1_500_000_000)}
	second := []copilot.Event{
		nanoSpan("c1", "t1", "s1", "m", 1_500_000_000), // replay
		nanoSpan("c1", "t2", "s2", "m", 250_000_000),
	}
	a := NewCopilot()
	feedCopilot(a, first...)
	raw, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var st CopilotState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	b := NewCopilot()
	if err := b.Restore(st); err != nil {
		t.Fatal(err)
	}
	feedCopilot(b, second...)
	if v := creditsByModel(b)["m"]; v != 1.75 {
		t.Fatalf("credits = %v, want 1.75", v)
	}
}

// States from before usage keys (versions 1 and 2, keyed by model name) are rejected; the service then
// re-reads the telemetry file so totals are rebuilt per day and prompt floor.
func TestCopilotRejectsPreUsageKeyState(t *testing.T) {
	for _, v := range []int{1, 2} {
		raw := `{"version":` + strconv.Itoa(v) + `,"sessions":{"c1":{"started_at":"2026-11-01T10:00:00Z","ended_at":"2026-11-01T10:00:01Z",` +
			`"tool_version":"0.68.0","models":{"m":{"input_tokens":5,"output_tokens":1,"cache_read_tokens":0,` +
			`"cache_creation_tokens":0,"cache_creation_1h_tokens":0,"turn_count":1}},"traces":[]}},"seen":[],"counters":{}}`
		var st CopilotState
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		if err := NewCopilot().Restore(st); err == nil {
			t.Fatalf("version %d state must not load", v)
		}
	}
	// A current-version state with a bare model name (no usage key) is invalid too.
	raw := `{"version":3,"sessions":{"c1":{"started_at":"2026-11-01T10:00:00Z","ended_at":"2026-11-01T10:00:01Z",` +
		`"tool_version":"0.68.0","models":{"m":{"input_tokens":5,"output_tokens":1,"turn_count":1}},"traces":[]}},"seen":[],"counters":{}}`
	var st CopilotState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if err := NewCopilot().Restore(st); err == nil {
		t.Fatal("bare model key must not load")
	}
}

func TestCopilotCounters(t *testing.T) {
	a := NewCopilot()
	feedCopilot(a,
		span("c1", "t1", "s1", "m", 0, 1, 1, 1, 0),
		span("c1", "t1", "s1", "m", 0, 1, 1, 1, 0),
		span("", "t1", "s9", "m", 0, 1, 1, 1, 0),
		copilot.Ignored{Type: "background_chat"},
		copilot.Ignored{Type: "background_chat"},
		copilot.Skipped{Reason: copilot.ReasonBadID},
	)
	c := a.Counters()
	if c.UniqueSpans != 1 || c.DuplicateSpans != 1 || c.MissingIDs != 1 || c.Ignored["background_chat"] != 2 || c.Skipped["bad_id"] != 1 {
		t.Fatalf("counters %+v", c)
	}
	c.Ignored["x"] = 1
	if a.Counters().Ignored["x"] != 0 {
		t.Fatal("Counters must return a copy")
	}
}

func TestCopilotCounterKindsBounded(t *testing.T) {
	a := NewCopilot()
	for i := 0; i < maxCounterKinds+10; i++ {
		a.Add(copilot.Ignored{Type: fmt.Sprintf("k%d", i)})
	}
	c := a.Counters()
	if len(c.Ignored) != maxCounterKinds+1 || c.Ignored[OtherKind] != 10 {
		t.Fatalf("ignored kinds %d, other %d", len(c.Ignored), c.Ignored[OtherKind])
	}
}

func TestCopilotTraceCap(t *testing.T) {
	a := NewCopilot(WithTraceCap(2))
	for i := 1; i <= 4; i++ {
		a.Add(span("c1", fmt.Sprintf("t%d", i), "s", "m", 0, 1, 1, 1, 0))
		a.Add(span("c1", fmt.Sprintf("t%d", i), "s-b", "m", 0, 1, 1, 1, 0))
	}
	rows := a.Rows()
	if len(rows) != 1 || turnsOf(rows) != 2 || rows[0].InputTokens != 8 {
		t.Fatalf("rows %+v", rows)
	}
	// Each of the two over-cap traces is refused on both of its spans.
	if got := a.Counters().TraceCapReached; got != 4 {
		t.Fatalf("TraceCapReached = %d, want 4", got)
	}
}

func TestCopilotSnapshotRestoreContinues(t *testing.T) {
	sec := time.Second
	first := []copilot.Event{
		span("c1", "t1", "s1", "m1", 0, sec, 10, 1, 0),
		span("c1", "t2", "s2", "m2", 2*sec, 3*sec, 20, 2, 5),
		copilot.Ignored{Type: "log"},
	}
	second := []copilot.Event{
		span("c1", "t2", "s2", "m2", 2*sec, 3*sec, 20, 2, 5), // replay after a crash
		span("c1", "t2", "s4", "m2", 4*sec, 5*sec, 1, 1, 0),  // known trace, no new turn
		span("c1", "t3", "s5", "m1", 6*sec, 7*sec, 1, 1, 0),
	}

	want := NewCopilot()
	feedCopilot(want, first...)
	feedCopilot(want, second...)

	a := NewCopilot()
	feedCopilot(a, first...)
	raw, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var st CopilotState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	b := NewCopilot()
	if err := b.Restore(st); err != nil {
		t.Fatal(err)
	}
	feedCopilot(b, second...)

	if !reflect.DeepEqual(b.Rows(), want.Rows()) {
		t.Fatalf("rows differ:\n got %+v\nwant %+v", b.Rows(), want.Rows())
	}
	if !reflect.DeepEqual(b.Counters(), want.Counters()) {
		t.Fatalf("counters differ: %+v vs %+v", b.Counters(), want.Counters())
	}
	raw2, _ := json.Marshal(b.Snapshot())
	raw3, _ := json.Marshal(want.Snapshot())
	if string(raw2) != string(raw3) {
		t.Fatal("snapshots differ")
	}
}

func TestCopilotSnapshotIsDeep(t *testing.T) {
	a := NewCopilot()
	a.Add(span("c1", "t1", "s1", "m", 0, 1, 1, 1, 0))
	st := a.Snapshot()
	st.Sessions["c1"].Models[cpKey("m")].InputTokens = 999
	st.Sessions["c1"].Traces[0] = 0
	st.Seen[0] = 0
	if a.Rows()[0].InputTokens != 1 {
		t.Fatal("snapshot aliases aggregator state")
	}
	a.Add(span("c1", "t1", "s1", "m", 0, 1, 1, 1, 0))
	if a.Counters().DuplicateSpans != 1 {
		t.Fatal("snapshot mutation leaked into seen set")
	}
}

func TestCopilotRestoreRejectsBadState(t *testing.T) {
	good := func() CopilotState {
		a := NewCopilot()
		a.Add(span("c1", "t1", "s1", "m", 0, 1, 1, 1, 0))
		a.Add(span("c1", "t2", "s2", "m", 0, 1, 1, 1, 0))
		return a.Snapshot()
	}
	tests := []struct {
		name   string
		mutate func(*CopilotState)
	}{
		{"unknown version", func(s *CopilotState) { s.Version = 99 }},
		{"nil session", func(s *CopilotState) { s.Sessions["c1"] = nil }},
		{"empty session id", func(s *CopilotState) { s.Sessions[""] = s.Sessions["c1"] }},
		{"no models", func(s *CopilotState) { s.Sessions["c1"].Models = nil }},
		{"empty model name", func(s *CopilotState) { s.Sessions["c1"].Models["m"] = &CopilotModelState{} }},
		{"nil model", func(s *CopilotState) { s.Sessions["c1"].Models[cpKey("m")] = nil }},
		{"negative tokens", func(s *CopilotState) { s.Sessions["c1"].Models[cpKey("m")].OutputTokens = -1 }},
		{"negative cache creation", func(s *CopilotState) { s.Sessions["c1"].Models[cpKey("m")].CacheCreationTokens = -1 }},
		{"1h cache creation set", func(s *CopilotState) { s.Sessions["c1"].Models[cpKey("m")].CacheCreation1hTokens = 1 }},
		{"negative turns", func(s *CopilotState) { s.Sessions["c1"].Models[cpKey("m")].TurnCount = -1 }},
		{"negative nano aiu", func(s *CopilotState) { s.Sessions["c1"].Models[cpKey("m")].NanoAIU = -1 }},
		{"nano aiu over limit", func(s *CopilotState) { s.Sessions["c1"].Models[cpKey("m")].NanoAIU = maxNanoAIU + 1 }},
		{"end before start", func(s *CopilotState) { s.Sessions["c1"].EndedAt = s.Sessions["c1"].StartedAt.Add(-time.Second) }},
		{"unsorted traces", func(s *CopilotState) { tr := s.Sessions["c1"].Traces; tr[0], tr[1] = tr[1], tr[0] }},
		{"duplicate traces", func(s *CopilotState) { tr := s.Sessions["c1"].Traces; tr[1] = tr[0] }},
		{"unsorted seen", func(s *CopilotState) { s.Seen[0], s.Seen[1] = s.Seen[1], s.Seen[0] }},
		{"negative counter", func(s *CopilotState) { s.Counters.DuplicateSpans = -1 }},
		{"empty counter kind", func(s *CopilotState) { s.Counters.Ignored[""] = 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := good()
			tc.mutate(&st)
			a := NewCopilot()
			a.Add(span("keep", "t1", "s1", "m", 0, 1, 1, 1, 0))
			if err := a.Restore(st); err == nil {
				t.Fatal("expected error")
			}
			if rows := a.Rows(); len(rows) != 1 || rows[0].SessionID != "keep" {
				t.Fatal("failed Restore must keep the current state")
			}
		})
	}
	if err := NewCopilot().Restore(good()); err != nil {
		t.Fatalf("good state rejected: %v", err)
	}
}

func TestCopilotRestoreRejectsOversizedTraceList(t *testing.T) {
	a := NewCopilot(WithTraceCap(1))
	a.Add(span("c1", "t1", "s1", "m", 0, 1, 1, 1, 0))
	st := a.Snapshot()
	st.Sessions["c1"].Traces = []uint64{1, 2}
	if err := a.Restore(st); err == nil {
		t.Fatal("expected error")
	}
}

// CopilotState must hold counters and hashes only; the sole strings are the tool version, the folder path, the per-request model
// name (history rows), the usage keys of trace records, the folders of a request's files (Requests.Dirs.Dir: the FOLDER part of a
// file path, absolute, local state only; product-owner decision of 2026-11-15, never the file name) and the map keys
// (conversation id, model name, counter kinds).
func TestCopilotStateHoldsNoFreeText(t *testing.T) {
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		switch typ.Kind() {
		case reflect.String:
			if path != "CopilotState.Sessions.ToolVersion" && path != "CopilotState.Sessions.Cwd" && path != "CopilotState.Sessions.Requests.Model" &&
				path != "CopilotState.Sessions.Requests.Dirs.Dir" && path != "CopilotState.Sessions.TraceUsage.Entries.Key" {
				t.Errorf("unexpected string field %s", path)
			}
		case reflect.Slice:
			if typ.Elem().Kind() == reflect.Uint8 || typ.Elem().Kind() == reflect.String {
				t.Errorf("unexpected byte/string slice %s", path)
				return
			}
			walk(path, typ.Elem())
		case reflect.Map:
			if typ.Key().Kind() != reflect.String {
				t.Errorf("unexpected map key %s", path)
			}
			walk(path, typ.Elem())
		case reflect.Pointer:
			walk(path, typ.Elem())
		case reflect.Array:
			walk(path, typ.Elem())
		case reflect.Interface:
			t.Errorf("unexpected type %s at %s", typ, path)
		case reflect.Struct:
			if typ == reflect.TypeOf(time.Time{}) {
				return
			}
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				walk(path+"."+f.Name, f.Type)
			}
		}
	}
	walk("CopilotState", reflect.TypeOf(CopilotState{}))
}

func TestCopilotFixtureReplayIsIdempotent(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "testdata", "copilot", "otel-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var evs []copilot.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), copilot.MaxLineBytes)
	for sc.Scan() {
		evs = append(evs, copilot.ParseLine(sc.Bytes()))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	a := NewCopilot()
	feedCopilot(a, evs...)
	once := a.Rows()
	if len(once) == 0 {
		t.Fatal("fixture produced no rows")
	}
	for _, r := range once {
		if r.Tool != ToolVSCodeCopilot || r.Cwd != "" || r.Quality.AnalyzedExchanges != 0 || r.DetectorVersion != 0 || r.SignalCoverage != CopilotCoverage || r.Topic != "" {
			t.Fatalf("unexpected row %+v", r)
		}
	}
	feedCopilot(a, evs...)
	if !reflect.DeepEqual(a.Rows(), once) {
		t.Fatal("replaying the fixture changed rows")
	}
	if c := a.Counters(); c.DuplicateSpans != c.UniqueSpans {
		t.Fatalf("counters %+v", c)
	}
}

func TestCopilotConcurrent(t *testing.T) {
	a := NewCopilot()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				// Every goroutine sends the same spans: each must be counted once.
				a.Add(span("c1", fmt.Sprintf("t%d", i%20), fmt.Sprintf("s%d", i), "m", 0, 1, 1, 1, 0))
				if i%50 == 0 {
					_ = a.Rows()
					_ = a.Counters()
					_ = a.Snapshot()
				}
			}
		}()
	}
	wg.Wait()
	rows := a.Rows()
	if len(rows) != 1 || rows[0].InputTokens != 200 || turnsOf(rows) != 20 {
		t.Fatalf("rows %+v", rows)
	}
	if c := a.Counters(); c.UniqueSpans != 200 || c.DuplicateSpans != 8*200-200 {
		t.Fatalf("counters %+v", c)
	}
}

// ---- chat session events ----

func creq(ex analyzer.Extractor, id string, at int, off time.Duration, text string) copilotchat.RequestStart {
	e := copilotchat.RequestStart{At: at, RequestID: id, Timestamp: cpT0.Add(off)}
	if ex != nil {
		e.Analyzed, e.Features = true, ex.UserFeatures(text)
	}
	return e
}

func cstate(i, state int, off time.Duration) copilotchat.RequestState {
	return copilotchat.RequestState{Index: i, State: state, CompletedAt: cpT0.Add(off)}
}

func ctools(i int, names ...string) copilotchat.RequestTools {
	e := copilotchat.RequestTools{Index: i, ToolCount: len(names)}
	copy(e.Tools[:], names)
	return e
}

func feedChat(a *CopilotAggregator, conv string, evs ...copilotchat.Event) {
	for _, e := range evs {
		a.AddChat(conv, e)
	}
}

func chatAgg(t *testing.T) (*CopilotAggregator, analyzer.Extractor) {
	t.Helper()
	ex := testExtractor(t)
	a := NewCopilot()
	a.SetAnalyzer(ex)
	a.Add(span("c1", "t1", "s1", "gpt-x", 0, time.Second, 10, 1, 0))
	return a, ex
}

func onlyRow(t *testing.T, a *CopilotAggregator) Row {
	t.Helper()
	rows := a.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	return rows[0]
}

const sec = time.Second

func TestCopilotChatPushbackMarksThePreviousRequest(t *testing.T) {
	a, ex := chatAgg(t)
	feedChat(a, "c1",
		creq(ex, "r1", -1, 0, "all good"), cstate(0, 1, 5*sec),
		creq(ex, "r2", -1, 60*sec, "fixture broken thing"), cstate(1, 1, 70*sec),
	)
	q := onlyRow(t, a).Quality
	if q.Exchanges != 2 || q.AnalyzedExchanges != 2 || q.FrictionExchanges != 1 || q.FxPushback != 1 || q.FirstFrictionExchange != 1 || q.SigErrorReport != 1 {
		t.Fatalf("quality %+v", q)
	}
}

func TestCopilotChatCancelledRequest(t *testing.T) {
	a, ex := chatAgg(t)
	feedChat(a, "c1", creq(ex, "r1", -1, 0, "all good"), cstate(0, 2, 5*sec))
	q := onlyRow(t, a).Quality
	if q.Responses != 1 || q.CancelledResponses != 1 || q.FxInterrupt != 1 || q.FirstFrictionExchange != 1 {
		t.Fatalf("quality %+v", q)
	}
}

func ccalls(idx int, calls ...copilotchat.ToolCall) copilotchat.RequestToolCalls {
	e := copilotchat.RequestToolCalls{Index: idx}
	for i, c := range calls {
		e.Calls[i] = c
	}
	e.N = len(calls)
	return e
}

func TestCopilotChatToolCallsDedupAndLoop(t *testing.T) {
	a, ex := chatAgg(t)
	fail := func(id uint64) copilotchat.ToolCall {
		return copilotchat.ToolCall{IDHash: id, ToolID: "copilot_runInTerminal", IsError: true, HasExit: true, ExitCode: 1, ErrorHash: 42}
	}
	edit := copilotchat.ToolCall{IDHash: 9, ToolID: "copilot_replaceString"}
	feedChat(a, "c1",
		creq(ex, "r1", -1, 0, "all good"),
		ccalls(0, edit, fail(1)),
		ccalls(0, edit, fail(1), fail(2)), // VS Code rewrites earlier calls: counted once
		cstate(0, 1, sec),
	)
	q := onlyRow(t, a).Quality
	// RepeatMinTurns is 2: the two identical failures form a tool loop.
	if q.ToolCalls != 3 || q.ToolErrors != 2 || q.TerminalNonzero != 2 || q.RepeatedErrorRuns != 1 || q.FxToolLoop != 1 {
		t.Fatalf("quality %+v", q)
	}
	if c := a.Counters(); c.ChatToolCalls != 3 {
		t.Fatalf("counters %+v", c)
	}
	// The edit call marks the exchange: a pasted error next counts as pushback.
	pasted := creq(ex, "r2", -1, 30*sec, "x")
	pasted.Features.Chars, pasted.Features.ErrorRunes = 100, 90
	feedChat(a, "c1", pasted)
	if q := onlyRow(t, a).Quality; q.SigErrorPaste != 1 || q.FxPushback != 1 {
		t.Fatalf("after paste %+v", q)
	}
}

func TestCopilotChatStateTransitions(t *testing.T) {
	a, ex := chatAgg(t)
	feedChat(a, "c1",
		creq(ex, "r1", -1, 0, "all good"),
		cstate(0, 0, 1*sec), cstate(0, 4, 2*sec), // not terminal
		cstate(0, 1, 3*sec), cstate(0, 2, 4*sec), cstate(0, 1, 5*sec), // counted once
		cstate(7, 1, 6*sec), cstate(-1, 1, 6*sec), // bad index
	)
	st := a.sessions["c1"].analysis.Stats
	if st.Q.Responses != 1 || st.Q.CancelledResponses != 0 {
		t.Fatalf("stats %+v", st)
	}
	if c := a.Counters(); c.ChatBadIndex != 2 || c.ChatRequests != 1 {
		t.Fatalf("counters %+v", c)
	}
}

func TestCopilotChatWithoutAnalyzerThenAnalyzer(t *testing.T) {
	a := NewCopilot()
	a.Add(span("c1", "t1", "s1", "m", 0, 1, 1, 1, 0))
	feedChat(a, "c1", creq(nil, "r1", -1, 0, ""), cstate(0, 1, sec))
	if st := a.sessions["c1"].analysis.Stats; st.UnanalyzedMessages != 1 || st.Q.AnalyzedExchanges != 0 {
		t.Fatalf("stats %+v", st)
	}
	if r := onlyRow(t, a); r.Quality.AnalyzedExchanges != 0 || r.DetectorVersion != 0 || r.Topic != "" {
		t.Fatalf("row without analyzer %+v", r)
	}
	ex := testExtractor(t)
	a.SetAnalyzer(ex)
	if r := onlyRow(t, a); r.Quality.AnalyzedExchanges != 0 {
		t.Fatal("no analyzed message yet")
	}
	feedChat(a, "c1", creq(ex, "r2", -1, 20*sec, "all good"))
	st := a.sessions["c1"].analysis.Stats
	if st.Q.AnalyzedExchanges != 1 || st.UnanalyzedMessages != 1 || st.Q.Exchanges != 2 {
		t.Fatalf("stats %+v", st)
	}
	if r := onlyRow(t, a); r.Quality.FrictionExchanges != 0 || r.DetectorVersion != 1 || r.Topic == "" {
		t.Fatalf("row %+v", r)
	}
}

func TestCopilotChatReplayAndCompactedSnapshot(t *testing.T) {
	a, ex := chatAgg(t)
	first := []copilotchat.Event{
		creq(ex, "A", -1, 0, "all good"), cstate(0, 1, 2*sec),
		creq(ex, "B", -1, 30*sec, "fixture broken thing"), cstate(1, 2, 31*sec),
	}
	feedChat(a, "c1", first...)
	want := a.Snapshot().Sessions["c1"].Analysis.Stats

	feedChat(a, "c1", first...) // replay
	if got := a.sessions["c1"].analysis.Stats; got != want {
		t.Fatalf("replay changed stats: %+v vs %+v", got, want)
	}

	// Compacted rewrite: A and B again (with their states inline), then a new request C.
	ra, rb := creq(ex, "A", -1, 0, "all good"), creq(ex, "B", -1, 30*sec, "fixture broken thing")
	ra.HasState, ra.State, ra.CompletedAt = true, 1, cpT0.Add(2*sec)
	rb.HasState, rb.State, rb.CompletedAt = true, 2, cpT0.Add(31*sec)
	feedChat(a, "c1", ra, rb, creq(ex, "C", -1, 60*sec, "all good"))
	st := a.sessions["c1"].analysis.Stats
	if st.Q.AnalyzedExchanges != 3 || st.Q.Responses != 2 || st.Q.CancelledResponses != 1 || st.Q.SigErrorReport != 1 {
		t.Fatalf("stats %+v", st)
	}
	if c := a.Counters(); c.ChatRequests != 3 || c.ChatDuplicateRequests != 4 {
		t.Fatalf("counters %+v", c)
	}
}

func TestCopilotChatTruncateAt(t *testing.T) {
	a, ex := chatAgg(t)
	feedChat(a, "c1",
		creq(ex, "A", 0, 0, "x"), creq(ex, "B", 1, sec, "x"), creq(ex, "C", 2, 2*sec, "x"),
		creq(ex, "D", 1, 3*sec, "x"), // replaces B and C
	)
	reqs := a.sessions["c1"].requests
	if len(reqs) != 2 || reqs[0].Hash != hashKey('q', "A", "") || reqs[1].Hash != hashKey('q', "D", "") {
		t.Fatalf("requests %+v", reqs)
	}
	feedChat(a, "c1", creq(ex, "A", 0, 0, "x")) // same hash at At: no truncation
	if len(a.sessions["c1"].requests) != 2 {
		t.Fatal("matching At must not truncate")
	}
	if c := a.Counters(); c.ChatRequests != 4 || c.ChatDuplicateRequests != 1 {
		t.Fatalf("counters %+v", c)
	}
}

func TestCopilotChatHiddenAndNoIDRequests(t *testing.T) {
	a, ex := chatAgg(t)
	h := creq(ex, "H", -1, 0, "all good")
	h.Hidden = true
	feedChat(a, "c1", h, creq(ex, "", -1, sec, "all good"))
	if an := a.sessions["c1"].analysis; an != nil && (an.Stats.Q.Exchanges != 0 || an.Stats.UnanalyzedMessages != 0) {
		t.Fatalf("hidden or id-less request counted: %+v", an.Stats)
	}
	if len(a.sessions["c1"].requests) != 2 {
		t.Fatal("indexes must stay aligned")
	}
}

func TestCopilotChatTokenRowsShareAnalysis(t *testing.T) {
	a, ex := chatAgg(t)
	a.Add(span("c1", "t2", "s2", "claude-y", 2*sec, 3*sec, 5, 1, 0))
	a.SetFolder("c1", "/work/project")
	rs := creq(ex, "r1", -1, 0, "all good")
	rs.Tools[0], rs.ToolCount = "Edit", 1
	feedChat(a, "c1", rs, ctools(0, "Edit", "Edit"), cstate(0, 1, sec))
	rows := a.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	for _, r := range rows {
		if r.Cwd != "/work/project" || r.Quality.AnalyzedExchanges != 1 || r.Quality.FrictionExchanges != 0 || r.Topic != "feature" {
			t.Fatalf("row %+v", r)
		}
	}
	if rows[0].Quality != rows[1].Quality {
		t.Fatal("model rows of one session must carry the same quality counters")
	}
	// Tools of one request count once even if reported twice.
	if n := a.sessions["c1"].analysis.Topic.Tool.Count(analyzer.SignalKey("Edit")); n != 1 {
		t.Fatalf("Edit count = %d", n)
	}
}

func TestCopilotChatWithoutTokensProducesNoRows(t *testing.T) {
	a := NewCopilot()
	a.SetAnalyzer(testExtractor(t))
	a.SetFolder("c9", "/w")
	feedChat(a, "c9", creq(testExtractor(t), "r1", -1, 0, "x"), cstate(0, 1, sec))
	if rows := a.Rows(); len(rows) != 0 {
		t.Fatalf("rows %+v", rows)
	}
	// The chat state arrived first; a later token span merges into the same session.
	a.Add(span("c9", "t1", "s1", "m", 0, 1, 1, 1, 0))
	if r := onlyRow(t, a); r.Cwd != "/w" || r.Quality.AnalyzedExchanges != 1 {
		t.Fatalf("row %+v", r)
	}
}

func TestCopilotSetFolderBounds(t *testing.T) {
	a, _ := chatAgg(t)
	a.SetFolder("c1", "/ok")
	a.SetFolder("c1", strings.Repeat("a", maxFolderLen+1))
	a.SetFolder("c1", "")
	if r := onlyRow(t, a); r.Cwd != "/ok" {
		t.Fatalf("cwd %q", r.Cwd)
	}
	a.SetFolder("c1", strings.Repeat("a", maxFolderLen))
	if len(onlyRow(t, a).Cwd) != maxFolderLen {
		t.Fatal("512-char folder must be accepted")
	}
}

// Tool names in a request result are topic signals only; outcomes come from tool invocations.
func TestCopilotChatToolNamesAreNotToolCalls(t *testing.T) {
	a, ex := chatAgg(t)
	feedChat(a, "c1", creq(ex, "r1", -1, 0, "all good"), ctools(0, "Edit", "Edit", "Edit", "Edit"), cstate(0, 1, sec))
	if q := onlyRow(t, a).Quality; q.ToolCalls != 0 || q.FrictionExchanges != 0 {
		t.Fatalf("quality %+v", q)
	}
}

func TestCopilotChatCounters(t *testing.T) {
	a := NewCopilot()
	feedChat(a, "c1",
		copilotchat.Ignored{Type: "x"}, copilotchat.Ignored{Type: "x"},
		copilotchat.Skipped{Reason: copilotchat.ReasonBadShape},
	)
	c := a.Counters()
	if c.ChatIgnored["x"] != 2 || c.ChatSkipped["bad_shape"] != 1 || len(c.Ignored) != 0 {
		t.Fatalf("counters %+v", c)
	}
	for i := 0; i < maxCounterKinds+5; i++ {
		a.AddChat("c1", copilotchat.Ignored{Type: fmt.Sprintf("k%d", i)})
	}
	if c := a.Counters(); c.ChatIgnored[OtherKind] != 6 {
		t.Fatalf("other = %d", c.ChatIgnored[OtherKind])
	}
}

func TestCopilotChatSnapshotRestoreContinues(t *testing.T) {
	events := func(ex analyzer.Extractor) ([]copilotchat.Event, []copilotchat.Event) {
		return []copilotchat.Event{
			creq(ex, "A", -1, 0, "all good"), cstate(0, 1, 2*sec), ctools(0, "Edit"),
		}, []copilotchat.Event{
			creq(ex, "A", -1, 0, "all good"), // replay across the restart
			creq(ex, "B", -1, 5*sec, "fixture broken thing"), cstate(1, 2, 6*sec),
		}
	}
	ex := testExtractor(t)
	first, second := events(ex)

	want, _ := chatAgg(t)
	want.SetFolder("c1", "/w")
	feedChat(want, "c1", first...)
	feedChat(want, "c1", second...)

	a, _ := chatAgg(t)
	a.SetFolder("c1", "/w")
	feedChat(a, "c1", first...)
	raw, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var st CopilotState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	b := NewCopilot()
	b.SetAnalyzer(ex)
	if err := b.Restore(st); err != nil {
		t.Fatal(err)
	}
	feedChat(b, "c1", second...)

	if !reflect.DeepEqual(b.Rows(), want.Rows()) {
		t.Fatalf("rows differ:\n got %+v\nwant %+v", b.Rows(), want.Rows())
	}
	r2, _ := json.Marshal(b.Snapshot())
	r3, _ := json.Marshal(want.Snapshot())
	if string(r2) != string(r3) {
		t.Fatal("snapshots differ")
	}
}

func TestCopilotRestoreRejectsBadChatState(t *testing.T) {
	good := func() CopilotState {
		a, ex := chatAgg(t)
		feedChat(a, "c1", creq(ex, "A", -1, 0, "x"), cstate(0, 1, sec))
		return a.Snapshot()
	}
	tests := []struct {
		name   string
		mutate func(*CopilotState)
	}{
		{"negative stat", func(s *CopilotState) { s.Sessions["c1"].Analysis.Stats.Q.CancelledResponses = -1 }},
		{"broken invariant", func(s *CopilotState) { s.Sessions["c1"].Analysis.Stats.Q.FrictionExchanges = 99 }},
		{"too many requests", func(s *CopilotState) {
			s.Sessions["c1"].Requests = make([]CopilotRequestState, maxChatRequests+1)
		}},
		{"long cwd", func(s *CopilotState) { s.Sessions["c1"].Cwd = strings.Repeat("a", maxFolderLen+1) }},
		{"negative chat counter", func(s *CopilotState) { s.Counters.ChatBadIndex = -1 }},
		{"empty chat counter kind", func(s *CopilotState) { s.Counters.ChatIgnored[""] = 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := good()
			tc.mutate(&st)
			if err := NewCopilot().Restore(st); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if err := NewCopilot().Restore(good()); err != nil {
		t.Fatalf("good state rejected: %v", err)
	}
}

func TestCopilotChatConcurrent(t *testing.T) {
	a, ex := chatAgg(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				a.AddChat("c1", creq(ex, fmt.Sprintf("r%d", i), -1, time.Duration(i)*sec, "all good"))
				a.AddChat("c1", cstate(i, 1, time.Duration(i)*sec))
				a.SetFolder("c1", "/w")
				if i%25 == 0 {
					_ = a.Rows()
					_ = a.Snapshot()
					a.SetAnalyzer(ex)
				}
			}
		}()
	}
	wg.Wait()
	st := a.sessions["c1"].analysis.Stats
	if st.Q.AnalyzedExchanges != 100 || st.Q.Responses != 100 {
		t.Fatalf("stats %+v", st)
	}
}

// chist is a chat request as found in sessions that predate OTel: model, credits and the last-round numbers.
func chist(id string, off time.Duration, model string, credits float64, prompt, comp int64, rounds int, hidden bool) copilotchat.RequestStart {
	e := creq(nil, id, -1, off, "")
	e.Model, e.Hidden = model, hidden
	if credits >= 0 {
		e.HasCredits, e.Credits = true, credits
	}
	if prompt > 0 {
		e.HasPrompt, e.PromptTokens, e.HasCompletion, e.CompletionTokens, e.Rounds = true, prompt, true, comp, rounds
	}
	return e
}

func TestCopilotHistoryRows(t *testing.T) {
	a := NewCopilot()
	day2 := 26 * time.Hour
	feedChat(a, "old",
		chist("r1", 0, "gpt-5.1", 2.5, 1000, 10, 1, false),
		chist("r2", time.Minute, "gpt-5.1", -1, 1000, 10, 3, false),
		copilotchat.RequestMetric{Index: 1, Kind: copilotchat.MetricCredits, Value: 1.25}, // credits arrive by patch
		chist("r3", day2, "claude-sonnet-5-5", 0.5, 0, 0, 0, false),                       // no prompt size: session average
		chist("r4", day2+time.Minute, "gpt-5.1", 9, 500, 5, 1, true),                      // hidden: not a user turn
		chist("r5", day2+2*time.Minute, "", 7, 500, 5, 1, false),                          // no model: skipped
		cstate(0, 1, 30*time.Second), cstate(2, 1, day2+30*time.Second),
	)
	a.SetFolder("old", "/Users/someone/proj")
	rows := a.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d: %+v", len(rows), rows)
	}
	byModel := map[string]Row{}
	for _, r := range rows {
		byModel[r.Model] = r
	}
	g, c := byModel["gpt-5.1"], byModel["claude-sonnet-5-5"]
	if g.Day != "2026-11-01" || *g.CopilotCredits != 3.75 || !g.TokensEstimated {
		t.Errorf("gpt row: %+v credits=%v", g, g.CopilotCredits)
	}
	// r1: single round (read 200, write 800); r2: three rounds (read 1240, write 800); output 10 each.
	if g.CacheReadTokens != 1440 || g.CacheCreationTokens != 1600 || g.OutputTokens != 20 || g.InputTokens != 0 {
		t.Errorf("gpt estimate: %+v", g.Totals)
	}
	// r3 has no prompt size: the session average (1000, 10, rounds 2) stands in.
	if c.Day != "2026-11-02" || *c.CopilotCredits != 0.5 || !c.TokensEstimated || c.CacheCreationTokens != 800 {
		t.Errorf("claude row: %+v", c)
	}
	for _, r := range rows {
		if r.Tool != ToolVSCodeCopilot || r.Speed != copilotSpeed || r.PromptFloor != -1 || r.Cwd != "/Users/someone/proj" {
			t.Errorf("shape: %+v", r)
		}
		if !r.StartedAt.Equal(cpT0) || !r.EndedAt.Equal(cpT0.Add(day2+2*time.Minute)) {
			t.Errorf("range: %v..%v", r.StartedAt, r.EndedAt)
		}
	}
}

func TestCopilotHistoryWithoutAnyPromptSizeHasNoTokens(t *testing.T) {
	a := NewCopilot()
	feedChat(a, "old", chist("r1", 0, "gpt-5.1", 2, 0, 0, 0, false), cstate(0, 1, time.Second))
	rows := a.Rows()
	if len(rows) != 1 || rows[0].TokensEstimated || rows[0].OutputTokens != 0 || *rows[0].CopilotCredits != 2 {
		t.Fatalf("credits and turns stay, tokens are not invented: %+v", rows)
	}
}

func TestCopilotHistoryIsIdempotentAndSurvivesRestore(t *testing.T) {
	feed := func(a *CopilotAggregator) {
		feedChat(a, "old",
			chist("r1", 0, "gpt-5.1", 2.5, 1000, 10, 2, false), cstate(0, 1, 5*sec),
			chist("r2", time.Minute, "gpt-5.1", 1, 2000, 20, 4, false), cstate(1, 1, time.Minute+5*sec),
		)
	}
	a := NewCopilot()
	feed(a)
	feed(a) // a replayed file must not double anything
	want := a.Rows()
	if len(want) != 1 || turnsOf(want) != 2 || *want[0].CopilotCredits != 3.5 {
		t.Fatalf("rows: %+v", want)
	}
	blob, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var back CopilotState
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	b := NewCopilot()
	if err := b.Restore(back); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := b.Rows(); !reflect.DeepEqual(got, want) {
		t.Errorf("after restore:\n got %+v\nwant %+v", got, want)
	}
}

func TestCopilotOTelSessionsKeepOTelRowsOnly(t *testing.T) {
	a := NewCopilot()
	a.Add(span("c1", "t1", "s1", "gpt-x", 0, time.Second, 10, 1, 0))
	feedChat(a, "c1", chist("r1", 0, "gpt-x", 4, 1000, 10, 2, false), chist("r2", time.Minute, "gpt-y", 4, 1000, 10, 2, false))
	rows := a.Rows()
	if len(rows) != 1 || rows[0].Model != "gpt-x" || rows[0].InputTokens != 10 || rows[0].TokensEstimated {
		t.Fatalf("OTel rows must stay as they are: %+v", rows)
	}
}

func TestCopilotOTelCacheCreationReachesTheRow(t *testing.T) {
	a := NewCopilot()
	u := span("c1", "t1", "s1", "m", 0, time.Second, 4, 70, 14223)
	u.Usage.CacheCreationTokens = 42327
	a.Add(u)
	r := onlyRow(t, a)
	if r.InputTokens != 4 || r.CacheReadTokens != 14223 || r.CacheCreationTokens != 42327 {
		t.Fatalf("totals: %+v", r.Totals)
	}
}

func TestCopilotHistoryNeedsATimeRange(t *testing.T) {
	a := NewCopilot()
	e := chist("r1", 0, "gpt-5.1", 1, 1000, 10, 1, false)
	e.Timestamp = time.Time{}
	feedChat(a, "old", e)
	if rows := a.Rows(); len(rows) != 0 {
		t.Fatalf("a session without any timestamp cannot be uploaded: %+v", rows)
	}
}
