package service

import (
	"bytes"
	"context"
	"os"
	"sync"
	"time"

	"github.com/harnext/harnext-agent/internal/account"
	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/attribution"
	"github.com/harnext/harnext-agent/internal/gitid"
	"github.com/harnext/harnext-agent/internal/identity"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/uploader"
	"github.com/harnext/harnext-agent/internal/watcher"
)

const (
	accountsStateName  = "claude_accounts"
	accountCheckPeriod = 30 * time.Second // observation and report check
	accountReportEvery = 30 * time.Minute // refresh of an unchanged report while candidates exist
	accountReportGap   = 2 * time.Minute  // never more than one report per gap
)

// GateView is the account gate as status, stats and preview use it.
type GateView interface {
	Decide(aggregate.Row) uploader.Decision
	Summary() GateSummary
}

// accountGate is the production upload gate: the attribution gate over the signed identity document. It holds hashes and
// masked labels only.
type accountGate struct {
	gate *attribution.Gate
	idm  *identity.Manager
	keys bool

	mu            sync.Mutex
	savedTimeline []byte
	lastReport    time.Time
	lastReportErr string
}

func newAccountGate(home string, roots func() []watcher.Root, savedTimeline []byte, idm *identity.Manager, hasKeys bool, logf func(string, ...any)) *accountGate {
	tl := &account.Timeline{}
	if len(savedTimeline) > 0 && tl.Restore(savedTimeline) != nil && logf != nil {
		logf("saved account history is unreadable: starting it again")
	}
	return &accountGate{
		idm: idm, keys: hasKeys,
		gate: attribution.New(attribution.Options{Home: home, Roots: roots, Identity: idm, Git: gitid.NewResolver(home), Timeline: tl}),
	}
}

func (a *accountGate) Decide(row aggregate.Row) uploader.Decision { return a.gate.Decide(row) }

// saveTimeline writes the account history when it changed; it holds no e-mail.
func (a *accountGate) saveTimeline(db *storage.Store) error {
	blob := a.gate.TimelineSnapshot()
	a.mu.Lock()
	same := bytes.Equal(blob, a.savedTimeline)
	a.mu.Unlock()
	if same {
		return nil
	}
	if err := db.SaveState(accountsStateName, blob); err != nil {
		return err
	}
	a.mu.Lock()
	a.savedTimeline = blob
	a.mu.Unlock()
	return nil
}

// Summary is a content-free picture of the identity and of what the gate keeps back.
func (a *accountGate) Summary() GateSummary {
	ids := a.idm.Status()
	state := IdentityPending
	switch {
	case !a.keys:
		state = IdentityNoTrustedKeys
	case ids.Valid:
		state = IdentityOK
	case ids.HasDocument:
		state = IdentityExpired
	}
	out := GateSummary{Identity: IdentityStatus{State: state, ExpiresAt: ids.ExpiresAt, LastCheck: ids.LastCheck, LastError: string(ids.LastError)}}
	if ids.HasDocument {
		out.Identity.Bindings = ids.Bindings
	}
	st := a.gate.Stats()
	out.Accounts.Sendable, out.Accounts.PendingTotal = st.Bound, st.Pending
	if len(st.Skipped) > 0 {
		out.Accounts.Skipped = st.Skipped
	}
	for _, c := range a.gate.Candidates(identity.MaxAccountsPerReport) {
		out.Accounts.Pending = append(out.Accounts.Pending, PendingAccount{Tool: c.Tool, Label: c.Label, Sessions: c.Sessions30d})
	}
	a.mu.Lock()
	out.Accounts.LastReport, out.Accounts.LastReportError = a.lastReport, a.lastReportErr
	a.mu.Unlock()
	return out
}

// observeLoop keeps the account timeline current while the agent runs.
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

// reportLoop tells the server which detected accounts have no binding yet, so the panel can offer to connect them. It reports
// when the candidates changed and otherwise every `every` while candidates exist, never more often than once per gap.
func (a *accountGate) reportLoop(ctx context.Context, check, every, gap time.Duration, logf func(string, ...any)) {
	t := time.NewTicker(check)
	defer t.Stop()
	var last time.Time
	dirty := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a.gate.CandidatesChanged() {
			dirty = true
		}
		cands := a.gate.Candidates(identity.MaxAccountsPerReport)
		if len(cands) == 0 {
			dirty = false
			continue
		}
		now := time.Now()
		if !dirty && !last.IsZero() && now.Sub(last) < every {
			continue
		}
		if !last.IsZero() && now.Sub(last) < gap {
			continue
		}
		last, dirty = now, false
		err := a.idm.Report(ctx, cands)
		if ctx.Err() != nil {
			return
		}
		a.mu.Lock()
		a.lastReport, a.lastReportErr = now.UTC(), ""
		if err != nil {
			a.lastReportErr = identityCode(err)
		}
		a.mu.Unlock()
		if err != nil {
			logf("account report failed: %s", identityCode(err))
			dirty = identity.IsTransient(err)
		}
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

// NewPreviewGate returns the gate as the agent would apply it now: the stored identity document (verified again, never
// fetched), the saved account history and the accounts found on this machine. It makes no request and writes nothing.
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
	saved, _, err := db.LoadState(accountsStateName)
	if err != nil {
		return nil, err
	}
	idm := identity.NewManager(identity.ManagerOptions{Keys: keys, Store: identity.NewStore(p.IdentityFile), Clock: o.Clock})
	a := newAccountGate(home, func() []watcher.Root { return watcher.DiscoverClaudeRoots(home) }, saved, idm, len(keys) > 0, nil)
	a.gate.ObserveAccounts()
	return a, nil
}
