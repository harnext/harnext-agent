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
	exp := time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)
	return service.GateSummary{
		Accounts: service.AccountsStatus{
			Harnext: []service.HarnextAccountStatus{
				{ID: "a1b2c3d4e5", Identity: service.IdentityStatus{State: service.IdentityOK, Bindings: 2, ExpiresAt: exp},
					ClaudeMatched: true, ClaudeLabel: "w***@example.test", CopilotSelected: true},
				{ID: "f6a7b8c9d0", Identity: service.IdentityStatus{State: service.IdentityOK, Bindings: 1, ExpiresAt: exp},
					LastReportError: "transient"},
				// An unmasked label must never be shown.
				{ID: "0011223344", Identity: service.IdentityStatus{State: service.IdentityOK, ExpiresAt: exp}, ClaudeMatched: true, ClaudeLabel: "leak@example.test"},
			},
			ClaudeFound:   3,
			CopilotLogins: 2,
			Sendable:      14,
			Skipped:       map[string]int{"no_matching_account": 3, "no_account": 1, "copilot_account_unknown": 2, "identity_pending": 0},
		},
	}
}

func TestStatusShowsAccountsReasonsAndHints(t *testing.T) {
	p := newPaths(t)
	exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	sum := heldBackSummary()
	now := time.Now().UTC()
	if err := p.WriteStatus(service.StatusFile{
		UpdatedAt: now, Accounts: sum.Accounts,
		Upload: service.UploadStatus{LastAttempt: now, Accepted: 14, Skipped: 6, Held: 1},
	}); err != nil {
		t.Fatal(err)
	}
	r := exec(t, p, "", "status")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	for _, want := range []string{
		"Accounts:  3 Harnext accounts on this machine\n",
		"  a1b2c3d4e5  identity ok (2 accounts connected, valid until ",
		"            Claude Code: w***@example.test\n",
		"            Copilot:     GitHub account selected\n",
		"            Claude Code: no signed-in account matches this Harnext account\n",
		"            Copilot:     no GitHub account selected\n",
		"            Report:      the last account report failed (transient)\n",
		"Found:     3 Claude Code accounts signed in, 2 GitHub accounts found for Copilot\n",
		"Sessions:  14 sessions ready to send; kept on this machine: no_account 1, copilot_account_unknown 2, no_matching_account 3\n",
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

// An unmatched Claude Code account is only counted, with a hint how to collect it; a selected GitHub account silences the Copilot hint.
func TestStatusHintsForUnmatchedAccounts(t *testing.T) {
	p := newPaths(t)
	exec(t, p, tok+"\n", "init", "--token-stdin", "--server", "https://app.example.com")
	ok := service.IdentityStatus{State: service.IdentityOK, ExpiresAt: time.Now().Add(time.Hour)}
	write := func(a service.AccountsStatus) string {
		if err := p.WriteStatus(service.StatusFile{UpdatedAt: time.Now(), Accounts: a}); err != nil {
			t.Fatal(err)
		}
		return exec(t, p, "", "status").out
	}
	out := write(service.AccountsStatus{Harnext: []service.HarnextAccountStatus{{ID: "a1b2c3d4e5", Identity: ok}}, ClaudeFound: 1, CopilotLogins: 1})
	for _, want := range []string{"Hint:      Claude Code is signed in with an address that is not the e-mail of a Harnext account", "Hint:      choose your GitHub account for Copilot in the Harnext panel under Agents."} {
		if !strings.Contains(out, want) {
			t.Fatalf("no hint %q:\n%s", want, out)
		}
	}
	out = write(service.AccountsStatus{Harnext: []service.HarnextAccountStatus{{ID: "a1b2c3d4e5", Identity: ok, ClaudeMatched: true, CopilotSelected: true}}, ClaudeFound: 1, CopilotLogins: 1})
	if strings.Contains(out, "Hint:") {
		t.Fatalf("hint although everything matches:\n%s", out)
	}
	// A status file from a run with a test gate has no accounts block.
	_ = p.WriteStatus(service.StatusFile{UpdatedAt: time.Now()})
	if r := exec(t, p, "", "status"); strings.Contains(r.out, "Accounts:") {
		t.Fatalf("accounts line without account data:\n%s", r.out)
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
	for _, want := range []string{"Showing 1 of 2 sessions", "Accounts:  3 Harnext accounts", "Claude Code: w***@example.test", "Found:     3 Claude Code accounts"} {
		if !strings.Contains(r.err, want) {
			t.Fatalf("preview stderr lacks %q:\n%s", want, r.err)
		}
	}
	if strings.Contains(r.out, "Accounts:") {
		t.Fatalf("the summary must not mix into the JSON on stdout:\n%s", r.out)
	}
	noClearIdentity(t, r.err)

	r = execView(t, p, view, "stats")
	if r.failure != nil {
		t.Fatal(r.failure)
	}
	if !strings.Contains(r.out, "TOTAL") || !strings.Contains(r.out, "Accounts:  3 Harnext accounts") || !strings.Contains(r.out, "Found:     3 Claude Code accounts") {
		t.Fatalf("stats:\n%s", r.out)
	}
	noClearIdentity(t, r.out)
}
