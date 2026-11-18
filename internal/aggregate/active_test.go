package aggregate

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/parser/claudecode"
	"github.com/harnext/harnext-agent/internal/parser/copilotchat"
)

// wordBase is a multiple of 64 minutes since the epoch (2026), so a case can straddle a bit-word boundary.
const wordBase int64 = 466_146 * 64

// tm is the time at minute number m (since the epoch) plus sec seconds.
func tm(m int64, sec int64) time.Time { return time.Unix(m*60+sec, 0).UTC() }

func setOf(times ...time.Time) *MinuteSet {
	s := &MinuteSet{}
	for _, t := range times {
		s.Add(t)
	}
	return s
}

func TestMinuteSetSeconds(t *testing.T) {
	midnight := time.Date(2026, 1, 1, 23, 58, 0, 0, time.UTC)
	tests := []struct {
		name  string
		times []time.Time
		want  int64
	}{
		{"empty set", nil, 0},
		{"zero time adds nothing", []time.Time{{}}, 0},
		{"single event", []time.Time{tm(wordBase, 0)}, 60},
		{"many events in one minute", []time.Time{tm(wordBase, 0), tm(wordBase, 7), tm(wordBase, 59)}, 60},
		{"gap of 1 minute", []time.Time{tm(wordBase, 59), tm(wordBase+1, 0)}, 120},
		{"gap of 3 minutes", []time.Time{tm(wordBase, 0), tm(wordBase+3, 30)}, 60 + 180},
		{"gap of exactly 5 minutes", []time.Time{tm(wordBase, 0), tm(wordBase+5, 0)}, 60 + 300},
		{"gap of 6 minutes", []time.Time{tm(wordBase, 0), tm(wordBase+6, 0)}, 60 + 60},
		{"three points mixed", []time.Time{tm(wordBase, 0), tm(wordBase+2, 0), tm(wordBase+10, 0)}, 60 + 120 + 60},
		{"six points mixed", []time.Time{tm(wordBase, 0), tm(wordBase+1, 0), tm(wordBase+7, 0), tm(wordBase+8, 0), tm(wordBase+20, 0), tm(wordBase+24, 0)}, 540},
		{"bit word boundary", []time.Time{tm(wordBase-1, 0), tm(wordBase, 0)}, 120},
		{"across midnight", []time.Time{midnight, midnight.Add(3 * time.Minute)}, 60 + 180},
		{"across midnight with a long gap", []time.Time{midnight, midnight.Add(10 * time.Minute)}, 60 + 60},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := setOf(tc.times...).Seconds(); got != tc.want {
				t.Errorf("Seconds() = %d, want %d", got, tc.want)
			}
		})
	}
	var nilSet *MinuteSet
	if nilSet.Seconds() != 0 {
		t.Error("nil set must be 0")
	}
}

func TestMinuteSetOrderAndRepeatsDoNotMatter(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var times []time.Time
	for i := 0; i < 300; i++ {
		times = append(times, tm(wordBase-100+int64(rng.Intn(400)), int64(rng.Intn(60))))
	}
	want := setOf(times...)
	for round := 0; round < 20; round++ {
		rng.Shuffle(len(times), func(i, j int) { times[i], times[j] = times[j], times[i] })
		got := setOf(times...)
		for _, t2 := range times[:50] { // the same events again
			got.Add(t2)
		}
		if got.Seconds() != want.Seconds() || !reflect.DeepEqual(got.Words, want.Words) {
			t.Fatalf("round %d: %d s, want %d s", round, got.Seconds(), want.Seconds())
		}
	}
}

func TestMinuteSetCap(t *testing.T) {
	s := &MinuteSet{}
	for i := int64(0); i < maxActiveMinutes; i++ {
		if !s.Add(tm(wordBase+i, 0)) {
			t.Fatalf("minute %d refused below the cap", i)
		}
	}
	if s.Add(tm(wordBase+maxActiveMinutes, 0)) || s.Add(tm(wordBase-1, 0)) {
		t.Error("a new minute above the cap was accepted")
	}
	if !s.Add(tm(wordBase+5, 30)) {
		t.Error("an existing minute must stay accepted at the cap")
	}
	if got := s.Seconds(); got != maxActiveMinutes*60 {
		t.Errorf("Seconds() = %d, want %d", got, maxActiveMinutes*60)
	}
}

func TestMinuteSetJSONRoundTrip(t *testing.T) {
	s := setOf(tm(wordBase-1, 0), tm(wordBase, 0), tm(wordBase+3, 0), tm(wordBase+500, 0))
	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back MinuteSet
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.check(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Words, s.Words) || back.Seconds() != s.Seconds() || back.n != s.n {
		t.Fatalf("round trip changed the set: %s", blob)
	}
	// A restored set keeps counting toward the cap.
	back.Add(tm(wordBase+501, 0))
	if back.n != 5 {
		t.Errorf("n = %d, want 5", back.n)
	}
	if blob, _ := json.Marshal(&MinuteSet{}); string(blob) != "{}" {
		t.Errorf("empty set = %s", blob)
	}
}

func TestMinuteSetCheckRejectsBadSets(t *testing.T) {
	over := &MinuteSet{}
	for w := int64(0); w < maxActiveMinutes/64+2; w++ {
		over.Words = append(over.Words, MinuteWord{W: w, B: ^uint64(0)})
	}
	bad := map[string]*MinuteSet{
		"empty word":   {Words: []MinuteWord{{W: 1, B: 0}}},
		"unsorted":     {Words: []MinuteWord{{W: 2, B: 1}, {W: 1, B: 1}}},
		"duplicate":    {Words: []MinuteWord{{W: 1, B: 1}, {W: 1, B: 2}}},
		"over the cap": over,
	}
	for name, s := range bad {
		if err := s.check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

const (
	cUser   = `{"type":"user","sessionId":"s1","uuid":"u1","timestamp":"2026-01-01T10:00:00Z","message":{"role":"user","content":"hello"}}`
	cAssist = `{"type":"assistant","sessionId":"s1","requestId":"r1","uuid":"a1","timestamp":"2026-01-01T10:02:10Z","message":{"id":"m1","model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}}`
	cTool   = `{"type":"user","sessionId":"s1","uuid":"u2","timestamp":"2026-01-01T10:04:59Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	cSide   = `{"type":"assistant","isSidechain":true,"sessionId":"s1","requestId":"r2","uuid":"a2","timestamp":"2026-01-01T10:10:00Z","message":{"id":"m2","model":"claude-y","usage":{"input_tokens":1,"output_tokens":1}}}`
	cUser2  = `{"type":"user","sessionId":"s1","uuid":"u3","timestamp":"2026-01-01T10:12:30Z","message":{"role":"user","content":"again"}}`
	// Not events of a session: an ignored type, and an assistant line whose time cannot be read.
	cIgnored = `{"type":"queue-operation","sessionId":"s1","timestamp":"2026-01-01T11:30:00Z"}`
	cBadTime = `{"type":"assistant","sessionId":"s1","requestId":"r3","uuid":"a3","timestamp":"yesterday","message":{"id":"m3","model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}}`
	cLate    = `{"type":"assistant","sessionId":"s1","requestId":"r4","uuid":"a4","timestamp":"2026-01-01T09:58:00Z","message":{"id":"m4","model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}}`
)

func claudeLines(lines ...string) [][]byte {
	out := make([][]byte, len(lines))
	for i, l := range lines {
		out[i] = []byte(l)
	}
	return out
}

func activeOf(t *testing.T, rows []Row) int64 {
	t.Helper()
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		if r.ActiveSeconds != rows[0].ActiveSeconds {
			t.Fatalf("active seconds differ between the rows of a session: %d vs %d", r.ActiveSeconds, rows[0].ActiveSeconds)
		}
	}
	return rows[0].ActiveSeconds
}

// Points 10:00, 10:02, 10:04, 10:10 (sidechain), 10:12: 60 + 120 + 120 + 60 (gap 6) + 120.
func TestClaudeActiveSeconds(t *testing.T) {
	a := New()
	feed(a, claudeLines(cUser, cAssist, cTool, cSide, cUser2, cIgnored, cBadTime))
	rows := a.Rows()
	if len(rows) < 2 {
		t.Fatalf("rows = %d, want a row per model", len(rows))
	}
	if got := activeOf(t, rows); got != 480 {
		t.Errorf("ActiveSeconds = %d, want 480", got)
	}

	// The same lines again (offset lost): unchanged. A late event with an older time updates the result.
	feed(a, claudeLines(cUser, cAssist, cTool, cSide, cUser2, cIgnored, cBadTime))
	if got := activeOf(t, a.Rows()); got != 480 {
		t.Errorf("after replay ActiveSeconds = %d, want 480", got)
	}
	feed(a, claudeLines(cLate))
	// 09:58 60, 10:00 +120, 10:02 +120, 10:04 +120, 10:10 +60, 10:12 +120.
	if got := activeOf(t, a.Rows()); got != 600 {
		t.Errorf("after a late event ActiveSeconds = %d, want 600", got)
	}
}

func TestClaudeActiveFromFixtures(t *testing.T) {
	// Every parsed event of these fixtures lies in one UTC minute (the ignored lines do not count).
	for _, name := range []string{"session-basic.jsonl", "session-subagent.jsonl"} {
		a := New()
		feed(a, fixtureLines(t, name))
		if got := activeOf(t, a.Rows()); got != 60 {
			t.Errorf("%s: ActiveSeconds = %d, want 60", name, got)
		}
	}
}

func TestClaudeSessionWithoutTimesHasNoActiveTime(t *testing.T) {
	a := New()
	a.Add(usage("s", "m", "x", "r", 1))
	a.Add(claudecode.UserTurn{SessionID: "s", UUID: "u"})
	if got := activeOf(t, a.Rows()); got != 0 {
		t.Errorf("ActiveSeconds = %d, want 0", got)
	}
}

func TestClaudeActiveSurvivesSnapshotRestoreAndReplay(t *testing.T) {
	lines := claudeLines(cUser, cAssist, cTool, cSide, cUser2)
	for split := 0; split <= len(lines); split++ {
		first := New()
		feed(first, lines[:split])
		var st State
		if err := json.Unmarshal([]byte(mustJSON(t, first.Snapshot())), &st); err != nil {
			t.Fatal(err)
		}
		second := New()
		if err := second.Restore(st); err != nil {
			t.Fatal(err)
		}
		feed(second, lines) // replay from line 0
		if got := activeOf(t, second.Rows()); got != 480 {
			t.Fatalf("split %d: ActiveSeconds = %d, want 480", split, got)
		}
	}
}

func TestClaudeActiveCap(t *testing.T) {
	a := New()
	for i := int64(0); i < maxActiveMinutes+3; i++ {
		a.Add(claudecode.UserTurn{SessionID: "s", UUID: fmt.Sprintf("u%d", i), Timestamp: tm(wordBase+i, 0)})
	}
	a.Add(usage("s", "m", "x", "r", 1, func(e *claudecode.AssistantUsage) { e.Timestamp = tm(wordBase, 5) }))
	if got := activeOf(t, a.Rows()); got != maxActiveMinutes*60 {
		t.Errorf("ActiveSeconds = %d, want %d", got, maxActiveMinutes*60)
	}
	if c := a.Counters(); c.ActiveCapReached != 3 {
		t.Errorf("ActiveCapReached = %d, want 3", c.ActiveCapReached)
	}
}

func TestClaudeRestoreDropsOldStateAndBadActive(t *testing.T) {
	if stateVersion != 4 {
		t.Fatalf("stateVersion = %d: the active-minutes bump is 4", stateVersion)
	}
	if err := New().Restore(State{Version: 3, Sessions: map[string]*SessionState{}}); err == nil {
		t.Error("a version 3 state (no active minutes) must be discarded")
	}
	bad := State{Version: stateVersion, Sessions: map[string]*SessionState{"s": {Active: &MinuteSet{Words: []MinuteWord{{W: 1, B: 0}}}}}}
	a := New()
	a.Add(usage("keep", "m", "x", "r", 1))
	if err := a.Restore(bad); err == nil {
		t.Error("an invalid minute set was accepted")
	}
	if rows := a.Rows(); len(rows) != 1 || rows[0].SessionID != "keep" {
		t.Error("a failed Restore must keep the current state")
	}
}

// OTel spans count with start and end, chat requests with their time: minutes 0 (span), 3 (span), 7 (request), 20 (request).
func TestCopilotActiveSeconds(t *testing.T) {
	feedAll := func(a *CopilotAggregator) {
		a.Add(span("c1", "t1", "s1", "gpt-x", 0, 30*sec, 1, 1, 0))
		a.Add(span("c1", "t2", "s2", "gpt-x", 180*sec, 200*sec, 1, 1, 0))
		feedChat(a, "c1", creq(nil, "r1", -1, 7*time.Minute+10*sec, ""), creq(nil, "r2", -1, 20*time.Minute, ""))
	}
	a := NewCopilot()
	feedAll(a)
	// 60 + 180 (gap 3) + 240 (gap 4) + 60 (gap 13).
	if got := activeOf(t, a.Rows()); got != 540 {
		t.Errorf("ActiveSeconds = %d, want 540", got)
	}
	feedAll(a) // replayed spans and requests change nothing
	if got := activeOf(t, a.Rows()); got != 540 {
		t.Errorf("after replay ActiveSeconds = %d, want 540", got)
	}

	// A late span with an older time is folded in even though it is a new span.
	a.Add(span("c1", "t0", "s0", "gpt-x", -1, 0, 1, 1, 0)) // no time: no point
	if got := activeOf(t, a.Rows()); got != 540 {
		t.Errorf("a span without times changed the result: %d", got)
	}
	late := span("c1", "t3", "s3", "gpt-x", 0, 0, 1, 1, 0)
	late.Start, late.End = cpT0.Add(-2*time.Minute), cpT0.Add(-2*time.Minute)
	a.Add(late)
	if got := activeOf(t, a.Rows()); got != 660 { // -2 min is the first point; 0 is then 2 minutes after it
		t.Errorf("after a late span ActiveSeconds = %d, want 660", got)
	}
}

func TestCopilotActiveSurvivesRestoreAndReplay(t *testing.T) {
	feedAll := func(a *CopilotAggregator) {
		a.Add(span("c1", "t1", "s1", "gpt-x", 0, 30*sec, 1, 1, 0))
		feedChat(a, "c1", creq(nil, "r1", -1, 4*time.Minute, ""))
	}
	first := NewCopilot()
	feedAll(first)
	var st CopilotState
	if err := json.Unmarshal([]byte(mustJSON(t, first.Snapshot())), &st); err != nil {
		t.Fatal(err)
	}
	second := NewCopilot()
	if err := second.Restore(st); err != nil {
		t.Fatal(err)
	}
	feedAll(second)
	if got := activeOf(t, second.Rows()); got != 60+240 {
		t.Errorf("ActiveSeconds = %d, want 300", got)
	}
}

// A conversation with chat requests only (history rows) gets its active time from the request times.
func TestCopilotHistoryActiveSeconds(t *testing.T) {
	a := NewCopilot()
	for i, off := range []time.Duration{0, 2 * time.Minute, 30 * time.Minute} {
		e := creq(nil, fmt.Sprintf("r%d", i), -1, off, "")
		e.Model = "gpt-x"
		feedChat(a, "h1", e)
	}
	rows := a.Rows()
	if len(rows) == 0 {
		t.Fatal("no history rows")
	}
	if got := activeOf(t, rows); got != 60+120+60 {
		t.Errorf("ActiveSeconds = %d, want 240", got)
	}
}

func TestCopilotRequestWithoutTimeHasNoPoint(t *testing.T) {
	a := NewCopilot()
	a.Add(span("c1", "t1", "s1", "gpt-x", -1, 0, 1, 1, 0))
	feedChat(a, "c1", copilotchat.RequestStart{At: -1, RequestID: "r1"})
	if got := activeOf(t, a.Rows()); got != 0 {
		t.Errorf("ActiveSeconds = %d, want 0", got)
	}
}

func TestCopilotRestoreDropsOldStateAndBadActive(t *testing.T) {
	if copilotStateVersion != 6 {
		t.Fatalf("copilotStateVersion = %d: the active-minutes bump is 6", copilotStateVersion)
	}
	if err := NewCopilot().Restore(CopilotState{Version: 5}); err == nil {
		t.Error("a version 5 state (no active minutes) must be discarded")
	}
	src := NewCopilot()
	src.Add(span("c1", "t1", "s1", "m", 0, 1, 1, 1, 0))
	st := src.Snapshot()
	st.Sessions["c1"].Active = &MinuteSet{Words: []MinuteWord{{W: 2, B: 1}, {W: 1, B: 1}}}
	if err := NewCopilot().Restore(st); err == nil {
		t.Error("an unsorted minute set was accepted")
	}
}
