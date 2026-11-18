package analyzer

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// matcher is a plain substring with an optional word-boundary requirement.
type matcher struct {
	pat   string
	whole bool
}

func foldRune(r rune) rune {
	switch r {
	case 'ı':
		return 'i'
	case '’':
		return '\''
	}
	return unicode.ToLower(r)
}

// count returns the number of non-overlapping matches in hay (already folded), stopping at limit.
func (m matcher) count(hay string, limit int) int {
	if m.pat == "" || limit <= 0 {
		return 0
	}
	n, from := 0, 0
	for from <= len(hay)-len(m.pat) {
		i := strings.Index(hay[from:], m.pat)
		if i < 0 {
			break
		}
		start := from + i
		end := start + len(m.pat)
		if m.whole && !wordBoundaries(hay, start, end) {
			from = start + 1
			continue
		}
		n++
		if n >= limit {
			break
		}
		from = end
	}
	return n
}

// wordBoundaries reports whether the runes around [start, end) are not letters or digits.
func wordBoundaries(s string, start, end int) bool {
	if start > 0 {
		if r, _ := utf8.DecodeLastRuneInString(s[:start]); isWordRune(r) {
			return false
		}
	}
	if end < len(s) {
		if r, _ := utf8.DecodeRuneInString(s[end:]); isWordRune(r) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }
