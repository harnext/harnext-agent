// Package attribution is the upload gate: it decides per session whether it leaves the machine and as which account. Account
// e-mails are read by other packages, hashed at once and dropped; this package keeps only hashes, masked labels and digests.
package attribution

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/harnext/harnext-agent/internal/account"
	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/identity"
	"github.com/harnext/harnext-agent/internal/uploader"
	"github.com/harnext/harnext-agent/internal/watcher"
)

// Reasons are stable lowercase codes; they are the only content of a skip.
const (
	ReasonNoAccount       = "no_account"
	ReasonGitEmailMissing = "git_email_missing"
	ReasonIdentityPending = "identity_pending"
	ReasonAccountUnbound  = "account_unbound"
	ReasonUnsupportedTool = "unsupported_tool"
)

const (
	cacheTTL             = 30 * time.Second
	candidateWindow      = 30 * 24 * time.Hour
	maxCandidates        = 64
	maxCandidateSessions = 2000
	maxTrackedSessions   = 5000
	maxCwdCache          = 1024
	maxLabels            = 64
	maxGapsPerDir        = 8
	transientGap         = 2 * time.Minute
	rereadAfter          = 10 * time.Minute
	prunePeriod          = time.Minute
	defaultLimit         = identity.MaxAccountsPerReport
)

// IdentitySource is satisfied by *identity.Manager.
type IdentitySource interface{ Current() identity.Snapshot }

// EmailResolver is satisfied by *gitid.Resolver.
type EmailResolver interface {
	Email(dir string) (email string, ok bool)
}

type Options struct {
	Home     string
	Roots    func() []watcher.Root
	Identity IdentitySource // nil: nothing is sent (identity_pending)
	Git      EmailResolver  // nil: Copilot rows get git_email_missing
	Timeline *account.Timeline
	Now      func() time.Time // default time.Now
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
	Bound   int      // distinct sessions that were allowed to be sent
	Pending int      // candidate accounts without a binding
	Labels  []string // masked labels of the pending candidates, most sessions first (at most 20)
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

type cwdEntry struct {
	hash, label string
	ok          bool
	at          time.Time
}

type candKey struct{ tool, hash string }

type candidate struct {
	label    string
	seen     time.Time
	sessions *sessionSet
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

	mu        sync.Mutex
	dirs      map[string]*dirState
	labels    map[string]string // account hash -> masked label of Claude accounts
	cwds      map[string]cwdEntry
	cands     map[candKey]*candidate
	changed   bool
	lastPrune time.Time
	skipped   map[string]*sessionSet
	bound     *sessionSet
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
	return &Gate{
		opts: opts, tl: tl,
		dirs: map[string]*dirState{}, labels: map[string]string{}, cwds: map[string]cwdEntry{},
		cands: map[candKey]*candidate{}, skipped: map[string]*sessionSet{}, bound: newSessionSet(maxTrackedSessions),
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

// Decide answers for one session (call it with the session's first row).
func (g *Gate) Decide(row aggregate.Row) uploader.Decision {
	now := g.opts.Now()
	var hash, label, reason string
	switch row.Tool {
	case aggregate.ToolClaudeCode:
		hash, label, reason = g.claudeAccount(row)
	case aggregate.ToolVSCodeCopilot:
		hash, label, reason = g.copilotAccount(row.Cwd, now)
	default:
		reason = ReasonUnsupportedTool
	}
	if reason != "" {
		return g.skip(row, reason, now)
	}
	snap := g.snapshot()
	if !snap.Valid() {
		return g.skip(row, ReasonIdentityPending, now)
	}
	if route, ok := snap.RouteFor(row.Tool, hash); ok {
		g.markBound(row, hash, now)
		return uploader.Decision{Send: true, AccountHash: hash, RouteID: route}
	}
	g.recordCandidate(row, hash, label, now)
	return g.skip(row, ReasonAccountUnbound, now)
}

func (g *Gate) snapshot() identity.Snapshot {
	if g.opts.Identity == nil {
		return identity.Snapshot{}
	}
	return g.opts.Identity.Current()
}

func (g *Gate) claudeAccount(row aggregate.Row) (hash, label, reason string) {
	at := row.FirstAt
	if at.IsZero() {
		at = row.StartedAt
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.dirs[row.Source]; st != nil {
		if !st.noneSince.IsZero() && !at.Before(st.noneSince) {
			return "", "", ReasonNoAccount
		}
		for _, gp := range st.gaps {
			if !at.Before(gp.from) && at.Before(gp.to) {
				return "", "", ReasonNoAccount
			}
		}
	}
	h, ok := g.tl.HashAt(row.Source, at)
	if !ok {
		return "", "", ReasonNoAccount
	}
	return h, g.labels[h], ""
}

func (g *Gate) copilotAccount(cwd string, now time.Time) (hash, label, reason string) {
	g.mu.Lock()
	e, hit := g.cwds[cwd]
	g.mu.Unlock()
	if !hit || now.Sub(e.at) < 0 || now.Sub(e.at) >= cacheTTL {
		e = cwdEntry{at: now}
		if g.opts.Git != nil {
			if email, ok := g.opts.Git.Email(cwd); ok && email != "" {
				e.hash, e.label, e.ok = account.Hash(email), account.MaskLabel(email), true
			}
		}
		g.mu.Lock()
		if len(g.cwds) >= maxCwdCache {
			g.cwds = map[string]cwdEntry{}
		}
		g.cwds[cwd] = e
		g.mu.Unlock()
	}
	if !e.ok {
		return "", "", ReasonGitEmailMissing
	}
	return e.hash, e.label, ""
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

func (g *Gate) markBound(row aggregate.Row, hash string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.forgetLocked(sessionDigest(row.Tool, row.SessionID), "")
	g.bound.add(sessionDigest(row.Tool, row.SessionID), now)
	k := candKey{row.Tool, hash}
	if _, ok := g.cands[k]; ok {
		delete(g.cands, k)
		g.changed = true
	}
}

func (g *Gate) recordCandidate(row aggregate.Row, hash, label string, now time.Time) {
	if label == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Sub(g.lastPrune) >= prunePeriod {
		g.pruneLocked(now)
	}
	k := candKey{row.Tool, hash}
	c := g.cands[k]
	if c == nil {
		if identity.ValidateAccounts([]identity.Account{{Tool: row.Tool, Hash: hash, Label: label}}) != nil {
			return
		}
		if len(g.cands) >= maxCandidates {
			g.evictLocked()
		}
		c = &candidate{label: label, sessions: newSessionSet(maxCandidateSessions)}
		g.cands[k] = c
		g.changed = true
	}
	c.seen = now
	if c.sessions.add(sessionDigest(row.Tool, row.SessionID), now) {
		g.changed = true
	}
}

// evictLocked drops the candidate with the fewest sessions, the least recently seen first.
func (g *Gate) evictLocked() {
	var worst candKey
	var wc *candidate
	for k, c := range g.cands {
		if wc == nil || len(c.sessions.m) < len(wc.sessions.m) ||
			(len(c.sessions.m) == len(wc.sessions.m) && c.seen.Before(wc.seen)) {
			worst, wc = k, c
		}
	}
	if wc != nil {
		delete(g.cands, worst)
		g.changed = true
	}
}

func (g *Gate) pruneLocked(now time.Time) {
	g.lastPrune = now
	cutoff := now.Add(-candidateWindow)
	for k, c := range g.cands {
		removed := c.sessions.prune(cutoff)
		if len(c.sessions.m) == 0 {
			delete(g.cands, k)
			g.changed = true
		} else if removed > 0 {
			g.changed = true
		}
	}
}

// pendingLocked prunes, forgets candidates that became bound and returns the rest, most sessions first.
func (g *Gate) pendingLocked(now time.Time, snap identity.Snapshot) []identity.Account {
	g.pruneLocked(now)
	out := make([]identity.Account, 0, len(g.cands))
	for k, c := range g.cands {
		if _, bound := snap.RouteFor(k.tool, k.hash); bound {
			delete(g.cands, k)
			g.changed = true
			continue
		}
		out = append(out, identity.Account{Tool: k.tool, Hash: k.hash, Label: c.label, Sessions30d: min(len(c.sessions.m), identity.MaxSessions30d)})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Sessions30d != b.Sessions30d:
			return a.Sessions30d > b.Sessions30d
		case a.Label != b.Label:
			return a.Label < b.Label
		case a.Tool != b.Tool:
			return a.Tool < b.Tool
		}
		return a.Hash < b.Hash
	})
	return out
}

// Candidates returns up to limit (default and maximum 20) unbound accounts, most sessions first.
func (g *Gate) Candidates(limit int) []identity.Account {
	if limit <= 0 || limit > defaultLimit {
		limit = defaultLimit
	}
	snap := g.snapshot()
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.pendingLocked(g.opts.Now(), snap)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// CandidatesChanged is true once after the candidate set or a session count changed.
func (g *Gate) CandidatesChanged() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.changed
	g.changed = false
	return c
}

func (g *Gate) Stats() Stats {
	snap := g.snapshot()
	g.mu.Lock()
	defer g.mu.Unlock()
	st := Stats{Skipped: make(map[string]int, len(g.skipped)), Bound: len(g.bound.m)}
	for reason, s := range g.skipped {
		if len(s.m) > 0 {
			st.Skipped[reason] = len(s.m)
		}
	}
	pending := g.pendingLocked(g.opts.Now(), snap)
	st.Pending = len(pending)
	for i, a := range pending {
		if i == defaultLimit {
			break
		}
		st.Labels = append(st.Labels, a.Label)
	}
	return st
}

// TimelineSnapshot is the JSON the service stores between runs.
func (g *Gate) TimelineSnapshot() []byte { return g.tl.Snapshot() }

func (g *Gate) RestoreTimeline(data []byte) error { return g.tl.Restore(data) }
