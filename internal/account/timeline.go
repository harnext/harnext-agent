package account

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type entry struct {
	Hash  string `json:"h"`
	Since int64  `json:"s"`
}

type snapshot struct {
	Version int                `json:"v"`
	Dirs    map[string][]entry `json:"dirs"`
}

// Timeline records which account hash each config directory had since when. It holds no e-mail and no path.
type Timeline struct {
	mu   sync.Mutex
	dirs map[string][]entry
}

// DirID identifies a config directory without storing its path.
func DirID(configDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(configDir)))
	return hex.EncodeToString(sum[:])[:16]
}

// Observe appends an entry only when the account changed.
func (t *Timeline) Observe(dirID, hash string, at time.Time) {
	if dirID == "" || hash == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dirs == nil {
		t.dirs = map[string][]entry{}
	}
	list := t.dirs[dirID]
	since := at.Unix()
	if n := len(list); n > 0 {
		if list[n-1].Hash == hash {
			return
		}
		if since < list[n-1].Since {
			since = list[n-1].Since // keep the list sorted
		}
	}
	t.dirs[dirID] = append(list, entry{Hash: hash, Since: since})
}

// HashAt returns the account of a directory at a moment. Before the first observation it answers only when the directory
// never had a second account; otherwise it fails closed.
func (t *Timeline) HashAt(dirID string, at time.Time) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.dirs[dirID]
	if len(list) == 0 {
		return "", false
	}
	sec := at.Unix()
	if i := sort.Search(len(list), func(i int) bool { return list[i].Since > sec }); i > 0 {
		return list[i-1].Hash, true
	}
	for _, e := range list[1:] {
		if e.Hash != list[0].Hash {
			return "", false
		}
	}
	return list[0].Hash, true
}

// Snapshot returns the JSON to persist.
func (t *Timeline) Snapshot() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	dirs := t.dirs
	if dirs == nil {
		dirs = map[string][]entry{}
	}
	b, _ := json.Marshal(snapshot{Version: 1, Dirs: dirs})
	return b
}

// Restore replaces the state from Snapshot output. Invalid entries are dropped; unreadable input leaves the state empty.
func (t *Timeline) Restore(data []byte) error {
	var s snapshot
	err := json.Unmarshal(data, &s)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dirs = map[string][]entry{}
	if err != nil {
		return err
	}
	for id, list := range s.Dirs {
		var clean []entry
		for _, e := range list {
			if isHexHash(e.Hash) {
				clean = append(clean, e)
			}
		}
		sort.SliceStable(clean, func(i, j int) bool { return clean[i].Since < clean[j].Since })
		if len(clean) > 0 {
			t.dirs[id] = clean
		}
	}
	return nil
}

func isHexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
