package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/storage"
)

func seedAggregate(t *testing.T, db *storage.Store) {
	t.Helper()
	agg := aggregate.New()
	agg.Add(asst("s1", "m1", "claude-sonnet-5", "/work/alpha", 10, 100))
	agg.Add(asst("s2", "m2", "claude-sonnet-5", "/work/beta", 1, 400))
	blob, _ := json.Marshal(agg.Snapshot())
	if err := db.SaveState("aggregate", blob); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewWithoutDataSendsAndPrintsNothing(t *testing.T) {
	r := exec(t, newPaths(t), "", "preview")
	if r.failure != nil || r.out != "" || !strings.Contains(r.err, "No data yet") {
		t.Fatalf("out=%q err=%q failure=%v", r.out, r.err, r.failure)
	}
}

func TestPreviewPrintsTheUploadBodyAndChangesNothing(t *testing.T) {
	p := newPaths(t)
	if r := exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com"); r.failure != nil {
		t.Fatal(r.failure)
	}
	db, err := storage.Open(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	seedAggregate(t, db)
	db.Close()

	r := exec(t, p, "", "preview", "--sessions", "2")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	var body struct {
		PayloadVersion int    `json:"payload_version"`
		AgentVersion   string `json:"agent_version"`
		Sessions       []struct {
			Project string `json:"project"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(r.out), &body); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, r.out)
	}
	if body.PayloadVersion != 6 || body.AgentVersion != "9.9.9" || len(body.Sessions) != 2 {
		t.Fatalf("body = %+v", body)
	}
	if !strings.Contains(r.err, "Showing 2 of 2 sessions") || !strings.Contains(r.err, "Nothing was sent") {
		t.Fatalf("stderr = %q", r.err)
	}
	if strings.Contains(r.out+r.err, tok) {
		t.Fatal("token printed")
	}

	// The default shows the newest session only.
	if r := exec(t, p, "", "preview"); !strings.Contains(r.err, "Showing 1 of 2 sessions") {
		t.Fatalf("stderr = %q", r.err)
	}

	// Without a test gate the production gate applies; this build has no trusted keys, so no session can be attributed.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	var out, errb bytes.Buffer
	root := New(Env{Paths: p, Version: "9.9.9", In: strings.NewReader(""), Out: &out, Err: &errb})
	root.SetArgs([]string{"preview"})
	if err := root.Execute(); err != nil || out.String() != "" ||
		!strings.Contains(errb.String(), "Nothing would be sent right now") || !strings.Contains(errb.String(), "no trusted keys") {
		t.Fatalf("production gate: out=%q err=%q failure=%v", out.String(), errb.String(), err)
	}
}
