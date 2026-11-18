package storage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "agent.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOffsetsRoundTripAndPersist(t *testing.T) {
	s, path := openTemp(t)
	want := FileState{Dev: 1<<63 + 5, Ino: 1<<64 - 1, Offset: 1234, Size: 2000}
	if err := s.SetOffset("/a/b.jsonl", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetOffset("/a/b.jsonl")
	if err != nil || !ok || got != want {
		t.Fatalf("got %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
	if _, ok, _ := s.GetOffset("/missing"); ok {
		t.Fatal("missing path reported as found")
	}

	want.Offset = 1500
	if err := s.SetOffset("/a/b.jsonl", want); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOffset("/a/a.jsonl", FileState{}); err != nil {
		t.Fatal(err)
	}
	paths, err := s.OffsetPaths()
	if err != nil || len(paths) != 2 || paths[0] != "/a/a.jsonl" {
		t.Fatalf("paths = %v err=%v", paths, err)
	}

	// A new process opening the same file sees the data.
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, ok, _ = s2.GetOffset("/a/b.jsonl")
	if !ok || got.Offset != 1500 {
		t.Fatalf("after reopen: %+v ok=%v", got, ok)
	}
	if err := s2.DeleteOffset("/a/b.jsonl"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s2.GetOffset("/a/b.jsonl"); ok {
		t.Fatal("deleted offset still present")
	}
}

func TestStateBlob(t *testing.T) {
	s, _ := openTemp(t)
	if _, ok, err := s.LoadState("aggregate"); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	for _, v := range []string{"one", "two"} {
		if err := s.SaveState("aggregate", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	b, ok, err := s.LoadState("aggregate")
	if err != nil || !ok || string(b) != "two" {
		t.Fatalf("got %q ok=%v err=%v", b, ok, err)
	}
}

func TestUploadedDigests(t *testing.T) {
	s, _ := openTemp(t)
	at := time.Unix(1_700_000_000, 0)
	if err := s.MarkUploaded(map[string]string{"k1": "d1", "k2": "d2"}, at); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUploaded(map[string]string{"k1": "d1b"}, at); err != nil {
		t.Fatal(err)
	}
	if d, ok, _ := s.UploadedDigest("k1"); !ok || d != "d1b" {
		t.Fatalf("k1 = %q ok=%v", d, ok)
	}
	if _, ok, _ := s.UploadedDigest("zz"); ok {
		t.Fatal("unknown key found")
	}
	if n, _ := s.UploadedCount(); n != 2 {
		t.Fatalf("count = %d", n)
	}
}

func TestFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	_, path := openTemp(t)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("db mode = %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", di.Mode().Perm())
	}
}

func TestNewerSchemaRejected(t *testing.T) {
	s, path := openTemp(t)
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path); err != ErrNewerSchema {
		t.Fatalf("err = %v, want ErrNewerSchema", err)
	}
}

func TestErrorsDoNotEchoPaths(t *testing.T) {
	// A directory in place of the database file makes Open fail with a path-bearing system error.
	dir := filepath.Join(t.TempDir(), "secret-project-name")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-project-name") {
		t.Fatalf("error echoes path: %v", err)
	}
}
