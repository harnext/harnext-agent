package gitid

import (
	"path/filepath"
	"strings"
)

// parse walks one config file in order and applies user.email, include and includeIf as git does.
func (s *state) parse(text, path string, depth int) {
	text = strings.TrimPrefix(text, "\xef\xbb\xbf")
	var section, sub string
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			var ok bool
			if section, sub, line, ok = parseHeader(line); !ok {
				section, sub = "", ""
				continue
			}
		}
		key, val, ok := parseKeyValue(line)
		if !ok {
			continue
		}
		switch {
		case section == "user" && sub == "" && key == "email":
			s.email = strings.ToLower(strings.TrimSpace(val))
		case key == "path" && section == "include" && sub == "":
			s.include(val, path, depth)
		case key == "path" && section == "includeif" && s.conditionMatches(sub, path):
			s.include(val, path, depth)
		}
	}
}

// parseHeader splits `[section]`, `[section "sub"]` and an optional key/value on the same line.
func parseHeader(line string) (section, sub, rest string, ok bool) {
	i := 1
	for i < len(line) && line[i] != ']' && line[i] != '"' && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	section = strings.ToLower(line[1:i])
	if section == "" || i >= len(line) {
		return "", "", "", false
	}
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i < len(line) && line[i] == '"' {
		var b strings.Builder
		i++
		for ; i < len(line) && line[i] != '"'; i++ {
			if line[i] == '\\' && i+1 < len(line) {
				i++
			}
			b.WriteByte(line[i])
		}
		if i >= len(line) {
			return "", "", "", false
		}
		sub = b.String()
		i++
	}
	if i >= len(line) || line[i] != ']' {
		return "", "", "", false
	}
	return section, sub, strings.TrimSpace(line[i+1:]), true
}

// parseKeyValue returns ok=false for comments, keys without a value and values that use unsupported syntax.
func parseKeyValue(line string) (key, val string, ok bool) {
	j := 0
	for j < len(line) && (isAlnum(line[j]) || (j > 0 && line[j] == '-')) {
		j++
	}
	if j == 0 || !isAlpha(line[0]) {
		return "", "", false
	}
	rest := strings.TrimSpace(line[j:])
	if !strings.HasPrefix(rest, "=") {
		return "", "", false
	}
	val, ok = parseValue(rest[1:])
	return strings.ToLower(line[:j]), val, ok
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isAlnum(c byte) bool { return isAlpha(c) || c >= '0' && c <= '9' }

// parseValue handles quotes, escapes and inline comments; a trailing backslash (line continuation) is unsupported.
func parseValue(s string) (string, bool) {
	s = strings.TrimSpace(s)
	var b []byte
	inQuote := false
	keep := 0 // length of b without trailing unquoted spaces
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			if i+1 >= len(s) {
				return "", false
			}
			i++
			switch s[i] {
			case 'n':
				c = '\n'
			case 't':
				c = '\t'
			case 'b':
				c = '\b'
			case '"', '\\':
				c = s[i]
			default:
				return "", false
			}
			b = append(b, c)
			keep = len(b)
		case c == '"':
			inQuote = !inQuote
			keep = len(b)
		case !inQuote && (c == '#' || c == ';'):
			i = len(s)
		case !inQuote && (c == ' ' || c == '\t'):
			b = append(b, c)
		default:
			b = append(b, c)
			keep = len(b)
		}
	}
	if inQuote {
		return "", false
	}
	return string(b[:keep]), true
}

// include reads another config file in place; its relative path is relative to the including file.
func (s *state) include(p, from string, depth int) {
	if p == "" || depth+1 > maxIncludeDepth {
		return
	}
	p = filepath.FromSlash(p)
	switch {
	case strings.HasPrefix(p, "~"+string(filepath.Separator)):
		if s.r.home == "" {
			return
		}
		p = filepath.Join(s.r.home, p[2:])
	case !filepath.IsAbs(p):
		p = filepath.Join(filepath.Dir(from), p)
	}
	s.file(p, depth+1)
}

// conditionMatches evaluates `gitdir:` and `gitdir/i:`; onbranch, hasconfig and anything else never match.
func (s *state) conditionMatches(cond, from string) bool {
	fold := false
	var pattern string
	switch {
	case hasPrefixFold(cond, "gitdir:"):
		pattern = cond[len("gitdir:"):]
	case hasPrefixFold(cond, "gitdir/i:"):
		pattern, fold = cond[len("gitdir/i:"):], true
	default:
		return false
	}
	if s.gitDir == "" {
		return false
	}
	pattern, ok := s.expandPattern(pattern, from)
	if !ok {
		return false
	}
	candidates := []string{filepath.ToSlash(s.gitDir)}
	if real, err := filepath.EvalSymlinks(s.gitDir); err == nil && real != s.gitDir {
		candidates = append(candidates, filepath.ToSlash(real))
	}
	if fold {
		pattern = strings.ToLower(pattern)
	}
	for _, c := range candidates {
		if fold {
			c = strings.ToLower(c)
		}
		if globMatch(pattern, c) {
			return true
		}
	}
	return false
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// expandPattern applies git's rules: ~/ and ./ prefixes, an implicit leading **/ for relative patterns and a trailing /
// meaning /**.
func (s *state) expandPattern(p, from string) (string, bool) {
	p = filepath.ToSlash(p)
	switch {
	case strings.HasPrefix(p, "~/"):
		if s.r.home == "" {
			return "", false
		}
		p = strings.TrimSuffix(filepath.ToSlash(s.r.home), "/") + p[1:]
	case strings.HasPrefix(p, "./"):
		p = strings.TrimSuffix(filepath.ToSlash(filepath.Dir(from)), "/") + p[1:]
	case !isAbsSlash(p):
		p = "**/" + p
	}
	if strings.HasSuffix(p, "/") {
		p += "**"
	}
	return p, true
}

func isAbsSlash(p string) bool {
	return strings.HasPrefix(p, "/") || len(p) >= 3 && isAlpha(p[0]) && p[1] == ':' && p[2] == '/'
}
