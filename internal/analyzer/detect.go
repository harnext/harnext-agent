package analyzer

import (
	"errors"
	"regexp"
	"strings"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

// feedbackRule is one compiled feedback pattern. The pattern text comes from the verified
// configuration; nothing here is derived from message text.
type feedbackRule struct {
	category string
	lang     string
	re       *regexp.Regexp // word-bounded search, or anchored when whole
	whole    bool
}

type negator struct {
	lang   string
	word   string
	before bool
}

type detector struct {
	rules    []feedbackRule
	negators []negator
}

// compileDetector turns the configuration's restricted patterns into RE2 expressions.
// Word boundaries are Unicode-aware: a match must be preceded and followed by a non-letter,
// non-digit character or the edge of the text (RE2's \b is ASCII-only and unusable here).
func compileDetector(f remoteconfig.Friction) (detector, error) {
	var d detector
	// Patterns with the same category, language and mode are joined into one alternation, so
	// the detector runs a handful of expressions instead of one per pattern.
	type key struct {
		category, lang string
		whole          bool
	}
	groups := map[key][]string{}
	var order []key
	for _, p := range f.Patterns {
		if !remoteconfig.ValidPattern(p.Pattern) {
			return d, errors.New("analyzer: invalid feedback pattern")
		}
		k := key{p.Category, p.Lang, p.WholeMessage}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], "(?:"+p.Pattern+")")
	}
	for _, k := range order {
		alt := strings.Join(groups[k], "|")
		var expr string
		if k.whole {
			expr = `^(?:` + alt + `)$`
		} else {
			expr = `(?:^|[^\pL\pN'])(` + alt + `)(?:[^\pL\pN']|$)`
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return d, errors.New("analyzer: feedback pattern does not compile")
		}
		d.rules = append(d.rules, feedbackRule{category: k.category, lang: k.lang, re: re, whole: k.whole})
	}
	for _, n := range f.Negators {
		d.negators = append(d.negators, negator{lang: n.Lang, word: n.Word, before: n.Position == "before"})
	}
	return d, nil
}

// feedback is the category result for one message part.
type feedback struct {
	errorReport bool
	correction  bool
	positiveAck bool
}

func (f *feedback) set(category string) {
	switch category {
	case remoteconfig.CategoryErrorReport:
		f.errorReport = true
	case remoteconfig.CategoryCorrection:
		f.correction = true
	case remoteconfig.CategoryPositiveAck:
		f.positiveAck = true
	}
}

// detect runs every rule on folded, cleaned text. A non-whole match is skipped when a
// negator of the same language sits within two words before (or after) it.
func (d *detector) detect(folded string) feedback {
	var out feedback
	ws := words(folded)
	whole := strings.Join(ws, " ")
	for i := range d.rules {
		r := &d.rules[i]
		if r.whole {
			if whole != "" && r.re.MatchString(whole) {
				out.set(r.category)
			}
			continue
		}
		for _, m := range r.re.FindAllStringSubmatchIndex(folded, -1) {
			start, end := m[2], m[3]
			if !d.negated(r.lang, folded[:start], folded[end:]) {
				out.set(r.category)
				break
			}
		}
	}
	return out
}

func (d *detector) negated(lang, before, after string) bool {
	if len(d.negators) == 0 {
		return false
	}
	var prev, next []string
	for _, n := range d.negators {
		if n.lang != lang {
			continue
		}
		if n.before {
			if prev == nil {
				prev = lastWords(before, 2)
			}
			if contains(prev, n.word) {
				return true
			}
		} else {
			if next == nil {
				next = firstWords(after, 2)
			}
			if contains(next, n.word) {
				return true
			}
		}
	}
	return false
}

func lastWords(s string, n int) []string {
	w := words(s)
	if len(w) > n {
		w = w[len(w)-n:]
	}
	return w
}

func firstWords(s string, n int) []string {
	w := words(s)
	if len(w) > n {
		w = w[:n]
	}
	return w
}

func contains(ws []string, w string) bool {
	for _, x := range ws {
		if x == w {
			return true
		}
	}
	return false
}
