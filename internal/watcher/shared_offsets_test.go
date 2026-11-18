package watcher

import (
	"path/filepath"
	"testing"
)

func TestWatcherKeepsOffsetsOfOtherWatchers(t *testing.T) {
	h := newHarness(t)
	foreign := filepath.Join(t.TempDir(), "other.jsonl")
	if err := h.store.Set(foreign, FileState{Offset: 5, Size: 5}); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if _, ok := h.store.Get(foreign); !ok {
		t.Fatal("offset outside the watcher root was forgotten")
	}
}
