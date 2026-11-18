// Package storage is the agent's local SQLite buffer: transcript offsets, the aggregator
// snapshot and the digest of every row already uploaded. It holds counters and local paths
// only, never transcript text, and its errors name the operation, never a path or value.
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	sqlite "modernc.org/sqlite" // pure-Go driver, no CGO
)

const schemaVersion = 1

const schemaSQL = `
CREATE TABLE IF NOT EXISTS offsets (
	path   TEXT PRIMARY KEY,
	dev    INTEGER NOT NULL,
	ino    INTEGER NOT NULL,
	off    INTEGER NOT NULL,
	size   INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS state (
	name TEXT PRIMARY KEY,
	blob BLOB NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS uploaded (
	row_key     TEXT PRIMARY KEY,
	digest      TEXT NOT NULL,
	uploaded_at INTEGER NOT NULL
) WITHOUT ROWID;
`

// FileState mirrors what the watcher remembers about one transcript.
type FileState struct {
	Dev, Ino     uint64
	Offset, Size int64
}

// ErrNewerSchema means the database was written by a newer agent.
var ErrNewerSchema = errors.New("storage: database schema is newer than this agent")

type Store struct {
	db *sql.DB
}

// Open creates the database (directory 0700, file 0600) and applies the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("storage: create directory: %w", sanitize(err))
	}
	// Create the file with 0600 before SQLite does, whatever the umask says.
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600); err != nil {
		return nil, fmt.Errorf("storage: create file: %w", sanitize(err))
	} else {
		f.Close()
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", sanitize(err))
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("storage: read version: %w", sanitize(err))
	}
	if v > schemaVersion {
		return ErrNewerSchema
	}
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("storage: apply schema: %w", sanitize(err))
	}
	if v < schemaVersion {
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("storage: set version: %w", sanitize(err))
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) GetOffset(path string) (FileState, bool, error) {
	var dev, ino int64
	var st FileState
	err := s.db.QueryRow("SELECT dev, ino, off, size FROM offsets WHERE path = ?", path).
		Scan(&dev, &ino, &st.Offset, &st.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return FileState{}, false, nil
	}
	if err != nil {
		return FileState{}, false, fmt.Errorf("storage: get offset: %w", sanitize(err))
	}
	st.Dev, st.Ino = uint64(dev), uint64(ino)
	return st, true, nil
}

func (s *Store) SetOffset(path string, st FileState) error {
	_, err := s.db.Exec(
		`INSERT INTO offsets (path, dev, ino, off, size) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET dev = excluded.dev, ino = excluded.ino,
		 off = excluded.off, size = excluded.size`,
		path, int64(st.Dev), int64(st.Ino), st.Offset, st.Size)
	if err != nil {
		return fmt.Errorf("storage: set offset: %w", sanitize(err))
	}
	return nil
}

func (s *Store) DeleteOffset(path string) error {
	if _, err := s.db.Exec("DELETE FROM offsets WHERE path = ?", path); err != nil {
		return fmt.Errorf("storage: delete offset: %w", sanitize(err))
	}
	return nil
}

// ApplyOffsets stores and deletes many offsets in one transaction.
func (s *Store) ApplyOffsets(set map[string]FileState, del []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: apply offsets: %w", sanitize(err))
	}
	defer tx.Rollback()
	for _, p := range del {
		if _, err := tx.Exec("DELETE FROM offsets WHERE path = ?", p); err != nil {
			return fmt.Errorf("storage: apply offsets: %w", sanitize(err))
		}
	}
	for p, st := range set {
		_, err := tx.Exec(
			`INSERT INTO offsets (path, dev, ino, off, size) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(path) DO UPDATE SET dev = excluded.dev, ino = excluded.ino,
			 off = excluded.off, size = excluded.size`,
			p, int64(st.Dev), int64(st.Ino), st.Offset, st.Size)
		if err != nil {
			return fmt.Errorf("storage: apply offsets: %w", sanitize(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: apply offsets: %w", sanitize(err))
	}
	return nil
}

// OffsetPaths returns the known paths, sorted.
func (s *Store) OffsetPaths() ([]string, error) {
	rows, err := s.db.Query("SELECT path FROM offsets")
	if err != nil {
		return nil, fmt.Errorf("storage: list offsets: %w", sanitize(err))
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("storage: list offsets: %w", sanitize(err))
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list offsets: %w", sanitize(err))
	}
	slices.Sort(paths)
	return paths, nil
}

// AllOffsets returns every stored offset.
func (s *Store) AllOffsets() (map[string]FileState, error) {
	rows, err := s.db.Query("SELECT path, dev, ino, off, size FROM offsets")
	if err != nil {
		return nil, fmt.Errorf("storage: list offsets: %w", sanitize(err))
	}
	defer rows.Close()
	out := map[string]FileState{}
	for rows.Next() {
		var p string
		var dev, ino int64
		var st FileState
		if err := rows.Scan(&p, &dev, &ino, &st.Offset, &st.Size); err != nil {
			return nil, fmt.Errorf("storage: list offsets: %w", sanitize(err))
		}
		st.Dev, st.Ino = uint64(dev), uint64(ino)
		out[p] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list offsets: %w", sanitize(err))
	}
	return out, nil
}

// SaveState stores an opaque blob (the aggregator snapshot) under name.
func (s *Store) SaveState(name string, blob []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO state (name, blob) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET blob = excluded.blob`, name, blob)
	if err != nil {
		return fmt.Errorf("storage: save state: %w", sanitize(err))
	}
	return nil
}

// DeleteState removes the blob stored under name; a missing name is not an error.
func (s *Store) DeleteState(name string) error {
	if _, err := s.db.Exec("DELETE FROM state WHERE name = ?", name); err != nil {
		return fmt.Errorf("storage: delete state: %w", sanitize(err))
	}
	return nil
}

func (s *Store) LoadState(name string) ([]byte, bool, error) {
	var blob []byte
	err := s.db.QueryRow("SELECT blob FROM state WHERE name = ?", name).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("storage: load state: %w", sanitize(err))
	}
	return blob, true, nil
}

// UploadedDigest returns the digest recorded when rowKey was last accepted by the server.
func (s *Store) UploadedDigest(rowKey string) (string, bool, error) {
	var d string
	err := s.db.QueryRow("SELECT digest FROM uploaded WHERE row_key = ?", rowKey).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("storage: get digest: %w", sanitize(err))
	}
	return d, true, nil
}

// MarkUploaded records the accepted digest of each row in one transaction.
func (s *Store) MarkUploaded(digests map[string]string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: mark uploaded: %w", sanitize(err))
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(
		`INSERT INTO uploaded (row_key, digest, uploaded_at) VALUES (?, ?, ?)
		 ON CONFLICT(row_key) DO UPDATE SET digest = excluded.digest, uploaded_at = excluded.uploaded_at`)
	if err != nil {
		return fmt.Errorf("storage: mark uploaded: %w", sanitize(err))
	}
	defer stmt.Close()
	for k, d := range digests {
		if _, err := stmt.Exec(k, d, at.Unix()); err != nil {
			return fmt.Errorf("storage: mark uploaded: %w", sanitize(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: mark uploaded: %w", sanitize(err))
	}
	return nil
}

// UploadedCount is the number of rows known to be uploaded.
func (s *Store) UploadedCount() (int, error) {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM uploaded").Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count uploaded: %w", sanitize(err))
	}
	return n, nil
}

// sanitize drops error text that could echo paths or values; only the error class is kept.
func sanitize(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		return fmt.Errorf("sqlite code %d", se.Code())
	}
	return errors.New("sqlite error")
}
