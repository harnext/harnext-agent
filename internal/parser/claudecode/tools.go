package claudecode

import (
	"encoding/json"
	"strings"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/dirpath"
)

// userFeatures decodes the text of a real user message only for the duration of this call
// and returns counters. The decoded text is not stored or returned.
func userFeatures(ex analyzer.Extractor, raw []byte) (f analyzer.UserFeatures) {
	if len(raw) == 0 {
		return f
	}
	switch raw[0] {
	case '"':
		var text string
		if json.Unmarshal(raw, &text) == nil {
			f = ex.UserFeatures(text)
		}
	case '[':
		var blocks []struct {
			Type lenStr `json:"type"`
			Text string `json:"text"`
		}
		// Element type errors are ignored on purpose; valid elements are still decoded.
		_ = json.Unmarshal(raw, &blocks)
		for i := range blocks {
			if blocks[i].Type == "text" {
				f.Merge(ex.UserFeatures(blocks[i].Text))
			}
		}
	}
	return f
}

// extractToolUses reads only type, id, name and input.file_path / input.notebook_path of each content block.
// Everything else (text, commands, file contents) is skipped by the decoder without being copied.
// A file suffix is kept only if the configuration has a signal for exactly that suffix, so
// fragments of file names ("_secret.go") never leave this function. Of the path only the FOLDER part is
// kept; the file name
// is dropped here and the folder never leaves the machine as a full path (the aggregator reduces it to a
// folder inside the project).
func extractToolUses(raw []byte, ex analyzer.Extractor) (tools [MaxToolUsesPerLine]ToolUse, n int) {
	if len(raw) == 0 || raw[0] != '[' {
		return tools, 0
	}
	configured := ex.Settings().Topics.ExtensionSignals
	var blocks []struct {
		Type  lenStr `json:"type"`
		ID    lenStr `json:"id"`
		Name  lenStr `json:"name"`
		Input object[struct {
			FilePath     lenStr `json:"file_path"`
			NotebookPath lenStr `json:"notebook_path"`
		}] `json:"input"`
	}
	_ = json.Unmarshal(raw, &blocks)
	for i := range blocks {
		b := &blocks[i]
		if b.Type != "tool_use" || !validToolName(string(b.Name)) {
			continue
		}
		if n == MaxToolUsesPerLine {
			break
		}
		path := string(b.Input.V.FilePath)
		if path == "" {
			path = string(b.Input.V.NotebookPath)
		}
		ext, compound := pathSuffixes(path)
		if _, ok := configured[ext]; !ok {
			ext = ""
		}
		if _, ok := configured[compound]; !ok {
			compound = ""
		}
		tools[n] = ToolUse{ID: string(b.ID), Name: string(b.Name), Ext: ext, CompoundExt: compound, Dir: dirpath.Dir(path)}
		n++
	}
	return tools, n
}

// pathSuffixes derives ".go" and "_test.go" style suffixes from a file path. Anything that
// is not extension-like yields "". The directory part and the base name are dropped.
func pathSuffixes(path string) (ext, compound string) {
	base := path
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if len(base) == 0 || len(base) > 255 {
		return "", ""
	}
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 {
		return "", ""
	}
	ext = strings.ToLower(base[dot:])
	if !validExt(ext) {
		return "", ""
	}
	if j := strings.LastIndexAny(base[:dot], "._"); j >= 0 {
		if c := strings.ToLower(base[j:]); validCompoundExt(c) {
			compound = c
		}
	}
	return ext, compound
}

// validExt: ^\.[a-z0-9]{1,8}$
func validExt(s string) bool {
	if len(s) < 2 || len(s) > 9 || s[0] != '.' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// validCompoundExt: ^[._][a-z0-9_.+-]{1,24}$ (the contract's extension_signals key pattern)
func validCompoundExt(s string) bool {
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

// validToolName: ^[A-Za-z0-9_.:-]{1,64}$ (the contract's tool_signals key pattern)
func validToolName(s string) bool {
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
