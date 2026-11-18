package analyzer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

// benchConfig is a synthetic configuration at the contract's upper bounds.
func benchConfig() *remoteconfig.Config {
	cfg := &remoteconfig.Config{Friction: remoteconfig.Friction{
		DetectorVersion: 1, RepeatMinTurns: 3, RepeatJaccard: 0.8, ErrorPasteMinShare: 0.6,
		Negators: []remoteconfig.Negator{{Lang: "en", Word: "not", Position: "before"}},
	}, Topics: remoteconfig.Topics{Labels: []string{"feature", "bugfix", "other"}, Fallback: "other", KeywordWeights: map[string][]remoteconfig.TermWeight{}}}
	for i := 0; i < 256; i++ {
		cfg.Friction.Patterns = append(cfg.Friction.Patterns, remoteconfig.FeedbackPattern{
			Category: "error_report", Lang: "en", Pattern: fmt.Sprintf("synthetic (pattern|thing) %03d", i), WholeMessage: i%4 == 0,
		})
	}
	for _, label := range []string{"feature", "bugfix"} {
		for i := 0; i < 64; i++ {
			cfg.Topics.KeywordWeights[label] = append(cfg.Topics.KeywordWeights[label],
				remoteconfig.TermWeight{Term: fmt.Sprintf("%s term %02d", label, i), Weight: 1})
		}
	}
	return cfg
}

// Worst realistic case: 8 MiB of text that matches nothing and mentions many near-misses.
func eightMiB() string {
	chunk := "synthetic pattern text without the numeric suffix, feature term, bugfix term; "
	return strings.Repeat(chunk, (8<<20)/len(chunk))
}

func BenchmarkUserFeatures8MiB(b *testing.B) {
	ex, err := Compile(benchConfig())
	if err != nil {
		b.Fatal(err)
	}
	text := eightMiB()
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ex.UserFeatures(text)
	}
}

func BenchmarkUserFeaturesTypical(b *testing.B) {
	ex, _ := Compile(benchConfig())
	text := "please fix the failing test in the parser, the error is still there after the last change"
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ex.UserFeatures(text)
	}
}

func BenchmarkErrorFingerprint(b *testing.B) {
	raw := []byte(strings.Repeat("Error: something failed at line 42\n", 200))
	b.SetBytes(int64(len(raw)))
	for i := 0; i < b.N; i++ {
		ErrorFingerprint(raw)
	}
}

func TestUserFeaturesLargeInputFinishesInReasonableTime(t *testing.T) {
	ex, err := Compile(benchConfig())
	if err != nil {
		t.Fatal(err)
	}
	text := eightMiB()
	start := time.Now()
	f := ex.UserFeatures(text)
	elapsed := time.Since(start)
	if f.Negative() || f.Chars != len(text) {
		t.Errorf("features = %+v", f)
	}
	if elapsed > 10*time.Second { // loose bound; the benchmark reports the real figure
		t.Errorf("8 MiB took %v", elapsed)
	}
	t.Logf("8 MiB with 64 patterns and 128 terms: %v", elapsed)
}
