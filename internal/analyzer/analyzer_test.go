package analyzer

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

func topicCfg() TopicConfig {
	return TopicConfig{
		Labels: []string{"feature", "bugfix", "test", "docs", "other"},
		ExtensionSignals: map[string][]remoteconfig.LabelWeight{
			".md":      {{Label: "docs", Weight: 3}},
			".go":      {{Label: "feature", Weight: 1}},
			"_test.go": {{Label: "test", Weight: 3}},
		},
		ToolSignals: map[string][]remoteconfig.LabelWeight{
			"Edit": {{Label: "feature", Weight: 1}},
			"Read": {{Label: "docs", Weight: 0.5}, {Label: "explore", Weight: 9}}, // explore is not in labels
		},
		MinScore: 2,
		Fallback: "other",
	}
}

func sigWith(ext, tool map[string]int, kw map[string]float64) TopicSignals {
	var s TopicSignals
	for k, n := range ext {
		for i := 0; i < n; i++ {
			s.Ext.Add(SignalKey(k))
		}
	}
	for k, n := range tool {
		for i := 0; i < n; i++ {
			s.Tool.Add(SignalKey(k))
		}
	}
	for label, pts := range kw {
		s.Keyword[LabelIndex(label)] = uint32(pts * 1000)
	}
	return s
}

func TestClassifyTopic(t *testing.T) {
	tests := []struct {
		name string
		sig  TopicSignals
		cfg  func(TopicConfig) TopicConfig
		want string
	}{
		{"no signals falls back", TopicSignals{}, nil, "other"},
		{"extension layer", sigWith(map[string]int{".md": 1}, nil, nil), nil, "docs"},
		{"tool layer", sigWith(nil, map[string]int{"Edit": 3}, nil), nil, "feature"},
		{"keyword layer", sigWith(nil, nil, map[string]float64{"bugfix": 4}), nil, "bugfix"},
		{"layers add up", sigWith(map[string]int{".go": 1}, map[string]int{"Edit": 1}, map[string]float64{"feature": 1}), nil, "feature"},
		{"below min_score falls back", sigWith(map[string]int{".go": 1}, nil, nil), nil, "other"},
		{"exactly min_score wins", sigWith(map[string]int{".go": 2}, nil, nil), nil, "feature"},
		{"highest total wins", sigWith(map[string]int{".md": 1}, map[string]int{"Edit": 2}, nil), nil, "docs"},
		{"tie goes to the label listed first", sigWith(map[string]int{".md": 1}, map[string]int{"Edit": 3}, nil), nil, "feature"},
		{"tie respects reordered labels", sigWith(map[string]int{".md": 1}, map[string]int{"Edit": 3}, nil),
			func(c TopicConfig) TopicConfig { c.Labels = []string{"docs", "feature", "other"}; return c }, "docs"},
		{"suffix signal for the same file fires both layers", sigWith(map[string]int{"_test.go": 1, ".go": 1}, nil, nil), nil, "test"},
		{"counts are capped per key", sigWith(map[string]int{".go": 500}, nil, map[string]float64{"docs": 11}), nil, "docs"},
		{"signal label outside cfg.Labels is ignored", sigWith(nil, map[string]int{"Read": 5}, nil), nil, "docs"},
		{"keyword label outside cfg.Labels is ignored", sigWith(nil, nil, map[string]float64{"explore": 50}), nil, "other"},
		{"min_score 0 with no signals still falls back", TopicSignals{},
			func(c TopicConfig) TopicConfig { c.MinScore = 0; return c }, "other"},
		{"fallback missing from labels uses first label", TopicSignals{},
			func(c TopicConfig) TopicConfig { c.Fallback = "docs"; c.Labels = []string{"feature", "test"}; return c }, "feature"},
		{"single label", sigWith(map[string]int{".md": 9}, nil, nil),
			func(c TopicConfig) TopicConfig { c.Labels = []string{"other"}; c.Fallback = "other"; return c }, "other"},
		{"empty labels", TopicSignals{}, func(c TopicConfig) TopicConfig { c.Labels = nil; return c }, ""},
		{"unknown signal keys do nothing", sigWith(map[string]int{".zzz": 5}, map[string]int{"Mystery": 5}, nil), nil, "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := topicCfg()
			if tt.cfg != nil {
				cfg = tt.cfg(cfg)
			}
			if got := ClassifyTopic(tt.sig, cfg); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyTopicIsDeterministicAndStaysInLabels(t *testing.T) {
	cfg := topicCfg()
	// Many equal-scoring keys: map iteration order must not change the answer.
	cfg.ExtensionSignals = map[string][]remoteconfig.LabelWeight{}
	var sig TopicSignals
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf(".e%d", i)
		cfg.ExtensionSignals[key] = []remoteconfig.LabelWeight{{Label: "feature", Weight: 0.1}, {Label: "docs", Weight: 0.1}}
		sig.Ext.Add(SignalKey(key))
	}
	first := ClassifyTopic(sig, cfg)
	for i := 0; i < 100; i++ {
		if got := ClassifyTopic(sig, cfg); got != first {
			t.Fatalf("run %d: %q != %q", i, got, first)
		}
	}
	if first != "feature" {
		t.Errorf("tie between feature and docs resolved to %q", first)
	}

	rng := rand.New(rand.NewSource(3))
	all := remoteconfig.TopicLabels
	for i := 0; i < 2000; i++ {
		var labels []string
		for _, l := range all {
			if rng.Intn(2) == 0 {
				labels = append(labels, l)
			}
		}
		if len(labels) == 0 {
			continue
		}
		c := TopicConfig{Labels: labels, Fallback: labels[rng.Intn(len(labels))], MinScore: float64(rng.Intn(5)),
			ExtensionSignals: map[string][]remoteconfig.LabelWeight{".x": {{Label: all[rng.Intn(len(all))], Weight: 1}}}}
		var s TopicSignals
		for j := 0; j < rng.Intn(5); j++ {
			s.Ext.Add(SignalKey(".x"))
		}
		for j := range s.Keyword {
			s.Keyword[j] = uint32(rng.Intn(5000))
		}
		got := ClassifyTopic(s, c)
		found := false
		for _, l := range labels {
			found = found || l == got
		}
		if !found {
			t.Fatalf("%q not in %v", got, labels)
		}
	}
}

func TestSignalTable(t *testing.T) {
	var tab SignalTable
	for i := 0; i < MaxSignalSlots+5; i++ {
		tab.Add(uint64(i + 1))
	}
	tab.Add(1)
	if tab.Used != MaxSignalSlots || tab.Dropped != 5 || tab.Count(1) != 2 || tab.Count(uint64(MaxSignalSlots+1)) != 0 {
		t.Errorf("table = used %d dropped %d count(1) %d", tab.Used, tab.Dropped, tab.Count(1))
	}

	var small SignalTable
	small.Add(SignalKey(".go"))
	small.Add(SignalKey(".go"))
	small.Add(SignalKey("Edit"))
	raw, err := json.Marshal(small)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 100 {
		t.Errorf("JSON is not compact: %d bytes", len(raw))
	}
	var back SignalTable
	if err := json.Unmarshal(raw, &back); err != nil || back != small {
		t.Errorf("round trip: %+v %v", back, err)
	}
	// Oversized or odd input is truncated, not fatal.
	var big SignalTable
	entries := make([][2]uint64, 0, 100)
	for i := 0; i < 100; i++ {
		entries = append(entries, [2]uint64{uint64(i + 1), 1 << 40})
	}
	in, _ := json.Marshal(map[string]any{"e": entries})
	if err := json.Unmarshal(in, &big); err != nil || big.Used != MaxSignalSlots || big.Dropped != 100-MaxSignalSlots || big.Counts[0] != math.MaxUint32 {
		t.Errorf("oversized: used=%d dropped=%d count=%d err=%v", big.Used, big.Dropped, big.Counts[0], err)
	}
	if err := json.Unmarshal([]byte(`{"e":"x"}`), &big); err == nil {
		t.Error("garbage accepted")
	}
}

func TestErrorFingerprint(t *testing.T) {
	a := ErrorFingerprint([]byte("Error: file not found at line 42, took 3ms\n"))
	if a == 0 {
		t.Fatal("zero fingerprint for real text")
	}
	same := []string{
		"error: FILE NOT FOUND at line 7, took 1500ms",
		"  Error:   file\tnot found at line 99999, took ms \r\n\r\n",
	}
	for _, s := range same {
		if got := ErrorFingerprint([]byte(s)); got != a {
			t.Errorf("%q fingerprints differently", s)
		}
	}
	if ErrorFingerprint([]byte("Error: permission denied")) == a {
		t.Error("different errors share a fingerprint")
	}
	for name, in := range map[string]string{"empty": "", "only digits": "123 456", "only whitespace": " \n\t"} {
		if got := ErrorFingerprint([]byte(in)); got != 0 {
			t.Errorf("%s: %d", name, got)
		}
	}
	long1 := strings.Repeat("x", 3000) + "tail one"
	long2 := strings.Repeat("x", 3000) + "tail two"
	if ErrorFingerprint([]byte(long1)) != ErrorFingerprint([]byte(long2)) {
		t.Error("bytes beyond the fingerprint window changed the result")
	}
}

func testConfig() *remoteconfig.Config {
	return &remoteconfig.Config{
		Friction: remoteconfig.Friction{
			DetectorVersion: 3, RepeatMinTurns: 2, RepeatJaccard: 0.8, ErrorPasteMinShare: 0.6,
			Patterns: []remoteconfig.FeedbackPattern{
				{Category: "error_report", Lang: "en", Pattern: "fixture broken thing"},
				{Category: "error_report", Lang: "en", Pattern: "(still )?(fails|failing)"},
				{Category: "error_report", Lang: "en", Pattern: "nope", WholeMessage: true},
				{Category: "error_report", Lang: "tr", Pattern: "kirik"},
				{Category: "error_report", Lang: "tr", Pattern: "calis(miyor|madi)"},
				{Category: "error_report", Lang: "tr", Pattern: "olmadi", WholeMessage: true},
				{Category: "correction", Lang: "en", Pattern: "i (said|told you)"},
				{Category: "correction", Lang: "tr", Pattern: "geri al"},
				{Category: "positive_ack", Lang: "en", Pattern: "(thanks|works now)"},
				{Category: "positive_ack", Lang: "tr", Pattern: "tesekkurler"},
			},
			Negators: []remoteconfig.Negator{
				{Lang: "en", Word: "not", Position: "before"},
				{Lang: "tr", Word: "degil", Position: "after"},
			},
		},
		Topics: remoteconfig.Topics{
			Labels: []string{"feature", "bugfix", "docs", "other"}, MinScore: 1, Fallback: "other",
			KeywordWeights: map[string][]remoteconfig.TermWeight{
				"bugfix":  {{Term: "crash", Weight: 2}, {Term: "stack trace", Weight: 1.5}},
				"feature": {{Term: "implement", Weight: 3}},
				"docs":    {{Term: "readme", Weight: 12}}, // 3 hits x 12 = 36 points, capped at 10 per label
			},
		},
	}
}

func TestExtractorUserFeatures(t *testing.T) {
	ex, err := Compile(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	type want struct{ err, corr, ack bool }
	tests := []struct {
		name  string
		text  string
		chars int
		want  want
	}{
		{"plain match", "this is a Fixture Broken Thing again", 36, want{err: true}},
		{"optional group", "it fails", 8, want{err: true}},
		{"whole message", "Nope!", 5, want{err: true}},
		{"whole message must be the whole message", "nope, but add a test", 20, want{}},
		{"word boundary inside a word", "nopes and denoped", 17, want{}},
		{"turkish capital I folds", "KIRIK", 5, want{err: true}},
		{"turkish without diacritics", "hala calismiyor", 15, want{err: true}},
		{"turkish with diacritics", "Hâlâ ÇALIŞMIYOR", 15, want{err: true}},
		{"turkish whole message", "Olmadı.", 7, want{err: true}},
		{"suffix blocks the boundary", "geri alındı mı", 14, want{}},
		{"command", "son değişikliği geri al", 23, want{corr: true}},
		{"english negator before", "it's not failing anymore", 24, want{}},
		{"turkish negator after", "çalışmıyor değil, yavaş", 23, want{}},
		{"correction", "I told you to keep it", 21, want{corr: true}},
		{"positive", "thanks, works now", 17, want{ack: true}},
		{"code block is ignored", "see:\n```\nit fails\n```", 21, want{}},
		{"inline code is ignored", "rename `fails` please", 21, want{}},
		{"quote is ignored", "> nope\nplease continue", 22, want{}},
		{"no match", "everything is fine", 18, want{}},
		{"empty", "", 0, want{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := ex.UserFeatures(tt.text)
			got := want{f.ErrorReport, f.Correction, f.PositiveAck}
			if f.Chars != tt.chars || got != tt.want {
				t.Errorf("chars=%d got=%+v, want %d %+v", f.Chars, got, tt.chars, tt.want)
			}
		})
	}

	t.Run("interrupt marker", func(t *testing.T) {
		f := ex.UserFeatures("[Request interrupted by user for tool use]")
		if !f.Interrupted || f.Negative() {
			t.Errorf("features = %+v", f)
		}
	})

	t.Run("pasted error output", func(t *testing.T) {
		f := ex.UserFeatures("```\nTypeError: x is undefined\n    at foo (src/a.ts:12:3)\n```")
		if f.ErrorShare() < 0.6 {
			t.Errorf("error share = %.2f", f.ErrorShare())
		}
		if g := ex.UserFeatures("please add a button"); g.ErrorShare() != 0 {
			t.Errorf("prose error share = %.2f", g.ErrorShare())
		}
	})

	t.Run("repeat signature", func(t *testing.T) {
		a := ex.UserFeatures("Please add input validation to the signup form")
		b := ex.UserFeatures("please add input validation to the signup form!!")
		c := ex.UserFeatures("Write a migration for the invoices table")
		if s := similarity(a.Sig, b.Sig); s < 0.8 {
			t.Errorf("near-identical similarity = %.2f", s)
		}
		if s := similarity(a.Sig, c.Sig); s > 0.3 {
			t.Errorf("different similarity = %.2f", s)
		}
		if ex.UserFeatures("devam").Sig.OK {
			t.Error("short message produced a signature")
		}
	})

	t.Run("keyword points", func(t *testing.T) {
		f := ex.UserFeatures("Please IMPLEMENT this; it will crash, crash, crash and crash, see stack trace")
		if got := f.KeywordScores[LabelIndex("feature")]; got != 3000 {
			t.Errorf("feature = %d", got)
		}
		// crash is capped at 3 hits (3 x 2.0) plus one stack trace (1.5).
		if got := f.KeywordScores[LabelIndex("bugfix")]; got != 7500 {
			t.Errorf("bugfix = %d", got)
		}
		g := ex.UserFeatures("readme readme readme readme")
		if got := g.KeywordScores[LabelIndex("docs")]; got != maxLabelMilliPerMessage {
			t.Errorf("per-label cap: docs = %d", got)
		}
	})

	t.Run("merge", func(t *testing.T) {
		a := ex.UserFeatures("one crash in the payment service")
		b := ex.UserFeatures("nope")
		a.Merge(b)
		if a.Chars != 32+4 || !a.ErrorReport || !a.Sig.OK || a.KeywordScores[LabelIndex("bugfix")] != 2000 {
			t.Errorf("merged = %+v", a)
		}
	})

	t.Run("settings", func(t *testing.T) {
		s := ex.Settings()
		if s.Params != (Params{RepeatMinTurns: 2, RepeatJaccard: 0.8, ErrorPasteMinShare: 0.6}) || s.DetectorVersion != 3 || s.Topics.Fallback != "other" {
			t.Errorf("settings = %+v", s)
		}
	})
}

func TestCompileRejectsBadInput(t *testing.T) {
	if _, err := Compile(nil); err == nil {
		t.Error("nil config accepted")
	}
	cfg := testConfig()
	cfg.Topics.KeywordWeights["bogus"] = []remoteconfig.TermWeight{{Term: "x", Weight: 1}}
	if _, err := Compile(cfg); err == nil {
		t.Error("unknown label accepted")
	}
	for _, bad := range []string{"", "(a", "a.*", "Wrong"} {
		cfg = testConfig()
		cfg.Friction.Patterns[0].Pattern = bad
		if _, err := Compile(cfg); err == nil {
			t.Errorf("pattern %q accepted", bad)
		}
	}
	// A config without patterns compiles: nothing matches.
	cfg = testConfig()
	cfg.Friction.Patterns = nil
	cfg.Topics.KeywordWeights = nil
	ex, err := Compile(cfg)
	if err != nil || ex.UserFeatures("nope").Negative() {
		t.Errorf("empty config: %v", err)
	}
}

// walkTypes fails on any type that could hold text: strings, byte slices, interfaces, maps
// with string keys or values, unsafe pointers. time.Time is the one allowed opaque leaf.
func walkTypes(t *testing.T, ty reflect.Type, path string, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[ty] {
		return
	}
	seen[ty] = true
	if ty == reflect.TypeOf(time.Time{}) {
		return
	}
	switch ty.Kind() {
	case reflect.String, reflect.Interface, reflect.UnsafePointer, reflect.Func, reflect.Chan:
		t.Errorf("%s has forbidden kind %s", path, ty.Kind())
	case reflect.Slice, reflect.Array, reflect.Pointer:
		if ty.Elem().Kind() == reflect.Uint8 && ty.Kind() == reflect.Slice {
			t.Errorf("%s is a byte slice", path)
			return
		}
		walkTypes(t, ty.Elem(), path+"[]", seen)
	case reflect.Map:
		walkTypes(t, ty.Key(), path+"{key}", seen)
		walkTypes(t, ty.Elem(), path+"{value}", seen)
	case reflect.Struct:
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			walkTypes(t, f.Type, path+"."+f.Name, seen)
		}
	}
}

func TestNoTextCarryingFields(t *testing.T) {
	for _, v := range []any{SessionStats{}, Quality{}, TopicSignals{}, UserFeatures{}, SignalTable{}} {
		ty := reflect.TypeOf(v)
		walkTypes(t, ty, ty.Name(), map[reflect.Type]bool{})
	}
}

// The walker itself must detect the things it forbids.
func TestWalkTypesDetectsText(t *testing.T) {
	type leaky struct {
		A int
		B struct{ Text string }
		C [][]byte
		D map[string]int
	}
	var errs int
	ft := &fakeT{onError: func() { errs++ }}
	walkTypesFake(ft, reflect.TypeOf(leaky{}))
	if errs < 3 {
		t.Errorf("walker found only %d problems in a leaky type", errs)
	}
}

type fakeT struct{ onError func() }

func walkTypesFake(ft *fakeT, ty reflect.Type) {
	switch ty.Kind() {
	case reflect.String, reflect.Interface:
		ft.onError()
	case reflect.Slice:
		if ty.Elem().Kind() == reflect.Uint8 {
			ft.onError()
			return
		}
		walkTypesFake(ft, ty.Elem())
	case reflect.Map:
		walkTypesFake(ft, ty.Key())
		walkTypesFake(ft, ty.Elem())
	case reflect.Struct:
		for i := 0; i < ty.NumField(); i++ {
			walkTypesFake(ft, ty.Field(i).Type)
		}
	}
}

// Text must not survive analysis: not in features, state, JSON or errors.
func TestTextDoesNotSurviveAnalysis(t *testing.T) {
	const sentinel = "SENTINEL-USER-TEXT-4711"
	cfg := testConfig()
	cfg.Friction.Patterns = append(cfg.Friction.Patterns, remoteconfig.FeedbackPattern{Category: "error_report", Lang: "en", Pattern: "sentinel-user-text"})
	ex, err := Compile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := ex.UserFeatures("please implement " + sentinel + " crash")
	if !f.ErrorReport {
		t.Fatal("sentinel pattern did not match; test is meaningless")
	}
	var s SessionStats
	var sig TopicSignals
	s.StartExchange(&f, ex.Settings().Params)
	sig.AddKeywords(f.KeywordScores)
	fp := ErrorFingerprint([]byte(sentinel))

	for name, v := range map[string]any{"features": f, "stats": s, "quality": s.Snapshot(), "signals": sig, "fingerprint": fp} {
		raw, _ := json.Marshal(v)
		for _, out := range []string{string(raw), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v)} {
			if strings.Contains(strings.ToLower(out), strings.ToLower(sentinel)) {
				t.Errorf("%s contains the text: %s", name, out)
			}
		}
	}
}

func TestErrorFingerprintHandlesJSONEncoding(t *testing.T) {
	a := ErrorFingerprint([]byte(`"Error: build failed at line 12\n"`))
	b := ErrorFingerprint([]byte(`"error: BUILD failed at line 340"`))
	c := ErrorFingerprint([]byte(`"Error:\r\n\tbuild   failed at line 7\\n\n\n"`))
	if a == 0 || a != b {
		t.Errorf("trailing JSON newline changes the fingerprint: %d %d", a, b)
	}
	if c == a {
		t.Log("escaped backslash is kept as text, so c differs from a")
	}
	// ANSI escapes and unicode escapes do not break the normalization.
	d := ErrorFingerprint([]byte(`"\u001b[31mfailed\u001b[0m: x"`))
	e := ErrorFingerprint([]byte(`"[mfailed[m: x"`))
	if d == 0 || d != e {
		t.Errorf("unicode escape handling: %d vs %d", d, e)
	}
	// A dangling backslash or truncated escape must not panic.
	for _, in := range []string{`\`, `abc\`, `\u`, `\u00`, `\u0041`, `\\`, `"\n`} {
		ErrorFingerprint([]byte(in))
	}
}

func TestPromptFloor(t *testing.T) {
	bands := []int64{100_000, 200_000}
	for _, c := range []struct{ prompt, want int64 }{
		{0, 0}, {100_000, 0}, {100_001, 100_000}, {200_000, 100_000}, {200_001, 200_000}, {5_000_000, 200_000},
	} {
		if got := PromptFloor(bands, c.prompt); got != c.want {
			t.Errorf("PromptFloor(%d) = %d, want %d", c.prompt, got, c.want)
		}
	}
	if got := PromptFloor(nil, 1); got != -1 {
		t.Errorf("no bands = %d, want -1", got)
	}
}
