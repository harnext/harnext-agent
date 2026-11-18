//go:build realdata

package aggregate

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/copilot"
	"github.com/harnext/harnext-agent/internal/parser/copilotchat"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

// Reads a real Copilot OTel file (HARNEXT_REAL_COPILOT_OTEL) and logs only counts and token sums.
func TestRealCopilotOTel(t *testing.T) {
	path := os.Getenv("HARNEXT_REAL_COPILOT_OTEL")
	if path == "" {
		t.Skip("HARNEXT_REAL_COPILOT_OTEL not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skip("file not readable")
	}
	defer f.Close()

	agg := NewCopilot()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), copilot.MaxLineBytes)
	lines := 0
	for sc.Scan() {
		lines++
		agg.Add(copilot.ParseLine(sc.Bytes()))
	}
	c := agg.Counters()
	t.Logf("lines=%d unique_spans=%d duplicates=%d ignored=%v skipped=%v", lines, c.UniqueSpans, c.DuplicateSpans, c.Ignored, c.Skipped)
	for _, r := range agg.Rows() {
		t.Logf("model=%s in=%d out=%d cache_read=%d credits=%v turns=%d started=%s ended=%s", r.Model, r.InputTokens, r.OutputTokens, r.CacheReadTokens, *r.CopilotCredits, turnsOf([]Row{r}), r.StartedAt.Format("15:04:05"), r.EndedAt.Format("15:04:05"))
	}
	if c.UniqueSpans == 0 {
		t.Error("no usage spans counted")
	}
}

// Joins a real OTel file and a real chatSessions file (HARNEXT_REAL_COPILOT_CHAT); negative
// patterns come from HARNEXT_REAL_NEG_PATTERNS ("a|b|c") so none are embedded here.
func TestRealCopilotFriction(t *testing.T) {
	otel, chat := os.Getenv("HARNEXT_REAL_COPILOT_OTEL"), os.Getenv("HARNEXT_REAL_COPILOT_CHAT")
	if otel == "" || chat == "" {
		t.Skip("HARNEXT_REAL_COPILOT_OTEL / HARNEXT_REAL_COPILOT_CHAT not set")
	}
	// Patterns are passed in (ASCII-folded, restricted syntax) so none are embedded in the agent.
	var pats []remoteconfig.FeedbackPattern
	for _, p := range strings.Split(os.Getenv("HARNEXT_REAL_NEG_PATTERNS"), ";") {
		if p != "" {
			pats = append(pats, remoteconfig.FeedbackPattern{Category: remoteconfig.CategoryErrorReport, Lang: "en", Pattern: p})
		}
	}
	ex, err := analyzer.Compile(&remoteconfig.Config{
		Friction: remoteconfig.Friction{
			DetectorVersion: 1, RepeatMinTurns: 3, RepeatJaccard: 0.8, ErrorPasteMinShare: 0.6, Patterns: pats,
		},
		Topics: remoteconfig.Topics{Labels: []string{"feature", "other"}, MinScore: 1, Fallback: "other"},
	})
	if err != nil {
		t.Fatal(err)
	}
	agg := NewCopilot()
	agg.SetAnalyzer(ex)
	feed := func(path string, line func([]byte)) {
		f, err := os.Open(path)
		if err != nil {
			t.Skip("file not readable")
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), copilot.MaxLineBytes)
		for sc.Scan() {
			line(sc.Bytes())
		}
	}
	session := strings.TrimSuffix(filepath.Base(chat), ".jsonl")
	feed(otel, func(b []byte) { agg.Add(copilot.ParseLine(b)) })
	feed(chat, func(b []byte) {
		copilotchat.Parse(b, ex, func(ev copilotchat.Event) { agg.AddChat(session, ev) })
	})
	for _, r := range agg.Rows() {
		t.Logf("model=%s turns=%d topic=%q quality=%+v", r.Model, turnsOf([]Row{r}), r.Topic, r.Quality)
	}
	st := agg.Snapshot().Sessions[session]
	if st == nil || st.Analysis == nil {
		t.Fatal("no analysis state")
	}
	q := st.Analysis.Stats.Snapshot()
	if q.FrictionExchanges > q.AnalyzedExchanges || q.ToolErrors > q.ToolCalls {
		t.Fatalf("invariant broken: %+v", q)
	}
}
