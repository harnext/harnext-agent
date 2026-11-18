package analyzer

// Session quality counters.
//
// The unit is the exchange: one real user request plus all assistant and tool activity until
// the next real user request. An exchange has friction when at least one channel fires:
//
//	pushback  (P): the NEXT user message reports an error, corrects the agent, repeats the
//	               request or pastes error output after the agent edited files;
//	tool loop (T): the same tool error fingerprint repeats repeat_min_turns times in a row;
//	interrupt (I): the user stopped a tool or response, or denied a tool permission.
//
// Pushback is written to the exchange that failed (the previous one). The agent uploads these
// counters; the score and First-Pass Success are computed on the server, so nothing here
// depends on weights. Only counts, flags and hashes are kept (zero-prompt-retention).

// Quality is the uploaded per-session counter set (contract field "quality").
type Quality struct {
	Exchanges             int64 `json:"exchanges"`
	AnalyzedExchanges     int64 `json:"analyzed_exchanges"`
	FrictionExchanges     int64 `json:"friction_exchanges"`
	FxPushback            int64 `json:"fx_pushback"`
	FxToolLoop            int64 `json:"fx_tool_loop"`
	FxInterrupt           int64 `json:"fx_interrupt"`
	FxPrimaryPushback     int64 `json:"fx_primary_pushback"`
	FxPrimaryToolLoop     int64 `json:"fx_primary_tool_loop"`
	FxPrimaryInterrupt    int64 `json:"fx_primary_interrupt"`
	SigErrorReport        int64 `json:"sig_error_report"`
	SigCorrection         int64 `json:"sig_correction"`
	SigRepeat             int64 `json:"sig_repeat"`
	SigErrorPaste         int64 `json:"sig_error_paste_after_fix"`
	SigPositiveAck        int64 `json:"sig_positive_ack"`
	ToolCalls             int64 `json:"tool_calls"`
	ToolErrors            int64 `json:"tool_errors"`
	TerminalNonzero       int64 `json:"terminal_nonzero"`
	RepeatedErrorRuns     int64 `json:"repeated_error_runs"`
	RepeatedErrorCalls    int64 `json:"repeated_error_calls"`
	Responses             int64 `json:"responses"`
	CancelledResponses    int64 `json:"cancelled_responses"`
	InterruptedTools      int64 `json:"interrupted_tools"`
	PermissionDenials     int64 `json:"permission_denials"`
	FirstFrictionExchange int64 `json:"first_friction_exchange"` // 0 = none (sent as null)
}

// Coverage bits: which channels a source can measure (contract field "signal_coverage").
const (
	CoveragePushback   = 1 << 0
	CoverageToolLoop   = 1 << 1
	CoverageInterrupt  = 1 << 2
	CoverageExitCode   = 1 << 3
	CoveragePermission = 1 << 4
)

// openExchange is the state of the exchange in progress.
type openExchange struct {
	Open      bool  `json:"open"`
	Analyzed  bool  `json:"analyzed"`
	Index     int64 `json:"index"` // 1-based among analyzed exchanges; 0 when not analyzed
	Pushback  bool  `json:"pushback"`
	ToolLoop  bool  `json:"tool_loop"`
	Interrupt bool  `json:"interrupt"`
	Edited    bool  `json:"edited"` // the agent changed files in this exchange
}

// SessionStats holds the counters and sequencing state of one session.
type SessionStats struct {
	Q                  Quality      `json:"q"`
	UnanalyzedMessages int64        `json:"unanalyzed_messages"`
	Cur                openExchange `json:"cur"`
	PrevSig            Signature    `json:"prev_sig"`
	ErrFingerprint     uint64       `json:"err_fingerprint"`
	ErrRun             int32        `json:"err_run"`
}

// StartExchange folds one real user message. f is nil when no configuration was loaded (the
// message is counted but cannot be analyzed). A Claude Code interrupt marker is not a request:
// it marks the open exchange as interrupted instead.
func (s *SessionStats) StartExchange(f *UserFeatures, p Params) {
	if f != nil && f.Interrupted {
		s.RecordInterrupt()
		return
	}
	if f != nil {
		if f.ErrorReport {
			s.Q.SigErrorReport++
		}
		if f.Correction {
			s.Q.SigCorrection++
		}
		if f.PositiveAck {
			s.Q.SigPositiveAck++
		}
		if s.Cur.Open && s.Cur.Analyzed {
			repeat := p.RepeatJaccard > 0 && similarity(s.PrevSig, f.Sig) >= p.RepeatJaccard
			paste := s.Cur.Edited && p.ErrorPasteMinShare > 0 && f.ErrorShare() >= p.ErrorPasteMinShare
			if repeat {
				s.Q.SigRepeat++
			}
			if paste {
				s.Q.SigErrorPaste++
			}
			if f.Negative() || repeat || paste {
				s.Cur.Pushback = true
			}
		}
	}
	s.closeExchange()
	s.Q.Exchanges++
	s.Cur = openExchange{Open: true}
	if f == nil {
		s.UnanalyzedMessages++
		s.PrevSig = Signature{}
		return
	}
	s.Q.AnalyzedExchanges++
	s.Cur.Analyzed = true
	s.Cur.Index = s.Q.AnalyzedExchanges
	s.PrevSig = f.Sig
}

// closeExchange folds the open exchange into the counters.
func (s *SessionStats) closeExchange() {
	foldExchange(&s.Q, s.Cur)
	s.Cur.Open = false
}

func foldExchange(q *Quality, c openExchange) {
	if !c.Open || !c.Analyzed || !(c.Pushback || c.ToolLoop || c.Interrupt) {
		return
	}
	q.FrictionExchanges++
	if c.Pushback {
		q.FxPushback++
	}
	if c.ToolLoop {
		q.FxToolLoop++
	}
	if c.Interrupt {
		q.FxInterrupt++
	}
	// One primary channel per exchange, by priority T > I > P (structural signals first).
	switch {
	case c.ToolLoop:
		q.FxPrimaryToolLoop++
	case c.Interrupt:
		q.FxPrimaryInterrupt++
	default:
		q.FxPrimaryPushback++
	}
	if q.FirstFrictionExchange == 0 {
		q.FirstFrictionExchange = c.Index
	}
}

// Snapshot returns the counters with the open exchange folded in (its pushback is unknown
// until the next message, so a final exchange counts only its tool loop and interrupts).
func (s *SessionStats) Snapshot() Quality {
	q := s.Q
	foldExchange(&q, s.Cur)
	return q
}

// RecordResponse counts one unique assistant response outside sidechains.
func (s *SessionStats) RecordResponse() { s.Q.Responses++ }

// RecordCancelled counts one cancelled response (the response itself is counted by RecordResponse).
func (s *SessionStats) RecordCancelled() {
	s.Q.CancelledResponses++
	s.markInterrupt()
}

// RecordInterrupt marks the open exchange as stopped by the user.
func (s *SessionStats) RecordInterrupt() { s.markInterrupt() }

func (s *SessionStats) markInterrupt() {
	if s.Cur.Open {
		s.Cur.Interrupt = true
	}
}

// RecordEdit notes that the agent changed files in the open exchange.
func (s *SessionStats) RecordEdit() {
	if s.Cur.Open {
		s.Cur.Edited = true
	}
}

// ToolResult describes one tool call outcome without any of its output.
type ToolResult struct {
	IsError     bool
	Fingerprint uint64 // normalized error fingerprint; 0 = none
	Interrupted bool
	Denied      bool // the user rejected the tool permission
	ExitCode    *int // terminal exit code when the source reports it
}

// RecordToolResult folds one tool call. Consecutive errors with the same fingerprint form a
// run; a run reaching repeat_min_turns marks the open exchange as a tool loop.
func (s *SessionStats) RecordToolResult(r ToolResult, p Params) {
	s.Q.ToolCalls++
	if r.ExitCode != nil && *r.ExitCode != 0 {
		s.Q.TerminalNonzero++
	}
	if r.Denied {
		s.Q.PermissionDenials++
		s.markInterrupt()
	}
	if r.Interrupted {
		s.Q.InterruptedTools++
		s.markInterrupt()
	}
	if !r.IsError || r.Denied || r.Interrupted {
		// A success, a refusal or a stop ends the error run.
		if !r.IsError {
			s.ErrFingerprint, s.ErrRun = 0, 0
		}
		if r.IsError {
			s.Q.ToolErrors++
		}
		return
	}
	s.Q.ToolErrors++
	if r.Fingerprint != 0 && r.Fingerprint == s.ErrFingerprint {
		s.ErrRun++
	} else {
		s.ErrFingerprint, s.ErrRun = r.Fingerprint, 1
	}
	n := int32(p.RepeatMinTurns)
	if n < 2 || r.Fingerprint == 0 {
		return
	}
	switch {
	case s.ErrRun == n:
		s.Q.RepeatedErrorRuns++
		s.Q.RepeatedErrorCalls += int64(n) // the whole run counts once it reaches the threshold
		if s.Cur.Open {
			s.Cur.ToolLoop = true
		}
	case s.ErrRun > n:
		s.Q.RepeatedErrorCalls++
	}
}

// additive lists the counters that only ever grow by addition. FirstFrictionExchange is a position
// (1-based index of the first friction exchange), not a count, so it is not here: callers that split a
// session by folder renumber it themselves. A reflection test fails when a field is added without
// being listed here (or named in the exclusion).
func (q *Quality) additive() []*int64 {
	return []*int64{
		&q.Exchanges, &q.AnalyzedExchanges, &q.FrictionExchanges, &q.FxPushback, &q.FxToolLoop, &q.FxInterrupt,
		&q.FxPrimaryPushback, &q.FxPrimaryToolLoop, &q.FxPrimaryInterrupt,
		&q.SigErrorReport, &q.SigCorrection, &q.SigRepeat, &q.SigErrorPaste, &q.SigPositiveAck,
		&q.ToolCalls, &q.ToolErrors, &q.TerminalNonzero, &q.RepeatedErrorRuns, &q.RepeatedErrorCalls,
		&q.Responses, &q.CancelledResponses, &q.InterruptedTools, &q.PermissionDenials,
	}
}

// Counters returns pointers to the additive counters in a fixed order, FirstFrictionExchange excluded. The order is part of
// persisted state (aggregate packs the counters by position): append new counters at the end only.
func (q *Quality) Counters() []*int64 { return q.additive() }

// Add adds the additive counters of o to q (FirstFrictionExchange is left alone).
func (q *Quality) Add(o Quality) {
	dst, src := q.additive(), o.additive()
	for i := range dst {
		*dst[i] += *src[i]
	}
}

// Sub returns the additive counters of q minus those of o; FirstFrictionExchange of the result is 0.
func (q Quality) Sub(o Quality) Quality {
	var out Quality
	d, a, b := out.additive(), q.additive(), o.additive()
	for i := range d {
		*d[i] = *a[i] - *b[i]
	}
	return out
}

// IsZero reports whether every counter, FirstFrictionExchange included, is zero.
func (q Quality) IsZero() bool { return q == Quality{} }

// SplitStart divides the counter change d caused by one StartExchange call into the part that belongs to the
// exchange the call CLOSED (its friction channels, and the repeat / error-paste signals that are written to the
// previous request) and the part that belongs to the exchange it OPENED (the request count and the signals of
// the new message). closing + opening == d for every additive counter.
func SplitStart(d Quality) (closing, opening Quality) {
	closing, opening = d, d
	cp, op := closing.additive(), opening.additive()
	// Indexes into additive(): the friction block (2..8) and SigRepeat, SigErrorPaste (11, 12) close the old exchange.
	isClosing := func(i int) bool { return i >= 2 && i <= 8 || i == 11 || i == 12 }
	for i := range cp {
		if isClosing(i) {
			*op[i] = 0
		} else {
			*cp[i] = 0
		}
	}
	return closing, opening
}

// VirtualFold returns the counters the open exchange would add if it were closed now (tool loop and
// interrupt friction; pushback is unknown until the next message). It equals Snapshot() minus Q.
func (s *SessionStats) VirtualFold() Quality {
	return s.Snapshot().Sub(s.Q)
}
