package copilotid

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeState(t *testing.T, dir string, kv map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "state.vscdb")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for k, v := range kv {
		if _, err := db.Exec(`INSERT INTO ItemTable VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

const usage = `[{"extensionId":"github.copilot-chat","extensionName":"x"}]`

func TestReaderReadsActiveKnownIDAndPlan(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, map[string]string{
		"github.copilot-github": "octocat",
		"github-octocat-usages": usage,
		"github-mona-usages":    usage,
		"github-other-usages":   `[{"extensionId":"some.other"}]`,
		keyPolicy:               `{"accountPolicyData":{"accountId":"4242","policyData":{}}}`,
		keySKU:                  "copilot_enterprise_seat_quota",
		"unrelated":             "x",
	})
	r := &Reader{Dirs: []string{dir}}
	st, ok := r.Read()
	if !ok {
		t.Fatal("not read")
	}
	if st.Active != "octocat" || st.ActiveID != "4242" || st.Plan != "enterprise" {
		t.Fatalf("state %+v", st)
	}
	if len(st.Known) != 2 {
		t.Fatalf("known %v", st.Known)
	}
	if IDHash("4242") == "" || len(IDHash("4242")) != 64 {
		t.Fatal("hash")
	}
}

func TestReaderIgnoresInvalidAndMissing(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, map[string]string{"github.copilot-github": "bad name!", keyPolicy: `{"accountPolicyData":{"accountId":"12x"}}`})
	r := &Reader{Dirs: []string{dir, filepath.Join(dir, "missing")}}
	st, ok := r.Read()
	if !ok || st.Active != "" || st.ActiveID != "" || len(st.Known) != 0 {
		t.Fatalf("state %+v ok=%v", st, ok)
	}
	if _, ok := (&Reader{Dirs: []string{filepath.Join(dir, "none")}}).Read(); ok {
		t.Fatal("missing directory must not be readable")
	}
}

func TestReaderDoesNotTouchOriginalAndCachesUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, map[string]string{"github.copilot-github": "octocat"})
	path := filepath.Join(dir, "state.vscdb")
	before, _ := os.ReadFile(path)
	r := &Reader{Dirs: []string{dir}}
	a, _ := r.Read()
	b, _ := r.Read()
	if a.Active != "octocat" || b.Active != "octocat" {
		t.Fatal("not read")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("the original database was modified")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("unexpected files next to the database: %v", entries)
	}
}

func TestPlanFromSKU(t *testing.T) {
	for sku, want := range map[string]string{
		"copilot_enterprise_seat_quota":   "enterprise",
		"copilot_for_business_seat_quota": "business",
		"free_limited_copilot":            "free",
		"monthly_subscriber_quota":        "pro",
		"max_monthly_subscriber_quota":    "pro",
		"":                                "",
		"mystery":                         "",
	} {
		if got := PlanFromSKU(sku); got != want {
			t.Errorf("%q: got %q want %q", sku, got, want)
		}
	}
}

func TestTimelineAttribution(t *testing.T) {
	t0 := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	var tl Timeline
	// Nothing observed: unknown.
	if _, ok := tl.LoginAt(t0); ok {
		t.Fatal("no observation must be unknown")
	}
	tl.Observe(State{Active: "alice", ActiveID: "1", Plan: "pro", Known: []string{"alice"}}, t0)
	// One account ever seen: earlier sessions belong to it.
	if l, ok := tl.LoginAt(t0.Add(-48 * time.Hour)); !ok || l != "alice" {
		t.Fatalf("single account: %q %v", l, ok)
	}
	tl.Observe(State{Active: "bob", ActiveID: "2", Known: []string{"alice", "bob"}}, t0.Add(time.Hour))
	if _, ok := tl.LoginAt(t0.Add(-time.Hour)); ok {
		t.Fatal("two accounts: before the first observation is unknown")
	}
	if l, _ := tl.LoginAt(t0.Add(30 * time.Minute)); l != "alice" {
		t.Fatalf("got %q", l)
	}
	if l, _ := tl.LoginAt(t0.Add(2 * time.Hour)); l != "bob" {
		t.Fatalf("got %q", l)
	}
	h, l, ok := tl.HashAt(t0.Add(30 * time.Minute))
	if !ok || l != "alice" || h != IDHash("1") {
		t.Fatalf("hash %q %q %v", h, l, ok)
	}
	// Signed out: nothing is attributed.
	tl.Observe(State{Known: []string{"alice", "bob"}}, t0.Add(3*time.Hour))
	if _, ok := tl.LoginAt(t0.Add(4 * time.Hour)); ok {
		t.Fatal("signed out must be unknown")
	}
	// Login list: known ids first.
	logins := tl.Logins()
	if len(logins) != 2 || logins[0].IDHash == "" {
		t.Fatalf("logins %+v", logins)
	}
	if logins[0].Plan != "pro" && logins[1].Plan != "pro" {
		t.Fatal("plan lost")
	}
}

func TestTimelineUnknownIDAndRestore(t *testing.T) {
	t0 := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	var tl Timeline
	tl.Observe(State{Active: "alice", Known: []string{"alice", "ghost"}}, t0)
	if _, _, ok := tl.HashAt(t0.Add(time.Minute)); ok {
		t.Fatal("an account without a known id has no hash")
	}
	var back Timeline
	if err := back.Restore(tl.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if l, ok := back.LoginAt(t0.Add(time.Minute)); !ok || l != "alice" {
		t.Fatal("restore lost the entries")
	}
	for _, bad := range []string{"", "{", `{"v":2}`, `{"v":1,"known":["bad name"]}`, `{"v":1,"info":{"a":{"h":"short"}}}`} {
		var x Timeline
		if x.Restore([]byte(bad)) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
