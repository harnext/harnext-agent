package service

import (
	"strings"
	"sync"

	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/watcher"
)

// OffsetStore keeps offsets in memory and writes them to SQLite on Flush. It lets the run
// loop persist the aggregator snapshot first and the offsets second: after a crash the
// watcher then re-reads lines the snapshot already holds, and the aggregator drops them as
// duplicates, whereas the opposite order would lose data.
type OffsetStore struct {
	mu      sync.Mutex
	db      *storage.Store
	m       map[string]watcher.FileState
	dirty   map[string]struct{}
	deleted map[string]struct{}
}

// LoadOffsets reads the persisted offsets.
func LoadOffsets(db *storage.Store) (*OffsetStore, error) {
	all, err := db.AllOffsets()
	if err != nil {
		return nil, err
	}
	o := &OffsetStore{db: db, m: make(map[string]watcher.FileState, len(all)),
		dirty: map[string]struct{}{}, deleted: map[string]struct{}{}}
	for p, st := range all {
		o.m[p] = watcher.FileState{ID: watcher.FileID{Dev: st.Dev, Ino: st.Ino}, Offset: st.Offset, Size: st.Size}
	}
	return o, nil
}

func (o *OffsetStore) Get(path string) (watcher.FileState, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st, ok := o.m[path]
	return st, ok
}

func (o *OffsetStore) Set(path string, st watcher.FileState) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.m[path] = st
	o.dirty[path] = struct{}{}
	delete(o.deleted, path)
	return nil
}

func (o *OffsetStore) Delete(path string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.m, path)
	delete(o.dirty, path)
	o.deleted[path] = struct{}{}
	return nil
}

// DeletePrefix forgets every offset whose path starts with prefix.
func (o *OffsetStore) DeletePrefix(prefix string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for p := range o.m {
		if strings.HasPrefix(p, prefix) {
			delete(o.m, p)
			delete(o.dirty, p)
			o.deleted[p] = struct{}{}
		}
	}
	return nil
}

func (o *OffsetStore) Paths() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	paths := make([]string, 0, len(o.m))
	for p := range o.m {
		paths = append(paths, p)
	}
	return paths
}

// Pending is a copy of the changes not yet written; pass it to Commit after the snapshot
// that covers them has been saved.
type Pending struct {
	set map[string]storage.FileState
	del []string
}

// Empty reports whether there is nothing to write.
func (p Pending) Empty() bool { return len(p.set) == 0 && len(p.del) == 0 }

// Pending copies the changes accumulated so far.
func (o *OffsetStore) Pending() Pending {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := Pending{set: make(map[string]storage.FileState, len(o.dirty))}
	for path := range o.dirty {
		st := o.m[path]
		p.set[path] = storage.FileState{Dev: st.ID.Dev, Ino: st.ID.Ino, Offset: st.Offset, Size: st.Size}
	}
	for path := range o.deleted {
		p.del = append(p.del, path)
	}
	return p
}

// Commit writes p. Paths changed again since Pending stay dirty; a failed write keeps
// everything dirty, so the next Commit retries.
func (o *OffsetStore) Commit(p Pending) error {
	if p.Empty() {
		return nil
	}
	if err := o.db.ApplyOffsets(p.set, p.del); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for path, written := range p.set {
		cur, ok := o.m[path]
		if ok && cur.Offset == written.Offset && cur.Size == written.Size && cur.ID.Dev == written.Dev && cur.ID.Ino == written.Ino {
			delete(o.dirty, path)
		}
	}
	for _, path := range p.del {
		if _, back := o.m[path]; !back {
			delete(o.deleted, path)
		}
	}
	return nil
}

// Reset forgets every offset (memory and database) so all transcripts are read again.
func (o *OffsetStore) Reset() error {
	o.mu.Lock()
	var del []string
	for p := range o.m {
		del = append(del, p)
	}
	for p := range o.deleted {
		del = append(del, p)
	}
	o.m = map[string]watcher.FileState{}
	o.dirty = map[string]struct{}{}
	o.deleted = map[string]struct{}{}
	o.mu.Unlock()
	return o.db.ApplyOffsets(nil, del)
}

var _ watcher.OffsetStore = (*OffsetStore)(nil)
