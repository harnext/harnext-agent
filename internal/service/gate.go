package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/harnext/harnext-agent/internal/account"
	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/attribution"
	"github.com/harnext/harnext-agent/internal/copilotid"
	"github.com/harnext/harnext-agent/internal/identity"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/uploader"
	"github.com/harnext/harnext-agent/internal/watcher"
)

const (
	accountsStateName  = "claude_accounts"
	copilotAcctState   = "copilot_accounts"
	accountCheckPeriod = 30 * time.Second // observation and report check
	accountReportEvery = 30 * time.Minute // refresh of an unchanged report
	accountReportGap   = 2 * time.Minute  // never more than one report per gap and account
)

// GateView is the account gate as status, stats and preview use it.
type GateView interface {
	Decide(aggregate.Row) uploader.Decision
	Summary() GateSummary
}

// acctRuntime is one Harnext account on this machine with its identity manager and report bookkeeping.
type acctRuntime struct {
	acct Account
	idm  *identity.Manager

	mu            sync.Mutex
	lastReport    time.Time
	lastReportErr string
	lastDigest    [32]byte
	haveDigest    bool
}

// accountGate is the production upload gate: the attribution gate over the signed identity documents of the Harnext accounts on
// this machine. It holds hashes, user names of GitHub accounts and masked labels only.
type accountGate struct {
	gate  *attribution.Gate
	accts []*acctRuntime
	keys  bool

	mu           sync.Mutex
	savedClaude  []byte
	savedCopilot []byte
}

func newAccountGate(home string, roots func() []watcher.Root, savedClaude, savedCopilot []byte, accts []*acctRuntime, cop attribution.CopilotSource, hasKeys bool, logf func(string, ...any)) *accountGate {
	tl := &account.Timeline{}
	if len(savedClaude) > 0 && tl.Restore(savedClaude) != nil && logf != nil {
		logf("saved account history is unreadable: starting it again")
	}
	ctl := &copilotid.Timeline{}
	if len(savedCopilot) > 0 && ctl.Restore(savedCopilot) != nil && logf != nil {
		logf("saved GitHub account history is unreadable: starting it again")
	}
	as := make([]attribution.Account, len(accts))
	for i, a := range accts {
		as[i] = attribution.Account{ID: a.acct.ID, Identity: a.idm}
	}
	return &accountGate{
		accts: accts, keys: hasKeys, savedClaude: savedClaude, savedCopilot: savedCopilot,
		gate: attribution.New(attribution.Options{Home: home, Roots: roots, Accounts: as, Timeline: tl, Copilot: cop, CopilotTimeline: ctl}),
	}
}

func (a *accountGate) Decide(row aggregate.Row) uploader.Decision { return a.gate.Decide(row) }

// saveTimeline writes the account histories when they changed; they hold no e-mail.
func (a *accountGate) saveTimeline(db *storage.Store) error {
	for _, h := range []struct {
		name  string
		blob  []byte
		saved *[]byte
	}{
		{accountsStateName, a.gate.TimelineSnapshot(), &a.savedClaude},
		{copilotAcctState, a.gate.CopilotSnapshot(), &a.savedCopilot},
	} {
		a.mu.Lock()
		same := bytes.Equal(h.blob, *h.saved)
		a.mu.Unlock()
		if same {
			continue
		}
		if err := db.SaveState(h.name, h.blob); err != nil {
			return err
		}
		a.mu.Lock()
		*h.saved = h.blob
		a.mu.Unlock()
	}
	return nil
}

// Summary is a content-free picture of the identities and of what the gate keeps back.
func (a *accountGate) Summary() GateSummary {
	var out GateSummary
	for _, rt := range a.accts {
		ids := rt.idm.Status()
		state := IdentityPending
		switch {
		case !a.keys:
			state = IdentityNoTrustedKeys
		case ids.Valid:
			state = IdentityOK
		case ids.HasDocument:
			state = IdentityExpired
		}
		h := HarnextAccountStatus{ID: rt.acct.ID, Identity: IdentityStatus{State: state, ExpiresAt: ids.ExpiresAt, LastCheck: ids.LastCheck, LastError: string(ids.LastError)}}
		if ids.HasDocument {
			h.Identity.Bindings = ids.Bindings
		}
		if rep, ok := a.gate.ReportFor(attribution.Account{ID: rt.acct.ID, Identity: rt.idm}); ok {
			h.ClaudeMatched, h.ClaudeLabel = rep.Claude.Matched, rep.Claude.Label
			if snap := rt.idm.Current(); snap.Valid() {
				_, h.CopilotSelected = snap.HashFor(identity.ToolVSCodeCopilot)
			}
		}
		rt.mu.Lock()
		h.LastReport, h.LastReportError = rt.lastReport, rt.lastReportErr
		rt.mu.Unlock()
		out.Accounts.Harnext = append(out.Accounts.Harnext, h)
	}
	st := a.gate.Stats()
	out.Accounts.Sendable = st.Bound
	if len(st.Skipped) > 0 {
		out.Accounts.Skipped = st.Skipped
	}
	out.Accounts.ClaudeFound, out.Accounts.CopilotLogins = a.gate.ClaudeFound(), a.gate.CopilotLogins()
	return out
}

// observeLoop keeps the account timelines current while the agent runs.
func (a *accountGate) observeLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.gate.ObserveAccounts()
		}
	}
}

// reportLoop tells the server, per Harnext account, how many Claude Code accounts are signed in here (and whether one is this
// account's own) and which GitHub accounts it can choose from for Copilot. A changed report is sent at once, an unchanged one
// every `every`; never more often than once per gap and account.
func (a *accountGate) reportLoop(ctx context.Context, check, every, gap time.Duration, logf func(string, ...any)) {
	t := time.NewTicker(check)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, rt := range a.accts {
			a.reportOne(ctx, rt, every, gap, logf)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func (a *accountGate) reportOne(ctx context.Context, rt *acctRuntime, every, gap time.Duration, logf func(string, ...any)) {
	rep, ok := a.gate.ReportFor(attribution.Account{ID: rt.acct.ID, Identity: rt.idm})
	if !ok {
		return
	}
	blob, err := json.Marshal(rep)
	if err != nil {
		return
	}
	digest := sha256.Sum256(blob)
	now := time.Now()
	rt.mu.Lock()
	changed := !rt.haveDigest || rt.lastDigest != digest
	since := now.Sub(rt.lastReport)
	never := rt.lastReport.IsZero()
	rt.mu.Unlock()
	if !never && (since < gap || (!changed && since < every)) {
		return
	}
	err = rt.idm.Report(ctx, rep)
	if ctx.Err() != nil {
		return
	}
	rt.mu.Lock()
	rt.lastReport, rt.lastReportErr = now.UTC(), ""
	if err != nil {
		rt.lastReportErr = identityCode(err)
	} else {
		rt.lastDigest, rt.haveDigest = digest, true
	}
	rt.mu.Unlock()
	if err != nil {
		logf("account report failed: %s", identityCode(err))
	}
}

func identityCode(err error) string {
	if c := identity.CodeOf(err); c != "" {
		return string(c)
	}
	return "error"
}

// PreviewGateOptions are the test seams of NewPreviewGate; the zero value is the production setting.
type PreviewGateOptions struct {
	Home  string
	Keys  remoteconfig.TrustedKeys
	Clock remoteconfig.Clock
}

// NewPreviewGate returns the gate as the agent would apply it now: the stored identity documents (verified again, never
// fetched), the saved account histories and the accounts found on this machine. It makes no request and writes nothing.
func NewPreviewGate(p Paths, db *storage.Store, o PreviewGateOptions) (GateView, error) {
	keys := o.Keys
	if keys == nil {
		var err error
		if keys, err = remoteconfig.BuildTrustedKeys(); err != nil {
			return nil, err
		}
	}
	home := o.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	savedClaude, _, err := db.LoadState(accountsStateName)
	if err != nil {
		return nil, err
	}
	savedCopilot, _, err := db.LoadState(copilotAcctState)
	if err != nil {
		return nil, err
	}
	list, err := p.LoadAccounts()
	if err != nil {
		return nil, err
	}
	rts := make([]*acctRuntime, len(list))
	for i, acct := range list {
		rts[i] = &acctRuntime{acct: acct, idm: identity.NewManager(identity.ManagerOptions{Keys: keys, Store: identity.NewStore(p.IdentityFileFor(acct.ID)), Clock: o.Clock})}
	}
	a := newAccountGate(home, func() []watcher.Root { return watcher.DiscoverClaudeRoots(home) }, savedClaude, savedCopilot, rts,
		&copilotid.Reader{Dirs: copilotid.DefaultDirs()}, len(keys) > 0, nil)
	a.gate.ObserveAccounts()
	return a, nil
}
