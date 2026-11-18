// Package vscodecfg edits the VS Code user settings.json (JSONC) as text, so comments, indentation,
// key order and trailing-comma style survive. Only the three Copilot OTel keys are ever touched.
package vscodecfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"
)

const (
	KeyEnabled   = "github.copilot.chat.otel.enabled"
	KeyExporter  = "github.copilot.chat.otel.exporterType"
	KeyOutfile   = "github.copilot.chat.otel.outfile"
	KeyCapture   = "github.copilot.chat.otel.captureContent"
	BackupSuffix = ".harnext-backup"
	MaxSize      = 1 << 20
)

var (
	// ErrUnsafe means the file cannot be edited without guessing; the caller prints manual steps.
	ErrUnsafe   = errors.New("vscodecfg: settings.json cannot be edited safely")
	ErrTooLarge = errors.New("vscodecfg: settings.json is larger than 1 MiB")
)

var editions = []string{"Code", "Code - Insiders"}

// Roots returns the per-edition User directories under the OS user config directory.
func Roots() []string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return nil
	}
	roots := make([]string, 0, len(editions))
	for _, e := range editions {
		roots = append(roots, filepath.Join(base, e, "User"))
	}
	return roots
}

// Path is the settings file inside a User directory.
func Path(userDir string) string { return filepath.Join(userDir, "settings.json") }

// Targets returns the settings paths of the User directories that exist.
func Targets(roots []string) []string {
	var out []string
	for _, r := range roots {
		if fi, err := os.Stat(r); err == nil && fi.IsDir() {
			out = append(out, Path(r))
		}
	}
	return out
}

// Doc is a settings file as read from disk; Exists is false when the file is missing.
type Doc struct {
	Path   string
	Exists bool
	Raw    []byte
	Text   string // without BOM
	BOM    bool
	Mode   fs.FileMode
}

// Want describes the desired state for Apply.
type Want struct {
	Outfile string
	// Force overwrites a different exporterType.
	Force bool
	// AcceptOutfile reports whether an existing outfile value is kept as is; nil means "absolute path".
	AcceptOutfile func(string) bool
}

// RemoveWant holds the values harnext writes; Remove deletes a key only when it still has that value.
type RemoveWant struct{ Outfile string }

// Change describes one key; Action is "added", "updated", "removed" or "kept" (left alone on purpose).
type Change struct {
	Key    string
	Action string
	Old    any
	New    any
}

// Changed reports whether any change modifies the file.
func Changed(cs []Change) bool {
	for _, c := range cs {
		if c.Action != "kept" {
			return true
		}
	}
	return false
}

// Read loads a settings file; a missing file yields a Doc with Exists=false.
func Read(path string) (Doc, error) {
	d := Doc{Path: path, Mode: 0o644}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if !fi.Mode().IsRegular() {
		return d, ErrUnsafe
	}
	if fi.Size() > MaxSize {
		return d, ErrTooLarge
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	if len(raw) > MaxSize {
		return d, ErrTooLarge
	}
	if !utf8.Valid(raw) {
		return d, ErrUnsafe
	}
	d.Exists, d.Raw, d.Mode = true, raw, fi.Mode().Perm()
	text := string(raw)
	if strings.HasPrefix(text, "\ufeff") {
		d.BOM, text = true, text[len("\ufeff"):]
	}
	d.Text = text
	return d, nil
}

func (d Doc) prefix() string {
	if d.BOM {
		return "\ufeff"
	}
	return ""
}

// Inspect returns the decoded top-level values of a settings file (empty for a missing/blank file).
func Inspect(doc Doc) (map[string]any, error) {
	text, blank, err := normalize(doc.Text)
	if err != nil {
		return nil, err
	}
	if blank {
		return map[string]any{}, nil
	}
	return Decode(text)
}

// Apply returns the new file text and the per-key changes; the file itself is not touched.
func Apply(doc Doc, want Want) (string, []Change, error) {
	if want.Outfile == "" {
		return "", nil, errors.New("vscodecfg: empty outfile")
	}
	text, indent := doc.Text, "    "
	if !doc.Exists {
		text, indent = "{\n}\n", "  "
	}
	text, _, err := normalize(text)
	if err != nil {
		return "", nil, err
	}
	orig, err := Decode(text)
	if err != nil {
		return "", nil, err
	}
	accept := want.AcceptOutfile
	if accept == nil {
		accept = filepath.IsAbs
	}
	expect := map[string]any{}
	var changes []Change
	set := func(key string, v any) error {
		old, had := orig[key]
		action := "added"
		if had {
			action = "updated"
		}
		var e error
		text, e = edit(text, key, v, indent)
		changes = append(changes, Change{Key: key, Action: action, Old: old, New: v})
		expect[key] = v
		return e
	}
	if v, ok := orig[KeyEnabled]; !ok || v != true {
		if err := set(KeyEnabled, true); err != nil {
			return "", nil, err
		}
	}
	v, ok := orig[KeyExporter]
	s, isStr := v.(string)
	switch {
	case ok && s == "file":
	case ok && isStr && !want.Force:
		changes = append(changes, Change{Key: KeyExporter, Action: "kept", Old: v})
	default:
		if err := set(KeyExporter, "file"); err != nil {
			return "", nil, err
		}
	}
	v, ok = orig[KeyOutfile]
	if s, isStr := v.(string); ok && isStr && accept(s) {
		changes = append(changes, Change{Key: KeyOutfile, Action: "kept", Old: v})
	} else if err := set(KeyOutfile, want.Outfile); err != nil {
		return "", nil, err
	}
	if !Changed(changes) {
		return doc.prefix() + doc.Text, changes, nil
	}
	return finish(doc, text, orig, expect, changes)
}

// Remove deletes the keys that still hold the values harnext wrote; others are reported as "kept".
func Remove(doc Doc, want RemoveWant) (string, []Change, error) {
	if !doc.Exists {
		return "", nil, nil
	}
	text, blank, err := normalize(doc.Text)
	if err != nil {
		return "", nil, err
	}
	if blank {
		return doc.prefix() + doc.Text, nil, nil
	}
	orig, err := Decode(text)
	if err != nil {
		return "", nil, err
	}
	targets := []struct {
		key string
		val any
	}{{KeyEnabled, true}, {KeyExporter, "file"}, {KeyOutfile, want.Outfile}}
	expect := map[string]any{}
	var changes []Change
	for _, t := range targets {
		v, ok := orig[t.key]
		if !ok {
			continue
		}
		if v != t.val {
			changes = append(changes, Change{Key: t.key, Action: "kept", Old: v})
			continue
		}
		for {
			var removed bool
			if text, removed, err = removeKey(text, t.key); err != nil {
				return "", nil, err
			}
			if !removed {
				break
			}
		}
		changes = append(changes, Change{Key: t.key, Action: "removed", Old: v})
		expect[t.key] = nil
	}
	if !Changed(changes) {
		return doc.prefix() + doc.Text, changes, nil
	}
	return finish(doc, text, orig, expect, changes)
}

// Write backs up the original once, then replaces the file atomically; backup is "" for a new file.
func Write(doc Doc, newText string) (backup string, err error) {
	if len(newText) > MaxSize {
		return "", ErrTooLarge
	}
	target := doc.Path
	if li, e := os.Lstat(target); e == nil && li.Mode()&fs.ModeSymlink != 0 {
		if r, e := filepath.EvalSymlinks(target); e == nil {
			target = r
		}
	}
	if doc.Exists {
		backup = target + BackupSuffix
		f, e := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, doc.Mode.Perm())
		switch {
		case e == nil:
			_, we := f.Write(doc.Raw)
			if ce := f.Close(); we == nil {
				we = ce
			}
			if we != nil {
				return "", we
			}
		case !errors.Is(e, fs.ErrExist):
			return "", e
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".settings.json.harnext-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	fail := func(e error) (string, error) {
		tmp.Close()
		os.Remove(name)
		return "", e
	}
	if _, err := tmp.WriteString(newText); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(doc.Mode.Perm()); err != nil && !isWindows() {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Rename(name, target); err != nil {
		os.Remove(name)
		return "", err
	}
	return backup, nil
}

func isWindows() bool { return filepath.Separator == '\\' }

// finish enforces the safety net: the result must parse and differ from the input only in our keys.
func finish(doc Doc, text string, orig, expect map[string]any, changes []Change) (string, []Change, error) {
	after, err := Decode(text)
	if err != nil {
		return "", nil, fmt.Errorf("%w: result is not valid JSONC", ErrUnsafe)
	}
	for k, want := range expect {
		got, ok := after[k]
		if want == nil && ok || want != nil && got != want {
			return "", nil, fmt.Errorf("%w: result verification failed", ErrUnsafe)
		}
	}
	rest := func(m map[string]any) map[string]any {
		c := make(map[string]any, len(m))
		for k, v := range m {
			c[k] = v
		}
		for _, k := range []string{KeyEnabled, KeyExporter, KeyOutfile} {
			delete(c, k)
		}
		return c
	}
	if !reflect.DeepEqual(rest(orig), rest(after)) {
		return "", nil, fmt.Errorf("%w: result changed other settings", ErrUnsafe)
	}
	out := doc.prefix() + text
	if len(out) > MaxSize {
		return "", nil, ErrTooLarge
	}
	return out, changes, nil
}

// Decode parses JSONC (comments and trailing commas allowed) into a top-level object.
func Decode(text string) (map[string]any, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	if _, err := parseObject(text); err != nil {
		return nil, err
	}
	strict, err := strictify(text)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strict), &m); err != nil || m == nil {
		return nil, ErrUnsafe
	}
	return m, nil
}

// normalize turns a comment-only/blank file into "{}" text (VS Code treats it as empty settings).
func normalize(text string) (string, bool, error) {
	i, err := skipSpace(text, 0)
	if err != nil {
		return "", false, err
	}
	if i < len(text) {
		return text, false, nil
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + "{\n}\n", true, nil
}

type member struct {
	key                        string
	keyStart, valStart, valEnd int
	comma                      int // -1 when no comma follows
}

type object struct {
	close   int
	members []member
}

func skipSpace(s string, i int) (int, error) {
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return i, ErrUnsafe
			}
			i += j + 4
		default:
			return i, nil
		}
	}
	return i, nil
}

func scanString(s string, i int) (int, error) {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '"':
			return j + 1, nil
		case '\n':
			return i, ErrUnsafe
		}
	}
	return i, ErrUnsafe
}

func scanValue(s string, i int) (int, error) {
	switch s[i] {
	case '"':
		return scanString(s, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(s); {
			switch s[j] {
			case '"':
				e, err := scanString(s, j)
				if err != nil {
					return i, err
				}
				j = e
				continue
			case '/':
				k, err := skipSpace(s, j)
				if err != nil {
					return i, err
				}
				if k > j {
					j = k
					continue
				}
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
			j++
		}
		return i, ErrUnsafe
	}
	j := i
	for j < len(s) && !strings.ContainsRune(",}] \t\r\n/", rune(s[j])) {
		j++
	}
	if j == i {
		return i, ErrUnsafe
	}
	return j, nil
}

func parseObject(s string) (object, error) {
	var o object
	i, err := skipSpace(s, 0)
	if err != nil || i >= len(s) || s[i] != '{' {
		return o, ErrUnsafe
	}
	i++
	for {
		if i, err = skipSpace(s, i); err != nil || i >= len(s) {
			return o, ErrUnsafe
		}
		if s[i] == '}' {
			o.close = i
			i++
			break
		}
		if s[i] != '"' {
			return o, ErrUnsafe
		}
		ke, err := scanString(s, i)
		if err != nil {
			return o, err
		}
		m := member{keyStart: i, comma: -1}
		if json.Unmarshal([]byte(s[i:ke]), &m.key) != nil {
			return o, ErrUnsafe
		}
		if i, err = skipSpace(s, ke); err != nil || i >= len(s) || s[i] != ':' {
			return o, ErrUnsafe
		}
		if i, err = skipSpace(s, i+1); err != nil || i >= len(s) {
			return o, ErrUnsafe
		}
		m.valStart = i
		if m.valEnd, err = scanValue(s, i); err != nil {
			return o, err
		}
		if i, err = skipSpace(s, m.valEnd); err != nil || i >= len(s) {
			return o, ErrUnsafe
		}
		switch s[i] {
		case ',':
			m.comma = i
			i++
		case '}':
		default:
			return o, ErrUnsafe
		}
		o.members = append(o.members, m)
	}
	if i, err = skipSpace(s, i); err != nil || i != len(s) {
		return o, ErrUnsafe
	}
	return o, nil
}

// strictify drops comments and trailing commas so encoding/json can parse the text.
func strictify(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case '"':
			e, err := scanString(s, i)
			if err != nil {
				return "", err
			}
			b.WriteString(s[i:e])
			i = e
		case '/':
			j, err := skipSpace(s, i)
			if err != nil {
				return "", err
			}
			if j == i {
				b.WriteByte('/')
				j++
			} else {
				b.WriteByte(' ')
			}
			i = j
		case ',':
			j, err := skipSpace(s, i+1)
			if err != nil {
				return "", err
			}
			if j >= len(s) || (s[j] != '}' && s[j] != ']') {
				b.WriteByte(',')
			}
			i++
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String(), nil
}

func enc(v any) string {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return strings.TrimSpace(buf.String())
}

func lineBlankBefore(s string, pos int) (lineStart int, blank bool) {
	lineStart = strings.LastIndex(s[:pos], "\n") + 1
	return lineStart, strings.TrimLeft(s[lineStart:pos], " \t") == ""
}

func edit(text, key string, v any, indent string) (string, error) {
	o, err := parseObject(text)
	if err != nil {
		return "", err
	}
	for i := len(o.members) - 1; i >= 0; i-- {
		if m := o.members[i]; m.key == key {
			return text[:m.valStart] + enc(v) + text[m.valEnd:], nil
		}
	}
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	if len(o.members) > 0 {
		if ls, blank := lineBlankBefore(text, o.members[0].keyStart); blank {
			indent = text[ls:o.members[0].keyStart]
		}
	}
	line := indent + enc(key) + ": " + enc(v)
	var last member
	trailing := false
	if n := len(o.members); n > 0 {
		last = o.members[n-1]
		trailing = last.comma >= 0
	}
	if trailing {
		line += ","
	}
	pos, ins := o.close, eol+line+eol
	if ls, blank := lineBlankBefore(text, o.close); blank {
		pos, ins = ls, line+eol
	}
	if len(o.members) > 0 && !trailing {
		return text[:last.valEnd] + "," + text[last.valEnd:pos] + ins + text[pos:], nil
	}
	return text[:pos] + ins + text[pos:], nil
}

func removeKey(text, key string) (string, bool, error) {
	o, err := parseObject(text)
	if err != nil {
		return "", false, err
	}
	for i := len(o.members) - 1; i >= 0; i-- {
		m := o.members[i]
		if m.key != key {
			continue
		}
		start, end := m.keyStart, m.valEnd
		if m.comma >= 0 {
			end = m.comma + 1
		}
		// Last member without a comma: drop the previous member's comma to keep the style valid.
		dropPrev := -1
		if m.comma < 0 && i > 0 && o.members[i-1].comma >= 0 {
			dropPrev = o.members[i-1].comma
		}
		if ls, blank := lineBlankBefore(text, start); blank {
			j := end
			for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
				j++
			}
			switch {
			case strings.HasPrefix(text[j:], "\r\n"):
				start, end = ls, j+2
			case j < len(text) && text[j] == '\n':
				start, end = ls, j+1
			case j == len(text):
				start, end = ls, j
			}
		}
		out := text[:start] + text[end:]
		if dropPrev >= 0 {
			out = out[:dropPrev] + out[dropPrev+1:]
		}
		return out, true, nil
	}
	return text, false, nil
}
