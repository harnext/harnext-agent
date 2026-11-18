// Package copilotchat turns single lines of VS Code Copilot chat session files
// (chatSessions/*.jsonl, a patch log) into typed events.
//
// Zero-prompt-retention: the user's message text is decoded only for the duration of one
// Extractor.UserFeatures call and reduced to counters. Responses, variables, tool results
// and every other field are never decoded or copied. Without an Extractor the text is not
// decoded at all.
package copilotchat

import (
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
)

// Event is one of RequestStart, RequestState, RequestTools, RequestToolCalls, RequestFiles, RequestMetric, Ignored or Skipped.
type Event interface{ isEvent() }

// MaxTools bounds the tool names reported per request; extras are dropped.
const MaxTools = 16

// MaxToolCalls bounds the tool invocations reported per event; extras are dropped.
const MaxToolCalls = 64

// MaxFiles bounds the file references reported per event; extras are dropped.
const MaxFiles = 32

// FileRef is a file the request edited or looked at, reduced to the FOLDER part of its path: Dir is absolute and has no file name. Hash is FNV-1a of the whole path and
// only lets consumers count a file once per request; the file name cannot be recovered from it.
type FileRef struct {
	Dir  string
	Hash uint64
	Edit bool // false: the file was only read or referenced
}

// RequestFiles reports files a request worked on (response parts textEditGroup, codeblockUri, inlineReference and the request
// fields editedFileEvents and contentReferences). The same file is repeated in later patch lines; consumers deduplicate by Hash.
type RequestFiles struct {
	Index int // request index
	Files [MaxFiles]FileRef
	N     int
}

// ToolCall is one completed tool invocation (response part kind "toolInvocationSerialized"),
// reduced to identifiers, flags and a fingerprint. The same call is rewritten in later patch
// lines; consumers deduplicate by IDHash. No message, result or command text is kept.
type ToolCall struct {
	IDHash   uint64 // FNV-1a of toolCallId; 0 when absent (cannot be deduplicated, skipped)
	ToolID   string // identifier-like toolId (e.g. "copilot_replaceString"), "" when invalid
	IsError  bool   // resultDetails.isError, a resultError string or resultError: true
	HasExit  bool   // terminal call with a numeric exit code
	ExitCode int
	// ErrorHash is the normalized fingerprint of resultError (0 = none or not analyzed).
	ErrorHash uint64
}

// RequestToolCalls reports tool invocations appended or set on a request's response.
type RequestToolCalls struct {
	Index int // request index; -1 when unknown
	Calls [MaxToolCalls]ToolCall
	N     int
}

// ModelState values (VS Code ResponseModelState).
const (
	StatePending       = 0
	StateComplete      = 1
	StateCancelled     = 2
	StateFailed        = 3
	StateNeedsInput    = 4
	maxKnownStateValue = StateNeedsInput
)

// RequestStart is one chat request. At is the array index for appended requests and -1
// for snapshot requests and appends without "i"; consumers count snapshot requests in
// order. A request that cannot be decoded yields a bare RequestStart so indexes stay aligned.
type RequestStart struct {
	At        int
	RequestID string
	Timestamp time.Time // zero when absent or implausible
	Model     string    // resolvedModel, else the part of modelId after the last "/"
	Hidden    bool

	// Analyzed is true only when an Extractor was present, the request is not hidden and
	// message.text was a string; Features is filled only then.
	Analyzed bool
	Features analyzer.UserFeatures

	HasState    bool
	State       int
	CompletedAt time.Time

	// Metrics Copilot recorded for the request: its own credits, and the prompt and completion tokens of the last
	// model round. Each Has flag is false when the field is absent or implausible.
	HasCredits, HasPrompt, HasCompletion bool
	Credits                              float64
	PromptTokens, CompletionTokens       int64
	// Rounds is the number of model rounds (toolCallRounds entries); 0 when unknown.
	Rounds int

	Tools     [MaxTools]string
	ToolCount int

	// Calls are the completed tool invocations found in the request's response.
	Calls     [MaxToolCalls]ToolCall
	CallCount int

	// Files are the folders of the files the request edited or referenced.
	Files     [MaxFiles]FileRef
	FileCount int
}

// RequestState reports a modelState update; State 2 means cancelled.
type RequestState struct {
	Index       int
	State       int
	CompletedAt time.Time
}

// RequestTools reports the tool names (repeats kept), resolved model and round count of a request result.
type RequestTools struct {
	Index     int
	Model     string
	Tools     [MaxTools]string
	ToolCount int
	Rounds    int
}

// Metric kinds of RequestMetric.
const (
	MetricCredits = iota + 1
	MetricPrompt
	MetricCompletion
)

// MaxMetricValue bounds a plausible per-request metric (credits or tokens); larger values are treated as absent.
const MaxMetricValue = 1e9

// MaxRounds bounds the reported round count.
const MaxRounds = 10000

// RequestMetric reports a numeric field set by patch (requests/<i>/copilotCredits, promptTokens, completionTokens).
// The value replaces any earlier one for the same request.
type RequestMetric struct {
	Index int
	Kind  int
	Value float64
}

// Ignored is a well-formed line without metrics.
type Ignored struct {
	Type string // fixed short code
}

// SkipReason is a fixed code; it never contains line content.
type SkipReason string

const (
	ReasonEmpty       SkipReason = "empty"
	ReasonInvalidJSON SkipReason = "invalid_json"
	ReasonBadShape    SkipReason = "bad_shape"
	ReasonLineTooLong SkipReason = "line_too_long"
	ReasonInternal    SkipReason = "internal"
)

// Skipped is a line that could not be interpreted.
type Skipped struct {
	Reason SkipReason
}

func (RequestStart) isEvent()     {}
func (RequestState) isEvent()     {}
func (RequestTools) isEvent()     {}
func (RequestToolCalls) isEvent() {}
func (RequestFiles) isEvent()     {}
func (RequestMetric) isEvent()    {}
func (Ignored) isEvent()          {}
func (Skipped) isEvent()          {}
