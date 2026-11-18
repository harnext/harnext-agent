package aggregate

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/copilot"
	"github.com/harnext/harnext-agent/internal/parser/copilotchat"
)

// cpSim feeds one Copilot conversation (chat events and OTel spans) into an aggregator that resolves folders with fakeResolve.
type cpSim struct {
	t    *testing.T
	a    *CopilotAggregator
	ex   analyzer.Extractor
	conv string
	n    int
}

func newCpSim(t *testing.T) *cpSim {
	a := NewCopilot()
	a.SetResolver(fakeResolve)
	ex := testExtractor(t)
	a.SetAnalyzer(ex)
	a.SetFolder("c1", "/proj")
	return &cpSim{t: t, a: a, ex: ex, conv: "c1"}
}

// request starts a chat request at off with the given edited folders (absolute) and credits.
func (s *cpSim) request(off time.Duration, text string, credits float64, model string, edited ...string) int {
	s.n++
	e := creq(s.ex, fmt.Sprintf("r%d", s.n), -1, off, text)
	e.Model = model
	if credits >= 0 {
		e.HasCredits, e.Credits = true, credits
	}
	for i, d := range edited {
		e.Files[i] = copilotchat.FileRef{Dir: d, Hash: uint64(s.n*100 + i + 1), Edit: true}
	}
	e.FileCount = len(edited)
	s.a.AddChat(s.conv, e)
	return s.n - 1
}

func (s *cpSim) done(idx int, off time.Duration) { s.a.AddChat(s.conv, cstate(idx, 1, off)) }

// trace feeds one OTel span of a trace that starts at off.
func (s *cpSim) trace(id string, off time.Duration, model string, out int64, nano int64) {
	e := span(s.conv, id, fmt.Sprintf("%s-%d", id, off), model, off, off+time.Second, 1, out, 0)
	e.NanoAIU = nano
	s.a.Add(e)
}

func (s *cpSim) rows(label string) (parts []FolderPart, rows []Row) {
	s.t.Helper()
	rows = s.a.Rows()
	st := s.a.Snapshot().Sessions[s.conv]
	if len(rows) == 0 || st == nil || st.Analysis == nil {
		return nil, rows
	}
	snap := st.Analysis.Stats.Snapshot()
	parts = rows[0].Parts
	for _, r := range rows {
		if !reflect.DeepEqual(r.Parts, parts) {
			s.t.Fatalf("%s: rows disagree on the folder list", label)
		}
		if r.Quality != snap {
			s.t.Fatalf("%s: row quality %+v != Snapshot %+v", label, r.Quality, snap)
		}
	}
	assertFoldersSumToSession(s.t, label, parts, snap)
	return parts, rows
}

func creditsOf(rows []Row, folder string) (c float64) {
	for _, r := range rows {
		if r.Folder == folder && r.CopilotCredits != nil {
			c += *r.CopilotCredits
		}
	}
	return c
}

func outputOf(rows []Row, folder string) (n int64) {
	for _, r := range rows {
		if r.Folder == folder {
			n += r.OutputTokens
		}
	}
	return n
}

func TestCopilotHistorySplitsCreditsAndRequestsByFolder(t *testing.T) {
	s := newCpSim(t)
	// 10 requests: 5 in y, 1 in x, 2 in r, 2 in p; credits 5/1/2/2 (1 credit each).
	folders := []string{"y", "x", "y", "r", "y", "p", "y", "r", "y", "p"}
	for i, d := range folders {
		idx := s.request(time.Duration(i)*time.Minute, "go on", 1, "gpt-5.1", "/proj/"+d+"/src")
		s.a.AddChat(s.conv, copilotchat.RequestMetric{Index: idx, Kind: copilotchat.MetricPrompt, Value: 1000})
		s.a.AddChat(s.conv, copilotchat.RequestMetric{Index: idx, Kind: copilotchat.MetricCompletion, Value: 10})
		s.done(idx, time.Duration(i)*time.Minute+10*time.Second)
	}
	parts, rows := s.check(t, "history")
	want := map[string]int64{"y": 5, "x": 1, "r": 2, "p": 2}
	for path, n := range want {
		if got := partOf(parts, path).Turns; got != n {
			t.Errorf("%s turns = %d, want %d", path, got, n)
		}
		if got := creditsOf(rows, path); got != float64(n) {
			t.Errorf("%s credits = %v, want %d", path, got, n)
		}
	}
	for _, r := range rows {
		if !r.TokensEstimated || r.MainFolder != "y" {
			t.Fatalf("row = %+v", r)
		}
	}
}

// check also verifies the session-level totals against an independent count.
func (s *cpSim) check(t *testing.T, label string) ([]FolderPart, []Row) {
	t.Helper()
	parts, rows := s.rows(label)
	var credits float64
	for _, r := range rows {
		if r.CopilotCredits != nil {
			credits += *r.CopilotCredits
		}
	}
	var want float64
	for _, r := range s.a.Snapshot().Sessions[s.conv].Requests {
		want += float64(r.NanoAIU) / 1e9
	}
	if len(s.a.Snapshot().Sessions[s.conv].Models) == 0 && abs(credits-want) > 1e-5 {
		t.Fatalf("%s: Σ folder credits %v != Σ request credits %v", label, credits, want)
	}
	return parts, rows
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func TestCopilotTracesFollowTheRequestThatStartedBeforeThem(t *testing.T) {
	s := newCpSim(t)
	// Requests at 0 s (folder a), 60 s (folder b), 120 s (folder a). Traces start inside each request's time.
	r0 := s.request(0, "one", 3, "m", "/proj/a/x")
	r1 := s.request(60*time.Second, "two", 5, "m", "/proj/b/x")
	r2 := s.request(120*time.Second, "three", 2, "m", "/proj/a/y")
	for _, i := range []int{r0, r1, r2} {
		s.done(i, time.Duration(i)*time.Minute+30*time.Second)
	}
	s.trace("t0", 2*time.Second, "m", 100, 3e9)
	s.trace("t1", 61*time.Second, "m", 200, 2e9)
	s.trace("t1", 70*time.Second, "m", 50, 3e9) // a second span of trace t1 (its start stays 61 s)
	s.trace("t2", 125*time.Second, "m", 10, 2e9)
	parts, rows := s.check(t, "otel")
	if outputOf(rows, "a") != 110 || outputOf(rows, "b") != 250 {
		t.Errorf("tokens a=%d b=%d", outputOf(rows, "a"), outputOf(rows, "b"))
	}
	if creditsOf(rows, "a") != 5 || creditsOf(rows, "b") != 5 {
		t.Errorf("credits a=%v b=%v", creditsOf(rows, "a"), creditsOf(rows, "b"))
	}
	if partOf(parts, "a").Turns != 2 || partOf(parts, "b").Turns != 1 {
		t.Errorf("parts = %+v", parts)
	}
	c := s.a.Counters()
	// Request credits 3, 5, 2 equal the credits of their traces: all three checks match.
	if c.TracesBound != 3 || c.TracesUnbound != 0 || c.TraceChecks != 3 || c.TraceMatches != 3 {
		t.Errorf("self-check = %+v", c)
	}
	// A request whose traces carry other credits fails the check.
	s.trace("t3", 62*time.Second, "m", 1, 1e9) // lands on request 1: now 6 credits against the file's 5
	s.check(t, "mismatch")
	if c = s.a.Counters(); c.TraceChecks != 3 || c.TraceMatches != 2 {
		t.Errorf("mismatch not counted: %+v", c)
	}
}

func TestCopilotUnboundTracesGoToTheMainFolderAndTotalsHold(t *testing.T) {
	s := newCpSim(t)
	r0 := s.request(100*time.Second, "one", -1, "m", "/proj/a/x")
	r1 := s.request(160*time.Second, "two", -1, "m", "/proj/a/y")
	s.done(r0, 120*time.Second)
	s.done(r1, 180*time.Second)
	s.request(220*time.Second, "three", -1, "m", "/proj/b/z") // one request in b: a is the main folder (2 vs 1)
	s.trace("early", 5*time.Second, "m", 7, 1e9)              // before any request
	s.trace("late", 170*time.Second, "m", 9, 1e9)             // bound to request two (folder a)
	parts, rows := s.check(t, "unbound")
	if outputOf(rows, "a") != 16 || outputOf(rows, "b") != 0 {
		t.Errorf("a=%d b=%d", outputOf(rows, "a"), outputOf(rows, "b"))
	}
	if c := s.a.Counters(); c.TracesUnbound != 1 || c.TracesBound != 1 {
		t.Errorf("counters = %+v", c)
	}
	if partOf(parts, "a").Turns != 2 || rows[0].MainFolder != "a" {
		t.Errorf("parts = %+v main = %q", parts, rows[0].MainFolder)
	}
}

func TestCopilotTraceCapRemainderGoesToMainFolderAndTotalsAddUp(t *testing.T) {
	a := NewCopilot(WithTraceCap(1))
	a.SetResolver(fakeResolve)
	a.SetFolder("c1", "/proj/web")
	for i := 1; i <= 3; i++ {
		e := span("c1", fmt.Sprintf("t%d", i), "s", "m", time.Duration(i)*time.Second, time.Duration(i)*time.Second+1, 1, 10, 0)
		e.NanoAIU = 1e9
		a.Add(e)
	}
	rows := a.Rows()
	var out int64
	var credits float64
	for _, r := range rows {
		out += r.OutputTokens
		credits += *r.CopilotCredits
	}
	// Spans of the traces beyond the cap still add tokens and credits to the session (as before); they sit in the main folder.
	if out != 30 || credits != 3 || turnsOf(rows) != 1 || rows[0].Folder != "web" {
		t.Fatalf("out=%d credits=%v turns=%d rows=%+v", out, credits, turnsOf(rows), rows)
	}
}

func TestCopilotFolderSplitSurvivesSnapshotRestore(t *testing.T) {
	s := newCpSim(t)
	for i := 0; i < 6; i++ {
		d := []string{"/proj/a/x", "/proj/b/x", "/proj/a/y"}[i%3]
		idx := s.request(time.Duration(i)*time.Minute, "this is a fixture broken thing", float64(i), "m", d)
		s.done(idx, time.Duration(i)*time.Minute+5*time.Second)
		s.trace(fmt.Sprintf("t%d", i), time.Duration(i)*time.Minute+2*time.Second, "m", int64(10+i), int64(i)*1e9)
	}
	before := s.a.Rows()
	b := NewCopilot()
	b.SetResolver(fakeResolve)
	b.SetAnalyzer(s.ex)
	if err := b.Restore(s.a.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if after := b.Rows(); !reflect.DeepEqual(before, after) {
		t.Fatalf("rows differ after restore:\nbefore %+v\nafter  %+v", before, after)
	}
}

// The chat file is rewritten (requests truncated and appended again): the counters of the dropped requests are kept, so
// folders still add up to the session.
func TestCopilotRewrittenChatFileKeepsFoldersSummingToSession(t *testing.T) {
	s := newCpSim(t)
	for i := 0; i < 4; i++ {
		idx := s.request(time.Duration(i)*time.Minute, "go on", 1, "m", "/proj/a/x")
		s.done(idx, time.Duration(i)*time.Minute+time.Second)
		s.a.AddChat(s.conv, copilotchat.RequestToolCalls{Index: idx, N: 1, Calls: [copilotchat.MaxToolCalls]copilotchat.ToolCall{{IDHash: uint64(i + 1), ToolID: "copilot_readFile", IsError: true, ErrorHash: 4}}})
	}
	e := creq(s.ex, "rewritten", 1, 10*time.Minute, "again") // truncates to one request and appends a new one
	e.Model = "m"
	s.a.AddChat(s.conv, e)
	s.a.AddChat(s.conv, cstate(1, 1, 11*time.Minute))
	parts, _ := s.check(t, "rewritten")
	var exchanges int64
	for _, p := range parts {
		exchanges += p.Quality.Exchanges
	}
	if exchanges != 5 { // 4 original exchanges + 1 new one: the analyzer's counters are not rolled back
		t.Errorf("Σ exchanges = %d", exchanges)
	}
}

// ---- random sequences ----

func TestCopilotFolderSplitRandomSequences(t *testing.T) {
	var multi, friction, calls, truncs, bound int
	defer func() {
		for name, n := range map[string]int{"multi-folder conversations": multi, "friction": friction, "tool calls": calls, "truncations": truncs, "bound traces": bound} {
			if n < 10 {
				t.Errorf("random sequences produced too few %s: %d", name, n)
			}
		}
	}()
	dirs := []string{"/proj/apps/web/src", "/proj/apps/api", "/proj/docs", "/proj/internal/a", "/proj", "/other/lib", "/tmp/x", "/proj/zz"}
	for seed := int64(1); seed <= 120; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := newCpSim(t)
		reqs := 0
		steps := 30 + rng.Intn(100)
		var off time.Duration
		var wantCredits float64
		for step := 0; step < steps; step++ {
			off += time.Duration(rng.Intn(40)+1) * time.Second
			label := fmt.Sprintf("seed %d step %d", seed, step)
			switch k := rng.Intn(20); {
			case k < 5: // new request
				text := randomTexts[rng.Intn(len(randomTexts))]
				var edited []string
				for i := 0; i < rng.Intn(3); i++ {
					edited = append(edited, dirs[rng.Intn(len(dirs))])
				}
				credits := float64(rng.Intn(4))
				e := creq(s.ex, fmt.Sprintf("q%d-%d", seed, step), -1, off, text)
				e.Model = "m"
				e.Hidden = rng.Intn(8) == 0
				e.HasCredits, e.Credits = true, credits
				if rng.Intn(2) == 0 {
					e.HasPrompt, e.PromptTokens, e.HasCompletion, e.CompletionTokens, e.Rounds = true, int64(100+rng.Intn(900)), true, 10, 1+rng.Intn(3)
				}
				for i, d := range edited {
					e.Files[i] = copilotchat.FileRef{Dir: d, Hash: uint64(rng.Intn(5) + 1), Edit: rng.Intn(3) != 0}
				}
				e.FileCount = len(edited)
				if rng.Intn(12) == 0 && reqs > 1 {
					e.At = rng.Intn(reqs) // the file is rewritten from here
					truncs++
					reqs = e.At
				}
				s.a.AddChat(s.conv, e)
				if !e.Hidden {
					wantCredits += credits
				}
				reqs++
			case k < 9 && reqs > 0:
				state := []int{1, 1, 2, 3}[rng.Intn(4)]
				s.a.AddChat(s.conv, cstate(rng.Intn(reqs), state, off))
			case k < 13 && reqs > 0:
				c := copilotchat.ToolCall{IDHash: uint64(rng.Intn(30) + 1), ToolID: []string{"copilot_readFile", "copilot_replaceString", "run_in_terminal"}[rng.Intn(3)],
					IsError: rng.Intn(2) == 0, ErrorHash: uint64(rng.Intn(3))}
				s.a.AddChat(s.conv, copilotchat.RequestToolCalls{Index: rng.Intn(reqs), N: 1, Calls: [copilotchat.MaxToolCalls]copilotchat.ToolCall{c}})
			case k < 15 && reqs > 0:
				f := copilotchat.RequestFiles{Index: rng.Intn(reqs), N: 1}
				f.Files[0] = copilotchat.FileRef{Dir: dirs[rng.Intn(len(dirs))], Hash: uint64(rng.Intn(9) + 1), Edit: rng.Intn(2) == 0}
				s.a.AddChat(s.conv, f)
			case k < 18 && rng.Intn(2) == 0: // OTel span
				e := span(s.conv, fmt.Sprintf("t%d", rng.Intn(12)), fmt.Sprintf("s%d", step), []string{"m", "n"}[rng.Intn(2)], off, off+time.Second, int64(rng.Intn(5)), int64(rng.Intn(100)), 0)
				e.NanoAIU = int64(rng.Intn(3)) * 1e9
				s.a.Add(e)
			default:
				s.a.SetFolder(s.conv, []string{"/proj", "/proj/apps/web", ""}[rng.Intn(3)])
			}
			if seed <= 25 || step == steps-1 {
				parts, rows := s.rows(label)
				if len(rows) == 0 {
					continue
				}
				if len(parts) > maxFolders {
					t.Fatalf("%s: %d folders", label, len(parts))
				}
				if step == steps-1 {
					if len(parts) > 1 {
						multi++
					}
					if rows[0].Quality.FrictionExchanges > 0 {
						friction++
					}
					if rows[0].Quality.ToolCalls > 0 {
						calls++
					}
					bound += int(s.a.Counters().TracesBound)
					// Tokens and credits are conserved: Σ rows equals the session's totals.
					st := s.a.Snapshot().Sessions[s.conv]
					if len(st.Models) > 0 {
						var wantOut, wantNano, wantTurns int64
						for _, m := range st.Models {
							wantOut += m.OutputTokens
							wantNano += m.NanoAIU
							wantTurns += m.TurnCount
						}
						var out, turns int64
						var credits float64
						for _, r := range rows {
							out += r.OutputTokens
							credits += *r.CopilotCredits
						}
						for _, p := range parts {
							turns += p.Turns
						}
						if out != wantOut || turns != wantTurns || abs(credits-float64(wantNano)/1e9) > 1e-5 {
							t.Fatalf("%s: out %d/%d turns %d/%d credits %v/%v", label, out, wantOut, turns, wantTurns, credits, float64(wantNano)/1e9)
						}
					}
				}
			}
		}
		_ = wantCredits
		// Snapshot/restore must not change the rows.
		before := s.a.Rows()
		b := NewCopilot()
		b.SetResolver(fakeResolve)
		b.SetAnalyzer(s.ex)
		if err := b.Restore(s.a.Snapshot()); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if after := b.Rows(); !reflect.DeepEqual(before, after) {
			t.Fatalf("seed %d: rows differ after restore", seed)
		}
	}
	_ = copilot.Ignored{}
}
