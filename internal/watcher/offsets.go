package watcher

import (
	"slices"
	"sync"
)

// FileID identifies a file independent of its path; zero where the platform provides none.
type FileID struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// FileState is what the watcher remembers about a transcript between scans.
type FileState struct {
	ID FileID `json:"id"`
	// Offset is the byte position after the last complete line that was processed.
	Offset int64 `json:"offset"`
	// Size is the file size seen at the last read; a smaller size later means truncation.
	Size int64 `json:"size"`
}

// OffsetStore persists FileState by path. Implementations must be safe for concurrent use
// and their errors must not contain the path.
type OffsetStore interface {
	Get(path string) (FileState, bool)
	Set(path string, st FileState) error
	Delete(path string) error
	Paths() []string
}

// MemoryOffsets is an in-memory OffsetStore; the SQLite store replaces it in phase 2c.
type MemoryOffsets struct {
	mu sync.Mutex
	m  map[string]FileState
}

func NewMemoryOffsets() *MemoryOffsets {
	return &MemoryOffsets{m: map[string]FileState{}}
}

func (s *MemoryOffsets) Get(path string) (FileState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[path]
	return st, ok
}

func (s *MemoryOffsets) Set(path string, st FileState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[path] = st
	return nil
}

func (s *MemoryOffsets) Delete(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, path)
	return nil
}

func (s *MemoryOffsets) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths := make([]string, 0, len(s.m))
	for p := range s.m {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths
}
