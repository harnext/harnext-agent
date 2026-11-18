package analyzer

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// cleaned is the result of preprocessing one message part. text is a temporary view used only
// inside UserFeatures; it is never stored (zero-prompt-retention).
type cleaned struct {
	text        string
	totalRunes  int
	errorRunes  int // runes removed as error output (stack traces, error logs)
	interrupted bool
}

var (
	// Claude Code writes these markers as user lines when the user stops a response.
	interruptPrefixes = []string{"[request interrupted by user"}

	// Tool and harness text injected into the user turn, not typed by the person.
	injectedTags = []*regexp.Regexp{
		regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`),
		regexp.MustCompile(`(?s)<command-name>.*?</command-name>`),
		regexp.MustCompile(`(?s)<command-message>.*?</command-message>`),
		regexp.MustCompile(`(?s)<command-args>.*?</command-args>`),
		regexp.MustCompile(`(?s)<user-prompt-submit-hook>.*?</user-prompt-submit-hook>`),
	}
	// Command output blocks: removed and counted as error output when they look like errors.
	outputTags = []*regexp.Regexp{
		regexp.MustCompile(`(?s)<local-command-stdout>(.*?)</local-command-stdout>`),
		regexp.MustCompile(`(?s)<local-command-stderr>(.*?)</local-command-stderr>`),
		regexp.MustCompile(`(?s)<bash-stdout>(.*?)</bash-stdout>`),
		regexp.MustCompile(`(?s)<bash-stderr>(.*?)</bash-stderr>`),
	}
	inlineCode = regexp.MustCompile("`[^`\n]{1,400}`")
	urlRe      = regexp.MustCompile(`(?i)\b(?:https?|file)://\S+`)
	// Path-like tokens: contain a slash or backslash between word characters, no spaces.
	pathRe = regexp.MustCompile(`(?:[~.]?[/\\][\w.\-@]+){2,}[/\\]?|\b[\w.\-]+(?:[/\\][\w.\-]+){2,}`)
	// Lines that look like error output or stack frames.
	errorLineRe = regexp.MustCompile(`(?i)(error|exception|traceback|panic|fatal|failed|undefined|cannot|not found|exit (code|status)|segmentation fault|^\s+at \S+|^\s+file "|:\d+:\d+|\.(go|ts|tsx|js|py|java|rb|rs):\d+)`)
)

// preprocess strips code, logs, paths, URLs, quotes and injected tool text so that patterns
// only see what the person wrote. It reports how much of the message was error output.
func preprocess(text string) cleaned {
	c := cleaned{totalRunes: utf8.RuneCountInString(text)}
	trimmed := strings.TrimSpace(text)
	low := strings.ToLower(trimmed)
	for _, p := range interruptPrefixes {
		if strings.HasPrefix(low, p) {
			c.interrupted = true
			return c
		}
	}
	for _, re := range injectedTags {
		text = re.ReplaceAllString(text, " ")
	}
	for _, re := range outputTags {
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			if errorLineRe.MatchString(m) {
				c.errorRunes += utf8.RuneCountInString(m)
			}
			return " "
		})
	}

	var kept strings.Builder
	inFence := false
	fenceErr := 0
	var fenceRunes int
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			if inFence {
				if fenceErr > 0 {
					c.errorRunes += fenceRunes
				}
				fenceErr, fenceRunes = 0, 0
			}
			inFence = !inFence
			continue
		}
		if inFence {
			fenceRunes += utf8.RuneCountInString(line) + 1
			if errorLineRe.MatchString(line) {
				fenceErr++
			}
			continue
		}
		if strings.HasPrefix(t, ">") {
			continue // quoted text is someone else's words
		}
		if looksLikeOutput(line) {
			c.errorRunes += utf8.RuneCountInString(line) + 1
			continue
		}
		kept.WriteString(line)
		kept.WriteByte('\n')
	}
	if inFence && fenceErr > 0 { // unterminated fence
		c.errorRunes += fenceRunes
	}
	out := kept.String()
	out = inlineCode.ReplaceAllString(out, " ")
	out = urlRe.ReplaceAllString(out, " ")
	out = pathRe.ReplaceAllString(out, " ")
	c.text = out
	return c
}

// looksLikeOutput flags pasted log or stack-trace lines outside fences: an error marker plus
// either leading indentation, a file:line reference or no lowercase prose words around it.
func looksLikeOutput(line string) bool {
	if !errorLineRe.MatchString(line) {
		return false
	}
	if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
		return true
	}
	t := strings.TrimSpace(line)
	// "Error: …", "TypeError: …", "FAIL src/…", "panic: …" and similar headers.
	if i := strings.IndexByte(t, ':'); i > 0 && i < 40 && !strings.ContainsRune(t[:i], ' ') {
		return true
	}
	return strings.HasPrefix(t, "FAIL") || strings.HasPrefix(t, "ERROR") || strings.HasPrefix(t, "Traceback")
}

// asciiFold lowercases text (Turkish dotted/dotless I aware) and maps Turkish and common
// Latin letters to ASCII, so "Görüşürüz", "gorusuruz" and "GÖRÜŞÜRÜZ" all become "gorusuruz".
func asciiFold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case 'İ', 'I', 'ı', 'î', 'Î', 'í', 'ì':
			r = 'i'
		case 'ç', 'Ç':
			r = 'c'
		case 'ğ', 'Ğ':
			r = 'g'
		case 'ö', 'Ö', 'ó', 'ò':
			r = 'o'
		case 'ş', 'Ş':
			r = 's'
		case 'ü', 'Ü', 'û', 'Û', 'ú', 'ù':
			r = 'u'
		case 'â', 'Â', 'á', 'à', 'ä':
			r = 'a'
		case 'é', 'è', 'ê', 'ë':
			r = 'e'
		case '’', '‘', '`':
			r = '\''
		default:
			if unicode.Is(unicode.Mn, r) {
				continue // combining marks
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// words splits folded text into words (letters, digits and apostrophes).
func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '\'')
	})
}
