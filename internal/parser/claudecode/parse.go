package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
)

// maxTokenValue mirrors the ingest contract's per-field maximum.
const maxTokenValue = 100_000_000_000

// maxFieldBytes bounds identifier-like strings; longer values are dropped as if absent.
const maxFieldBytes = 1024

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// ParseLine interprets one transcript line without content analysis. It never panics and
// never returns an error; anything it cannot use is reported as Skipped with a fixed reason code.
func ParseLine(line []byte) Event { return parseLine(line, nil) }

func parseLine(line []byte, ex analyzer.Extractor) (ev Event) {
	defer func() {
		if recover() != nil {
			ev = Skipped{Reason: ReasonInternal}
		}
	}()

	line = bytes.TrimSpace(bytes.TrimPrefix(line, utf8BOM))
	if len(line) == 0 {
		return Skipped{Reason: ReasonEmpty}
	}

	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return Skipped{Reason: ReasonNotObject}
		}
		return Skipped{Reason: ReasonInvalidJSON}
	}

	switch typ := string(env.Type); typ {
	case "":
		return Skipped{Reason: ReasonMissingType}
	case "assistant":
		return env.assistant(ex)
	case "user":
		return env.user(ex)
	default:
		return Ignored{Type: sanitizeType(typ)}
	}
}

func (e *envelope) assistant(ex analyzer.Extractor) Event {
	if e.SessionID == "" {
		return Skipped{Reason: ReasonMissingSession}
	}
	if !e.Message.OK || !e.Message.V.Usage.OK {
		return Skipped{Reason: ReasonMissingUsage}
	}
	msg := &e.Message.V
	usage, reason := msg.Usage.V.validate()
	if reason != "" {
		return Skipped{Reason: reason}
	}
	au := AssistantUsage{
		SessionID:   string(e.SessionID),
		MessageID:   string(msg.ID),
		RequestID:   string(e.RequestID),
		Model:       string(msg.Model),
		Timestamp:   parseTime(string(e.Timestamp)),
		Cwd:         string(e.Cwd),
		GitBranch:   string(e.GitBranch),
		ToolVersion: string(e.Version),
		IsSidechain: bool(e.IsSidechain),
		Usage:       usage,
		StopReason:  string(msg.StopReason),
	}
	if ex != nil {
		au.Tools, au.ToolCount = extractToolUses(msg.Content.raw, ex)
	}
	return au
}

func (e *envelope) user(ex analyzer.Extractor) Event {
	if e.SessionID == "" {
		return Skipped{Reason: ReasonMissingSession}
	}
	content := &e.Message.V.Content
	turn := UserTurn{
		SessionID:    string(e.SessionID),
		UUID:         string(e.UUID),
		Timestamp:    parseTime(string(e.Timestamp)),
		Cwd:          string(e.Cwd),
		GitBranch:    string(e.GitBranch),
		IsSidechain:  bool(e.IsSidechain),
		IsToolResult: content.toolResult,
		IsError:      content.isError,
		Interrupted:  bool(e.ToolUseResult.V.Interrupted),
	}
	// Sub-agent lines are not analyzed: their prompts are written by the parent agent.
	if ex == nil || turn.IsSidechain {
		// Without analysis the blocks are still counted (flags only, no fingerprint).
		turn.ResultCount = content.n
		for i := 0; i < content.n; i++ {
			turn.Results[i] = ToolResult{IsError: content.blocks[i].isError, Denied: content.blocks[i].denied}
		}
		return turn
	}
	turn.Analyzed = true
	if !turn.IsToolResult {
		turn.Features = userFeatures(ex, content.raw)
		return turn
	}
	turn.ResultCount = content.n
	for i := 0; i < content.n; i++ {
		b := &content.blocks[i]
		turn.Results[i] = ToolResult{IsError: b.isError, Denied: b.denied}
		if b.isError && !b.denied {
			turn.Results[i].ErrorHash = analyzer.ErrorFingerprint(b.errRaw)
		}
	}
	return turn
}

// envelope declares only the fields we need; every other key, including message text
// and tool output, is skipped by encoding/json without being copied.
type envelope struct {
	Type          lenStr                `json:"type"`
	SessionID     lenStr                `json:"sessionId"`
	UUID          lenStr                `json:"uuid"`
	Timestamp     lenStr                `json:"timestamp"`
	Cwd           lenStr                `json:"cwd"`
	GitBranch     lenStr                `json:"gitBranch"`
	Version       lenStr                `json:"version"`
	RequestID     lenStr                `json:"requestId"`
	IsSidechain   lenBool               `json:"isSidechain"`
	Message       object[message]       `json:"message"`
	ToolUseResult object[toolUseResult] `json:"toolUseResult"`
}

type message struct {
	ID         lenStr        `json:"id"`
	Model      lenStr        `json:"model"`
	StopReason lenStr        `json:"stop_reason"`
	Usage      object[usage] `json:"usage"`
	Content    content       `json:"content"`
}

type usage struct {
	Input         tokenCount                `json:"input_tokens"`
	Output        tokenCount                `json:"output_tokens"`
	CacheRead     tokenCount                `json:"cache_read_input_tokens"`
	CacheCreation tokenCount                `json:"cache_creation_input_tokens"`
	CacheDetail   object[cacheDetail]       `json:"cache_creation"`
	OutputDetail  object[outputTokenDetail] `json:"output_tokens_details"`
	Speed         lenStr                    `json:"speed"`
	ServerTools   object[serverToolUse]     `json:"server_tool_use"`
}

type serverToolUse struct {
	WebSearch tokenCount `json:"web_search_requests"`
	WebFetch  tokenCount `json:"web_fetch_requests"`
}

type cacheDetail struct {
	OneHour tokenCount `json:"ephemeral_1h_input_tokens"`
}

type outputTokenDetail struct {
	Thinking tokenCount `json:"thinking_tokens"`
}

// speedOf maps usage.speed to a contract value; anything unexpected is unknown (priced as standard).
func speedOf(s string) string {
	switch s {
	case SpeedStandard, SpeedFast:
		return s
	}
	return SpeedUnknown
}

type toolUseResult struct {
	Interrupted lenBool `json:"interrupted"`
}

func (u *usage) validate() (Usage, SkipReason) {
	fields := [...]*tokenCount{
		&u.Input, &u.Output, &u.CacheRead, &u.CacheCreation,
		&u.CacheDetail.V.OneHour, &u.OutputDetail.V.Thinking,
		&u.ServerTools.V.WebSearch, &u.ServerTools.V.WebFetch,
	}
	for _, f := range fields {
		if f.bad {
			return Usage{}, ReasonBadTokens
		}
		if f.n < 0 || f.n > maxTokenValue {
			return Usage{}, ReasonOutOfRange
		}
	}
	if u.CacheDetail.V.OneHour.n > u.CacheCreation.n {
		return Usage{}, ReasonOutOfRange
	}
	return Usage{
		InputTokens:           u.Input.n,
		OutputTokens:          u.Output.n,
		CacheReadTokens:       u.CacheRead.n,
		CacheCreationTokens:   u.CacheCreation.n,
		CacheCreation1hTokens: u.CacheDetail.V.OneHour.n,
		ThinkingTokens:        u.OutputDetail.V.Thinking.n,
		Speed:                 speedOf(string(u.Speed)),
		WebSearchRequests:     u.ServerTools.V.WebSearch.n,
		WebFetchRequests:      u.ServerTools.V.WebFetch.n,
	}, ""
}

// lenStr ignores non-string values and oversized strings instead of failing the line.
type lenStr string

func (s *lenStr) UnmarshalJSON(b []byte) error {
	if len(b) < 2 || b[0] != '"' {
		return nil
	}
	inner := b[1 : len(b)-1]
	if len(inner) > maxFieldBytes {
		return nil
	}
	if bytes.IndexByte(inner, '\\') < 0 {
		*s = lenStr(inner)
		return nil
	}
	var v string
	if json.Unmarshal(b, &v) == nil {
		*s = lenStr(v)
	}
	return nil
}

// lenBool ignores non-boolean values.
type lenBool bool

func (v *lenBool) UnmarshalJSON(b []byte) error {
	*v = string(b) == "true"
	return nil
}

// tokenCount records non-integer values as bad rather than failing the line.
type tokenCount struct {
	n   int64
	bad bool
}

func (t *tokenCount) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		t.bad = true
		return nil
	}
	t.n = n
	return nil
}

// object decodes a JSON object leniently: anything else leaves OK false.
type object[T any] struct {
	V  T
	OK bool
}

func (o *object[T]) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '{' {
		o.OK = json.Unmarshal(b, &o.V) == nil
	}
	return nil
}

// content classifies message.content. raw and errRaw alias the input line (no copy); they
// are only valid during the ParseLine call and are never stored in an event.
type content struct {
	raw        []byte
	toolResult bool
	isError    bool
	blocks     [MaxToolResultsPerLine]resultBlock
	n          int
}

type resultBlock struct {
	isError bool
	denied  bool
	errRaw  []byte // raw JSON of the block's content field (aliases the input line)
}

// Claude Code's fixed refusal texts when the user rejects a tool permission (lowercased).
var deniedMarkers = [][]byte{
	[]byte("doesn't want to proceed with this tool use"),
	[]byte("doesn't want to take this action"),
	[]byte("tool use was rejected"),
}

func isDenied(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	low := bytes.ToLower(raw[:min(len(raw), 2048)])
	for _, m := range deniedMarkers {
		if bytes.Contains(low, m) {
			return true
		}
	}
	return false
}

// rawJSON captures a value as a slice of the input without copying it.
type rawJSON []byte

func (r *rawJSON) UnmarshalJSON(b []byte) error {
	*r = b
	return nil
}

func (c *content) UnmarshalJSON(b []byte) error {
	c.raw = b
	if len(b) == 0 || b[0] != '[' {
		return nil
	}
	var blocks []struct {
		Type    lenStr  `json:"type"`
		IsError lenBool `json:"is_error"`
		Content rawJSON `json:"content"`
	}
	// Element type errors are ignored on purpose; valid elements are still decoded.
	_ = json.Unmarshal(b, &blocks)
	for i := range blocks {
		if blocks[i].Type != "tool_result" {
			continue
		}
		c.toolResult = true
		isErr := bool(blocks[i].IsError)
		c.isError = c.isError || isErr
		if c.n < len(c.blocks) {
			c.blocks[c.n] = resultBlock{isError: isErr, errRaw: blocks[i].Content, denied: isErr && isDenied(blocks[i].Content)}
			c.n++
		}
	}
	return nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// sanitizeType keeps type names short and identifier-like so arbitrary content cannot leak into counters.
func sanitizeType(s string) string {
	if len(s) == 0 || len(s) > 40 {
		return "_invalid"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
		if !ok {
			return "_invalid"
		}
	}
	return s
}
