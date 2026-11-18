// Package claudecode turns single Claude Code transcript lines into typed events.
//
// Zero-prompt-retention: message text, tool output, commands and file names are never copied
// into events. With an analyzer.Extractor the text of a real user message is decoded only
// for the duration of the call and reduced to counters (analyzer.UserFeatures); tool_use
// blocks contribute only the tool name, short validated file-name suffixes and the folder part
// of the file path (never the file name); error
// tool_results contribute a normalized hash. Without an Extractor no content is analyzed.
package claudecode

import (
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
)

// Event is one of AssistantUsage, UserTurn, Ignored or Skipped.
type Event interface{ isEvent() }

// Usage holds the token counts of one API response.
type Usage struct {
	InputTokens           int64
	OutputTokens          int64
	CacheReadTokens       int64
	CacheCreationTokens   int64
	CacheCreation1hTokens int64
	// ThinkingTokens is informational; it is assumed to be part of OutputTokens (unverified).
	ThinkingTokens int64
	// Speed is usage.speed: SpeedStandard, SpeedFast or SpeedUnknown (absent or other value).
	Speed string
	// Server-side tool calls billed per request (usage.server_tool_use).
	WebSearchRequests int64
	WebFetchRequests  int64
}

// Speed values; they match the ingest contract.
const (
	SpeedStandard = "standard"
	SpeedFast     = "fast"
	SpeedUnknown  = "unknown"
)

// PromptTokens is the whole prompt of the request: uncached input plus cache reads and writes.
func (u Usage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// MaxToolUsesPerLine bounds the tool_use blocks reported per assistant line; extras are ignored.
const MaxToolUsesPerLine = 8

// ToolUse is the only thing kept from a tool_use block. ID is used for deduplication
// (the same response is repeated across lines); Ext and CompoundExt are lowercase
// file-name suffixes such as ".go" and "_test.go", empty when absent or not extension-like.
// Dir is the FOLDER part of the file path the tool works on (input.file_path or notebook_path), absolute and
// without the file name, "" when absent; the product owner allowed reading it locally (2026-11-15). It is the only
// part of a path kept: the file name and the rest of the tool input (commands, contents) are never decoded or
// copied, and the aggregator turns Dir into a folder inside the project before anything leaves the machine.
type ToolUse struct {
	ID          string
	Name        string
	Ext         string
	CompoundExt string
	Dir         string
}

// AssistantUsage is a line carrying the usage of an API response. The same response is
// written once per content block, so callers must deduplicate by (MessageID, RequestID).
type AssistantUsage struct {
	SessionID   string
	MessageID   string
	RequestID   string
	Model       string
	Timestamp   time.Time // zero when absent or unparsable
	Cwd         string
	GitBranch   string
	ToolVersion string
	IsSidechain bool
	Usage       Usage
	StopReason  string
	// Tools lists tool_use blocks (first ToolCount entries); filled only when the parser
	// has an Extractor.
	Tools     [MaxToolUsesPerLine]ToolUse
	ToolCount int
}

// MaxToolResultsPerLine bounds the tool_result blocks reported per user line; extras are ignored.
const MaxToolResultsPerLine = 16

// ToolResult is one tool_result block, reduced to flags and a fingerprint.
type ToolResult struct {
	IsError bool
	// Denied: the user rejected the tool permission (Claude Code writes a fixed refusal text).
	Denied bool
	// ErrorHash is the normalized error fingerprint (0 = none, not analyzed or no content).
	ErrorHash uint64
}

// UserTurn is a user line; it is a real user message unless IsToolResult is set.
type UserTurn struct {
	SessionID    string
	UUID         string
	Timestamp    time.Time // zero when absent or unparsable
	Cwd          string
	GitBranch    string
	IsSidechain  bool
	IsToolResult bool
	IsError      bool // some tool_result block has is_error = true
	Interrupted  bool // toolUseResult.interrupted

	// Results lists the line's tool_result blocks (first ResultCount entries).
	Results     [MaxToolResultsPerLine]ToolResult
	ResultCount int

	// Analyzed is true when the parser had an Extractor and this line was analyzed (never
	// for sidechain lines). Features is set for real user messages. Nothing here contains text.
	Analyzed bool
	Features analyzer.UserFeatures
}

// Ignored is a well-formed line of a type that carries no metrics.
type Ignored struct {
	Type string // sanitized: short identifier or "_invalid"
}

// SkipReason is a fixed code; it never contains line content.
type SkipReason string

const (
	ReasonEmpty          SkipReason = "empty"
	ReasonInvalidJSON    SkipReason = "invalid_json"
	ReasonNotObject      SkipReason = "not_object"
	ReasonMissingType    SkipReason = "missing_type"
	ReasonMissingSession SkipReason = "missing_session"
	ReasonMissingUsage   SkipReason = "missing_usage"
	ReasonBadTokens      SkipReason = "bad_tokens"
	ReasonOutOfRange     SkipReason = "out_of_range"
	ReasonLineTooLong    SkipReason = "line_too_long"
	ReasonInternal       SkipReason = "internal"
)

// Skipped is a line that could not be interpreted.
type Skipped struct {
	Reason SkipReason
}

func (AssistantUsage) isEvent() {}
func (UserTurn) isEvent()       {}
func (Ignored) isEvent()        {}
func (Skipped) isEvent()        {}
