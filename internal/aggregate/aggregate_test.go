package aggregate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/harnext/harnext-agent/internal/analyzer"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/parser/claudecode"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

func fixtureLines(t *testing.T, name string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude-code", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
}

func feed(a *Aggregator, lines [][]byte) {
	for _, l := range lines {
		a.Add(claudecode.ParseLine(l))
	}
}

func at(s string) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return ts
}

func TestFixtureBasic(t *testing.T) {
	a := New()
	feed(a, fixtureLines(t, "session-basic.jsonl"))

	want := []Row{{
		SessionID: "00000000-0000-4000-8000-000000000001", Tool: ToolClaudeCode, Model: "claude-sonnet-5",
		Day: "2026-01-01", Speed: claudecode.SpeedUnknown, PromptFloor: -1,
		Totals:    Totals{InputTokens: 15, OutputTokens: 300, CacheReadTokens: 11000, CacheCreationTokens: 1000},
		StartedAt: at("2026-01-01T10:00:01Z"), EndedAt: at("2026-01-01T10:00:10Z"),
		Cwd: "/home/dev/example-project", GitBranch: "main", ToolVersion: "2.1.237",
	}}
	if got := tokensOnly(a.Rows()); !reflect.DeepEqual(got, want) {
		t.Errorf("Rows() =\n%+v\nwant\n%+v", got, want)
	}
	if n := turnsOf(a.Rows()); n != 1 {
		t.Errorf("turns = %d, want 1", n)
	}

	c := a.Counters()
	checks := map[string][2]int64{
		"UniqueResponses":    {c.UniqueResponses, 2},
		"DuplicateResponses": {c.DuplicateResponses, 1},
		"UserTurns":          {c.UserTurns, 1},
		"ToolResults":        {c.ToolResults, 1},
		"Skipped.invalid":    {c.Skipped["invalid_json"], 1},
		"Ignored.future":     {c.Ignored["future-event-type"], 1},
		"Ignored.queue":      {c.Ignored["queue-operation"], 1},
		"Ignored.ai-title":   {c.Ignored["ai-title"], 1},
	}
	for name, v := range checks {
		if v[0] != v[1] {
			t.Errorf("%s = %d, want %d", name, v[0], v[1])
		}
	}
}

func TestFixtureSubagent(t *testing.T) {
	a := New()
	feed(a, fixtureLines(t, "session-subagent.jsonl"))

	const sid = "00000000-0000-4000-8000-000000000002"
	meta := Row{
		SessionID: sid, Tool: ToolClaudeCode, Day: "2026-01-02", Speed: claudecode.SpeedUnknown, PromptFloor: -1,
		StartedAt: at("2026-01-02T09:00:00Z"), EndedAt: at("2026-01-02T09:00:20Z"),
		Cwd: "/home/dev/example-project", GitBranch: "feature/example", ToolVersion: "2.1.246",
	}
	haiku, sonnet := meta, meta
	haiku.Model, haiku.Totals = "claude-haiku-5", Totals{InputTokens: 3, OutputTokens: 50, CacheReadTokens: 400}
	sonnet.Model, sonnet.Totals = "claude-sonnet-5",
		Totals{InputTokens: 25, OutputTokens: 140, CacheReadTokens: 2500, CacheCreationTokens: 500, CacheCreation1hTokens: 200}
	if got, want := tokensOnly(a.Rows()), []Row{haiku, sonnet}; !reflect.DeepEqual(got, want) {
		t.Errorf("Rows() =\n%+v\nwant\n%+v", got, want)
	}
	if n := turnsOf(a.Rows()); n != 1 {
		t.Errorf("turns = %d, want 1", n)
	}

	c := a.Counters()
	checks := map[string][2]int64{
		"UniqueResponses":    {c.UniqueResponses, 3},
		"DuplicateResponses": {c.DuplicateResponses, 1},
		"SidechainResponses": {c.SidechainResponses, 1},
		"SidechainUserTurns": {c.SidechainUserTurns, 1},
		"UserTurns":          {c.UserTurns, 1},
		"ToolResults":        {c.ToolResults, 2},
		"SyntheticResponses": {c.SyntheticResponses, 1},
		"MissingIDs":         {c.MissingIDs, 1},
		"Skipped.range":      {c.Skipped["out_of_range"], 1},
	}
	for name, v := range checks {
		if v[0] != v[1] {
			t.Errorf("%s = %d, want %d", name, v[0], v[1])
		}
	}
}

func TestFixtureUnknownVersion(t *testing.T) {
	a := New()
	feed(a, fixtureLines(t, "session-unknown-version.jsonl"))

	rows := a.Rows()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if want := (Totals{InputTokens: 8, OutputTokens: 80, CacheReadTokens: 800, CacheCreationTokens: 80}); r.Totals != want {
		t.Errorf("totals = %+v, want %+v", r.Totals, want)
	}
	if turnsOf(rows) != 1 || r.ToolVersion != "9.9.9" {
		t.Errorf("turns = %d, version = %q", turnsOf(rows), r.ToolVersion)
	}
	c := a.Counters()
	if c.UniqueResponses != 2 || c.UserTurns != 1 || c.Ignored["hologram-event"] != 1 ||
		c.Skipped["bad_tokens"] != 1 || c.Skipped["missing_usage"] != 1 {
		t.Errorf("counters = %+v", c)
	}
}

// Replaying lines after a restore, or after a snapshot taken mid-stream, must not change totals.
func TestSnapshotRestoreDoesNotDoubleCount(t *testing.T) {
	for _, name := range []string{"session-basic.jsonl", "session-subagent.jsonl", "session-unknown-version.jsonl"} {
		lines := fixtureLines(t, name)

		ref := New()
		feed(ref, lines)
		wantRows, wantTotals := ref.Rows(), ref.Counters().UniqueResponses

		for split := 0; split <= len(lines); split++ {
			first := New()
			feed(first, lines[:split])

			raw, err := json.Marshal(first.Snapshot())
			if err != nil {
				t.Fatal(err)
			}
			var st State
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatal(err)
			}
			second := New()
			if err := second.Restore(st); err != nil {
				t.Fatal(err)
			}

			// Replay from line 0 (offset lost) over the restored state, then the rest.
			feed(second, lines[:split])
			feed(second, lines[split:])

			if got := second.Rows(); !reflect.DeepEqual(got, wantRows) {
				t.Fatalf("%s split %d: rows =\n%+v\nwant\n%+v", name, split, got, wantRows)
			}
			if got := second.Counters().UniqueResponses; got != wantTotals {
				t.Fatalf("%s split %d: unique = %d, want %d", name, split, got, wantTotals)
			}
		}
	}
}

func TestSnapshotIsDeepAndDeterministic(t *testing.T) {
	a := New()
	feed(a, fixtureLines(t, "session-subagent.jsonl"))
	s1, s2 := a.Snapshot(), a.Snapshot()
	b1, _ := json.Marshal(s1)
	b2, _ := json.Marshal(s2)
	if !bytes.Equal(b1, b2) {
		t.Error("snapshots differ")
	}
	for _, s := range s1.Sessions {
		s.Open.Turns = 99
		for _, f := range s.Folders {
			f.Turns = 99
			for _, m := range f.Usage {
				m.OutputTokens = 99
			}
		}
	}
	s1.Counters.Skipped["x"] = 1
	if rows := a.Rows(); turnsOf(rows) >= 99 || rows[0].OutputTokens == 99 || a.Counters().Skipped["x"] != 0 {
		t.Error("snapshot shares memory with aggregator")
	}
	for i := 1; i < len(s1.Seen); i++ {
		if s1.Seen[i-1] >= s1.Seen[i] {
			t.Fatal("Seen not sorted and unique")
		}
	}
}

func TestRestoreRejectsUnknownVersion(t *testing.T) {
	if err := New().Restore(State{Version: 99}); err == nil {
		t.Error("expected error")
	}
}

func usage(session, model, msg, req string, out int64, opts ...func(*claudecode.AssistantUsage)) claudecode.AssistantUsage {
	e := claudecode.AssistantUsage{
		SessionID: session, Model: model, MessageID: msg, RequestID: req,
		Usage: claudecode.Usage{OutputTokens: out},
	}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func user(session, uuid string) claudecode.UserTurn {
	return claudecode.UserTurn{SessionID: session, UUID: uuid}
}

func TestAggregation(t *testing.T) {
	ts := func(s string) func(*claudecode.AssistantUsage) {
		return func(e *claudecode.AssistantUsage) { e.Timestamp = at(s) }
	}
	tests := []struct {
		name   string
		events []claudecode.Event
		check  func(t *testing.T, a *Aggregator)
	}{
		{
			name: "missing message id is not counted",
			events: []claudecode.Event{
				usage("s", "m", "", "r", 10), usage("s", "m", "x", "", 10), usage("s", "m", "x", "r", 5),
			},
			check: func(t *testing.T, a *Aggregator) {
				if got := a.Rows()[0].OutputTokens; got != 5 {
					t.Errorf("output = %d, want 5", got)
				}
				if got := a.Counters().MissingIDs; got != 2 {
					t.Errorf("MissingIDs = %d, want 2", got)
				}
			},
		},
		{
			name: "synthetic and missing models are not counted",
			events: []claudecode.Event{
				usage("s", "<synthetic>", "a", "a", 10), usage("s", "", "b", "b", 10), usage("s", "m", "c", "c", 1),
			},
			check: func(t *testing.T, a *Aggregator) {
				rows := a.Rows()
				if len(rows) != 1 || rows[0].Model != "m" || rows[0].OutputTokens != 1 {
					t.Errorf("rows = %+v", rows)
				}
				c := a.Counters()
				if c.SyntheticResponses != 1 || c.MissingModel != 1 {
					t.Errorf("counters = %+v", c)
				}
			},
		},
		{
			name: "same pair from another session is a duplicate",
			events: []claudecode.Event{
				usage("s1", "m", "x", "r", 10), usage("s2", "m", "x", "r", 10),
			},
			check: func(t *testing.T, a *Aggregator) {
				rows := a.Rows()
				if len(rows) != 1 || rows[0].SessionID != "s1" || rows[0].OutputTokens != 10 {
					t.Errorf("rows = %+v", rows)
				}
			},
		},
		{
			name: "different request id with same message id counts twice",
			events: []claudecode.Event{
				usage("s", "m", "x", "r1", 10), usage("s", "m", "x", "r2", 10),
			},
			check: func(t *testing.T, a *Aggregator) {
				if got := a.Rows()[0].OutputTokens; got != 20 {
					t.Errorf("output = %d, want 20", got)
				}
			},
		},
		{
			name: "skipped and ignored events are counted by kind",
			events: []claudecode.Event{
				claudecode.Skipped{Reason: claudecode.ReasonInvalidJSON}, claudecode.Skipped{Reason: claudecode.ReasonInvalidJSON},
				claudecode.Ignored{Type: "attachment"},
			},
			check: func(t *testing.T, a *Aggregator) {
				c := a.Counters()
				if c.Skipped["invalid_json"] != 2 || c.Ignored["attachment"] != 1 || len(a.Rows()) != 0 {
					t.Errorf("counters = %+v", c)
				}
			},
		},
		{
			name: "ignored kinds are capped",
			events: func() []claudecode.Event {
				var evs []claudecode.Event
				for i := 0; i < maxCounterKinds+10; i++ {
					evs = append(evs, claudecode.Ignored{Type: fmt.Sprintf("t%d", i)})
				}
				return evs
			}(),
			check: func(t *testing.T, a *Aggregator) {
				c := a.Counters()
				if len(c.Ignored) != maxCounterKinds+1 || c.Ignored[OtherKind] != 10 {
					t.Errorf("kinds = %d, other = %d", len(c.Ignored), c.Ignored[OtherKind])
				}
			},
		},
		{
			name: "turns are counted per session and folder, not per model",
			events: []claudecode.Event{
				user("s", "u1"), usage("s", "a", "m1", "r1", 1),
				user("s", "u2"), user("s", "u3"), usage("s", "b", "m2", "r2", 1),
				user("s", "u4"),
			},
			check: func(t *testing.T, a *Aggregator) {
				rows := a.Rows()
				if len(rows) != 2 || rows[0].Model != "a" || rows[1].Model != "b" || turnsOf(rows) != 4 {
					t.Errorf("rows = %+v", rows)
				}
			},
		},
		{
			name: "replayed user uuid counts once",
			events: []claudecode.Event{
				user("s", "u1"), user("s", "u1"), usage("s", "a", "m1", "r1", 1),
			},
			check: func(t *testing.T, a *Aggregator) {
				if got := turnsOf(a.Rows()); got != 1 {
					t.Errorf("turns = %d, want 1", got)
				}
				if got := a.Counters().DuplicateUserTurns; got != 1 {
					t.Errorf("DuplicateUserTurns = %d, want 1", got)
				}
			},
		},
		{
			name: "user turns without uuid are all counted",
			events: []claudecode.Event{
				user("s", ""), user("s", ""), usage("s", "a", "m1", "r1", 1),
			},
			check: func(t *testing.T, a *Aggregator) {
				if got := turnsOf(a.Rows()); got != 2 {
					t.Errorf("turns = %d, want 2", got)
				}
			},
		},
		{
			name: "tool results and sidechain prompts are not turns",
			events: []claudecode.Event{
				claudecode.UserTurn{SessionID: "s", IsToolResult: true, IsError: true},
				claudecode.UserTurn{SessionID: "s", IsSidechain: true, UUID: "x"},
				usage("s", "a", "m1", "r1", 1),
			},
			check: func(t *testing.T, a *Aggregator) {
				if got := turnsOf(a.Rows()); got != 0 {
					t.Errorf("turns = %d, want 0", got)
				}
			},
		},
		{
			name: "time bounds and latest metadata are order independent",
			events: []claudecode.Event{
				usage("s", "m", "x2", "r", 1, ts("2026-01-01T12:00:00Z"), func(e *claudecode.AssistantUsage) { e.Cwd, e.GitBranch, e.ToolVersion = "/late", "late", "2" }),
				usage("s", "m", "x1", "r", 1, ts("2026-01-01T10:00:00Z"), func(e *claudecode.AssistantUsage) { e.Cwd, e.GitBranch, e.ToolVersion = "/early", "early", "1" }),
				usage("s", "m", "x3", "r", 1, ts("2026-01-01T11:00:00Z")),
				usage("s", "m", "x4", "r", 1),
			},
			check: func(t *testing.T, a *Aggregator) {
				r := a.Rows()[0]
				if !r.StartedAt.Equal(at("2026-01-01T10:00:00Z")) || !r.EndedAt.Equal(at("2026-01-01T12:00:00Z")) ||
					r.Cwd != "/late" || r.GitBranch != "late" || r.ToolVersion != "2" {
					t.Errorf("row = %+v", r)
				}
			},
		},
		{
			name: "metadata without timestamps only fills gaps",
			events: []claudecode.Event{
				usage("s", "m", "x1", "r", 1, func(e *claudecode.AssistantUsage) { e.Cwd = "/first" }),
				usage("s", "m", "x2", "r", 1, func(e *claudecode.AssistantUsage) { e.Cwd, e.GitBranch = "/second", "b" }),
			},
			check: func(t *testing.T, a *Aggregator) {
				if r := a.Rows()[0]; r.Cwd != "/first" || r.GitBranch != "b" || !r.StartedAt.IsZero() {
					t.Errorf("row = %+v", r)
				}
			},
		},
		{
			name: "rows are sorted and carry all token kinds",
			events: []claudecode.Event{
				claudecode.AssistantUsage{SessionID: "b", Model: "m", MessageID: "1", RequestID: "1", Usage: claudecode.Usage{
					InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4, CacheCreation1hTokens: 2,
				}},
				usage("a", "z", "2", "2", 1), usage("a", "y", "3", "3", 1),
			},
			check: func(t *testing.T, a *Aggregator) {
				rows := a.Rows()
				order := fmt.Sprintf("%s/%s %s/%s %s/%s", rows[0].SessionID, rows[0].Model, rows[1].SessionID, rows[1].Model, rows[2].SessionID, rows[2].Model)
				if order != "a/y a/z b/m" {
					t.Errorf("order = %s", order)
				}
				if want := (Totals{1, 2, 3, 4, 2}); rows[2].Totals != want {
					t.Errorf("totals = %+v", rows[2].Totals)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New()
			for _, e := range tt.events {
				a.Add(e)
			}
			tt.check(t, a)
		})
	}
}

// tokensOnly clears the session quality fields so fixture tests can compare token rows; the
// quality counters have their own tests (analysis_test.go, copilot_test.go).
func tokensOnly(rows []Row) []Row {
	for i := range rows {
		rows[i].Quality, rows[i].SignalCoverage, rows[i].DetectorVersion = analyzer.Quality{}, 0, 0
		rows[i].Parts, rows[i].MainFolder = nil, ""
		rows[i].ActiveSeconds = 0 // covered by active_test.go
		rows[i].FirstAt = time.Time{}
	}
	return rows
}

// turnsOf is the request count of the session of rows[0]: the sum over its folders (rows carry no per-row turns).
func turnsOf(rows []Row) int64 {
	if len(rows) == 0 {
		return 0
	}
	var n int64
	for _, p := range rows[0].Parts {
		n += p.Turns
	}
	return n
}

// turnsBySession sums the folder turns of every session in rows.
func turnsBySession(rows []Row) map[string]int64 {
	out := map[string]int64{}
	done := map[string]bool{}
	for _, r := range rows {
		if done[r.SessionID] {
			continue
		}
		done[r.SessionID] = true
		for _, p := range r.Parts {
			out[r.SessionID] += p.Turns
		}
	}
	return out
}

func orEmpty(m map[string]int64) map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return m
}

func bandExtractor(t *testing.T, bands ...int64) analyzer.Extractor {
	t.Helper()
	ex, err := analyzer.Compile(&remoteconfig.Config{
		Friction: remoteconfig.Friction{DetectorVersion: 1, RepeatMinTurns: 2, RepeatJaccard: 0.8, ErrorPasteMinShare: 0.6,
			Patterns: []remoteconfig.FeedbackPattern{{Category: "error_report", Lang: "en", Pattern: "fixture broken thing"}}},
		Topics: remoteconfig.Topics{Labels: []string{"feature", "other"}, MinScore: 1, Fallback: "other"},
		Usage:  remoteconfig.Usage{ContextBands: bands},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

// Usage keys: responses split by UTC day, speed and prompt floor; web tool requests are summed;
// a response without a timestamp falls back to the session's start day and merges with it.
func TestUsageKeys(t *testing.T) {
	day1 := time.Date(2026, 1, 1, 23, 50, 0, 0, time.UTC)
	day2 := day1.Add(20 * time.Minute)
	opts := func(ts time.Time, speed string, in, cacheRead, web int64) func(*claudecode.AssistantUsage) {
		return func(e *claudecode.AssistantUsage) {
			e.Timestamp = ts
			e.Usage.Speed, e.Usage.InputTokens, e.Usage.CacheReadTokens, e.Usage.WebSearchRequests = speed, in, cacheRead, web
		}
	}
	a := New()
	a.SetAnalyzer(bandExtractor(t, 100_000, 200_000))
	a.Add(user("s", "u1"))
	a.Add(usage("s", "m", "r1", "q1", 10, opts(day1, claudecode.SpeedStandard, 50, 1000, 1)))
	a.Add(usage("s", "m", "r2", "q2", 20, opts(day1, claudecode.SpeedStandard, 10, 150_000, 2)))
	a.Add(usage("s", "m", "r3", "q3", 30, opts(day2, claudecode.SpeedFast, 10, 250_000, 0)))
	a.Add(usage("s", "m", "r4", "q4", 40, opts(time.Time{}, claudecode.SpeedStandard, 5, 0, 0)))
	a.Add(usage("s", "m", "r4", "q4", 40, opts(time.Time{}, claudecode.SpeedStandard, 5, 0, 0))) // replay

	type key struct {
		day, speed string
		floor      int64
		out, web   int64
	}
	var got []key
	turns := int64(0)
	for _, r := range a.Rows() {
		got = append(got, key{r.Day, r.Speed, r.PromptFloor, r.OutputTokens, r.WebSearchRequests})
	}
	turns = turnsOf(a.Rows())
	want := []key{
		{"2026-01-01", "standard", 0, 50, 1},
		{"2026-01-01", "standard", 100_000, 20, 2},
		{"2026-01-02", "fast", 200_000, 30, 0},
	}
	if !reflect.DeepEqual(got, want) || turns != 1 {
		t.Fatalf("rows = %+v (turns %d), want %+v", got, turns, want)
	}

	// Without a configuration the band is unknown (-1).
	b := New()
	b.Add(usage("s", "m", "r1", "q1", 1, opts(day1, claudecode.SpeedStandard, 500_000, 0, 0)))
	if r := b.Rows()[0]; r.PromptFloor != -1 {
		t.Fatalf("floor without config = %d", r.PromptFloor)
	}
}

func TestUsageKeyRoundTrip(t *testing.T) {
	k := UsageKey{Model: "claude-x", Day: "2026-01-01", Speed: "fast", PromptFloor: 200_000}
	if got, ok := ParseUsageKey(k.String()); !ok || got != k {
		t.Fatalf("round trip = %+v %v", got, ok)
	}
	for _, bad := range []string{"m", "", UsageKey{Model: "", Day: "", Speed: "fast"}.String(),
		UsageKey{Model: "m", Day: "2026-13-01", Speed: "fast"}.String(), UsageKey{Model: "m", Speed: "turbo"}.String(),
		UsageKey{Model: "m", Speed: "fast", PromptFloor: -2}.String()} {
		if _, ok := ParseUsageKey(bad); ok {
			t.Errorf("ParseUsageKey(%q) accepted", bad)
		}
	}
}
