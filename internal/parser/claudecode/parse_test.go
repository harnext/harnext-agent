package claudecode

import (
	"bytes"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	assistantLine = `{"type":"assistant","isSidechain":false,"sessionId":"s1","requestId":"req_1","uuid":"a1",` +
		`"cwd":"/home/dev/example-project","gitBranch":"main","version":"2.1.237","timestamp":"2026-01-01T10:00:05.500Z",` +
		`"unknownField":{"x":[1,2,null]},"message":{"id":"msg_1","model":"claude-sonnet-5","stop_reason":"end_turn",` +
		`"content":[{"type":"text","text":"fixture answer"}],"usage":{"input_tokens":10,"output_tokens":200,` +
		`"cache_read_input_tokens":5000,"cache_creation_input_tokens":1000,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":900,"ephemeral_1h_input_tokens":100},` +
		`"output_tokens_details":{"thinking_tokens":50},"service_tier":"standard","speed":"fast",` +
		`"server_tool_use":{"web_search_requests":2,"web_fetch_requests":1}}}}`
	userStringLine = `{"type":"user","sessionId":"s1","uuid":"u1","cwd":"/home/dev/p","gitBranch":"main",` +
		`"timestamp":"2026-01-01T10:00:01Z","message":{"role":"user","content":"fixture prompt"}}`
)

func usageLine(usage string) string {
	return `{"type":"assistant","sessionId":"s1","requestId":"r","message":{"id":"m","model":"x","usage":` + usage + `}}`
}

func TestParseLine(t *testing.T) {
	ts := time.Date(2026, 1, 1, 10, 0, 5, 500_000_000, time.UTC)
	tests := []struct {
		name string
		line string
		want Event
	}{
		{
			name: "assistant with full usage",
			line: assistantLine,
			want: AssistantUsage{
				SessionID: "s1", MessageID: "msg_1", RequestID: "req_1", Model: "claude-sonnet-5",
				Timestamp: ts, Cwd: "/home/dev/example-project", GitBranch: "main", ToolVersion: "2.1.237",
				StopReason: "end_turn",
				Usage: Usage{
					InputTokens: 10, OutputTokens: 200, CacheReadTokens: 5000,
					CacheCreationTokens: 1000, CacheCreation1hTokens: 100, ThinkingTokens: 50,
					Speed: SpeedFast, WebSearchRequests: 2, WebFetchRequests: 1,
				},
			},
		},
		{
			name: "assistant unexpected speed is unknown",
			line: `{"type":"assistant","sessionId":"s1","message":{"usage":{"output_tokens":1,"speed":"turbo"}}}`,
			want: AssistantUsage{SessionID: "s1", Usage: Usage{OutputTokens: 1, Speed: SpeedUnknown}},
		},
		{
			name: "assistant standard speed",
			line: `{"type":"assistant","sessionId":"s1","message":{"usage":{"output_tokens":1,"speed":"standard"}}}`,
			want: AssistantUsage{SessionID: "s1", Usage: Usage{OutputTokens: 1, Speed: SpeedStandard}},
		},
		{
			name: "assistant bad server tool count",
			line: `{"type":"assistant","sessionId":"s1","message":{"usage":{"server_tool_use":{"web_search_requests":"2"}}}}`,
			want: Skipped{ReasonBadTokens},
		},
		{
			name: "assistant negative server tool count",
			line: `{"type":"assistant","sessionId":"s1","message":{"usage":{"server_tool_use":{"web_fetch_requests":-1}}}}`,
			want: Skipped{ReasonOutOfRange},
		},
		{
			name: "assistant sidechain flag",
			line: `{"type":"assistant","isSidechain":true,"sessionId":"s1","message":{"usage":{"output_tokens":1}}}`,
			want: AssistantUsage{SessionID: "s1", IsSidechain: true, Usage: Usage{OutputTokens: 1, Speed: SpeedUnknown}},
		},
		{
			name: "assistant missing optional fields",
			line: `{"type":"assistant","sessionId":"s1","message":{"usage":{}}}`,
			want: AssistantUsage{SessionID: "s1", Usage: Usage{Speed: SpeedUnknown}},
		},
		{
			name: "assistant null token values",
			line: usageLine(`{"input_tokens":null,"output_tokens":7,"cache_creation":null}`),
			want: AssistantUsage{SessionID: "s1", MessageID: "m", RequestID: "r", Model: "x", Usage: Usage{OutputTokens: 7, Speed: SpeedUnknown}},
		},
		{
			name: "assistant token exactly at maximum",
			line: usageLine(`{"output_tokens":100000000000}`),
			want: AssistantUsage{SessionID: "s1", MessageID: "m", RequestID: "r", Model: "x", Usage: Usage{OutputTokens: 100_000_000_000, Speed: SpeedUnknown}},
		},
		{name: "assistant token above maximum", line: usageLine(`{"output_tokens":100000000001}`), want: Skipped{ReasonOutOfRange}},
		{name: "assistant negative token", line: usageLine(`{"input_tokens":-1}`), want: Skipped{ReasonOutOfRange}},
		{name: "assistant token overflows int64", line: usageLine(`{"input_tokens":99999999999999999999}`), want: Skipped{ReasonBadTokens}},
		{name: "assistant string token", line: usageLine(`{"input_tokens":"12"}`), want: Skipped{ReasonBadTokens}},
		{name: "assistant float token", line: usageLine(`{"input_tokens":1.5}`), want: Skipped{ReasonBadTokens}},
		{name: "assistant boolean token", line: usageLine(`{"output_tokens":true}`), want: Skipped{ReasonBadTokens}},
		{name: "assistant array token", line: usageLine(`{"output_tokens":[1]}`), want: Skipped{ReasonBadTokens}},
		{name: "assistant 1h exceeds total", line: usageLine(`{"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":6}}`), want: Skipped{ReasonOutOfRange}},
		{name: "assistant bad thinking tokens", line: usageLine(`{"output_tokens_details":{"thinking_tokens":"x"}}`), want: Skipped{ReasonBadTokens}},
		{name: "assistant without message", line: `{"type":"assistant","sessionId":"s1"}`, want: Skipped{ReasonMissingUsage}},
		{name: "assistant message null", line: `{"type":"assistant","sessionId":"s1","message":null}`, want: Skipped{ReasonMissingUsage}},
		{name: "assistant message string", line: `{"type":"assistant","sessionId":"s1","message":"x"}`, want: Skipped{ReasonMissingUsage}},
		{name: "assistant usage null", line: `{"type":"assistant","sessionId":"s1","message":{"usage":null}}`, want: Skipped{ReasonMissingUsage}},
		{name: "assistant usage string", line: `{"type":"assistant","sessionId":"s1","message":{"usage":"x"}}`, want: Skipped{ReasonMissingUsage}},
		{name: "assistant usage array", line: `{"type":"assistant","sessionId":"s1","message":{"usage":[]}}`, want: Skipped{ReasonMissingUsage}},
		{name: "assistant without session", line: `{"type":"assistant","message":{"usage":{}}}`, want: Skipped{ReasonMissingSession}},
		{name: "assistant numeric session", line: `{"type":"assistant","sessionId":5,"message":{"usage":{}}}`, want: Skipped{ReasonMissingSession}},
		{
			name: "assistant wrong-typed soft fields are ignored",
			line: `{"type":"assistant","sessionId":"s1","isSidechain":"yes","cwd":5,"gitBranch":[],"version":{},"timestamp":12,"requestId":null,"message":{"id":7,"model":false,"stop_reason":{},"usage":{}}}`,
			want: AssistantUsage{SessionID: "s1", Usage: Usage{Speed: SpeedUnknown}},
		},
		{
			name: "assistant unparsable timestamp",
			line: `{"type":"assistant","sessionId":"s1","timestamp":"yesterday","message":{"usage":{}}}`,
			want: AssistantUsage{SessionID: "s1", Usage: Usage{Speed: SpeedUnknown}},
		},
		{
			name: "assistant escaped strings",
			line: `{"type":"assistant","sessionId":"s1","cwd":"C:\\dev\\p\u00e9","message":{"usage":{}}}`,
			want: AssistantUsage{SessionID: "s1", Cwd: `C:\dev\pé`, Usage: Usage{Speed: SpeedUnknown}},
		},
		{
			name: "assistant oversized identifier dropped",
			line: `{"type":"assistant","sessionId":"` + strings.Repeat("a", maxFieldBytes+1) + `","message":{"usage":{}}}`,
			want: Skipped{ReasonMissingSession},
		},
		{
			name: "user real message with string content",
			line: userStringLine,
			want: UserTurn{SessionID: "s1", UUID: "u1", Cwd: "/home/dev/p", GitBranch: "main", Timestamp: time.Date(2026, 1, 1, 10, 0, 1, 0, time.UTC)},
		},
		{
			name: "user real message with text blocks",
			line: `{"type":"user","sessionId":"s1","message":{"content":[{"type":"text","text":"fixture"},{"type":"image","source":{}}]}}`,
			want: UserTurn{SessionID: "s1"},
		},
		{
			name: "user tool_result",
			line: `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"fixture output","is_error":false}]}}`,
			want: UserTurn{SessionID: "s1", IsToolResult: true},
		},
		{
			name: "user tool_result error",
			line: `{"type":"user","sessionId":"s1","message":{"content":[{"type":"text"},{"type":"tool_result","is_error":true}]}}`,
			want: UserTurn{SessionID: "s1", IsToolResult: true, IsError: true},
		},
		{
			name: "user tool_result is_error of wrong type",
			line: `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","is_error":"true"}]}}`,
			want: UserTurn{SessionID: "s1", IsToolResult: true},
		},
		{
			name: "user content array with non-object elements",
			line: `{"type":"user","sessionId":"s1","message":{"content":["x",null,{"type":"tool_result","is_error":true}]}}`,
			want: UserTurn{SessionID: "s1", IsToolResult: true, IsError: true},
		},
		{
			name: "user interrupted",
			line: `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result"}]},"toolUseResult":{"interrupted":true,"stdout":"fixture"}}`,
			want: UserTurn{SessionID: "s1", IsToolResult: true, Interrupted: true},
		},
		{
			name: "user toolUseResult string",
			line: `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result"}]},"toolUseResult":"fixture error"}`,
			want: UserTurn{SessionID: "s1", IsToolResult: true},
		},
		{
			name: "user toolUseResult null",
			line: `{"type":"user","sessionId":"s1","message":{"content":"x"},"toolUseResult":null}`,
			want: UserTurn{SessionID: "s1"},
		},
		{
			name: "user sidechain",
			line: `{"type":"user","isSidechain":true,"sessionId":"s1","message":{"content":"fixture"}}`,
			want: UserTurn{SessionID: "s1", IsSidechain: true},
		},
		{name: "user without session", line: `{"type":"user","message":{"content":"x"}}`, want: Skipped{ReasonMissingSession}},
		{name: "user nested nulls", line: `{"type":"user","sessionId":"s1","message":{"content":null},"toolUseResult":null}`, want: UserTurn{SessionID: "s1"}},
		{name: "ignored queue-operation", line: `{"type":"queue-operation","operation":"enqueue","sessionId":"s1"}`, want: Ignored{"queue-operation"}},
		{name: "ignored attachment", line: `{"type":"attachment","sessionId":"s1","attachment":{"type":"x"}}`, want: Ignored{"attachment"}},
		{name: "ignored ai-title", line: `{"type":"ai-title","aiTitle":"fixture"}`, want: Ignored{"ai-title"}},
		{name: "ignored unknown future type", line: `{"type":"future-event-type","someNewField":{"nested":true}}`, want: Ignored{"future-event-type"}},
		{name: "ignored type with odd characters", line: `{"type":"fixture secret text"}`, want: Ignored{"_invalid"}},
		{name: "ignored type too long", line: `{"type":"` + strings.Repeat("a", 41) + `"}`, want: Ignored{"_invalid"}},
		{name: "ignored type with non-string message", line: `{"type":"system","message":"x","toolUseResult":[1]}`, want: Ignored{"system"}},
		{name: "numeric type", line: `{"type":5}`, want: Skipped{ReasonMissingType}},
		{name: "null type", line: `{"type":null,"sessionId":"s1"}`, want: Skipped{ReasonMissingType}},
		{name: "no type", line: `{"sessionId":"s1"}`, want: Skipped{ReasonMissingType}},
		{name: "empty object", line: `{}`, want: Skipped{ReasonMissingType}},
		{name: "empty line", line: ``, want: Skipped{ReasonEmpty}},
		{name: "whitespace only", line: " \t\r\n", want: Skipped{ReasonEmpty}},
		{name: "BOM only", line: "\xEF\xBB\xBF", want: Skipped{ReasonEmpty}},
		{name: "json null", line: `null`, want: Skipped{ReasonMissingType}},
		{name: "json array", line: `[1,2]`, want: Skipped{ReasonNotObject}},
		{name: "json string", line: `"text"`, want: Skipped{ReasonNotObject}},
		{name: "json number", line: `123`, want: Skipped{ReasonNotObject}},
		{name: "truncated json", line: `{"type":"assistant","message":{"id":"msg_BROKEN","usage":`, want: Skipped{ReasonInvalidJSON}},
		{name: "plain text", line: `not json at all`, want: Skipped{ReasonInvalidJSON}},
		{name: "trailing garbage", line: `{"type":"x"} trailing`, want: Skipped{ReasonInvalidJSON}},
		{name: "binary bytes", line: "\x00\x01\x02\xff\xfe", want: Skipped{ReasonInvalidJSON}},
		{name: "BOM prefix", line: "\xEF\xBB\xBF" + `{"type":"ai-title"}`, want: Ignored{"ai-title"}},
		{name: "CRLF terminator", line: `{"type":"ai-title"}` + "\r\n", want: Ignored{"ai-title"}},
		{name: "LF terminator", line: `{"type":"ai-title"}` + "\n", want: Ignored{"ai-title"}},
		{name: "duplicate keys last wins", line: `{"type":"assistant","type":"ai-title"}`, want: Ignored{"ai-title"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withoutResults(ParseLine([]byte(tt.line))); got != tt.want {
				t.Errorf("ParseLine() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseLineDeepNesting(t *testing.T) {
	deep := strings.Repeat("[", 20000) + strings.Repeat("]", 20000)
	for name, line := range map[string]string{
		"deep unknown field":  `{"type":"ai-title","x":` + deep + `}`,
		"deep content":        `{"type":"user","sessionId":"s","message":{"content":` + deep + `}}`,
		"deep usage":          `{"type":"assistant","sessionId":"s","message":{"usage":` + deep + `}}`,
		"deep top-level":      deep,
		"deep object nesting": strings.Repeat(`{"a":`, 20000) + `1` + strings.Repeat("}", 20000),
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParseLine([]byte(line)).(Skipped); !ok {
				// Valid but absurd nesting may also be accepted; the requirement is only "no panic".
				t.Log("accepted")
			}
		})
	}
}

func TestParseLineLargeLines(t *testing.T) {
	filler := strings.Repeat("x", 2<<20)
	tests := []struct {
		name string
		line string
		want Event
	}{
		{
			name: "2 MB user prompt string",
			line: `{"type":"user","sessionId":"s1","message":{"role":"user","content":"` + filler + `"}}`,
			want: UserTurn{SessionID: "s1"},
		},
		{
			name: "2 MB tool result",
			line: `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":"` + filler + `","is_error":true}]},"toolUseResult":{"stdout":"` + filler + `"}}`,
			want: UserTurn{SessionID: "s1", IsToolResult: true, IsError: true},
		},
		{
			name: "2 MB assistant text",
			line: `{"type":"assistant","sessionId":"s1","message":{"id":"m","content":[{"type":"text","text":"` + filler + `"}],"usage":{"output_tokens":3}}}`,
			want: AssistantUsage{SessionID: "s1", MessageID: "m", Usage: Usage{OutputTokens: 3, Speed: SpeedUnknown}},
		},
		{name: "2 MB ignored attachment", line: `{"type":"attachment","attachment":"` + filler + `"}`, want: Ignored{"attachment"}},
		{name: "2 MB unterminated", line: `{"type":"user","message":"` + filler, want: Skipped{ReasonInvalidJSON}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withoutResults(ParseLine([]byte(tt.line))); got != tt.want {
				t.Errorf("ParseLine() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Content must neither appear in events nor be copied into memory as strings.
func TestParseLineDoesNotRetainContent(t *testing.T) {
	const sentinel = "SENTINEL-PROMPT-TEXT"
	lines := []string{
		`{"type":"user","sessionId":"s1","message":{"content":"` + sentinel + `"}}`,
		`{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":"` + sentinel + `"}]},"toolUseResult":{"stdout":"` + sentinel + `"}}`,
		`{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"` + sentinel + `"}],"usage":{}}}`,
		`{"type":"ai-title","aiTitle":"` + sentinel + `"}`,
		`{"type":"` + sentinel + ` text"}`,
	}
	for _, line := range lines {
		if out := fmt.Sprintf("%+v", ParseLine([]byte(line))); strings.Contains(out, sentinel) {
			t.Errorf("event contains content: %s", out)
		}
	}

	big := strings.Repeat("y", 2<<20)
	for name, line := range map[string]string{
		"user string":   `{"type":"user","sessionId":"s1","message":{"content":"` + big + `"}}`,
		"user blocks":   `{"type":"user","sessionId":"s1","message":{"content":[{"type":"text","text":"` + big + `"}]}}`,
		"tool result":   `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":"` + big + `"}]},"toolUseResult":{"stdout":"` + big + `"}}`,
		"assistant":     `{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"` + big + `"}],"usage":{"output_tokens":1}}}`,
		"ignored field": `{"type":"attachment","x":"` + big + `"}`,
	} {
		b := []byte(line)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		ParseLine(b)
		runtime.ReadMemStats(&after)
		if grown := after.TotalAlloc - before.TotalAlloc; grown > 64<<10 {
			t.Errorf("%s: allocated %d bytes for a %d byte line", name, grown, len(b))
		}
	}
}

func TestParseLineNeverPanics(t *testing.T) {
	seeds := []string{assistantLine, userStringLine, `{"type":"user","sessionId":"s","message":{"content":[{"type":"tool_result","is_error":true}]},"toolUseResult":{"interrupted":true}}`}
	rng := rand.New(rand.NewSource(1))
	for _, seed := range seeds {
		raw := []byte(seed)
		for i := 0; i <= len(raw); i++ {
			ParseLine(raw[:i])
			ParseLine(raw[i:])
		}
		for n := 0; n < 3000; n++ {
			mutated := bytes.Clone(raw)
			for k := rng.Intn(4) + 1; k > 0; k-- {
				mutated[rng.Intn(len(mutated))] = byte(rng.Intn(256))
			}
			ParseLine(mutated)
		}
	}
}

func FuzzParseLine(f *testing.F) {
	for _, s := range []string{assistantLine, userStringLine, ``, `{`, `null`, `[[[`, `{"type":"user","message":{"content":[`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		if _, ok := ParseLine(line).(interface{ isEvent() }); !ok {
			t.Fatal("ParseLine returned nil event")
		}
	})
}

// withoutResults clears the per-block tool results so table tests can compare the line-level
// fields; the blocks themselves are covered in analysis_test.go. A tool result line must
// report at least one block, matching IsError.
func withoutResults(ev Event) Event {
	u, ok := ev.(UserTurn)
	if !ok {
		return ev
	}
	if u.IsToolResult && (u.ResultCount < 1 || u.Results[0].IsError != u.IsError && u.ResultCount == 1) {
		return Skipped{Reason: "results_mismatch"}
	}
	u.Results, u.ResultCount = [MaxToolResultsPerLine]ToolResult{}, 0
	return u
}
