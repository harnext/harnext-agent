package uploader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/harnext/harnext-agent/internal/aggregate"
)

func TestPreviewIsExactlyWhatSyncSends(t *testing.T) {
	var sent []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{"accepted":2,"rejected":0,"errors":[]}`)
	}))
	defer ts.Close()
	rows := []aggregate.Row{row("a"), row("b")}
	u, _ := newUploader(t, ts.URL, newLedger())
	if _, err := u.Sync(context.Background(), rows, 7); err != nil {
		t.Fatal(err)
	}

	p := Preview(rows, PreviewOptions{AgentVersion: "0.1.0", ConfigVersion: 7})
	if string(p.Body) != string(sent) || p.Sessions != 2 || p.Total != 2 || p.Invalid != 0 {
		t.Fatalf("preview differs from the upload:\n%s\n%s\n%+v", p.Body, sent, p)
	}
}

func TestPreviewLimitsToTheNewestSessions(t *testing.T) {
	old, newer := row("old"), row("new")
	old.EndedAt = old.EndedAt.AddDate(0, 0, -1)
	old.StartedAt = old.StartedAt.AddDate(0, 0, -1)

	p := Preview([]aggregate.Row{old, newer}, PreviewOptions{AgentVersion: "0.1.0", MaxSessions: 1})
	if p.Sessions != 1 || p.Total != 2 {
		t.Fatalf("preview = %+v", p)
	}
	if !contains(p.Body, `"session_external_id":"new"`) || contains(p.Body, `"session_external_id":"old"`) {
		t.Fatalf("not the newest session: %s", p.Body)
	}
}

func TestPreviewCountsInvalidSessionsAndSendsNothingForThem(t *testing.T) {
	bad := row("bad")
	bad.Model = ""
	p := Preview([]aggregate.Row{bad}, PreviewOptions{AgentVersion: "0.1.0"})
	if p.Invalid != 1 || p.Total != 0 || p.Sessions != 0 || contains(p.Body, "bad") {
		t.Fatalf("preview = %+v body=%s", p, p.Body)
	}
}

func contains(b []byte, s string) bool { return bytes.Contains(b, []byte(s)) }
