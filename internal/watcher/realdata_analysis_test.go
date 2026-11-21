//go:build realdata

package watcher

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/claudecode"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

// Read-only sanity check of the analysis against the developer's own transcripts. Prints
// counts only, never paths, text or pattern content. It is NOT calibration evidence.
//
// The configuration JSON (dumped from the server defaults to a temporary file) is signed with
// a throwaway key generated here and loaded through remoteconfig.Verify, so production code
// keeps having no unsigned loading path.
//
//	HARNEXT_REALDATA_CONFIG=/tmp/cfg.json go test -tags realdata -run TestRealDataAnalysis -v ./internal/watcher
func TestRealDataAnalysis(t *testing.T) {
	cfgPath := os.Getenv("HARNEXT_REALDATA_CONFIG")
	if cfgPath == "" {
		t.Skip("HARNEXT_REALDATA_CONFIG is not set")
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal("cannot read the configuration file")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	env := remoteconfig.Envelope{
		Payload: base64.RawURLEncoding.EncodeToString(raw),
		Sig:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, raw)),
		KID:     "realdata-throwaway",
	}
	cfg, err := remoteconfig.Verify(env, remoteconfig.TrustedKeys{env.KID: pub}, time.Now(), 0)
	if err != nil {
		t.Fatalf("configuration rejected by the production validator: %s", remoteconfig.CodeOf(err))
	}
	ex, err := analyzer.Compile(cfg)
	if err != nil {
		t.Fatal("configuration does not compile")
	}
	root, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}

	// Baseline pass without analysis, then the analyzed pass; token totals must be identical.
	plain, _ := scanReal(t, root, nil)
	agg, events := scanReal(t, root, ex)
	plainTotals, plainUnique := sumTotals(plain)
	totals, unique := sumTotals(agg)
	t.Logf("token totals: input=%d output=%d cache_read=%d cache_creation=%d unique_responses=%d",
		totals.InputTokens, totals.OutputTokens, totals.CacheReadTokens, totals.CacheCreationTokens, unique)
	if totals != plainTotals || unique != plainUnique {
		t.Errorf("analysis changed the token totals: plain %+v/%d vs analyzed %+v/%d", plainTotals, plainUnique, totals, unique)
	}
	// Totals observed in the earlier phase 2a check; new transcripts written since then make a mismatch possible.
	expected := aggregate.Totals{InputTokens: 430, OutputTokens: 184031, CacheReadTokens: 50263764, CacheCreationTokens: 335626}
	t.Logf("matches the earlier phase 2a totals (input 430, output 184031, cache_read 50263764, cache_creation 335626, unique 215): %v",
		totals.InputTokens == expected.InputTokens && totals.OutputTokens == expected.OutputTokens &&
			totals.CacheReadTokens == expected.CacheReadTokens && totals.CacheCreationTokens == expected.CacheCreationTokens && unique == 215)

	// Session quality v2: per-session friction rate buckets, FPS@1 and pooled channel counts.
	rows := agg.Rows()
	rate := map[string]int{}
	topics := map[string]int{}
	seen := map[string]bool{}
	var pooled analyzer.Quality
	var fps1, scored int
	for _, r := range rows {
		if seen[r.SessionID] {
			continue
		}
		seen[r.SessionID] = true
		q := r.Quality
		topics[r.Topic]++
		if q.AnalyzedExchanges == 0 {
			rate["5_unscored"]++
			continue
		}
		scored++
		if q.FirstFrictionExchange != 1 {
			fps1++
		}
		rate[bucket(float64(q.FrictionExchanges)/float64(q.AnalyzedExchanges))]++
		for _, f := range []struct{ dst, src *int64 }{
			{&pooled.Exchanges, &q.Exchanges}, {&pooled.AnalyzedExchanges, &q.AnalyzedExchanges},
			{&pooled.FrictionExchanges, &q.FrictionExchanges}, {&pooled.FxPushback, &q.FxPushback},
			{&pooled.FxToolLoop, &q.FxToolLoop}, {&pooled.FxInterrupt, &q.FxInterrupt},
			{&pooled.SigErrorReport, &q.SigErrorReport}, {&pooled.SigCorrection, &q.SigCorrection},
			{&pooled.SigRepeat, &q.SigRepeat}, {&pooled.SigErrorPaste, &q.SigErrorPaste},
			{&pooled.SigPositiveAck, &q.SigPositiveAck}, {&pooled.ToolCalls, &q.ToolCalls},
			{&pooled.ToolErrors, &q.ToolErrors}, {&pooled.RepeatedErrorRuns, &q.RepeatedErrorRuns},
			{&pooled.InterruptedTools, &q.InterruptedTools}, {&pooled.PermissionDenials, &q.PermissionDenials},
		} {
			*f.dst += *f.src
		}
	}
	t.Logf("events=%d sessions_with_rows=%d scored_sessions=%d", events, len(seen), scored)
	logCounts(t, "friction_rate_bucket", toInt64(rate))
	if scored > 0 {
		t.Logf("fps1_rate=%.3f", float64(fps1)/float64(scored))
	}
	if pooled.AnalyzedExchanges > 0 {
		t.Logf("pooled_friction_rate=%.3f (friction %d / analyzed %d exchanges)",
			float64(pooled.FrictionExchanges)/float64(pooled.AnalyzedExchanges), pooled.FrictionExchanges, pooled.AnalyzedExchanges)
	}
	t.Logf("channels: pushback=%d tool_loop=%d interrupt=%d", pooled.FxPushback, pooled.FxToolLoop, pooled.FxInterrupt)
	t.Logf("signals: error_report=%d correction=%d repeat=%d error_paste=%d positive_ack=%d",
		pooled.SigErrorReport, pooled.SigCorrection, pooled.SigRepeat, pooled.SigErrorPaste, pooled.SigPositiveAck)
	t.Logf("tools: calls=%d errors=%d loops=%d interrupted=%d denied=%d",
		pooled.ToolCalls, pooled.ToolErrors, pooled.RepeatedErrorRuns, pooled.InterruptedTools, pooled.PermissionDenials)
	logCounts(t, "topic", toInt64(topics))
	c := agg.Counters()
	t.Logf("counters: analyzed=%d unanalyzed=%d", c.AnalyzedUserTurns, c.UnanalyzedUserTurns)
	logCounts(t, "skipped", c.Skipped)
}

func scanReal(t *testing.T, root string, ex analyzer.Extractor) (*aggregate.Aggregator, int) {
	t.Helper()
	agg := aggregate.New()
	var p *claudecode.Parser
	if ex != nil {
		agg.SetAnalyzer(ex)
		p = claudecode.NewParser(ex)
	}
	sink := newSink(agg)
	w, err := New(Options{Roots: []Root{rootOf(root)}, Offsets: NewMemoryOffsets(), Sink: sink, Parser: p, OnError: func(error) {}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return agg, sink.count()
}

func sumTotals(agg *aggregate.Aggregator) (aggregate.Totals, int64) {
	var sum aggregate.Totals
	for _, r := range agg.Rows() {
		sum.InputTokens += r.InputTokens
		sum.OutputTokens += r.OutputTokens
		sum.CacheReadTokens += r.CacheReadTokens
		sum.CacheCreationTokens += r.CacheCreationTokens
		sum.CacheCreation1hTokens += r.CacheCreation1hTokens
	}
	return sum, agg.Counters().UniqueResponses
}

func bucket(f float64) string {
	switch {
	case f == 0:
		return "0_zero"
	case f <= 0.1:
		return "1_(0,0.1]"
	case f <= 0.25:
		return "2_(0.1,0.25]"
	case f <= 0.5:
		return "3_(0.25,0.5]"
	}
	return "4_>0.5"
}

func toInt64(m map[string]int) map[string]int64 {
	out := make(map[string]int64, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = int64(m[k])
	}
	return out
}
