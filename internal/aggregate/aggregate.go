// Package aggregate folds Claude Code events into per-(session, usage key) totals, where a usage key
// is model × UTC day × speed × prompt floor (see UsageKey).
//
// Replaying the same lines is safe: responses are deduplicated by (message id, request id),
// tool_use blocks by their id, and user lines (messages and tool results) by uuid, so offset
// and aggregator state need not be committed atomically.
//
// With an analyzer.Extractor the aggregator also accumulates per-session SessionStats and
// TopicSignals (counters and hashes only, never text) and derives the friction score,
// First-Pass Success and topic per row. Without one these stay nil / empty.
package aggregate

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/claudecode"
)

const (
	ToolClaudeCode = "claude_code"

	// 3: requests are split by folder (SessionState.Folders / Open; older states are discarded and transcripts re-read).
	// 2: per-model totals are keyed by UsageKey.
	stateVersion = 3
	// maxCounterKinds bounds the Ignored/Skipped maps; extra kinds share OtherKind.
	maxCounterKinds = 64
	OtherKind       = "_other"
)

// Totals are token sums; 5m cache writes are CacheCreationTokens - CacheCreation1hTokens.
type Totals struct {
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	CacheReadTokens       int64 `json:"cache_read_tokens"`
	CacheCreationTokens   int64 `json:"cache_creation_tokens"`
	CacheCreation1hTokens int64 `json:"cache_creation_1h_tokens"`
}

// UsageKey identifies a usage row inside a session. Day is the UTC day of the response ("" when
// unknown; Rows falls back to the session's start day). PromptFloor is analyzer.PromptFloor of the
// request's whole prompt (-1 when no bands are configured).
type UsageKey struct {
	Model       string
	Day         string
	Speed       string
	PromptFloor int64
}

const usageKeySep = "\x1f"

// String encodes the key for the persisted map; model names never contain the separator.
func (k UsageKey) String() string {
	return k.Model + usageKeySep + k.Day + usageKeySep + k.Speed + usageKeySep + strconv.FormatInt(k.PromptFloor, 10)
}

// ParseUsageKey decodes UsageKey.String; ok is false for anything else.
func ParseUsageKey(s string) (UsageKey, bool) {
	parts := strings.Split(s, usageKeySep)
	if len(parts) != 4 || parts[0] == "" {
		return UsageKey{}, false
	}
	floor, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || floor < -1 {
		return UsageKey{}, false
	}
	if parts[1] != "" {
		if _, err := time.Parse(time.DateOnly, parts[1]); err != nil {
			return UsageKey{}, false
		}
	}
	switch parts[2] {
	case claudecode.SpeedStandard, claudecode.SpeedFast, claudecode.SpeedUnknown:
	default:
		return UsageKey{}, false
	}
	return UsageKey{Model: parts[0], Day: parts[1], Speed: parts[2], PromptFloor: floor}, true
}

// dayOf is the UTC day of t, "" for the zero time.
func dayOf(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
}

func contextBands(ex analyzer.Extractor) []int64 {
	if ex == nil {
		return nil
	}
	return ex.Settings().ContextBands
}

// Row is one (session, tool, folder, usage key) aggregate. Session fields repeat on every row of the
// session; the uploader groups rows into one ingest session with a usage list and a folder list.
type Row struct {
	SessionID string
	Tool      string
	Model     string
	// Day is the UTC day (YYYY-MM-DD); Speed and PromptFloor complete the usage key.
	Day         string
	Speed       string
	PromptFloor int64
	Totals
	WebSearchRequests int64
	WebFetchRequests  int64
	StartedAt         time.Time
	EndedAt           time.Time
	Cwd               string
	GitBranch         string
	ToolVersion       string

	// Folder is the folder of the project this usage row belongs to ("" = the project root). Parts lists ALL folders of the
	// session (sorted by path, repeated on every row) with their request counts and counters; they add up to the
	// session's turn count and Quality. MainFolder is the folder with the most requests: the session counts there.
	Folder     string
	Parts      []FolderPart
	MainFolder string

	// Session-level quality counters, repeated on every model row of the session (the server
	// stores them once per session). Always set; AnalyzedExchanges is 0 without a configuration.
	Quality         analyzer.Quality
	SignalCoverage  int      // analyzer.Coverage* bits this source can measure
	DetectorVersion int      // 0 when no configuration was loaded
	CopilotCredits  *float64 // nil for Claude Code rows
	// TokensEstimated marks Copilot history rows whose token totals are estimated (EstimateRequestTokens), not measured.
	TokensEstimated bool
	Topic           string // "" when no analyzer is set; the server contract needs a label, map "" to "other" at upload
}

// Counters describe what was counted, deduplicated or dropped. They hold counts only.
type Counters struct {
	UniqueResponses    int64 `json:"unique_responses"`
	DuplicateResponses int64 `json:"duplicate_responses"`
	SidechainResponses int64 `json:"sidechain_responses"`
	SyntheticResponses int64 `json:"synthetic_responses"`
	MissingIDs         int64 `json:"missing_ids"`
	MissingModel       int64 `json:"missing_model"`

	UserTurns          int64 `json:"user_turns"`
	ToolResults        int64 `json:"tool_results"`
	SidechainUserTurns int64 `json:"sidechain_user_turns"`
	DuplicateUserTurns int64 `json:"duplicate_user_turns"`

	// AnalyzedUserTurns and UnanalyzedUserTurns split real user messages by whether a
	// configuration was available when the line was read. Unanalyzed lines cannot be
	// analyzed later because their text was discarded.
	AnalyzedUserTurns    int64 `json:"analyzed_user_turns"`
	UnanalyzedUserTurns  int64 `json:"unanalyzed_user_turns"`
	DuplicateToolResults int64 `json:"duplicate_tool_results"`
	ToolUses             int64 `json:"tool_uses"`

	Skipped map[string]int64 `json:"skipped"`
	Ignored map[string]int64 `json:"ignored"`
}

// ModelState is the persisted per-usage-key part of a Copilot session.
type ModelState struct {
	Totals
	TurnCount         int64 `json:"turn_count"`
	WebSearchRequests int64 `json:"web_search_requests,omitempty"`
	WebFetchRequests  int64 `json:"web_fetch_requests,omitempty"`
}

// Analysis is the persisted per-session analysis state. It holds counters and hashes only;
// a reflection test fails if a string or byte slice is ever added.
type Analysis struct {
	Stats analyzer.SessionStats `json:"stats"`
	Topic analyzer.TopicSignals `json:"topic"`
}

// SessionState is the persisted per-session part.
type SessionState struct {
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	MetaAt      time.Time `json:"meta_at"`
	Cwd         string    `json:"cwd"`
	GitBranch   string    `json:"git_branch"`
	ToolVersion string    `json:"tool_version"`
	// Folders holds the closed requests by folder inside the project (CollapsePath; "" = the project root); Open is
	// the request in progress. Usage is keyed by UsageKey.String. Σ Folders + Open is the whole session.
	Folders map[string]*FolderState `json:"folders,omitempty"`
	Open    Request                 `json:"open"`
	// Analysis is absent in snapshots written before phase 2b; it is created on demand.
	Analysis *Analysis `json:"analysis,omitempty"`
}

// State is the serializable aggregator state. Seen holds 64-bit hashes of response and
// user-turn keys (8 bytes each instead of the raw identifiers), sorted for stable output.
type State struct {
	Version  int                      `json:"version"`
	Sessions map[string]*SessionState `json:"sessions"`
	Seen     []uint64                 `json:"seen"`
	Counters Counters                 `json:"counters"`
}

// Aggregator is safe for concurrent use.
type Aggregator struct {
	mu       sync.Mutex
	sessions map[string]*SessionState
	seen     map[uint64]struct{}
	counters Counters
	ex       analyzer.Extractor
	resolve  ResolveFunc
}

func New() *Aggregator {
	return &Aggregator{
		sessions: map[string]*SessionState{},
		seen:     map[uint64]struct{}{},
		counters: newCounters(),
		resolve:  defaultResolver.Resolve,
	}
}

// SetResolver replaces the project resolver used to place folders (tests; nil restores the default).
func (a *Aggregator) SetResolver(r ResolveFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r == nil {
		r = defaultResolver.Resolve
	}
	a.resolve = r
}

func newCounters() Counters {
	return Counters{Skipped: map[string]int64{}, Ignored: map[string]int64{}}
}

// SetAnalyzer sets the configuration used for friction thresholds and for deriving scores
// in Rows. nil (no or expired configuration) makes Friction, FirstPassSuccess and Topic empty;
// accumulated state is kept. Use the same Extractor as the parser.
func (a *Aggregator) SetAnalyzer(ex analyzer.Extractor) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ex = ex
}

func (a *Aggregator) params() analyzer.Params {
	if a.ex == nil {
		return analyzer.Params{}
	}
	return a.ex.Settings().Params
}

func (s *SessionState) analysis() *Analysis {
	if s.Analysis == nil {
		s.Analysis = &Analysis{}
	}
	return s.Analysis
}

// Add folds one parser event into the totals.
func (a *Aggregator) Add(ev claudecode.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch e := ev.(type) {
	case claudecode.AssistantUsage:
		a.addUsage(e)
	case claudecode.UserTurn:
		a.addUser(e)
	case claudecode.Ignored:
		bump(a.counters.Ignored, e.Type)
	case claudecode.Skipped:
		bump(a.counters.Skipped, string(e.Reason))
	}
}

func (a *Aggregator) addUsage(e claudecode.AssistantUsage) {
	// Synthetic placeholders (e.g. "<synthetic>") are not real API calls.
	if strings.HasPrefix(e.Model, "<") && strings.HasSuffix(e.Model, ">") {
		a.counters.SyntheticResponses++
		return
	}
	if e.Model == "" {
		a.counters.MissingModel++
		return
	}
	// Without both ids duplicates cannot be told apart; counting could inflate, so do not count.
	if e.MessageID == "" || e.RequestID == "" {
		a.counters.MissingIDs++
		return
	}

	s := a.session(e.SessionID)
	s.touch(e.Timestamp, e.Cwd, e.GitBranch, e.ToolVersion)
	// Every content block is its own line with the same response key, so tool_use blocks
	// are deduplicated by their own id before the response itself is.
	a.addToolUses(s, e)

	if !a.markSeen(hashKey('r', e.MessageID, e.RequestID)) {
		a.counters.DuplicateResponses++
		return
	}
	a.counters.UniqueResponses++
	if e.IsSidechain {
		a.counters.SidechainResponses++
	} else {
		st := &s.analysis().Stats
		s.Open.Q.Add(track(st, st.RecordResponse))
	}

	speed := e.Usage.Speed
	if speed == "" {
		speed = claudecode.SpeedUnknown
	}
	key := UsageKey{
		Model: e.Model, Day: dayOf(e.Timestamp), Speed: speed,
		PromptFloor: analyzer.PromptFloor(contextBands(a.ex), e.Usage.PromptTokens()),
	}.String()
	m := s.Open.usage(key)
	m.InputTokens += e.Usage.InputTokens
	m.OutputTokens += e.Usage.OutputTokens
	m.CacheReadTokens += e.Usage.CacheReadTokens
	m.CacheCreationTokens += e.Usage.CacheCreationTokens
	m.CacheCreation1hTokens += e.Usage.CacheCreation1hTokens
	m.WebSearchRequests += e.Usage.WebSearchRequests
	m.WebFetchRequests += e.Usage.WebFetchRequests
}

func (a *Aggregator) addToolUses(s *SessionState, e claudecode.AssistantUsage) {
	for i := 0; i < e.ToolCount && i < len(e.Tools); i++ {
		t := e.Tools[i]
		if t.ID == "" || !a.markSeen(hashKey('t', t.ID, "")) {
			continue
		}
		a.counters.ToolUses++
		if !e.IsSidechain && IsEditTool(t.Name) {
			s.analysis().Stats.RecordEdit()
		}
		// The folder of the file counts for the open request (sub-agent work too: it serves the same request).
		switch {
		case IsEditTool(t.Name):
			s.Open.Dirs = addDir(s.Open.Dirs, t.Dir, true)
		case IsReadTool(t.Name):
			s.Open.Dirs = addDir(s.Open.Dirs, t.Dir, false)
		}
		topic := &s.analysis().Topic
		topic.Tool.Add(analyzer.SignalKey(t.Name))
		if t.Ext != "" {
			topic.Ext.Add(analyzer.SignalKey(t.Ext))
		}
		if t.CompoundExt != "" {
			topic.Ext.Add(analyzer.SignalKey(t.CompoundExt))
		}
	}
}

func (a *Aggregator) addUser(e claudecode.UserTurn) {
	s := a.session(e.SessionID)
	s.touch(e.Timestamp, e.Cwd, e.GitBranch, "")

	switch {
	case e.IsToolResult:
		if e.UUID != "" && !a.markSeen(hashKey('u', e.UUID, "")) {
			a.counters.DuplicateToolResults++
			return
		}
		a.counters.ToolResults++
		if !e.IsSidechain {
			st := &s.analysis().Stats
			p := a.params()
			s.Open.Q.Add(track(st, func() {
				for i := 0; i < e.ResultCount; i++ {
					r := e.Results[i]
					st.RecordToolResult(analyzer.ToolResult{
						IsError: r.IsError, Fingerprint: r.ErrorHash, Denied: r.Denied,
						Interrupted: e.Interrupted && i == 0, // the line-level flag applies once
					}, p)
				}
				if e.ResultCount == 0 && e.Interrupted {
					st.RecordInterrupt()
				}
			}))
		}
	case e.IsSidechain:
		// Sub-agent prompts are written by the parent agent, not a person (unverified).
		a.counters.SidechainUserTurns++
	case e.UUID != "" && !a.markSeen(hashKey('u', e.UUID, "")):
		a.counters.DuplicateUserTurns++
	default:
		a.counters.UserTurns++
		an := s.analysis()
		before := an.Stats.Q
		if e.Analyzed {
			a.counters.AnalyzedUserTurns++
			f := e.Features
			an.Stats.StartExchange(&f, a.params())
			an.Topic.AddKeywords(e.Features.KeywordScores)
		} else {
			a.counters.UnanalyzedUserTurns++
			an.Stats.StartExchange(nil, a.params())
		}
		d := an.Stats.Q.Sub(before)
		if an.Stats.Q.Exchanges == before.Exchanges {
			// An interrupt marker is a message but not a new request: it belongs to the open one.
			s.Open.Q.Add(d)
			s.Open.Turns++
			return
		}
		// The new message closes the open request (it writes pushback and friction to it) and opens the next one.
		closing, opening := analyzer.SplitStart(d)
		s.Open.Q.Add(closing)
		a.closeRequest(s)
		s.Open.Q.Add(opening)
		s.Open.Turns = 1
	}
}

// closeRequest folds the open request into the folder it belongs to and starts an empty one.
func (a *Aggregator) closeRequest(s *SessionState) {
	if s.Open.empty() {
		s.Open = Request{}
		return
	}
	name := folderTarget(s.Folders, RequestFolder(a.resolve, s.Cwd, s.Open.Dirs))
	if s.Folders == nil {
		s.Folders = map[string]*FolderState{}
	}
	f := s.Folders[name]
	if f == nil {
		f = &FolderState{}
		s.Folders[name] = f
	}
	f.merge(s.Open)
	s.Open = Request{}
}

func (a *Aggregator) session(id string) *SessionState {
	s := a.sessions[id]
	if s == nil {
		s = &SessionState{}
		a.sessions[id] = s
	}
	return s
}

// markSeen reports whether the key was new.
func (a *Aggregator) markSeen(k uint64) bool {
	if _, ok := a.seen[k]; ok {
		return false
	}
	a.seen[k] = struct{}{}
	return true
}

func (s *SessionState) touch(ts time.Time, cwd, branch, version string) {
	if !ts.IsZero() {
		if s.StartedAt.IsZero() || ts.Before(s.StartedAt) {
			s.StartedAt = ts
		}
		if ts.After(s.EndedAt) {
			s.EndedAt = ts
		}
		if !ts.Before(s.MetaAt) {
			s.MetaAt = ts
			s.Cwd, s.GitBranch, s.ToolVersion = keep(cwd, s.Cwd), keep(branch, s.GitBranch), keep(version, s.ToolVersion)
		}
		return
	}
	if s.MetaAt.IsZero() {
		s.Cwd, s.GitBranch, s.ToolVersion = keep(s.Cwd, cwd), keep(s.GitBranch, branch), keep(s.ToolVersion, version)
	}
}

// keep prefers next when non-empty.
func keep(next, current string) string {
	if next != "" {
		return next
	}
	return current
}

func bump(m map[string]int64, kind string) {
	if _, ok := m[kind]; !ok && len(m) >= maxCounterKinds {
		kind = OtherKind
	}
	m[kind]++
}

// hashKey is FNV-1a over domain, a and b. A 64-bit collision (~n²/2^65, about 3e-8 at a
// million keys) would drop one response; accepted for the 8-byte footprint.
func hashKey(domain byte, a, b string) uint64 {
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	mix := func(c byte) { h = (h ^ uint64(c)) * prime }
	mix(domain)
	for i := 0; i < len(a); i++ {
		mix(a[i])
	}
	mix(0)
	for i := 0; i < len(b); i++ {
		mix(b[i])
	}
	return h
}

// Rows returns one row per (session, folder, usage key), sorted by session, folder and usage key.
// Sessions without a counted response produce no row.
func (a *Aggregator) Rows() []Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	var rows []Row
	for id, s := range a.sessions {
		quality, detector, topic := a.derive(s)
		folders := a.foldersView(s)
		parts := partsOf(folders)
		hasUsage := make(map[string]bool, len(folders))
		for name, f := range folders {
			hasUsage[name] = len(f.Usage) > 0
		}
		main := mainFolderOf(parts, hasUsage)
		for name, f := range folders {
			for key, m := range f.Usage {
				k, ok := ParseUsageKey(key)
				if !ok {
					continue
				}
				row := Row{
					SessionID: id, Tool: ToolClaudeCode, Model: k.Model,
					Day: k.Day, Speed: k.Speed, PromptFloor: k.PromptFloor,
					Totals: m.Totals, WebSearchRequests: m.WebSearchRequests, WebFetchRequests: m.WebFetchRequests,
					StartedAt: s.StartedAt, EndedAt: s.EndedAt,
					Cwd: s.Cwd, GitBranch: s.GitBranch, ToolVersion: s.ToolVersion,
					Folder: name, Parts: parts, MainFolder: main,
					Topic: topic, Quality: quality, DetectorVersion: detector, SignalCoverage: ClaudeCodeCoverage,
				}
				if row.Day == "" {
					row.Day = dayOf(s.StartedAt)
				}
				rows = append(rows, row)
			}
		}
	}
	return finishRows(rows)
}

// foldersView returns the session's folders with the open request folded in virtually (its tool loop and
// interrupt friction included, as in SessionStats.Snapshot). The persisted folders are not changed.
func (a *Aggregator) foldersView(s *SessionState) map[string]*FolderState {
	open := s.Open
	if s.Analysis != nil {
		open.Q.Add(s.Analysis.Stats.VirtualFold())
	}
	if open.empty() {
		return s.Folders
	}
	name := folderTarget(s.Folders, RequestFolder(a.resolve, s.Cwd, open.Dirs))
	view := make(map[string]*FolderState, len(s.Folders)+1)
	for k, f := range s.Folders {
		view[k] = f
	}
	var c *FolderState
	if f := s.Folders[name]; f != nil {
		c = f.clone()
	} else {
		c = &FolderState{}
	}
	c.merge(open)
	view[name] = c
	return view
}

// finishRows sorts rows and merges rows whose usage key became equal after the day fallback
// (responses without a timestamp take the session's start day).
func finishRows(rows []Row) []Row {
	sortRows(rows)
	out := rows[:0]
	for _, r := range rows {
		if n := len(out); n > 0 {
			p := &out[n-1]
			if p.SessionID == r.SessionID && p.Folder == r.Folder && p.Model == r.Model && p.Day == r.Day && p.Speed == r.Speed && p.PromptFloor == r.PromptFloor {
				p.InputTokens += r.InputTokens
				p.OutputTokens += r.OutputTokens
				p.CacheReadTokens += r.CacheReadTokens
				p.CacheCreationTokens += r.CacheCreationTokens
				p.CacheCreation1hTokens += r.CacheCreation1hTokens
				p.WebSearchRequests += r.WebSearchRequests
				p.WebFetchRequests += r.WebFetchRequests
				if r.CopilotCredits != nil {
					sum := *r.CopilotCredits
					if p.CopilotCredits != nil {
						sum += *p.CopilotCredits
					}
					p.CopilotCredits = &sum
				}
				continue
			}
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sortRows(rows []Row) {
	slices.SortFunc(rows, func(x, y Row) int {
		if c := strings.Compare(x.SessionID, y.SessionID); c != 0 {
			return c
		}
		if c := strings.Compare(x.Folder, y.Folder); c != 0 {
			return c
		}
		if c := strings.Compare(x.Model, y.Model); c != 0 {
			return c
		}
		if c := strings.Compare(x.Day, y.Day); c != 0 {
			return c
		}
		if c := strings.Compare(x.Speed, y.Speed); c != 0 {
			return c
		}
		return int(min(max(x.PromptFloor-y.PromptFloor, -1), 1))
	})
}

// ClaudeCodeCoverage: pushback, tool loops, interrupts and permission refusals are visible in
// Claude Code transcripts; terminal exit codes are not reported structurally.
const ClaudeCodeCoverage = analyzer.CoveragePushback | analyzer.CoverageToolLoop | analyzer.CoverageInterrupt | analyzer.CoveragePermission

// derive computes the session-level analysis; a.mu must be held.
func (a *Aggregator) derive(s *SessionState) (q analyzer.Quality, detector int, topic string) {
	var an Analysis
	if s.Analysis != nil {
		an = *s.Analysis
	}
	q = an.Stats.Snapshot()
	if a.ex == nil {
		return q, 0, ""
	}
	cfg := a.ex.Settings()
	return q, cfg.DetectorVersion, analyzer.ClassifyTopic(an.Topic, cfg.Topics)
}

// IsReadTool reports whether a tool name only reads one file (Claude Code names).
func IsReadTool(name string) bool {
	switch strings.ToLower(name) {
	case "read", "notebookread", "view":
		return true
	}
	return false
}

// IsEditTool reports whether a tool name changes files (Claude Code and Copilot names).
func IsEditTool(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"edit", "write", "replace", "createfile", "create_file", "insert", "applypatch", "apply_patch"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// Counters returns a copy of the counters.
func (a *Aggregator) Counters() Counters {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneCounters(a.counters)
}

// Snapshot returns a deep copy of the state suitable for json.Marshal.
func (a *Aggregator) Snapshot() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := State{
		Version:  stateVersion,
		Sessions: make(map[string]*SessionState, len(a.sessions)),
		Seen:     make([]uint64, 0, len(a.seen)),
		Counters: cloneCounters(a.counters),
	}
	for id, s := range a.sessions {
		st.Sessions[id] = cloneSession(s)
	}
	for k := range a.seen {
		st.Seen = append(st.Seen, k)
	}
	slices.Sort(st.Seen)
	return st
}

// Restore replaces the aggregator state with st.
func (a *Aggregator) Restore(st State) error {
	if st.Version != stateVersion {
		return fmt.Errorf("aggregate: unsupported state version %d", st.Version)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = make(map[string]*SessionState, len(st.Sessions))
	for id, s := range st.Sessions {
		if s != nil {
			a.sessions[id] = cloneSession(s)
		}
	}
	a.seen = make(map[uint64]struct{}, len(st.Seen))
	for _, k := range st.Seen {
		a.seen[k] = struct{}{}
	}
	a.counters = cloneCounters(st.Counters)
	return nil
}

func cloneSession(s *SessionState) *SessionState {
	c := *s
	c.Folders = make(map[string]*FolderState, len(s.Folders))
	for name, f := range s.Folders {
		c.Folders[name] = f.clone()
	}
	c.Open = cloneRequest(s.Open)
	if s.Analysis != nil {
		an := *s.Analysis
		c.Analysis = &an
	}
	return &c
}

func cloneCounters(c Counters) Counters {
	out := c
	out.Skipped = make(map[string]int64, len(c.Skipped))
	for k, v := range c.Skipped {
		out.Skipped[k] = v
	}
	out.Ignored = make(map[string]int64, len(c.Ignored))
	for k, v := range c.Ignored {
		out.Ignored[k] = v
	}
	return out
}

func cloneRequest(r Request) Request {
	c := r
	c.Usage = make(map[string]*UsageState, len(r.Usage))
	for k, u := range r.Usage {
		uc := *u
		c.Usage[k] = &uc
	}
	c.Dirs = slices.Clone(r.Dirs)
	return c
}

// CountSessions is the number of distinct sessions in rows (rows are per session, folder and usage key).
func CountSessions(rows []Row) int {
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		seen[r.Tool+"\x00"+r.SessionID] = struct{}{}
	}
	return len(seen)
}
