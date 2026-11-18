package copilot

import (
	"bufio"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const sentinel = "SENTINEL_PROMPT_TEXT"

func fixtureLines(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("../../../testdata/copilot/otel-sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestParseFixture(t *testing.T) {
	lines := fixtureLines(t)
	want := []Event{
		ChatUsage{
			ConversationID: "conv-synthetic-0001", TraceID: "aaaa000000000000000000000000aa01", SpanID: "bbbb000000000001",
			Model: "claude-test-1", Agent: "panel/editAgent",
			Start:       time.Unix(1794258020, 118000000).UTC(),
			End:         time.Unix(1794258022, 51492375).UTC(),
			Usage:       Usage{InputTokens: 200, OutputTokens: 200, CacheReadTokens: 800, PromptTokens: 1000},
			NanoAIU:     2000000,
			ToolVersion: "0.68.0",
		},
		ChatUsage{
			ConversationID: "conv-synthetic-0001", TraceID: "aaaa000000000000000000000000aa02", SpanID: "bbbb000000000002",
			Model: "claude-test-1", Agent: "panel/editAgent",
			Start:       time.Unix(1794258030, 0).UTC(),
			End:         time.Unix(1794258031, 500000000).UTC(),
			Usage:       Usage{InputTokens: 500, OutputTokens: 50, PromptTokens: 500},
			ToolVersion: "0.68.0",
		},
		Ignored{Type: "background_chat"},
		Ignored{Type: "tool_span"},
		Ignored{Type: "log"},
		Ignored{Type: "other_span"},
		Skipped{Reason: ReasonInvalidJSON},
		Skipped{Reason: ReasonMissingModel},
		Skipped{Reason: ReasonOutOfRange},
		ChatUsage{
			ConversationID: "conv-synthetic-0002", TraceID: "aaaa000000000000000000000000aa09", SpanID: "bbbb000000000009",
			Model: "claude-test-1", Agent: "panel/editAgent",
			Start: time.Unix(1794258090, 0).UTC(),
			End:   time.Unix(1794258091, 0).UTC(),
			Usage: Usage{InputTokens: 40, OutputTokens: 4, PromptTokens: 40},
		},
	}
	if len(lines) != len(want) {
		t.Fatalf("fixture has %d lines, want %d", len(lines), len(want))
	}
	for i, line := range lines {
		got := ParseLine([]byte(line))
		if got != want[i] {
			t.Errorf("line %d: got %+v, want %+v", i+1, got, want[i])
		}
		if strings.Contains(fmt.Sprintf("%+v", got), sentinel) {
			t.Errorf("line %d: sentinel leaked into event", i+1)
		}
	}
}

func TestParseLineCases(t *testing.T) {
	span := func(attrs string) string {
		return `{"traceId":"t1","spanId":"s1","name":"chat m","attributes":{"gen_ai.operation.name":"chat","gen_ai.request.model":"m","gen_ai.conversation.id":"c1",` + attrs + `}}`
	}
	tests := []struct {
		name string
		line string
		want Event
	}{
		{"empty", "  ", Skipped{ReasonEmpty}},
		{"not object", `[1]`, Skipped{ReasonNotObject}},
		{"too long", strings.Repeat("x", MaxLineBytes+1), Skipped{ReasonLineTooLong}},
		{"float tokens", span(`"gen_ai.usage.input_tokens":1.5,"gen_ai.usage.output_tokens":1`), Skipped{ReasonBadTokens}},
		{"string tokens", span(`"gen_ai.usage.input_tokens":"7","gen_ai.usage.output_tokens":1`), Skipped{ReasonBadTokens}},
		{"huge tokens", span(`"gen_ai.usage.input_tokens":100000000001,"gen_ai.usage.output_tokens":1`), Skipped{ReasonOutOfRange}},
		{"missing usage", span(`"gen_ai.usage.output_tokens":1`), Skipped{ReasonMissingUsage}},
		{"cache above input clamps", span(`"gen_ai.usage.input_tokens":5,"gen_ai.usage.output_tokens":1,"gen_ai.usage.cache_read.input_tokens":9`),
			ChatUsage{ConversationID: "c1", TraceID: "t1", SpanID: "s1", Model: "m", Usage: Usage{OutputTokens: 1, CacheReadTokens: 9, PromptTokens: 9}}},
		{"bad agent dropped", span(`"gen_ai.agent.name":"a b","gen_ai.usage.input_tokens":1,"gen_ai.usage.output_tokens":1`),
			ChatUsage{ConversationID: "c1", TraceID: "t1", SpanID: "s1", Model: "m", Usage: Usage{InputTokens: 1, OutputTokens: 1, PromptTokens: 1}}},
		{"bad conversation id", `{"traceId":"t1","spanId":"s1","name":"chat m","attributes":{"gen_ai.operation.name":"chat","gen_ai.request.model":"m","gen_ai.conversation.id":"a b","gen_ai.usage.input_tokens":1,"gen_ai.usage.output_tokens":1}}`, Skipped{ReasonBadID}},
		{"missing trace id", `{"spanId":"s1","name":"chat m","attributes":{"gen_ai.operation.name":"chat","gen_ai.request.model":"m","gen_ai.conversation.id":"c1","gen_ai.usage.input_tokens":1,"gen_ai.usage.output_tokens":1}}`, Skipped{ReasonBadID}},
		{"non-chat operation", `{"traceId":"t1","spanId":"s1","name":"chat m","attributes":{"gen_ai.operation.name":"embeddings"}}`, Ignored{"other_span"}},
		{"bad times give zero", `{"traceId":"t1","spanId":"s1","name":"chat m","startTime":"x","endTime":[1],"attributes":{"gen_ai.operation.name":"chat","gen_ai.request.model":"m","gen_ai.conversation.id":"c1","gen_ai.usage.input_tokens":1,"gen_ai.usage.output_tokens":1}}`,
			ChatUsage{ConversationID: "c1", TraceID: "t1", SpanID: "s1", Model: "m", Usage: Usage{InputTokens: 1, OutputTokens: 1, PromptTokens: 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseLine([]byte(tc.line)); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNoFreeTextFields(t *testing.T) {
	allowed := map[string]bool{
		"ConversationID": true, "TraceID": true, "SpanID": true, "Model": true,
		"Agent": true, "ToolVersion": true, "Type": true, "Reason": true,
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(ChatUsage{}), reflect.TypeOf(Ignored{}), reflect.TypeOf(Skipped{}), reflect.TypeOf(Usage{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			switch f.Type.Kind() {
			case reflect.String:
				if !allowed[f.Name] {
					t.Errorf("%s.%s: unexpected string field", typ.Name(), f.Name)
				}
			case reflect.Int64, reflect.Struct:
			default:
				t.Errorf("%s.%s: unexpected kind %s", typ.Name(), f.Name, f.Type.Kind())
			}
		}
	}

	for i, line := range fixtureLines(t) {
		got := fmt.Sprintf("%+v", ParseLine([]byte(line)))
		if strings.Contains(got, sentinel) {
			t.Errorf("line %d leaked sentinel: %s", i+1, got)
		}
	}
}

// Real spans: input_tokens contains cache reads and cache writes; the plain input is the rest.
func TestCacheCreationIsSplitFromPlainInput(t *testing.T) {
	line := `{"traceId":"t1","spanId":"s1","name":"chat claude-sonnet-5-5","startTime":[1794672751,0],"endTime":[1794672752,0],"attributes":{"gen_ai.operation.name":"chat","gen_ai.conversation.id":"c1","gen_ai.response.model":"claude-sonnet-5-5","gen_ai.usage.input_tokens":56554,"gen_ai.usage.output_tokens":70,"gen_ai.usage.cache_read.input_tokens":14223,"gen_ai.usage.cache_creation.input_tokens":42327,"copilot_chat.copilot_usage_nano_aiu":10794800000}}`
	got, ok := ParseLine([]byte(line)).(ChatUsage)
	if !ok {
		t.Fatalf("not a ChatUsage: %+v", ParseLine([]byte(line)))
	}
	u := got.Usage
	if u.InputTokens != 4 || u.CacheReadTokens != 14223 || u.CacheCreationTokens != 42327 || u.OutputTokens != 70 || u.PromptTokens != 56554 {
		t.Fatalf("usage = %+v", u)
	}
}
