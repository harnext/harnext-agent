package gitid

import "strings"

const maxPatternLen = 1024

// globMatch is git's wildmatch for paths: * and ? stop at "/", "**" crosses it when it forms a whole path segment.
func globMatch(pattern, path string) bool {
	if len(pattern) > maxPatternLen {
		return false
	}
	return wild(pattern, path, true)
}

// boundary is true while the pattern is at its start or just after a "/".
func wild(p, s string, boundary bool) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			n := 0
			for n < len(p) && p[n] == '*' {
				n++
			}
			rest := p[n:]
			if n >= 2 && boundary {
				if rest == "" {
					return true
				}
				if rest[0] == '/' {
					rest = rest[1:]
					for i := 0; ; {
						if wild(rest, s[i:], true) {
							return true
						}
						j := strings.IndexByte(s[i:], '/')
						if j < 0 {
							return false
						}
						i += j + 1
					}
				}
			}
			for i := 0; i <= len(s); i++ {
				if wild(rest, s[i:], false) {
					return true
				}
				if i == len(s) || s[i] == '/' {
					return false
				}
			}
			return false
		case '?':
			if s == "" || s[0] == '/' {
				return false
			}
			boundary = false
		default:
			if s == "" || s[0] != p[0] {
				return false
			}
			boundary = p[0] == '/'
		}
		p, s = p[1:], s[1:]
	}
	return s == ""
}
