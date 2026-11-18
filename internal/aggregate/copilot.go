package aggregate

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/copilot"
	"github.com/harnext/harnext-agent/internal/parser/copilotchat"
)

const (
	ToolVSCodeCopilot = "vscode_copilot"
	copilotSpeed      = "unknown"

	// 5: requests are split by folder: per-request counters and folders (CopilotRequestState.Q/Dirs), per-trace usage records
	// and the open exchange (older states are discarded and the files re-read).
	// 4: cache-creation tokens are counted (OTel cache_creation.input_tokens) and chat requests carry model, time, credits and
	// token metrics for history rows; older states are discarded and the files re-read.
	// 3: per-model totals are keyed by UsageKey.
	copilotStateVersion = 5
	// maxNanoAIU is 1e9 credits, the contract maximum of copilot_credits.
	maxNanoAIU int64 = 1_000_000_000_000_000_000
	// maxTracesPerSession bounds the per-session trace hash list (8 bytes each).
	maxTracesPerSession = 100_000
	// maxChatRequests bounds the per-session request list (9 bytes each).
	maxChatRequests = 10_000
	// maxFolderLen bounds the stored folder path.
	maxFolderLen = 512
)

// CopilotCounters describe what was counted, deduplicated or dropped. They hold counts only.
type CopilotCounters struct {
	UniqueSpans     int64 `json:"unique_spans"`
	DuplicateSpans  int64 `json:"duplicate_spans"`
	TraceCapReached int64 `json:"trace_cap_reached"`
	MissingIDs      int64 `json:"missing_ids"`

	// Chat session counters.
	ChatRequests          int64 `json:"chat_requests,omitempty"`
	ChatDuplicateRequests int64 `json:"chat_duplicate_requests,omitempty"`
	ChatBadIndex          int64 `json:"chat_bad_index,omitempty"`
	ChatRequestCapReached int64 `json:"chat_request_cap_reached,omitempty"`
	ChatToolCalls         int64 `json:"chat_tool_calls,omitempty"`

	// Folder split self-check, recomputed by every Rows call (gauges, not running totals): TracesBound traces were tied to a
	// chat request by start time, TracesUnbound found no request and went to the conversation's main folder. A bound
	// request that has Copilot's own credits is checked (TraceChecks): the credits of its traces must equal them
	// (TraceMatches). The ratio tells how reliable the time binding is.
	TracesBound   int64 `json:"traces_bound,omitempty"`
	TracesUnbound int64 `json:"traces_unbound,omitempty"`
	TraceChecks   int64 `json:"trace_checks,omitempty"`
	TraceMatches  int64 `json:"trace_matches,omitempty"`

	Skipped map[string]int64 `json:"skipped"`
	Ignored map[string]int64 `json:"ignored"`

	ChatSkipped map[string]int64 `json:"chat_skipped,omitempty"`
	ChatIgnored map[string]int64 `json:"chat_ignored,omitempty"`
}

// CopilotRequestState is one chat request, identified by a 64-bit hash of its id. The remaining fields feed the history
// rows of conversations that have no OTel usage: model, time (unix seconds), Copilot's credits (nano AIU) and the numbers
// the chat file keeps (last-round prompt and completion tokens, round count). Numbers and a model name only.
type CopilotRequestState struct {
	Hash       uint64 `json:"hash"`
	Terminal   bool   `json:"terminal,omitempty"`
	ToolsSeen  bool   `json:"tools_seen,omitempty"`
	Hidden     bool   `json:"hidden,omitempty"`
	Model      string `json:"model,omitempty"`
	Time       int64  `json:"time,omitempty"`
	NanoAIU    int64  `json:"nano_aiu,omitempty"`
	Prompt     int64  `json:"prompt,omitempty"`
	Completion int64  `json:"completion,omitempty"`
	Rounds     int    `json:"rounds,omitempty"`
	// Q are the counters of the exchange this request opened (packQ: position/value pairs of the non-zero ones), including the
	// part the NEXT message wrote to it. Dirs are the absolute folders of the files it edited or read (local state only,
	// at most maxDirsPerRequest); the request's folder is decided from them when rows are built.
	Q    []int64    `json:"q,omitempty"`
	Dirs []DirCount `json:"dirs,omitempty"`
}

// CopilotTraceEntry is one usage key's tokens and credits inside a trace.
type CopilotTraceEntry struct {
	Key string `json:"key"`
	Totals
	NanoAIU int64 `json:"nano_aiu,omitempty"`
}

// CopilotTraceUsage is what the OTel spans of one counted trace added to the session's model totals. Start (unix ms of the
// earliest span with a time) ties the trace to a chat request when rows are built; the entries let the trace's tokens and
// credits follow the request into its folder.
type CopilotTraceUsage struct {
	Hash    uint64              `json:"hash"`
	Start   int64               `json:"start,omitempty"`
	Entries []CopilotTraceEntry `json:"entries"`
}

// CopilotModelState adds the Copilot-only credit sum (nano AIU) to the shared model totals.
type CopilotModelState struct {
	ModelState
	NanoAIU int64 `json:"nano_aiu,omitempty"`
}

// CopilotSessionState is the persisted per-conversation part.
type CopilotSessionState struct {
	StartedAt   time.Time                     `json:"started_at"`
	EndedAt     time.Time                     `json:"ended_at"`
	ToolVersion string                        `json:"tool_version"`
	Models      map[string]*CopilotModelState `json:"models"`
	// Traces are sorted hashes of the trace ids already counted as a turn.
	Traces []uint64 `json:"traces"`
	// Requests are the chat requests in document order; Analysis holds counters and hashes only.
	Requests []CopilotRequestState `json:"requests,omitempty"`
	Analysis *Analysis             `json:"analysis,omitempty"`
	Cwd      string                `json:"cwd,omitempty"`
	// ChatStartedAt and ChatEndedAt span the chat requests (history rows only).
	ChatStartedAt time.Time `json:"chat_started_at,omitempty"`
	ChatEndedAt   time.Time `json:"chat_ended_at,omitempty"`
	// CurRequest is the hash of the request that opened the current exchange (0 = none): the analyzer's counters change
	// there until the next message. PreQ holds counters that belong to no request (before the first one, or of requests
	// dropped when the chat file was rewritten), so the folders always add up to the session's counters.
	CurRequest uint64  `json:"cur_request,omitempty"`
	PreQ       []int64 `json:"pre_q,omitempty"`
	// TraceUsage lists the counted traces' usage, sorted by hash.
	TraceUsage []CopilotTraceUsage `json:"trace_usage,omitempty"`
}

// CopilotState is the serializable aggregator state. Seen holds sorted 64-bit hashes of (trace, span).
type CopilotState struct {
	Version  int                             `json:"version"`
	Sessions map[string]*CopilotSessionState `json:"sessions"`
	Seen     []uint64                        `json:"seen"`
	Counters CopilotCounters                 `json:"counters"`
}

type copilotSession struct {
	startedAt, endedAt time.Time
	toolVersion        string
	models             map[string]*CopilotModelState
	traces             map[uint64]struct{}
	requests           []CopilotRequestState
	analysis           *Analysis
	cwd                string
	chatStart, chatEnd time.Time
	curHash            uint64
	curIdx             int // cache of the index of curHash in requests (verified before use)
	preQ               analyzer.Quality
	traceUsage         map[uint64]*CopilotTraceUsage
}

// CopilotAggregator folds VS Code Copilot events into per-(conversation, model) totals.
// Chat session events add friction, First-Pass Success, topic and folder to those rows.
// Replaying the same spans or requests is safe. It is safe for concurrent use.
type CopilotAggregator struct {
	mu       sync.Mutex
	sessions map[string]*copilotSession
	seen     map[uint64]struct{}
	counters CopilotCounters
	traceCap int
	ex       analyzer.Extractor
	resolve  ResolveFunc
}

// CopilotOption configures a CopilotAggregator.
type CopilotOption func(*CopilotAggregator)

// WithTraceCap lowers the per-session trace limit; values outside 1..maxTracesPerSession are ignored.
func WithTraceCap(n int) CopilotOption {
	return func(a *CopilotAggregator) {
		if n > 0 && n <= maxTracesPerSession {
			a.traceCap = n
		}
	}
}

func NewCopilot(opts ...CopilotOption) *CopilotAggregator {
	a := &CopilotAggregator{
		sessions: map[string]*copilotSession{},
		seen:     map[uint64]struct{}{},
		counters: newCopilotCounters(),
		traceCap: maxTracesPerSession,
		resolve:  defaultResolver.Resolve,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

func newCopilotCounters() CopilotCounters {
	return CopilotCounters{
		Skipped: map[string]int64{}, Ignored: map[string]int64{},
		ChatSkipped: map[string]int64{}, ChatIgnored: map[string]int64{},
	}
}

// SetResolver replaces the project resolver used to place folders (tests; nil restores the default).
func (a *CopilotAggregator) SetResolver(r ResolveFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r == nil {
		r = defaultResolver.Resolve
	}
	a.resolve = r
}

// SetAnalyzer sets the configuration used for friction thresholds and for deriving scores
// in Rows; nil makes Friction, FirstPassSuccess and Topic empty while state is kept.
func (a *CopilotAggregator) SetAnalyzer(ex analyzer.Extractor) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ex = ex
}

func (a *CopilotAggregator) params() analyzer.Params {
	if a.ex == nil {
		return analyzer.Params{}
	}
	return a.ex.Settings().Params
}

func (s *copilotSession) analysisState() *Analysis {
	if s.analysis == nil {
		s.analysis = &Analysis{}
	}
	return s.analysis
}

func (a *CopilotAggregator) session(id string) *copilotSession {
	s := a.sessions[id]
	if s == nil {
		s = &copilotSession{models: map[string]*CopilotModelState{}, traces: map[uint64]struct{}{}, traceUsage: map[uint64]*CopilotTraceUsage{}}
		a.sessions[id] = s
	}
	return s
}

// SetFolder records the workspace folder of a session; empty or oversized values are ignored.
func (a *CopilotAggregator) SetFolder(sessionID, folder string) {
	if sessionID == "" || folder == "" || len(folder) > maxFolderLen {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.session(sessionID).cwd = folder
}

// AddChat folds one chat session event (document order) into the session's analysis.
func (a *CopilotAggregator) AddChat(sessionID string, ev copilotchat.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch e := ev.(type) {
	case copilotchat.Ignored:
		bump(a.counters.ChatIgnored, e.Type)
	case copilotchat.Skipped:
		bump(a.counters.ChatSkipped, string(e.Reason))
	case copilotchat.RequestStart:
		if sessionID != "" {
			a.chatStart(a.session(sessionID), e)
		}
	case copilotchat.RequestState:
		if sessionID != "" {
			s := a.session(sessionID)
			if r := a.requestAt(s, e.Index); r != nil {
				a.applyState(s, r, e.State, e.CompletedAt)
			}
		}
	case copilotchat.RequestTools:
		if sessionID != "" {
			s := a.session(sessionID)
			if r := a.requestAt(s, e.Index); r != nil {
				if r.Hash != 0 {
					if e.Model != "" {
						r.Model = e.Model // the resolved model replaces the requested one
					}
					if e.Rounds > 0 {
						r.Rounds = e.Rounds
					}
				}
				a.applyTools(s, r, e.Tools[:], e.ToolCount)
			}
		}
	case copilotchat.RequestMetric:
		if sessionID != "" {
			s := a.session(sessionID)
			if r := a.requestAt(s, e.Index); r != nil && r.Hash != 0 {
				switch e.Kind {
				case copilotchat.MetricCredits:
					r.NanoAIU = nanoOf(e.Value)
				case copilotchat.MetricPrompt:
					r.Prompt = int64(e.Value)
				case copilotchat.MetricCompletion:
					r.Completion = int64(e.Value)
				}
			}
		}
	case copilotchat.RequestToolCalls:
		if sessionID != "" {
			a.applyCalls(a.session(sessionID), e.Calls[:e.N])
		}
	case copilotchat.RequestFiles:
		if sessionID != "" {
			s := a.session(sessionID)
			if r := a.requestAt(s, e.Index); r != nil {
				a.applyFiles(r, e.Files[:e.N])
			}
		}
	}
}

// applyFiles records the folder of each file once per request and kind (VS Code repeats parts in later patch lines). The
// folder counts decide the request's folder when rows are built.
func (a *CopilotAggregator) applyFiles(r *CopilotRequestState, files []copilotchat.FileRef) {
	if r.Hash == 0 {
		return
	}
	for _, f := range files {
		salt := uint64(0x66696c65) // "file"
		if f.Edit {
			salt = 0x65646974 // "edit"
		}
		if f.Dir == "" || !a.markSeen(splitmix64(r.Hash^f.Hash^salt)) {
			continue
		}
		r.Dirs = addDir(r.Dirs, f.Dir, f.Edit)
	}
}

// curRequest returns the request that opened the current exchange, nil when there is none.
func (s *copilotSession) curRequest() *CopilotRequestState {
	if s.curHash == 0 {
		return nil
	}
	if s.curIdx < 0 || s.curIdx >= len(s.requests) || s.requests[s.curIdx].Hash != s.curHash {
		s.curIdx = slices.IndexFunc(s.requests, func(r CopilotRequestState) bool { return r.Hash == s.curHash })
		if s.curIdx < 0 {
			return nil
		}
	}
	return &s.requests[s.curIdx]
}

// addQ writes a counter change to the open exchange's request (or to PreQ when there is none).
func (s *copilotSession) addQ(d analyzer.Quality) {
	if d.IsZero() {
		return
	}
	if r := s.curRequest(); r != nil {
		q := unpackQ(r.Q)
		q.Add(d)
		r.Q = packQ(q)
		return
	}
	s.preQ.Add(d)
}

// trackQ runs f, which may change the analyzer's counters, and books the change to the open exchange's request.
func (s *copilotSession) trackQ(f func(st *analyzer.SessionStats)) {
	st := &s.analysisState().Stats
	s.addQ(track(st, func() { f(st) }))
}

// packQ stores the non-zero additive counters as position/value pairs (most requests have a handful).
func packQ(q analyzer.Quality) []int64 {
	var out []int64
	for i, p := range q.Counters() {
		if *p != 0 {
			out = append(out, int64(i), *p)
		}
	}
	return out
}

func unpackQ(p []int64) analyzer.Quality {
	var q analyzer.Quality
	c := q.Counters()
	for i := 0; i+1 < len(p); i += 2 {
		if p[i] >= 0 && int(p[i]) < len(c) {
			*c[p[i]] = p[i+1]
		}
	}
	return q
}

// applyCalls folds completed tool invocations once each (VS Code rewrites the same call in
// later patch lines; toolCallId deduplicates). Calls are attributed to the open exchange.
func (a *CopilotAggregator) applyCalls(s *copilotSession, calls []copilotchat.ToolCall) {
	p := a.params()
	s.trackQ(func(st *analyzer.SessionStats) {
		for _, c := range calls {
			if c.IDHash == 0 || !a.markSeen(splitmix64(c.IDHash^0x63616c6c)) { // "call" domain
				continue
			}
			a.counters.ChatToolCalls++
			if IsEditTool(c.ToolID) {
				st.RecordEdit()
			}
			r := analyzer.ToolResult{IsError: c.IsError, Fingerprint: c.ErrorHash}
			if c.HasExit {
				ec := c.ExitCode
				r.ExitCode = &ec
			}
			st.RecordToolResult(r, p)
		}
	})
}

// nanoOf converts Copilot credits to nano AIU (the unit of the OTel sum), capped at the contract maximum.
func nanoOf(credits float64) int64 {
	if credits <= 0 || math.IsNaN(credits) {
		return 0
	}
	if credits >= float64(maxNanoAIU)/1e9 {
		return maxNanoAIU
	}
	return int64(math.Round(credits * 1e9))
}

// touchChat widens the chat time range; zero times are ignored.
func (s *copilotSession) touchChat(ts ...time.Time) {
	for _, t := range ts {
		if t.IsZero() {
			continue
		}
		if s.chatStart.IsZero() || t.Before(s.chatStart) {
			s.chatStart = t
		}
		if t.After(s.chatEnd) {
			s.chatEnd = t
		}
	}
}

func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

func (a *CopilotAggregator) requestAt(s *copilotSession, i int) *CopilotRequestState {
	if i < 0 || i >= len(s.requests) {
		a.counters.ChatBadIndex++
		return nil
	}
	return &s.requests[i]
}

// chatStart handles a request start. A request without an id cannot be deduplicated: it only
// keeps indexes aligned and is never counted.
func (a *CopilotAggregator) chatStart(s *copilotSession, e copilotchat.RequestStart) {
	var hash uint64
	if e.RequestID != "" {
		hash = hashKey('q', e.RequestID, "")
	}
	idx := -1
	if e.At >= 0 && e.At < len(s.requests) {
		if hash != 0 && s.requests[e.At].Hash == hash {
			idx = e.At
		} else {
			s.dropRequestsFrom(e.At)
		}
	}
	if idx < 0 && hash != 0 {
		idx = slices.IndexFunc(s.requests, func(r CopilotRequestState) bool { return r.Hash == hash })
	}
	s.touchChat(e.Timestamp, e.CompletedAt)
	if idx >= 0 {
		a.counters.ChatDuplicateRequests++
		r := &s.requests[idx]
		applyRequestMetrics(r, e)
		if e.HasState {
			a.applyState(s, r, e.State, e.CompletedAt)
		}
		a.applyTools(s, r, e.Tools[:], e.ToolCount)
		a.applyCalls(s, e.Calls[:e.CallCount])
		a.applyFiles(r, e.Files[:e.FileCount])
		return
	}
	if len(s.requests) >= maxChatRequests {
		a.counters.ChatRequestCapReached++
		return
	}
	req := CopilotRequestState{Hash: hash}
	if hash != 0 {
		req.Hidden = e.Hidden
		if !e.Timestamp.IsZero() {
			req.Time = e.Timestamp.Unix()
		}
		applyRequestMetrics(&req, e)
	}
	s.requests = append(s.requests, req)
	r := &s.requests[len(s.requests)-1]
	if hash == 0 {
		return
	}
	a.counters.ChatRequests++
	if !e.Hidden {
		an := s.analysisState()
		before := an.Stats.Q
		if e.Analyzed {
			f := e.Features
			an.Stats.StartExchange(&f, a.params())
			an.Topic.AddKeywords(e.Features.KeywordScores)
		} else {
			an.Stats.StartExchange(nil, a.params())
		}
		d := an.Stats.Q.Sub(before)
		// The new message closes the open exchange (pushback and friction belong to the request that opened it) and opens its own.
		closing, opening := analyzer.SplitStart(d)
		s.addQ(closing)
		s.curHash, s.curIdx = hash, len(s.requests)-1
		s.addQ(opening)
	}
	if e.HasState {
		a.applyState(s, r, e.State, e.CompletedAt)
	}
	a.applyTools(s, r, e.Tools[:], e.ToolCount)
	a.applyCalls(s, e.Calls[:e.CallCount])
	a.applyFiles(r, e.Files[:e.FileCount])
}

// dropRequestsFrom removes the requests from index i on (the chat file was rewritten). The analyzer's counters are not rolled
// back, so the counters of the removed requests move to PreQ and the folders keep adding up to the session.
func (s *copilotSession) dropRequestsFrom(i int) {
	for _, r := range s.requests[i:] {
		s.preQ.Add(unpackQ(r.Q))
	}
	s.requests = s.requests[:i]
}

// applyRequestMetrics copies the model and the numbers of a request event; a replayed request refreshes them.
func applyRequestMetrics(r *CopilotRequestState, e copilotchat.RequestStart) {
	if e.Model != "" {
		r.Model = e.Model
	}
	if e.HasCredits {
		r.NanoAIU = nanoOf(e.Credits)
	}
	if e.HasPrompt {
		r.Prompt = e.PromptTokens
	}
	if e.HasCompletion {
		r.Completion = e.CompletionTokens
	}
	if e.Rounds > 0 {
		r.Rounds = e.Rounds
	}
}

// applyState: states 1 (complete), 2 (cancelled) and 3 (failed) are terminal and counted once.
func (a *CopilotAggregator) applyState(s *copilotSession, r *CopilotRequestState, state int, completedAt time.Time) {
	s.touchChat(completedAt)
	if r.Hash == 0 || r.Terminal || state < copilotchat.StateComplete || state > copilotchat.StateFailed {
		return
	}
	r.Terminal = true
	s.trackQ(func(st *analyzer.SessionStats) {
		st.RecordResponse()
		if state == copilotchat.StateCancelled {
			st.RecordCancelled()
		}
	})
}

// applyTools adds tool names once per request (topic signals). Tool outcomes come from the
// response's toolInvocationSerialized parts (applyCalls).
func (a *CopilotAggregator) applyTools(s *copilotSession, r *CopilotRequestState, tools []string, n int) {
	if r.ToolsSeen || r.Hash == 0 || n <= 0 {
		return
	}
	r.ToolsSeen = true
	topic := &s.analysisState().Topic
	for i := 0; i < n && i < len(tools); i++ {
		topic.Tool.Add(analyzer.SignalKey(tools[i]))
	}
}

// Add folds one parser event into the totals.
func (a *CopilotAggregator) Add(ev copilot.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch e := ev.(type) {
	case copilot.ChatUsage:
		a.addUsage(e)
	case copilot.Ignored:
		bump(a.counters.Ignored, e.Type)
	case copilot.Skipped:
		bump(a.counters.Skipped, string(e.Reason))
	}
}

func (a *CopilotAggregator) addUsage(e copilot.ChatUsage) {
	// Without all ids a replay cannot be told apart from a new span; do not count.
	if e.ConversationID == "" || e.TraceID == "" || e.SpanID == "" || e.Model == "" {
		a.counters.MissingIDs++
		return
	}
	if !a.markSeen(hashKey('p', e.TraceID, e.SpanID)) {
		a.counters.DuplicateSpans++
		return
	}
	a.counters.UniqueSpans++

	s := a.session(e.ConversationID)
	s.touch(e.Start, e.End, e.ToolVersion)

	// Copilot reports no speed; the request's whole prompt (cache reads included) selects the band.
	key := UsageKey{
		Model: e.Model, Day: dayOf(e.Start), Speed: copilotSpeed,
		PromptFloor: analyzer.PromptFloor(contextBands(a.ex), e.Usage.PromptTokens),
	}.String()
	m := s.models[key]
	if m == nil {
		m = &CopilotModelState{}
		s.models[key] = m
	}
	m.InputTokens += e.Usage.InputTokens
	m.OutputTokens += e.Usage.OutputTokens
	m.CacheReadTokens += e.Usage.CacheReadTokens
	m.CacheCreationTokens += e.Usage.CacheCreationTokens
	if e.NanoAIU > 0 {
		if m.NanoAIU > maxNanoAIU-e.NanoAIU {
			m.NanoAIU = maxNanoAIU
		} else {
			m.NanoAIU += e.NanoAIU
		}
	}

	tk := hashKey('c', e.TraceID, "")
	if _, ok := s.traces[tk]; !ok {
		if len(s.traces) >= a.traceCap {
			a.counters.TraceCapReached++
			return
		}
		s.traces[tk] = struct{}{}
		m.TurnCount++
	}
	// The counted trace remembers what its spans added, so its tokens and credits can follow the chat request it belongs to.
	s.addTraceUsage(tk, e, key)
}

// addTraceUsage adds one span's tokens and credits to its trace's record.
func (s *copilotSession) addTraceUsage(tk uint64, e copilot.ChatUsage, key string) {
	t := s.traceUsage[tk]
	if t == nil {
		t = &CopilotTraceUsage{Hash: tk}
		s.traceUsage[tk] = t
	}
	if !e.Start.IsZero() && (t.Start == 0 || e.Start.UnixMilli() < t.Start) {
		t.Start = e.Start.UnixMilli()
	}
	i := slices.IndexFunc(t.Entries, func(x CopilotTraceEntry) bool { return x.Key == key })
	if i < 0 {
		t.Entries = append(t.Entries, CopilotTraceEntry{Key: key})
		i = len(t.Entries) - 1
	}
	en := &t.Entries[i]
	en.InputTokens += e.Usage.InputTokens
	en.OutputTokens += e.Usage.OutputTokens
	en.CacheReadTokens += e.Usage.CacheReadTokens
	en.CacheCreationTokens += e.Usage.CacheCreationTokens
	en.NanoAIU = satAdd(en.NanoAIU, e.NanoAIU, maxNanoAIU)
}

// touch widens the time range; zero times leave it unchanged.
func (s *copilotSession) touch(start, end time.Time, version string) {
	if end.IsZero() {
		end = start
	}
	if !start.IsZero() && (s.startedAt.IsZero() || start.Before(s.startedAt)) {
		s.startedAt = start
	}
	if !end.IsZero() && end.After(s.endedAt) {
		s.endedAt = end
	}
	s.toolVersion = keep(version, s.toolVersion)
}

func (a *CopilotAggregator) markSeen(k uint64) bool {
	if _, ok := a.seen[k]; ok {
		return false
	}
	a.seen[k] = struct{}{}
	return true
}

// maxEstimatedTokens bounds summed token estimates (the contract's token limit is far above it).
const maxEstimatedTokens int64 = 1 << 50

// satAdd adds b to a, stopping at limit.
func satAdd(a, b, limit int64) int64 {
	if b <= 0 {
		return a
	}
	if a > limit-b {
		return limit
	}
	return a + b
}

// CopilotCoverage: chat session files carry user text, cancellations, tool errors and terminal
// exit codes. Permission refusals are not used until VS Code's confirmation enum is verified.
const CopilotCoverage = analyzer.CoveragePushback | analyzer.CoverageToolLoop | analyzer.CoverageInterrupt | analyzer.CoverageExitCode

// derive computes the session-level analysis; a.mu must be held.
func (a *CopilotAggregator) derive(s *copilotSession) (q analyzer.Quality, detector int, topic string) {
	var an Analysis
	if s.analysis != nil {
		an = *s.analysis
	}
	q = an.Stats.Snapshot()
	if a.ex == nil {
		return q, 0, ""
	}
	cfg := a.ex.Settings()
	return q, cfg.DetectorVersion, analyzer.ClassifyTopic(an.Topic, cfg.Topics)
}

// Counters returns a copy of the counters.
func (a *CopilotAggregator) Counters() CopilotCounters {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneCopilotCounters(a.counters)
}

// Snapshot returns a deep copy of the state suitable for json.Marshal.
func (a *CopilotAggregator) Snapshot() CopilotState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := CopilotState{
		Version:  copilotStateVersion,
		Sessions: make(map[string]*CopilotSessionState, len(a.sessions)),
		Seen:     make([]uint64, 0, len(a.seen)),
		Counters: cloneCopilotCounters(a.counters),
	}
	for id, s := range a.sessions {
		cs := &CopilotSessionState{
			StartedAt: s.startedAt, EndedAt: s.endedAt, ToolVersion: s.toolVersion,
			Models:   make(map[string]*CopilotModelState, len(s.models)),
			Traces:   make([]uint64, 0, len(s.traces)),
			Requests: cloneRequests(s.requests), Cwd: s.cwd,
			ChatStartedAt: s.chatStart, ChatEndedAt: s.chatEnd,
			CurRequest: s.curHash, PreQ: packQ(s.preQ),
		}
		for _, t := range s.traceUsage {
			tc := *t
			tc.Entries = slices.Clone(t.Entries)
			cs.TraceUsage = append(cs.TraceUsage, tc)
		}
		slices.SortFunc(cs.TraceUsage, func(a, b CopilotTraceUsage) int { return cmp.Compare(a.Hash, b.Hash) })
		if s.analysis != nil {
			an := *s.analysis
			cs.Analysis = &an
		}
		for name, m := range s.models {
			mc := *m
			cs.Models[name] = &mc
		}
		for k := range s.traces {
			cs.Traces = append(cs.Traces, k)
		}
		slices.Sort(cs.Traces)
		st.Sessions[id] = cs
	}
	for k := range a.seen {
		st.Seen = append(st.Seen, k)
	}
	slices.Sort(st.Seen)
	return st
}

// Restore replaces the aggregator state with st; on error the current state is kept.
func (a *CopilotAggregator) Restore(st CopilotState) error {
	if st.Version != copilotStateVersion {
		return fmt.Errorf("aggregate: unsupported copilot state version %d", st.Version)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := validateCopilotState(st, a.traceCap); err != nil {
		return err
	}
	sessions := make(map[string]*copilotSession, len(st.Sessions))
	for id, s := range st.Sessions {
		rs := &copilotSession{
			startedAt: s.StartedAt, endedAt: s.EndedAt, toolVersion: s.ToolVersion,
			models:   make(map[string]*CopilotModelState, len(s.Models)),
			traces:   make(map[uint64]struct{}, len(s.Traces)),
			requests: cloneRequests(s.Requests), cwd: s.Cwd,
			chatStart: s.ChatStartedAt, chatEnd: s.ChatEndedAt,
			curHash: s.CurRequest, preQ: unpackQ(s.PreQ), traceUsage: make(map[uint64]*CopilotTraceUsage, len(s.TraceUsage)),
		}
		for _, t := range s.TraceUsage {
			tc := t
			tc.Entries = slices.Clone(t.Entries)
			rs.traceUsage[t.Hash] = &tc
		}
		if s.Analysis != nil {
			an := *s.Analysis
			rs.analysis = &an
		}
		for name, m := range s.Models {
			mc := *m
			rs.models[name] = &mc
		}
		for _, k := range s.Traces {
			rs.traces[k] = struct{}{}
		}
		sessions[id] = rs
	}
	seen := make(map[uint64]struct{}, len(st.Seen))
	for _, k := range st.Seen {
		seen[k] = struct{}{}
	}
	a.sessions, a.seen = sessions, seen
	a.counters = cloneCopilotCounters(st.Counters)
	return nil
}

func validateCopilotState(st CopilotState, traceCap int) error {
	if !sortedUnique(st.Seen) {
		return fmt.Errorf("aggregate: copilot state: seen hashes not sorted and unique")
	}
	for id, s := range st.Sessions {
		switch {
		case id == "" || s == nil:
			return fmt.Errorf("aggregate: copilot state: invalid session entry")
		case len(s.Models) == 0 && (len(s.Traces) > 0 || (len(s.Requests) == 0 && s.Analysis == nil && s.Cwd == "")):
			return fmt.Errorf("aggregate: copilot state: session without models")
		case len(s.Requests) > maxChatRequests || len(s.Cwd) > maxFolderLen:
			return fmt.Errorf("aggregate: copilot state: invalid chat fields")
		case s.Analysis != nil && invalidStats(s.Analysis.Stats):
			return fmt.Errorf("aggregate: copilot state: invalid analysis stats")
		case !s.StartedAt.IsZero() && s.EndedAt.Before(s.StartedAt):
			return fmt.Errorf("aggregate: copilot state: session ends before it starts")
		case !s.ChatStartedAt.IsZero() && s.ChatEndedAt.Before(s.ChatStartedAt):
			return fmt.Errorf("aggregate: copilot state: chat ends before it starts")
		case len(s.Traces) > traceCap || !sortedUnique(s.Traces):
			return fmt.Errorf("aggregate: copilot state: invalid trace list")
		}
		for _, r := range s.Requests {
			if r.NanoAIU < 0 || r.NanoAIU > maxNanoAIU || r.Prompt < 0 || r.Completion < 0 || r.Rounds < 0 || r.Time < 0 || len(r.Model) > maxFolderLen {
				return fmt.Errorf("aggregate: copilot state: invalid request entry")
			}
			if !validPackedQ(r.Q) || len(r.Dirs) > maxDirsPerRequest || !validDirs(r.Dirs) {
				return fmt.Errorf("aggregate: copilot state: invalid request counters or folders")
			}
		}
		if !validPackedQ(s.PreQ) {
			return fmt.Errorf("aggregate: copilot state: invalid unattributed counters")
		}
		traces := make(map[uint64]struct{}, len(s.Traces))
		for _, h := range s.Traces {
			traces[h] = struct{}{}
		}
		for i, t := range s.TraceUsage {
			if _, ok := traces[t.Hash]; !ok || (i > 0 && s.TraceUsage[i-1].Hash >= t.Hash) || t.Start < 0 {
				return fmt.Errorf("aggregate: copilot state: invalid trace usage list")
			}
			for _, e := range t.Entries {
				if _, ok := ParseUsageKey(e.Key); !ok || e.InputTokens < 0 || e.OutputTokens < 0 || e.CacheReadTokens < 0 ||
					e.CacheCreationTokens < 0 || e.CacheCreation1hTokens != 0 || e.NanoAIU < 0 || e.NanoAIU > maxNanoAIU {
					return fmt.Errorf("aggregate: copilot state: invalid trace usage entry")
				}
			}
		}
		for name, m := range s.Models {
			if _, ok := ParseUsageKey(name); !ok || m == nil {
				return fmt.Errorf("aggregate: copilot state: invalid model entry")
			}
			if m.InputTokens < 0 || m.OutputTokens < 0 || m.CacheReadTokens < 0 ||
				m.CacheCreationTokens < 0 || m.CacheCreation1hTokens != 0 || m.TurnCount < 0 ||
				m.NanoAIU < 0 || m.NanoAIU > maxNanoAIU {
				return fmt.Errorf("aggregate: copilot state: invalid model totals")
			}
		}
	}
	c := st.Counters
	if c.UniqueSpans < 0 || c.DuplicateSpans < 0 || c.TraceCapReached < 0 || c.MissingIDs < 0 ||
		c.ChatRequests < 0 || c.ChatDuplicateRequests < 0 || c.ChatBadIndex < 0 || c.ChatRequestCapReached < 0 {
		return fmt.Errorf("aggregate: copilot state: negative counter")
	}
	for _, m := range []map[string]int64{c.Skipped, c.Ignored, c.ChatSkipped, c.ChatIgnored} {
		for k, v := range m {
			if k == "" || v < 0 {
				return fmt.Errorf("aggregate: copilot state: invalid counter entry")
			}
		}
	}
	return nil
}

func invalidStats(s analyzer.SessionStats) bool {
	q := s.Q
	for _, v := range []int64{q.Exchanges, q.AnalyzedExchanges, q.FrictionExchanges, q.ToolCalls, q.ToolErrors,
		q.Responses, q.CancelledResponses, q.FirstFrictionExchange, s.UnanalyzedMessages, int64(s.ErrRun)} {
		if v < 0 {
			return true
		}
	}
	return q.FrictionExchanges > q.AnalyzedExchanges || q.AnalyzedExchanges > q.Exchanges
}

func sortedUnique(s []uint64) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] >= s[i] {
			return false
		}
	}
	return true
}

func cloneCopilotCounters(c CopilotCounters) CopilotCounters {
	out := c
	out.Skipped = make(map[string]int64, len(c.Skipped))
	for k, v := range c.Skipped {
		out.Skipped[k] = v
	}
	out.Ignored = make(map[string]int64, len(c.Ignored))
	for k, v := range c.Ignored {
		out.Ignored[k] = v
	}
	out.ChatSkipped = make(map[string]int64, len(c.ChatSkipped))
	for k, v := range c.ChatSkipped {
		out.ChatSkipped[k] = v
	}
	out.ChatIgnored = make(map[string]int64, len(c.ChatIgnored))
	for k, v := range c.ChatIgnored {
		out.ChatIgnored[k] = v
	}
	return out
}

func cloneRequests(in []CopilotRequestState) []CopilotRequestState {
	out := slices.Clone(in)
	for i := range out {
		out[i].Q = slices.Clone(out[i].Q)
		out[i].Dirs = slices.Clone(out[i].Dirs)
	}
	return out
}

// validPackedQ: position/value pairs, positions inside the counter list, values >= 0.
func validPackedQ(p []int64) bool {
	if len(p)%2 != 0 {
		return false
	}
	n := int64(len((&analyzer.Quality{}).Counters()))
	for i := 0; i < len(p); i += 2 {
		if p[i] < 0 || p[i] >= n || p[i+1] < 0 {
			return false
		}
	}
	return true
}

func validDirs(dirs []DirCount) bool {
	for _, d := range dirs {
		if d.Dir == "" || len(d.Dir) > 1024 || d.Edit < 0 || d.Read < 0 {
			return false
		}
	}
	return true
}
