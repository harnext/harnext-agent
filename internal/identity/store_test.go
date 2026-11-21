package identity

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

func TestStoreRoundTripAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	s := NewStore(filepath.Join(dir, "agent-identity.json"))
	if _, ok := s.Load(); ok {
		t.Fatal("missing file reported as loaded")
	}
	want := StoredState{
		Envelope: remoteconfig.Envelope{Payload: "p", Sig: "s", KID: "k"}, ETag: `"e"`,
		LastAcceptedIssuedAt: t0, LastCheck: t0.Add(5 * time.Minute),
	}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Load()
	if !ok || got != want {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(s.path)
		di, _ := os.Stat(dir)
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Errorf("file %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
		}
	}
	want.LastAcceptedIssuedAt = t0.Add(time.Hour)
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(); !got.LastAcceptedIssuedAt.Equal(want.LastAcceptedIssuedAt) {
		t.Errorf("overwrite: %+v", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("leftover temporary files: %d entries", len(entries))
	}
}

func TestStoreCorruptFilesMeanNoDocument(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"empty":         {},
		"garbage":       []byte("\x00\x01\x02 not json"),
		"wrong version": []byte(`{"v":99,"envelope":{"payload":"a","sig":"b","kid":"c"}}`),
		"oversized":     []byte(`{"v":1,"etag":"` + strings.Repeat("a", maxStoreBytes) + `"}`),
		"truncated":     []byte(`{"v":1,"envelope":{"payload":"a"`),
		"array":         []byte(`[]`),
	}
	for name, content := range cases {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if st, ok := NewStore(path).Load(); ok || st != (StoredState{}) {
			t.Errorf("%s: loaded %+v", name, st)
		}
	}
	if _, ok := NewStore(dir).Load(); ok {
		t.Error("directory loaded")
	}
}

func TestStoreSaveFailureIsReported(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent "directory" is a regular file.
	if err := NewStore(filepath.Join(blocker, "agent-identity.json")).Save(StoredState{}); err == nil {
		t.Fatal("want an error")
	}
}
