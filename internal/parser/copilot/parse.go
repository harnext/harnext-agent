package copilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxLineBytes is the largest line ParseLine decodes; the caller bounds reads earlier.
	MaxLineBytes = 8 << 20

	maxTokenValue = 100_000_000_000
	maxFieldBytes = 1024
	maxIDLen      = 200
	maxModelLen   = 128
	maxAgentLen   = 64
	maxVersionLen = 32
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// ParseLine interprets one OTel line. It never panics and never returns an error; anything
// it cannot use is reported as Skipped with a fixed reason code.
func ParseLine(line []byte) (ev Event) {
	defer func() {
		if recover() != nil {
			ev = Skipped{Reason: ReasonInternal}
		}
	}()

	if len(line) > MaxLineBytes {
		return Skipped{Reason: ReasonLineTooLong}
	}
	line = bytes.TrimSpace(bytes.TrimPrefix(line, utf8BOM))
	if len(line) == 0 {
		return Skipped{Reason: ReasonEmpty}
	}

	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return Skipped{Reason: ReasonNotObject}
		}
		return Skipped{Reason: ReasonInvalidJSON}
	}

	if r.SpanID == "" {
		return Ignored{Type: "log"}
	}
	switch {
	case strings.HasPrefix(string(r.Name), "execute_tool"):
		return Ignored{Type: "tool_span"}
	case !strings.HasPrefix(string(r.Name), "chat ") || r.Attributes.V.Operation != "chat":
		return Ignored{Type: "other_span"}
	}
	return r.chat()
}

func (r *record) chat() Event {
	a := &r.Attributes.V
	if a.ConversationID == "" {
		return Ignored{Type: "background_chat"}
	}
	if !validID(string(a.ConversationID)) || !validID(string(r.TraceID)) || !validID(string(r.SpanID)) {
		return Skipped{Reason: ReasonBadID}
	}
	model := string(a.ResponseModel)
	if model == "" {
		model = string(a.RequestModel)
	}
	if model == "" || !validModel(model) {
		return Skipped{Reason: ReasonMissingModel}
	}
	if !a.Input.set || !a.Output.set {
		return Skipped{Reason: ReasonMissingUsage}
	}
	fields := [...]*tokenCount{&a.Input, &a.Output, &a.CacheRead, &a.CacheCreate, &a.NanoAIU}
	for _, f := range fields {
		if f.bad {
			return Skipped{Reason: ReasonBadTokens}
		}
		if f.n < 0 || f.n > maxTokenValue {
			return Skipped{Reason: ReasonOutOfRange}
		}
	}
	// gen_ai input_tokens includes cache reads and cache writes; the plain input is what is left. Checked against real spans:
	// with this split, price x tokens equals Copilot's own credits / 100 exactly (writes are priced above plain input).
	input := max(a.Input.n-a.CacheRead.n-a.CacheCreate.n, 0)

	cu := ChatUsage{
		ConversationID: string(a.ConversationID),
		TraceID:        string(r.TraceID),
		SpanID:         string(r.SpanID),
		Model:          model,
		Start:          r.StartTime.time(),
		End:            r.EndTime.time(),
		Usage:          Usage{InputTokens: input, OutputTokens: a.Output.n, CacheReadTokens: a.CacheRead.n, CacheCreationTokens: a.CacheCreate.n, PromptTokens: max(a.Input.n, a.CacheRead.n+a.CacheCreate.n)},
		NanoAIU:        a.NanoAIU.n,
	}
	if agent := string(a.Agent); len(agent) <= maxAgentLen && validChars(agent, "._/-") {
		cu.Agent = agent
	}
	if v := string(r.Resource.V.Attributes.V.ServiceVersion); len(v) <= maxVersionLen {
		cu.ToolVersion = v
	}
	return cu
}

// record declares only the allow-listed fields; every other key, including message
// content and tool arguments, is skipped by encoding/json without being copied.
type record struct {
	TraceID    lenStr           `json:"traceId"`
	SpanID     lenStr           `json:"spanId"`
	Name       lenStr           `json:"name"`
	StartTime  epoch            `json:"startTime"`
	EndTime    epoch            `json:"endTime"`
	Attributes object[attrs]    `json:"attributes"`
	Resource   object[resource] `json:"resource"`
}

type attrs struct {
	Operation      lenStr     `json:"gen_ai.operation.name"`
	RequestModel   lenStr     `json:"gen_ai.request.model"`
	ResponseModel  lenStr     `json:"gen_ai.response.model"`
	ConversationID lenStr     `json:"gen_ai.conversation.id"`
	Agent          lenStr     `json:"gen_ai.agent.name"`
	Input          tokenCount `json:"gen_ai.usage.input_tokens"`
	Output         tokenCount `json:"gen_ai.usage.output_tokens"`
	CacheRead      tokenCount `json:"gen_ai.usage.cache_read.input_tokens"`
	CacheCreate    tokenCount `json:"gen_ai.usage.cache_creation.input_tokens"`
	NanoAIU        tokenCount `json:"copilot_chat.copilot_usage_nano_aiu"`
}

type resource struct {
	Attributes object[resourceAttrs] `json:"attributes"`
}

type resourceAttrs struct {
	ServiceVersion lenStr `json:"service.version"`
}

func validID(s string) bool {
	return s != "" && len(s) <= maxIDLen && validChars(s, "._:-")
}

func validModel(s string) bool {
	return len(s) <= maxModelLen && validChars(s, "._:/@+-")
}

func validChars(s, extra string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte(extra, c) >= 0:
		default:
			return false
		}
	}
	return true
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

// tokenCount records non-integer values as bad rather than failing the line.
type tokenCount struct {
	n   int64
	set bool
	bad bool
}

func (t *tokenCount) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	t.set = true
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		t.bad = true
		return nil
	}
	t.n = n
	return nil
}

// epoch is OTel's [seconds, nanoseconds] pair; anything else yields the zero time.
type epoch struct{ t time.Time }

func (e *epoch) UnmarshalJSON(b []byte) error {
	var p []int64
	if json.Unmarshal(b, &p) == nil && len(p) == 2 && p[0] >= 0 && p[1] >= 0 && p[1] < 1_000_000_000 {
		e.t = time.Unix(p[0], p[1]).UTC()
	}
	return nil
}

func (e epoch) time() time.Time { return e.t }

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
