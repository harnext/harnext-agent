// Package copilot turns single lines of the VS Code Copilot Chat OTel JSONL file into typed events.
//
// Zero-prompt-retention: only a fixed allow-list of span attributes is decoded. Prompt,
// response and tool-argument attributes (present when captureContent is on) are skipped by
// encoding/json without being copied, and never reach an event or a reason code.
package copilot

import "time"

// Event is one of ChatUsage, Ignored or Skipped.
type Event interface{ isEvent() }

// Usage holds token counts with Claude Code semantics: InputTokens excludes cache reads.
type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	// CacheCreationTokens are the prompt tokens written to the cache (0 when the exporter does not report them).
	CacheCreationTokens int64
	// PromptTokens is the request's whole prompt (gen_ai input_tokens, cache reads included).
	PromptTokens int64
}

// ChatUsage is a chat span of a real conversation. ConversationID doubles as the session id.
type ChatUsage struct {
	ConversationID string
	TraceID        string
	SpanID         string
	Model          string
	Agent          string // empty when absent or not a plain identifier
	Start          time.Time
	End            time.Time
	Usage          Usage
	NanoAIU        int64 // 0 when absent
	ToolVersion    string
}

// Ignored is a well-formed record that carries no metrics.
type Ignored struct {
	// Type is one of "log", "tool_span", "other_span", "background_chat".
	Type string
}

// SkipReason is a fixed code; it never contains line content.
type SkipReason string

const (
	ReasonEmpty        SkipReason = "empty"
	ReasonInvalidJSON  SkipReason = "invalid_json"
	ReasonNotObject    SkipReason = "not_object"
	ReasonMissingUsage SkipReason = "missing_usage"
	ReasonMissingModel SkipReason = "missing_model"
	ReasonBadID        SkipReason = "bad_id"
	ReasonBadTokens    SkipReason = "bad_tokens"
	ReasonOutOfRange   SkipReason = "out_of_range"
	ReasonLineTooLong  SkipReason = "line_too_long"
	ReasonInternal     SkipReason = "internal"
)

// Skipped is a line that could not be interpreted.
type Skipped struct {
	Reason SkipReason
}

func (ChatUsage) isEvent() {}
func (Ignored) isEvent()   {}
func (Skipped) isEvent()   {}
