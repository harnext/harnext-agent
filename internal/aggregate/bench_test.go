package aggregate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/harnext/harnext-agent/internal/parser/claudecode"
)

// BenchmarkParseAndAdd is the per-line cost of the hot path (parse one transcript line, fold it into the aggregator) on a
// synthetic fixture, without friction analysis. SetBytes makes the result a throughput in MB/s.
//
//	go test -run '^$' -bench ParseAndAdd -benchmem ./internal/aggregate
func BenchmarkParseAndAdd(b *testing.B) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude-code", "session-basic.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := New()
		for _, l := range lines {
			a.Add(claudecode.ParseLine(l))
		}
	}
}
