package claudecode

import (
	"strings"
	"testing"
)

func benchLine(b *testing.B, line string) {
	raw := []byte(line)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseLine(raw)
	}
}

func BenchmarkParseAssistantSmall(b *testing.B) { benchLine(b, assistantLine) }

func BenchmarkParseUserSmall(b *testing.B) { benchLine(b, userStringLine) }

func BenchmarkParseUserPrompt2MB(b *testing.B) {
	benchLine(b, `{"type":"user","sessionId":"s1","message":{"content":"`+strings.Repeat("x", 2<<20)+`"}}`)
}

func BenchmarkParseToolResult2MB(b *testing.B) {
	big := strings.Repeat("x", 2<<20)
	benchLine(b, `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":"`+big+`","is_error":false}]},"toolUseResult":{"stdout":"`+big[:1<<10]+`"}}`)
}

func BenchmarkParseAssistantText2MB(b *testing.B) {
	benchLine(b, `{"type":"assistant","sessionId":"s1","message":{"id":"m","content":[{"type":"text","text":"`+strings.Repeat("x", 2<<20)+`"}],"usage":{"output_tokens":1}}}`)
}

func BenchmarkParseIgnored2MB(b *testing.B) {
	benchLine(b, `{"type":"attachment","attachment":"`+strings.Repeat("x", 2<<20)+`"}`)
}

func benchLineAnalyzed(b *testing.B, line string) {
	p := NewParser(analysisExtractor(b))
	raw := []byte(line)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.ParseLine(raw)
	}
}

func BenchmarkAnalyzedAssistantSmall(b *testing.B) { benchLineAnalyzed(b, assistantLine) }

func BenchmarkAnalyzedToolUseSmall(b *testing.B) {
	benchLineAnalyzed(b, string(toolLine(`[{"type":"tool_use","id":"t","name":"Edit","input":{"file_path":"/p/a_test.go","old_string":"x","new_string":"y"}}]`)))
}

func BenchmarkAnalyzedUserSmall(b *testing.B) { benchLineAnalyzed(b, userStringLine) }

func BenchmarkAnalyzedUserPrompt2MB(b *testing.B) {
	benchLineAnalyzed(b, `{"type":"user","sessionId":"s1","message":{"content":"`+strings.Repeat("x", 2<<20)+`"}}`)
}

func BenchmarkAnalyzedToolResult2MB(b *testing.B) {
	big := strings.Repeat("x", 2<<20)
	benchLineAnalyzed(b, `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":"`+big+`","is_error":false}]},"toolUseResult":{"stdout":"`+big[:1<<10]+`"}}`)
}

func BenchmarkAnalyzedToolResultError2MB(b *testing.B) {
	big := strings.Repeat("x", 2<<20)
	benchLineAnalyzed(b, `{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result","content":"`+big+`","is_error":true}]}}`)
}

func BenchmarkAnalyzedAssistantText2MB(b *testing.B) {
	benchLineAnalyzed(b, `{"type":"assistant","sessionId":"s1","message":{"id":"m","content":[{"type":"text","text":"`+strings.Repeat("x", 2<<20)+`"}],"usage":{"output_tokens":1}}}`)
}
