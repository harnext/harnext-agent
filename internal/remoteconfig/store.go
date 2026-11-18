package remoteconfig

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	storeVersion  = 1
	maxStoreBytes = 256 * 1024
)

// StoredState is what survives a restart. The envelope is stored as received (signed), so
// it is re-verified on every load; the file itself carries no trust. There is no encryption
// by decision: a local key would not protect against the local user, and integrity comes
// from the signature.
type StoredState struct {
	Envelope Envelope
	ETag     string
	// LastAcceptedVersion is the rollback floor: the highest config_version ever accepted.
	LastAcceptedVersion int64
	LastCheck           time.Time
}

type storeFile struct {
	Version             int       `json:"v"`
	Envelope            *Envelope `json:"envelope,omitempty"`
	ETag                string    `json:"etag,omitempty"`
	LastAcceptedVersion int64     `json:"last_accepted_version"`
	LastCheck           time.Time `json:"last_check"`
}

// Store persists StoredState in one file (0600) inside a 0700 directory.
type Store struct{ path string }

// NewStore returns a store for path, normally inside the user's config directory.
func NewStore(path string) *Store { return &Store{path: path} }

// Load returns the stored state. A missing, unreadable, oversized or corrupt file yields
// the zero state and ok=false; it never fails hard.
func (s *Store) Load() (st StoredState, ok bool) {
	f, err := os.Open(s.path)
	if err != nil {
		return StoredState{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStoreBytes+1))
	if err != nil || len(data) > maxStoreBytes {
		return StoredState{}, false
	}
	var raw storeFile
	if json.Unmarshal(data, &raw) != nil || raw.Version != storeVersion || raw.LastAcceptedVersion < 0 {
		return StoredState{}, false
	}
	st = StoredState{ETag: raw.ETag, LastAcceptedVersion: raw.LastAcceptedVersion, LastCheck: raw.LastCheck.UTC()}
	if raw.Envelope != nil {
		st.Envelope = *raw.Envelope
	}
	return st, true
}

// Save writes atomically: temporary file in the same directory, fsync, rename.
func (s *Store) Save(st StoredState) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("remoteconfig: store directory")
	}
	raw := storeFile{Version: storeVersion, ETag: st.ETag, LastAcceptedVersion: st.LastAcceptedVersion, LastCheck: st.LastCheck.UTC()}
	if st.Envelope.Payload != "" {
		env := st.Envelope
		raw.Envelope = &env
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return errors.New("remoteconfig: store encode")
	}

	tmp, err := os.CreateTemp(dir, ".agent-config-*.tmp")
	if err != nil {
		return errors.New("remoteconfig: store write")
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return errors.New("remoteconfig: store write")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return errors.New("remoteconfig: store write")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return errors.New("remoteconfig: store write")
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return errors.New("remoteconfig: store write")
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		cleanup()
		return errors.New("remoteconfig: store write")
	}
	return nil
}
