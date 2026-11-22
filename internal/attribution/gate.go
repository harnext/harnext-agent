// Package attribution is the upload gate: it decides per session whether it leaves the machine and as which account. Account
// e-mails are read by other packages, hashed at once and dropped; this package keeps only hashes, masked labels and digests.
package attribution

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/harnext/harnext-agent/internal/account"
	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/copilotid"
	"github.com/harnext/harnext-agent/internal/identity"
	"github.com/harnext/harnext-agent/internal/uploader"
	"github.com/harnext/harnext-agent/internal/watcher"
)

// Reasons are stable lowercase codes; they are the only content of a skip.
const (
	// ReasonNoAccount: no Claude Code account was signed in when the session started.
	ReasonNoAccount = "no_account"
	// ReasonCopilotUnknown: the GitHub account behind a Copilot session could not be determined.
	ReasonCopilotUnknown = "copilot_account_unknown"
	// ReasonIdentityPending: no Harnext account on this machine holds a valid signed identity document yet.
	ReasonIdentityPending = "identity_pending"
	// ReasonNoMatch: the session's tool account is not the account of any Harnext account on this machine (for Claude Code the
	// sign-in e-mail differs from every Harnext e-mail; for Copilot the GitHub account is not the selected one).
	ReasonNoMatch = "no_matching_account"
	// ReasonAccountUnbound: the server refused the session (set by the uploader's answer, listed here for status output).
	ReasonAccountUnbound  = "account_unbound"
	ReasonUnsupportedTool = "unsupported_tool"
)

const (
	maxTrackedSessions = 5000
	maxLabels          = 64
	maxGapsPerDir      = 8
	transientGap       = 2 * time.Minute
	rereadAfter        = 10 * time.Minute
)

// IdentitySource is satisfied by *identity.Manager.
type IdentitySource interface{ Current() identity.Snapshot }

// Account is one Harnext account (one agent token) on this machine. ID is the opaque local id the uploader routes by.
type Account struct {
	ID       string
	Identity IdentitySource
}

// CopilotSource is satisfied by *copilotid.Reader.
type CopilotSource interface {
	Read() (copilotid.State, bool)
}

type Options struct {
	Home  string
	Roots func() []watcher.Root
	// Accounts are the Harnext accounts whose identity documents decide where a session goes; none: nothing is sent.
	Accounts []Account
	Timeline *account.Timeline
	// Copilot reads the editor's state database (nil: Copilot sessions get copilot_account_unknown); CopilotTimeline holds what
	// it showed over time.
	Copilot         CopilotSource
	CopilotTimeline *copilotid.Timeline
	Now             func() time.Time // default time.Now
	// ReadClaudeEmail defaults to account.ClaudeEmail.
	ReadClaudeEmail func(configDir, home string) (string, bool)
	// Stat reports size and mtime of a Claude global config so an unchanged file is not parsed again; default os.Lstat.
	Stat func(path string) (size int64, mod time.Time, ok bool)
}

// Stats is a content-free snapshot for status output.
type Stats struct {
	// Skipped counts distinct sessions per reason by their latest verdict since the process started (each reason tracks at
	// most 5000); a session that was later sent or skipped for another reason leaves its earlier reason.
	Skipped map[string]int
	Bound   int // distinct sessions that were allowed to be sent
}

type stamp struct {
	size int64
	mod  time.Time
}

type gap struct{ from, to time.Time }

type dirState struct {
	stamp     stamp
	stamped   bool
	readAt    time.Time
	lastHash  string
	noneSince time.Time // zero while an account is present
	gaps      []gap
}

// sessionSet holds 64-bit digests of session ids with the time they were last seen; it stops growing at max.
type sessionSet struct {
	m   map[uint64]time.Time
	max int
}

func newSessionSet(max int) *sessionSet { return &sessionSet{m: map[uint64]time.Time{}, max: max} }

func (s *sessionSet) add(id uint64, now time.Time) bool {
	if _, ok := s.m[id]; ok {
		s.m[id] = now
		return false
	}
	if len(s.m) >= s.max {
		return false
	}
	s.m[id] = now
	return true
}

func (s *sessionSet) prune(cutoff time.Time) (removed int) {
	for id, at := range s.m {
		if at.Before(cutoff) {
			delete(s.m, id)
			removed++
		}
	}
	return removed
}

func sessionDigest(tool, sessionID string) uint64 {
	sum := sha256.Sum256([]byte(tool + "\x00" + sessionID))
	return binary.BigEndian.Uint64(sum[:8])
}

// Gate is safe for concurrent use.
type Gate struct {
	opts Options
	tl   *account.Timeline
	cop  *copilotid.Timeline

	mu      sync.Mutex
	dirs    map[string]*dirState
	labels  map[string]string // account hash -> masked label of Claude accounts
	skipped map[string]*sessionSet
	bound   *sessionSet
}

func New(opts Options) *Gate {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ReadClaudeEmail == nil {
		opts.ReadClaudeEmail = account.ClaudeEmail
	}
	if opts.Stat == nil {
		opts.Stat = lstat
	}
	tl := opts.Timeline
	if tl == nil {
		tl = &account.Timeline{}
	}
	cop := opts.CopilotTimeline
	if cop == nil {
		cop = &copilotid.Timeline{}
	}
	return &Gate{
		opts: opts, tl: tl, cop: cop,
		dirs: map[string]*dirState{}, labels: map[string]string{},
		skipped: map[string]*sessionSet{}, bound: newSessionSet(maxTrackedSessions),
	}
}

func lstat(path string) (int64, time.Time, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, time.Time{}, false
	}
	return fi.Size(), fi.ModTime(), true
}

// configPath follows the rule of account.ClaudeEmail.
func configPath(dir, home string) string {
	dir, home = filepath.Clean(dir), filepath.Clean(home)
	if dir == filepath.Join(home, ".claude") {
		return filepath.Join(home, ".claude.json")
	}
	return filepath.Join(dir, ".claude.json")
}

// ObserveAccounts reads the account of every Claude root and feeds the timeline. Call it at start and periodically.
func (g *Gate) ObserveAccounts() {
	if g.opts.Copilot != nil {
		if st, ok := g.opts.Copilot.Read(); ok {
			g.cop.Observe(st, g.opts.Now())
		}
	}
	if g.opts.Roots == nil {
		return
	}
	for _, r := range g.opts.Roots() {
		g.observe(r.Dir)
	}
}

func (g *Gate) observe(dir string) {
	id := account.DirID(dir)
	now := g.opts.Now()
	size, mod, stamped := g.opts.Stat(configPath(dir, g.opts.Home))
	cur := stamp{size, mod}

	g.mu.Lock()
	if st := g.dirs[id]; st != nil && stamped && st.stamped && st.stamp == cur && now.Sub(st.readAt) < rereadAfter {
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()

	hash, label, ok := g.readAccount(dir)

	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.dirs[id]
	if st == nil {
		st = &dirState{}
		g.dirs[id] = st
	}
	st.stamp, st.stamped, st.readAt = cur, stamped, now
	if !ok {
		if st.noneSince.IsZero() {
			st.noneSince = now
		}
		return
	}
	if !st.noneSince.IsZero() {
		// A short outage that ends with the same account is a read glitch, not a logout.
		if st.lastHash != hash || now.Sub(st.noneSince) >= transientGap {
			if len(st.gaps) >= maxGapsPerDir {
				st.gaps = st.gaps[1:]
			}
			st.gaps = append(st.gaps, gap{st.noneSince, now})
		}
		st.noneSince = time.Time{}
	}
	st.lastHash = hash
	if label != "" {
		if len(g.labels) >= maxLabels {
			g.labels = map[string]string{}
		}
		g.labels[hash] = label
	}
	g.tl.Observe(id, hash, now)
}

// readAccount returns only the hash and the masked label; the e-mail does not leave this function.
func (g *Gate) readAccount(dir string) (hash, label string, ok bool) {
	email, ok := g.opts.ReadClaudeEmail(dir, g.opts.Home)
	if !ok || email == "" {
		return "", "", false
	}
	return account.Hash(email), account.MaskLabel(email), true
}

// Decide answers for one session (call it with the session's first row). The session goes out under the first Harnext account
// of this machine whose identity document holds the session's tool account; the token of that account is used for the upload.
func (g *Gate) Decide(row aggregate.Row) uploader.Decision {
	now := g.opts.Now()
	var hash, reason string
	switch row.Tool {
	case aggregate.ToolClaudeCode:
		hash, reason = g.claudeAccount(row)
	case aggregate.ToolVSCodeCopilot:
		hash, reason = g.copilotAccount(row)
	default:
		reason = ReasonUnsupportedTool
	}
	if reason != "" {
		return g.skip(row, reason, now)
	}
	valid := false
	for _, a := range g.opts.Accounts {
		snap := a.Identity.Current()
		if !snap.Valid() {
			continue
		}
		valid = true
		if snap.Bound(row.Tool, hash) {
			g.markBound(row, now)
			return uploader.Decision{Send: true, AccountHash: hash, RouteID: a.ID}
		}
	}
	if !valid {
		return g.skip(row, ReasonIdentityPending, now)
	}
	return g.skip(row, ReasonNoMatch, now)
}

func (g *Gate) claudeAccount(row aggregate.Row) (hash, reason string) {
	at := row.FirstAt
	if at.IsZero() {
		at = row.StartedAt
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.dirs[row.Source]; st != nil {
		if !st.noneSince.IsZero() && !at.Before(st.noneSince) {
			return "", ReasonNoAccount
		}
		for _, gp := range st.gaps {
			if !at.Before(gp.from) && at.Before(gp.to) {
				return "", ReasonNoAccount
			}
		}
	}
	h, ok := g.tl.HashAt(row.Source, at)
	if !ok {
		return "", ReasonNoAccount
	}
	return h, ""
}

// copilotAccount is the hash of the GitHub account that was active in VS Code when the session started. A session is not
// attributed when that account cannot be determined (fail closed).
func (g *Gate) copilotAccount(row aggregate.Row) (hash, reason string) {
	at := row.FirstAt
	if at.IsZero() {
		at = row.StartedAt
	}
	h, _, ok := g.cop.HashAt(at)
	if !ok {
		return "", ReasonCopilotUnknown
	}
	return h, ""
}

func (g *Gate) skip(row aggregate.Row, reason string, now time.Time) uploader.Decision {
	g.mu.Lock()
	d := sessionDigest(row.Tool, row.SessionID)
	g.forgetLocked(d, reason)
	delete(g.bound.m, d)
	s := g.skipped[reason]
	if s == nil {
		s = newSessionSet(maxTrackedSessions)
		g.skipped[reason] = s
	}
	s.add(d, now)
	g.mu.Unlock()
	return uploader.Decision{Reason: reason}
}

// forgetLocked removes a session from every skip set except keep, so Stats counts it under its latest verdict only.
func (g *Gate) forgetLocked(digest uint64, keep string) {
	for r, s := range g.skipped {
		if r != keep {
			delete(s.m, digest)
		}
	}
}

func (g *Gate) markBound(row aggregate.Row, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.forgetLocked(sessionDigest(row.Tool, row.SessionID), "")
	g.bound.add(sessionDigest(row.Tool, row.SessionID), now)
}

// Stats returns what the gate decided so far.
func (g *Gate) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := Stats{Skipped: make(map[string]int, len(g.skipped)), Bound: len(g.bound.m)}
	for reason, s := range g.skipped {
		if len(s.m) > 0 {
			st.Skipped[reason] = len(s.m)
		}
	}
	return st
}

// ClaudeFound is the number of distinct Claude Code accounts currently signed in on this machine (at most 20).
func (g *Gate) ClaudeFound() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.currentHashesLocked())
}

func (g *Gate) currentHashesLocked() map[string]string {
	out := map[string]string{}
	for _, st := range g.dirs {
		if st.noneSince.IsZero() && st.lastHash != "" {
			out[st.lastHash] = g.labels[st.lastHash]
		}
	}
	return out
}

// ReportFor builds the account report for one Harnext account from its identity document: how many Claude Code accounts are
// signed in here and whether one is this Harnext account's own; the GitHub accounts found (all of them while no Copilot account
// is selected, only the selected one afterwards). ok is false while the account holds no valid document, because "matched"
// cannot be known without its hash.
func (g *Gate) ReportFor(a Account) (rep identity.Report, ok bool) {
	snap := a.Identity.Current()
	if !snap.Valid() {
		return identity.Report{}, false
	}
	g.mu.Lock()
	cur := g.currentHashesLocked()
	g.mu.Unlock()
	rep.Claude.Found = min(len(cur), identity.MaxClaudeAccounts)
	if h, has := snap.HashFor(identity.ToolClaudeCode); has {
		if label, in := cur[h]; in && label != "" {
			rep.Claude.Matched, rep.Claude.Label = true, label
		}
	}
	rep.Copilot.Logins = []identity.CopilotLogin{}
	selected, hasSel := snap.HashFor(identity.ToolVSCodeCopilot)
	for _, l := range g.cop.Logins() {
		if hasSel && l.IDHash != selected {
			continue
		}
		if len(rep.Copilot.Logins) >= identity.MaxCopilotLogins {
			break
		}
		rep.Copilot.Logins = append(rep.Copilot.Logins, identity.CopilotLogin{Login: l.Login, IDHash: l.IDHash, Plan: l.Plan})
	}
	return rep, true
}

// CopilotLogins is the number of GitHub accounts found on this machine.
func (g *Gate) CopilotLogins() int { return len(g.cop.Logins()) }

// TimelineSnapshot is the JSON the service stores between runs.
func (g *Gate) TimelineSnapshot() []byte { return g.tl.Snapshot() }

func (g *Gate) RestoreTimeline(data []byte) error { return g.tl.Restore(data) }

// CopilotSnapshot is the JSON of the GitHub account history the service stores between runs; it holds user names, hashes
// and plans only.
func (g *Gate) CopilotSnapshot() []byte { return g.cop.Snapshot() }
