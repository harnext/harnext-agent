// Package analyzer turns locally read transcript text into numeric features and computes
// the Session Friction Score, First-Pass Success and the topic label.
//
// Zero-prompt-retention: text only ever passes through Extractor.UserFeatures and the error
// fingerprint, which return counters and hashes. No type in this package holds a string or
// byte slice derived from message text, tool output, paths or commands (enforced by a
// reflection test). It imports only the leaf remoteconfig package.
package analyzer

import (
	"errors"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

// NumLabels is the size of the fixed topic label set (remoteconfig.TopicLabels).
const NumLabels = len(remoteconfig.TopicLabels)

const (
	// maxTermHitsPerMessage and maxLabelMilliPerMessage bound the keyword layer so one
	// pasted wall of text cannot dominate a session's topic.
	maxTermHitsPerMessage   = 3
	maxLabelMilliPerMessage = 10_000
)

// TopicConfig is the verified remote configuration's own type.
type TopicConfig = remoteconfig.Topics

// LabelIndex returns the position of label in the fixed label set, or -1.
func LabelIndex(label string) int {
	for i, l := range remoteconfig.TopicLabels {
		if l == label {
			return i
		}
	}
	return -1
}

// UserFeatures describes one real user message without containing any of its text.
type UserFeatures struct {
	Chars int
	// ErrorRunes counts runes removed as pasted error output (logs, stack traces).
	ErrorRunes int
	// Interrupted: the line is Claude Code's "[Request interrupted by user]" marker, not a message.
	Interrupted bool
	// Feedback categories.
	ErrorReport bool
	Correction  bool
	PositiveAck bool
	// Sig is a MinHash sketch used only to compare with the previous message (never uploaded).
	Sig Signature
	// KeywordScores are per-label keyword points in thousandths, indexed by LabelIndex.
	KeywordScores [NumLabels]uint32
}

// Negative reports whether the message pushes back on the previous exchange by wording alone.
func (f UserFeatures) Negative() bool { return f.ErrorReport || f.Correction }

// ErrorShare is the share of the message that was pasted error output.
func (f UserFeatures) ErrorShare() float64 {
	if f.Chars == 0 {
		return 0
	}
	return float64(f.ErrorRunes) / float64(f.Chars)
}

// Merge combines the features of two parts of the same message.
func (f *UserFeatures) Merge(o UserFeatures) {
	f.Chars += o.Chars
	f.ErrorRunes += o.ErrorRunes
	f.Interrupted = f.Interrupted || o.Interrupted
	f.ErrorReport = f.ErrorReport || o.ErrorReport
	f.Correction = f.Correction || o.Correction
	f.PositiveAck = f.PositiveAck || o.PositiveAck
	f.Sig.merge(o.Sig)
	for i := range f.KeywordScores {
		f.KeywordScores[i] = satAdd(f.KeywordScores[i], o.KeywordScores[i])
	}
}

// Params are the detector thresholds the aggregator needs.
type Params struct {
	RepeatMinTurns     int
	RepeatJaccard      float64
	ErrorPasteMinShare float64
}

// Settings is a read-only view of the compiled configuration.
type Settings struct {
	Params          Params
	DetectorVersion int
	Topics          TopicConfig
	// ContextBands are the prompt-size boundaries (tokens, strictly increasing); see PromptFloor.
	ContextBands []int64
}

// PromptFloor returns the largest band boundary the prompt exceeds, 0 when it exceeds none,
// and -1 when no bands are configured (unknown band).
func PromptFloor(bands []int64, promptTokens int64) int64 {
	if len(bands) == 0 {
		return -1
	}
	var floor int64
	for _, b := range bands {
		if promptTokens > b {
			floor = b
		}
	}
	return floor
}

// Extractor matches message text against the configured patterns. Implementations must not
// retain text. A nil Extractor means "no configuration": callers must not analyze content.
type Extractor interface {
	// UserFeatures analyzes one text part of a real user message.
	UserFeatures(text string) UserFeatures
	Settings() Settings
}

type term struct {
	m     matcher
	milli uint32
}

type extractor struct {
	settings Settings
	detector detector
	keywords [NumLabels][]term
}

// Compile builds an Extractor from a verified configuration. Feedback patterns are restricted
// RE2 expressions (remoteconfig.ValidPattern); topic keywords are plain substrings.
func Compile(cfg *remoteconfig.Config) (Extractor, error) {
	if cfg == nil {
		return nil, errors.New("analyzer: nil configuration")
	}
	e := &extractor{settings: Settings{
		Params: Params{
			RepeatMinTurns:     cfg.Friction.RepeatMinTurns,
			RepeatJaccard:      cfg.Friction.RepeatJaccard,
			ErrorPasteMinShare: cfg.Friction.ErrorPasteMinShare,
		},
		DetectorVersion: cfg.Friction.DetectorVersion,
		Topics:          cfg.Topics,
		ContextBands:    append([]int64(nil), cfg.Usage.ContextBands...),
	}}
	d, err := compileDetector(cfg.Friction)
	if err != nil {
		return nil, err
	}
	e.detector = d
	for label, terms := range cfg.Topics.KeywordWeights {
		i := LabelIndex(label)
		if i < 0 {
			return nil, errors.New("analyzer: unknown label")
		}
		for _, t := range terms {
			if t.Term == "" {
				return nil, errors.New("analyzer: empty term")
			}
			e.keywords[i] = append(e.keywords[i], term{m: matcher{pat: fold(t.Term)}, milli: uint32(math.Round(t.Weight * 1000))})
		}
	}
	return e, nil
}

func (e *extractor) Settings() Settings { return e.settings }

func (e *extractor) UserFeatures(text string) UserFeatures {
	f := UserFeatures{Chars: utf8.RuneCountInString(text)}
	if text == "" {
		return f
	}
	// A person's feedback sits at the start of a message; pasted walls of text are bounded so
	// preprocessing and up to 256 patterns never scan megabytes. The error share of the head
	// stands in for the whole message.
	c := preprocess(truncateRunes(text, maxPreprocessRunes))
	if c.interrupted {
		f.Interrupted = true
		return f
	}
	if c.totalRunes > 0 {
		f.ErrorRunes = min(f.Chars, int(int64(c.errorRunes)*int64(f.Chars)/int64(c.totalRunes)))
	}
	folded := asciiFold(truncateRunes(c.text, maxDetectRunes))
	fb := e.detector.detect(folded)
	f.ErrorReport, f.Correction, f.PositiveAck = fb.errorReport, fb.correction, fb.positiveAck
	f.Sig = signature(folded)
	low := fold(text)
	for i, terms := range e.keywords {
		var sum uint32
		for _, t := range terms {
			if n := m64(t.m.count(low, maxTermHitsPerMessage)); n > 0 {
				sum = satAdd(sum, n*t.milli)
			}
		}
		f.KeywordScores[i] = min(sum, maxLabelMilliPerMessage)
	}
	return f
}

func m64(n int) uint32 { return uint32(n) }

// maxPreprocessRunes and maxDetectRunes bound the text that preprocessing and the feedback
// detector see.
const (
	maxPreprocessRunes = 64 << 10
	maxDetectRunes     = 16 << 10
)

func truncateRunes(s string, n int) string {
	if len(s) <= n { // bytes >= runes
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

func satAdd(a, b uint32) uint32 {
	if s := a + b; s >= a {
		return s
	}
	return math.MaxUint32
}

// fold lowercases and maps the typographic apostrophe and dotless i so that Turkish
// capitals and curly quotes match the lowercase ASCII-apostrophe patterns.
func fold(s string) string {
	return strings.Map(foldRune, s)
}
