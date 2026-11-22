package service

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/watcher"
)

func TestValidateToken(t *testing.T) {
	for tok, ok := range map[string]bool{
		"hxt_" + strings.Repeat("a", 43):  true,
		"hxt_A-b_9":                       true,
		"":                                false,
		"hxt_":                            false,
		"abc_123":                         false,
		"hxt_has space":                   false,
		"hxt_new\nline":                   false,
		"hxt_" + strings.Repeat("a", 200): false,
	} {
		if err := ValidateToken(tok); (err == nil) != ok {
			t.Errorf("ValidateToken(%q) = %v, want ok=%v", tok, err, ok)
		}
	}
}

func TestValidateServerURL(t *testing.T) {
	for _, c := range []struct {
		url      string
		insecure bool
		ok       bool
	}{
		{"https://app.example.com", false, true},
		{"https://app.example.com/prefix", false, true},
		{"http://app.example.com", true, false},
		{"http://localhost:3000", false, false},
		{"http://localhost:3000", true, true},
		{"http://[::1]:3000", true, true},
		{"https://user:pw@app.example.com", false, false},
		{"https://app.example.com?x=1", false, false},
		{"file:///etc/passwd", true, false},
		{"", false, false},
	} {
		if err := ValidateServerURL(c.url, c.insecure); (err == nil) != c.ok {
			t.Errorf("ValidateServerURL(%q, %v) = %v, want ok=%v", c.url, c.insecure, err, c.ok)
		}
	}
}

func TestSettingsAndAccountsRoundTrip(t *testing.T) {
	p := PathsIn(filepath.Join(t.TempDir(), "harnext"))
	if _, err := p.LoadSettings(); err != ErrNotInitialized {
		t.Fatalf("LoadSettings = %v, want ErrNotInitialized", err)
	}
	if _, err := p.LoadAccounts(); err != ErrNotInitialized {
		t.Fatalf("LoadAccounts = %v, want ErrNotInitialized", err)
	}
	want := Settings{ServerURL: "http://127.0.0.1:3000", AllowInsecureLocalhost: true}
	if err := p.SaveSettings(want); err != nil {
		t.Fatal(err)
	}
	tok := "hxt_" + strings.Repeat("Z", 43)
	if err := p.SaveToken(tok); err != nil {
		t.Fatal(err)
	}
	if got, err := p.LoadSettings(); err != nil || got != want {
		t.Fatalf("settings = %+v err=%v", got, err)
	}
	got, err := p.LoadAccounts()
	if err != nil || len(got) != 1 || got[0].Token != tok || got[0].ID != AccountID(tok) || len(got[0].ID) != 10 {
		t.Fatalf("accounts = %+v err=%v", got, err)
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{p.Settings, p.Accounts} {
			if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", filepath.Base(f), fi.Mode().Perm())
			}
		}
		if di, _ := os.Stat(p.Dir); di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
		}
	}
}

func TestAddAndRemoveAccounts(t *testing.T) {
	p := PathsIn(filepath.Join(t.TempDir(), "harnext"))
	a, b := "hxt_"+strings.Repeat("A", 43), "hxt_"+strings.Repeat("B", 43)
	first, err := p.AddAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	// Adding the same token again changes nothing.
	again, err := p.AddAccount(a)
	if err != nil || again != first {
		t.Fatalf("again = %+v, %v", again, err)
	}
	second, err := p.AddAccount(b)
	if err != nil || second.ID == first.ID {
		t.Fatalf("second = %+v, %v", second, err)
	}
	list, _ := p.LoadAccounts()
	if len(list) != 2 || list[0].Token != a || list[1].Token != b {
		t.Fatalf("order not kept: %+v", list)
	}
	if err := os.WriteFile(p.IdentityFileFor(first.ID), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An empty prefix is ambiguous with two accounts, an unknown one matches nothing.
	if _, err := p.RemoveAccount(""); err != ErrAmbiguousAccount {
		t.Fatalf("empty prefix = %v", err)
	}
	if _, err := p.RemoveAccount("zzzz"); err != ErrNoSuchAccount {
		t.Fatalf("unknown id = %v", err)
	}
	gone, err := p.RemoveAccount(first.ID[:4])
	if err != nil || gone != first {
		t.Fatalf("removed %+v, %v", gone, err)
	}
	if _, err := os.Stat(p.IdentityFileFor(first.ID)); !os.IsNotExist(err) {
		t.Error("the identity document of the removed account was kept")
	}
	if list, _ := p.LoadAccounts(); len(list) != 1 || list[0].Token != b {
		t.Fatalf("after removal: %+v", list)
	}
	// The last account can be removed by an empty prefix; the file goes away.
	if _, err := p.RemoveAccount(""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.LoadAccounts(); err != ErrNotInitialized {
		t.Fatalf("after the last removal: %v", err)
	}
}

func TestAccountLimitAndInvalidFile(t *testing.T) {
	p := PathsIn(filepath.Join(t.TempDir(), "harnext"))
	for i := 0; i < MaxAccounts; i++ {
		if _, err := p.AddAccount(fmt.Sprintf("hxt_%043d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.AddAccount(fmt.Sprintf("hxt_%043d", MaxAccounts)); err != ErrTooManyAccounts {
		t.Fatalf("ninth account = %v", err)
	}
	for _, bad := range []string{`{`, `{"v":2,"accounts":[]}`, `{"v":1,"accounts":[{"token":"nope"}]}`,
		`{"v":1,"accounts":[{"token":"hxt_aaaa"},{"token":"hxt_aaaa"}]}`} {
		if err := os.WriteFile(p.Accounts, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := p.LoadAccounts(); err == nil || err == ErrNotInitialized {
			t.Errorf("accepted %q: %v", bad, err)
		}
	}
}

func TestInvalidInputsAreRejectedAndNotWritten(t *testing.T) {
	p := PathsIn(filepath.Join(t.TempDir(), "harnext"))
	if err := p.SaveToken("not-a-token"); err == nil {
		t.Fatal("bad token saved")
	}
	if err := p.SaveSettings(Settings{ServerURL: "http://example.com"}); err == nil {
		t.Fatal("insecure remote URL saved")
	}
	if _, err := os.Stat(p.Accounts); !os.IsNotExist(err) {
		t.Fatal("accounts file exists")
	}
	// A tampered settings file is refused at load time, not trusted.
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Settings, []byte(`{"server_url":"http://evil.example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.LoadSettings(); err == nil || err == ErrNotInitialized {
		t.Fatalf("tampered settings: err = %v", err)
	}
}

func TestCopilotOTelPathSetting(t *testing.T) {
	p := PathsIn(filepath.Join(t.TempDir(), "harnext"))
	if got, want := p.CopilotOTelFile(Settings{}), filepath.Join(p.Dir, "copilot-otel.jsonl"); got != want {
		t.Fatalf("default = %q, want %q", got, want)
	}
	good := filepath.Join(t.TempDir(), "otel.jsonl")
	if got := p.CopilotOTelFile(Settings{CopilotOTelPath: good}); got != good {
		t.Fatalf("override = %q", got)
	}
	for _, bad := range []string{"relative/otel.jsonl", "~/otel.jsonl", good + "\x00", good + "\n", "/" + strings.Repeat("a", 1024)} {
		if err := p.SaveSettings(Settings{ServerURL: "https://example.com", CopilotOTelPath: bad}); err == nil {
			t.Fatalf("path %q saved", bad)
		}
	}
	if err := p.SaveSettings(Settings{ServerURL: "https://example.com", CopilotOTelPath: good}); err != nil {
		t.Fatal(err)
	}
	if s, err := p.LoadSettings(); err != nil || s.CopilotOTelPath != good {
		t.Fatalf("load = %+v, %v", s, err)
	}
	raw := `{"server_url":"https://example.com","copilot_otel_path":"rel/x"}`
	if err := os.WriteFile(p.Settings, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.LoadSettings(); err == nil {
		t.Fatal("tampered copilot path accepted")
	}
}

func TestErrorsDoNotLeakTokenOrPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secret-dir")
	p := PathsIn(dir)
	if err := os.MkdirAll(p.Accounts, 0o700); err != nil { // a directory where the file should be
		t.Fatal(err)
	}
	tok := "hxt_" + strings.Repeat("Q", 43)
	err := p.SaveToken(tok)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-dir") || strings.Contains(err.Error(), tok) {
		t.Fatalf("error leaks: %v", err)
	}
}

func TestHomeEnvOverride(t *testing.T) {
	t.Setenv(envHome, "/tmp/harnext-test-home")
	if d, err := Home(); err != nil || d != "/tmp/harnext-test-home" {
		t.Fatalf("Home = %q err=%v", d, err)
	}
}

func openDB(t *testing.T) (*storage.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestOffsetStoreWriteBehind(t *testing.T) {
	db, path := openDB(t)
	o, err := LoadOffsets(db)
	if err != nil {
		t.Fatal(err)
	}
	a := watcher.FileState{ID: watcher.FileID{Dev: 1, Ino: 2}, Offset: 10, Size: 20}
	_ = o.Set("/a", a)
	_ = o.Set("/b", watcher.FileState{Offset: 5, Size: 5})
	if got, ok := o.Get("/a"); !ok || got != a {
		t.Fatalf("Get = %+v ok=%v", got, ok)
	}
	if paths, _ := db.OffsetPaths(); len(paths) != 0 {
		t.Fatalf("written before Commit: %v", paths)
	}

	p := o.Pending()
	// A change after Pending must stay dirty after Commit.
	_ = o.Set("/a", watcher.FileState{ID: a.ID, Offset: 15, Size: 20})
	if err := o.Commit(p); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := db.GetOffset("/a"); got.Offset != 10 {
		t.Fatalf("stored offset = %d, want the Pending value 10", got.Offset)
	}
	p2 := o.Pending()
	if len(p2.set) != 1 {
		t.Fatalf("still dirty = %d, want 1 (/a)", len(p2.set))
	}
	if err := o.Commit(p2); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := db.GetOffset("/a"); got.Offset != 15 {
		t.Fatalf("stored offset = %d, want 15", got.Offset)
	}
	if !o.Pending().Empty() {
		t.Fatal("pending not empty after full commit")
	}

	_ = o.Delete("/b")
	if err := o.Commit(o.Pending()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.GetOffset("/b"); ok {
		t.Fatal("deleted offset still stored")
	}

	// A new process sees what was committed.
	db.Close()
	db2, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	o2, err := LoadOffsets(db2)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := o2.Get("/a"); !ok || got.Offset != 15 || got.ID.Ino != 2 {
		t.Fatalf("reloaded = %+v ok=%v", got, ok)
	}
	if len(o2.Paths()) != 1 {
		t.Fatalf("paths = %v", o2.Paths())
	}
}

func TestOffsetStoreReset(t *testing.T) {
	db, _ := openDB(t)
	o, _ := LoadOffsets(db)
	_ = o.Set("/a", watcher.FileState{Offset: 1})
	if err := o.Commit(o.Pending()); err != nil {
		t.Fatal(err)
	}
	_ = o.Set("/c", watcher.FileState{Offset: 2})
	if err := o.Reset(); err != nil {
		t.Fatal(err)
	}
	if len(o.Paths()) != 0 || !o.Pending().Empty() {
		t.Fatal("memory not cleared")
	}
	if paths, _ := db.OffsetPaths(); len(paths) != 0 {
		t.Fatalf("database not cleared: %v", paths)
	}
}
