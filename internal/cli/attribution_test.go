package cli

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/service"
	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/uploader"
)

type fakeView struct{ sum service.GateSummary }

func (f fakeView) Decide(aggregate.Row) uploader.Decision { return testGate(aggregate.Row{}) }
func (f fakeView) Summary() service.GateSummary           { return f.sum }

var (
	fullHash   = regexp.MustCompile(`[0-9a-f]{64}`)
	clearEmail = regexp.MustCompile(`[A-Za-z0-9._-]+@[A-Za-z0-9.-]+`)
)

// noClearIdentity fails when the output holds a full hash or an e-mail address that is not masked.
func noClearIdentity(t *testing.T, out string) {
	t.Helper()
	if fullHash.MatchString(out) {
		t.Fatalf("output holds a full hash:\n%s", out)
	}
	for _, m := range clearEmail.FindAllString(out, -1) {
		if !strings.Contains(m, "*") {
			t.Fatalf("output holds an unmasked e-mail %q:\n%s", m, out)
		}
	}
}

func heldBackSummary() service.GateSummary {
	return service.GateSummary{
		Identity: service.IdentityStatus{State: service.IdentityOK, Bindings: 2, ExpiresAt: time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)},
		Accounts: service.AccountsStatus{
			Sendable: 14,
			Skipped:  map[string]int{"account_unbound": 3, "no_account": 1, "git_email_missing": 2, "identity_pending": 0},
			Pending: []service.PendingAccount{
				{Tool: "claude_code", Label: "w***@example.test", Sessions: 3},
				{Tool: "vscode_copilot", Label: "d***@example.test", Sessions: 1},
				{Tool: "claude_code", Label: "leak@example.test", Sessions: 9}, // an unmasked label must never be shown
			},
			PendingTotal: 5,
		},
	}
}

func TestStatusShowsIdentityReasonsAndPendingLabels(t *testing.T) {
	p := newPaths(t)
	exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	sum := heldBackSummary()
	now := time.Now().UTC()
	if err := p.WriteStatus(service.StatusFile{
		UpdatedAt: now, Identity: sum.Identity, Accounts: sum.Accounts,
		Upload: service.UploadStatus{LastAttempt: now, Accepted: 14, Skipped: 6, Held: 1},
	}); err != nil {
		t.Fatal(err)
	}
	r := exec(t, p, "", "status")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	for _, want := range []string{
		"Identity:  ok (2 accounts connected, valid until ",
		"Accounts:  14 sessions ready to send; kept on this machine: no_account 1, git_email_missing 2, account_unbound 3\n",
		"Pending:   w***@example.test (claude_code, 3 sessions); d***@example.test (vscode_copilot, 1 session) and 3 more\n",
		"           Connect them in the Harnext panel under Connected Agents.\n",
		"6 kept on this machine; 1 refused by the server (account not connected)",
	} {
		if !strings.Contains(r.out, want) {
			t.Fatalf("status lacks %q:\n%s", want, r.out)
		}
	}
	if strings.Contains(r.out, "leak@") || strings.Contains(r.out, "identity_pending") {
		t.Fatalf("status shows an unmasked label or an empty reason:\n%s", r.out)
	}
	noClearIdentity(t, r.out)
}

func TestStatusIdentityStates(t *testing.T) {
	p := newPaths(t)
	exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	for state, want := range map[string]string{
		service.IdentityPending:       "Identity:  pending (no verified identity yet; sessions stay on this machine); last error: transient\n",
		service.IdentityExpired:       "Identity:  expired (sessions stay on this machine until it is renewed)\n",
		service.IdentityNoTrustedKeys: "Identity:  no trusted keys (this build cannot verify identities; sessions stay on this machine)\n",
	} {
		id := service.IdentityStatus{State: state}
		if state == service.IdentityPending {
			id.LastError = "transient"
		}
		if err := p.WriteStatus(service.StatusFile{UpdatedAt: time.Now(), Identity: id}); err != nil {
			t.Fatal(err)
		}
		r := exec(t, p, "", "status")
		if !strings.Contains(r.out, want) || !strings.Contains(r.out, "Accounts:  0 sessions ready to send\n") || strings.Contains(r.out, "Connect them") {
			t.Fatalf("state %s:\n%s", state, r.out)
		}
	}
	// A status file from a run with a test gate has no identity block.
	_ = p.WriteStatus(service.StatusFile{UpdatedAt: time.Now()})
	if r := exec(t, p, "", "status"); strings.Contains(r.out, "Identity:") {
		t.Fatalf("identity line without identity data:\n%s", r.out)
	}
}

func execView(t *testing.T, p service.Paths, view service.GateView, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	root := New(Env{Paths: p, Version: "9.9.9", In: strings.NewReader(""), Out: &out, Err: &errb,
		PreviewGate: func(*storage.Store) (service.GateView, error) { return view, nil }})
	root.SetArgs(args)
	err := root.Execute()
	return result{out.String(), errb.String(), err}
}

func TestPreviewAndStatsPrintTheGateSummary(t *testing.T) {
	p := newPaths(t)
	db, err := storage.Open(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	seedAggregate(t, db)
	db.Close()
	view := fakeView{sum: heldBackSummary()}

	r := execView(t, p, view, "preview")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	for _, want := range []string{"Showing 1 of 2 sessions", "Identity:  ok", "Pending:   w***@example.test", "Connected Agents"} {
		if !strings.Contains(r.err, want) {
			t.Fatalf("preview stderr lacks %q:\n%s", want, r.err)
		}
	}
	if strings.Contains(r.out, "Identity:") {
		t.Fatalf("the summary must not mix into the JSON on stdout:\n%s", r.out)
	}
	noClearIdentity(t, r.err)

	r = execView(t, p, view, "stats")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if !strings.Contains(r.out, "TOTAL") || !strings.Contains(r.out, "Identity:  ok") || !strings.Contains(r.out, "Connected Agents") {
		t.Fatalf("stats:\n%s", r.out)
	}
	noClearIdentity(t, r.out)
}
