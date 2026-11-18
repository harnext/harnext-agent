//go:build realdata

package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type countSink struct{ lines, tooLong int }

func (c *countSink) HandleLine(_ ChatFile, _ []byte, tooLong bool) {
	c.lines++
	if tooLong {
		c.tooLong++
	}
}

// Prints counts and duration only, never paths or content.
// Run: go test -tags realdata -run TestRealChat -v ./internal/watcher
func TestRealChat(t *testing.T) {
	roots := DefaultChatRoots()
	files := 0
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, "*", "chatSessions", "*.jsonl"))
		for _, m := range matches {
			if fi, err := os.Lstat(m); err == nil && fi.Mode().IsRegular() {
				files++
			}
		}
	}
	sink := &countSink{}
	errCount := 0
	w, err := NewChatWatcher(ChatOptions{Roots: roots, Offsets: NewMemoryOffsets(), Sink: sink,
		OnError: func(error) { errCount++ }})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("roots=%d files=%d lines=%d tooLong=%d errors=%d duration=%s",
		len(roots), files, sink.lines, sink.tooLong, errCount, time.Since(start))
}
