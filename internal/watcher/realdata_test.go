//go:build realdata

package watcher

import (
	"context"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
)

// Read-only check against the developer's own transcripts. Prints counts and token sums
// only, never paths or content. Run: go test -tags realdata -run TestRealData -v ./internal/watcher
func TestRealData(t *testing.T) {
	root, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	agg := aggregate.New()
	store := NewMemoryOffsets()
	sink := newSink(agg)
	w, err := New(Options{Root: root, Offsets: store, Sink: sink, OnError: func(err error) { t.Logf("error: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	var sum aggregate.Totals
	var turns int64
	sessions := map[string]bool{}
	models := map[string]bool{}
	for _, r := range agg.Rows() {
		sessions[r.SessionID] = true
		models[r.Model] = true
		sum.InputTokens += r.InputTokens
		sum.OutputTokens += r.OutputTokens
		sum.CacheReadTokens += r.CacheReadTokens
		sum.CacheCreationTokens += r.CacheCreationTokens
		sum.CacheCreation1hTokens += r.CacheCreation1hTokens
	}
	if rows := agg.Rows(); len(rows) > 0 {
		seen := map[string]bool{}
		for _, r := range rows {
			if !seen[r.SessionID] {
				seen[r.SessionID] = true
				for _, p := range r.Parts {
					turns += p.Turns
				}
			}
		}
	}
	partial := 0
	for _, p := range store.Paths() {
		st, _ := store.Get(p)
		if st.Offset != st.Size {
			partial++
		}
	}

	c := agg.Counters()
	t.Logf("files=%d files_with_trailing_partial_line=%d events=%d", len(store.Paths()), partial, sink.count())
	t.Logf("sessions=%d rows=%d models=%d", len(sessions), len(agg.Rows()), len(models))
	t.Logf("unique_responses=%d duplicate_responses=%d sidechain_responses=%d synthetic=%d missing_ids=%d missing_model=%d",
		c.UniqueResponses, c.DuplicateResponses, c.SidechainResponses, c.SyntheticResponses, c.MissingIDs, c.MissingModel)
	t.Logf("user_turns=%d tool_results=%d sidechain_user_turns=%d duplicate_user_turns=%d turn_count_sum=%d",
		c.UserTurns, c.ToolResults, c.SidechainUserTurns, c.DuplicateUserTurns, turns)
	t.Logf("input=%d output=%d cache_read=%d cache_creation=%d cache_creation_1h=%d",
		sum.InputTokens, sum.OutputTokens, sum.CacheReadTokens, sum.CacheCreationTokens, sum.CacheCreation1hTokens)
	logCounts(t, "skipped", c.Skipped)
	logCounts(t, "ignored", c.Ignored)
	logUsageKeys(t, agg.Rows())
}

// logUsageKeys reports the usage-key distribution (payload v4) and fails when a row's day falls
// outside its session's UTC days ±1 (the server would reject the session). Counts only.
func logUsageKeys(t *testing.T, rows []aggregate.Row) {
	speeds, floors := map[string]int64{}, map[string]int64{}
	days := map[string]map[string]bool{}
	var web int64
	for _, r := range rows {
		speeds[r.Speed]++
		floors[strconv.FormatInt(r.PromptFloor, 10)]++
		web += r.WebSearchRequests + r.WebFetchRequests
		if days[r.SessionID] == nil {
			days[r.SessionID] = map[string]bool{}
		}
		days[r.SessionID][r.Day] = true
		day, err := time.Parse(time.DateOnly, r.Day)
		first := r.StartedAt.UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
		last := r.EndedAt.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		if err != nil || day.Before(first) || day.After(last) {
			t.Errorf("usage day outside the session range (row %d)", len(speeds))
		}
	}
	multi := 0
	for _, d := range days {
		if len(d) > 1 {
			multi++
		}
	}
	t.Logf("usage_rows=%d multi_day_sessions=%d web_tool_requests=%d", len(rows), multi, web)
	logCounts(t, "speed", speeds)
	logCounts(t, "prompt_floor", floors)
}

func logCounts(t *testing.T, label string, m map[string]int64) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%s[%s]=%d", label, k, m[k])
	}
}
