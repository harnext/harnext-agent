package remoteconfig

import (
	"encoding/json"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// PayloadVersion is the only supported payload_version.
const PayloadVersion = 3

// TopicLabels mirrors TOPICS in the server's ingest schema; topics.labels must be a subset.
var TopicLabels = [...]string{"feature", "bugfix", "refactor", "test", "docs", "devops", "data", "explore", "other"}

// Limits mirror contracts/agent-config.v1.schema.json.
const (
	maxFeedbackPatterns = 256
	maxPatternLen       = 120
	maxNegators         = 32
	maxTermsPerLabel    = 64
	maxExtensionSignals = 128
	maxToolSignals      = 64
	maxSignalsPerKey    = 8
	maxTermRunes        = 40
	maxConfigTTL        = 90 * 24 * time.Hour
	maxSafeInteger      = 1<<53 - 1
	maxContextBands     = 8
	minContextBand      = 1000
	maxContextBand      = 10_000_000
)

// Config is a verified configuration. Treat it as immutable.
type Config struct {
	ConfigVersion   int64
	IssuedAt        time.Time
	ExpiresAt       time.Time
	MinAgentVersion string
	Friction        Friction
	Topics          Topics
	Usage           Usage
}

// Usage holds the prompt-size band boundaries (tokens, strictly increasing). Each counted
// response is filed under the largest boundary its prompt exceeds (0 below the first).
type Usage struct {
	ContextBands []int64
}

// Friction holds the detector parameters. The score itself is computed
// on the server from uploaded counters, so there are no weights here.
type Friction struct {
	DetectorVersion    int
	RepeatMinTurns     int
	RepeatJaccard      float64
	ErrorPasteMinShare float64
	Patterns           []FeedbackPattern
	Negators           []Negator
}

// Feedback categories (contract enum).
const (
	CategoryErrorReport = "error_report"
	CategoryCorrection  = "correction"
	CategoryPositiveAck = "positive_ack"
)

// FeedbackPattern is a restricted pattern: lowercase ASCII, metacharacters limited to ( ) | ?.
// It is matched on word boundaries against ASCII-folded text.
type FeedbackPattern struct {
	Category     string
	Lang         string
	Pattern      string
	WholeMessage bool
}

// Negator suppresses a match when Word is within two words before or after it.
type Negator struct {
	Lang     string
	Word     string
	Position string // "before" or "after"
}

type Topics struct {
	Labels           []string
	ExtensionSignals map[string][]LabelWeight
	ToolSignals      map[string][]LabelWeight
	KeywordWeights   map[string][]TermWeight
	MinScore         float64
	Fallback         string
}

type LabelWeight struct {
	Label  string
	Weight float64
}

type TermWeight struct {
	Term   string
	Weight float64
}

// Wire types: pointers detect missing required fields; unknown fields are ignored.
type wireConfig struct {
	PayloadVersion  *int64      `json:"payload_version"`
	ConfigVersion   *int64      `json:"config_version"`
	IssuedAt        *string     `json:"issued_at"`
	ExpiresAt       *string     `json:"expires_at"`
	MinAgentVersion *string     `json:"min_agent_version"`
	Friction        *wireFric   `json:"friction"`
	Topics          *wireTopics `json:"topics"`
	Usage           *wireUsage  `json:"usage"`
}

type wireUsage struct {
	ContextBands *[]int64 `json:"context_bands"`
}

type wireFric struct {
	DetectorVersion    *int64         `json:"detector_version"`
	RepeatMinTurns     *int64         `json:"repeat_min_turns"`
	RepeatJaccard      *float64       `json:"repeat_jaccard"`
	ErrorPasteMinShare *float64       `json:"error_paste_min_share"`
	Patterns           *[]wirePattern `json:"feedback_patterns"`
	Negators           *[]wireNegator `json:"negators"`
}

type wirePattern struct {
	Category     *string `json:"category"`
	Lang         *string `json:"lang"`
	Pattern      *string `json:"pattern"`
	WholeMessage *bool   `json:"whole_message"`
}

type wireNegator struct {
	Lang     *string `json:"lang"`
	Word     *string `json:"word"`
	Position *string `json:"position"`
}

type wireTopics struct {
	Labels           *[]string                    `json:"labels"`
	ExtensionSignals map[string][]wireLabelWeight `json:"extension_signals"`
	ToolSignals      map[string][]wireLabelWeight `json:"tool_signals"`
	KeywordWeights   map[string][]wireTermWeight  `json:"keyword_weights"`
	MinScore         *float64                     `json:"min_score"`
	Fallback         *string                      `json:"fallback"`
}

type wireLabelWeight struct {
	Label  *string  `json:"label"`
	Weight *float64 `json:"weight"`
}

type wireTermWeight struct {
	Term   *string  `json:"term"`
	Weight *float64 `json:"weight"`
}

// parsePayload runs check 4 (UTF-8, JSON, schema). It reports only a boolean: the reason
// would otherwise tempt callers to log payload fragments.
func parsePayload(raw []byte) (*Config, bool) {
	if !utf8.Valid(raw) {
		return nil, false
	}
	var w wireConfig
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, false
	}
	return w.validate()
}

func (w *wireConfig) validate() (*Config, bool) {
	if w.PayloadVersion == nil || *w.PayloadVersion != PayloadVersion {
		return nil, false
	}
	if w.ConfigVersion == nil || *w.ConfigVersion < 1 || *w.ConfigVersion > maxSafeInteger {
		return nil, false
	}
	issued, ok1 := parseUTCTime(w.IssuedAt)
	expires, ok2 := parseUTCTime(w.ExpiresAt)
	if !ok1 || !ok2 {
		return nil, false
	}
	if span := expires.Sub(issued); span <= 0 || span > maxConfigTTL {
		return nil, false
	}
	cfg := &Config{ConfigVersion: *w.ConfigVersion, IssuedAt: issued, ExpiresAt: expires}
	if w.MinAgentVersion != nil {
		if !validMinVersion(*w.MinAgentVersion) {
			return nil, false
		}
		cfg.MinAgentVersion = *w.MinAgentVersion
	}
	if w.Friction == nil || w.Topics == nil {
		return nil, false
	}
	var ok bool
	if cfg.Friction, ok = w.Friction.validate(); !ok {
		return nil, false
	}
	if cfg.Topics, ok = w.Topics.validate(); !ok {
		return nil, false
	}
	if cfg.Usage, ok = w.Usage.validate(); !ok {
		return nil, false
	}
	return cfg, true
}

func (u *wireUsage) validate() (Usage, bool) {
	if u == nil || u.ContextBands == nil || len(*u.ContextBands) > maxContextBands {
		return Usage{}, false
	}
	bands := make([]int64, 0, len(*u.ContextBands))
	for i, b := range *u.ContextBands {
		if b < minContextBand || b > maxContextBand || (i > 0 && b <= bands[i-1]) {
			return Usage{}, false
		}
		bands = append(bands, b)
	}
	return Usage{ContextBands: bands}, true
}

func (f *wireFric) validate() (Friction, bool) {
	var out Friction
	ver, ok1 := intIn(f.DetectorVersion, 1, 1_000_000)
	rep, ok2 := intIn(f.RepeatMinTurns, 2, 20)
	if !ok1 || !ok2 || f.RepeatJaccard == nil || f.ErrorPasteMinShare == nil {
		return out, false
	}
	// Comparisons also reject NaN.
	if !(*f.RepeatJaccard >= 0.5 && *f.RepeatJaccard <= 0.99) || !(*f.ErrorPasteMinShare >= 0.3 && *f.ErrorPasteMinShare <= 1) {
		return out, false
	}
	out.DetectorVersion, out.RepeatMinTurns = ver, rep
	out.RepeatJaccard, out.ErrorPasteMinShare = *f.RepeatJaccard, *f.ErrorPasteMinShare

	if f.Patterns == nil || len(*f.Patterns) < 1 || len(*f.Patterns) > maxFeedbackPatterns {
		return out, false
	}
	out.Patterns = make([]FeedbackPattern, 0, len(*f.Patterns))
	for _, p := range *f.Patterns {
		if p.Category == nil || p.Lang == nil || p.Pattern == nil || !validCategory(*p.Category) || !validLang(*p.Lang) || !ValidPattern(*p.Pattern) {
			return out, false
		}
		out.Patterns = append(out.Patterns, FeedbackPattern{
			Category: *p.Category, Lang: *p.Lang, Pattern: *p.Pattern, WholeMessage: p.WholeMessage != nil && *p.WholeMessage,
		})
	}
	if f.Negators == nil || len(*f.Negators) > maxNegators {
		return out, false
	}
	out.Negators = make([]Negator, 0, len(*f.Negators))
	for _, n := range *f.Negators {
		if n.Lang == nil || n.Word == nil || n.Position == nil || !validLang(*n.Lang) || !validNegatorWord(*n.Word) ||
			(*n.Position != "before" && *n.Position != "after") {
			return out, false
		}
		out.Negators = append(out.Negators, Negator{Lang: *n.Lang, Word: *n.Word, Position: *n.Position})
	}
	return out, true
}

func validCategory(s string) bool {
	return s == CategoryErrorReport || s == CategoryCorrection || s == CategoryPositiveAck
}

func validLang(s string) bool { return s == "en" || s == "tr" }

// validNegatorWord: ^[a-z']{2,20}$
func validNegatorWord(s string) bool {
	if len(s) < 2 || len(s) > 20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c == '\'') {
			return false
		}
	}
	return true
}

// ValidPattern mirrors the contract's feedback pattern rule (the server's agent-config schema):
// 2-120 characters of [a-z0-9 '()|?-], balanced non-empty groups and alternatives, "?" only after
// a literal or ")", no leading or trailing space and no trailing "|". The analyzer relies on this
// to compile the pattern as RE2 without surprises.
func ValidPattern(s string) bool {
	if len(s) < 2 || len(s) > maxPatternLen || strings.TrimSpace(s) != s || strings.HasSuffix(s, "|") {
		return false
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var prev byte
		if i > 0 {
			prev = s[i-1]
		}
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == ' ', c == '\'', c == '-':
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth < 0 || prev == '(' || prev == '|' {
				return false
			}
		case c == '|':
			if i == 0 || prev == '(' || prev == '|' {
				return false
			}
		case c == '?':
			if i == 0 || prev == '(' || prev == '|' || prev == '?' || prev == ' ' {
				return false
			}
		default:
			return false
		}
	}
	return depth == 0
}

func (t *wireTopics) validate() (Topics, bool) {
	var out Topics
	if t.Labels == nil || t.MinScore == nil || t.Fallback == nil || t.ExtensionSignals == nil || t.ToolSignals == nil || t.KeywordWeights == nil {
		return out, false
	}
	if n := len(*t.Labels); n < 1 || n > len(TopicLabels) {
		return out, false
	}
	labels := map[string]bool{}
	for _, l := range *t.Labels {
		if !knownLabel(l) || labels[l] {
			return out, false
		}
		labels[l] = true
	}
	out.Labels = append([]string(nil), *t.Labels...)
	if !(*t.MinScore >= 0 && *t.MinScore <= 100) || !labels[*t.Fallback] {
		return out, false
	}
	out.MinScore, out.Fallback = *t.MinScore, *t.Fallback

	var ok bool
	if out.ExtensionSignals, ok = convertSignals(t.ExtensionSignals, maxExtensionSignals, validExtensionKey, labels); !ok {
		return out, false
	}
	if out.ToolSignals, ok = convertSignals(t.ToolSignals, maxToolSignals, validToolKey, labels); !ok {
		return out, false
	}
	out.KeywordWeights = make(map[string][]TermWeight, len(t.KeywordWeights))
	for label, terms := range t.KeywordWeights {
		if !labels[label] || len(terms) > maxTermsPerLabel {
			return out, false
		}
		conv := make([]TermWeight, 0, len(terms))
		for _, tw := range terms {
			if tw.Term == nil || tw.Weight == nil || !validPlainTerm(*tw.Term) || !validSignalWeight(*tw.Weight) {
				return out, false
			}
			conv = append(conv, TermWeight{Term: *tw.Term, Weight: *tw.Weight})
		}
		out.KeywordWeights[label] = conv
	}
	return out, true
}

func convertSignals(in map[string][]wireLabelWeight, maxKeys int, validKey func(string) bool, labels map[string]bool) (map[string][]LabelWeight, bool) {
	if len(in) > maxKeys {
		return nil, false
	}
	out := make(map[string][]LabelWeight, len(in))
	for key, sigs := range in {
		if !validKey(key) || len(sigs) < 1 || len(sigs) > maxSignalsPerKey {
			return nil, false
		}
		conv := make([]LabelWeight, 0, len(sigs))
		for _, s := range sigs {
			if s.Label == nil || s.Weight == nil || !labels[*s.Label] || !validSignalWeight(*s.Weight) {
				return nil, false
			}
			conv = append(conv, LabelWeight{Label: *s.Label, Weight: *s.Weight})
		}
		out[key] = conv
	}
	return out, true
}

func intIn(p *int64, lo, hi int64) (int, bool) {
	if p == nil || *p < lo || *p > hi {
		return 0, false
	}
	return int(*p), true
}

func validSignalWeight(w float64) bool { return w > 0 && w <= 10 && !math.IsNaN(w) }

func knownLabel(s string) bool {
	for _, l := range TopicLabels {
		if l == s {
			return true
		}
	}
	return false
}

// parseUTCTime accepts the contract's UTC form: YYYY-MM-DDTHH:MM:SS[.fraction]Z.
func parseUTCTime(p *string) (time.Time, bool) {
	if p == nil || len(*p) < 20 || len(*p) > 40 || !strings.HasSuffix(*p, "Z") {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, *p)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func validMinVersion(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if len(p) < 1 || len(p) > 6 {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

// validPlainTerm mirrors the contract's plain-substring rule: 2-40 characters, letters,
// digits, ASCII space, "'" and "-", starting and ending with a letter or digit, lowercase.
// NFC is not verified (no stdlib normalizer); the server emits NFC.
func validPlainTerm(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < 2 || n > maxTermRunes || !utf8.ValidString(s) || s != strings.ToLower(s) {
		return false
	}
	i := 0
	for _, r := range s {
		alnum := unicode.IsLetter(r) || unicode.IsNumber(r)
		if !alnum && r != ' ' && r != '\'' && r != '-' {
			return false
		}
		if !alnum && (i == 0 || i == n-1) {
			return false
		}
		i++
	}
	return true
}

// validExtensionKey: ^[._][a-z0-9_.+-]{1,24}$
func validExtensionKey(s string) bool {
	if len(s) < 2 || len(s) > 25 || (s[0] != '.' && s[0] != '_') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '+' || c == '-') {
			return false
		}
	}
	return true
}

// validToolKey: ^[A-Za-z0-9_.:-]{1,64}$
func validToolKey(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}
