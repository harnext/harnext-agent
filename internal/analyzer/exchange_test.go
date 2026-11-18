package analyzer

import "testing"

var exParams = Params{RepeatMinTurns: 3, RepeatJaccard: 0.8, ErrorPasteMinShare: 0.6}

func msg(opts ...func(*UserFeatures)) *UserFeatures {
	f := &UserFeatures{Chars: 40}
	for _, o := range opts {
		o(f)
	}
	return f
}

func negative(f *UserFeatures)   { f.ErrorReport = true }
func correction(f *UserFeatures) { f.Correction = true }
func errPaste(f *UserFeatures)   { f.ErrorRunes = 30 }

func sigOf(s string) func(*UserFeatures) {
	return func(f *UserFeatures) { f.Sig = signature(s) }
}

func errResult(fp uint64) ToolResult { return ToolResult{IsError: true, Fingerprint: fp} }

func TestPushbackIsWrittenToThePreviousExchange(t *testing.T) {
	var s SessionStats
	s.StartExchange(msg(), exParams)         // 1
	s.StartExchange(msg(negative), exParams) // 2: pushes back on 1
	s.StartExchange(msg(), exParams)         // 3
	q := s.Snapshot()
	if q.Exchanges != 3 || q.AnalyzedExchanges != 3 || q.FrictionExchanges != 1 || q.FxPushback != 1 || q.FirstFrictionExchange != 1 {
		t.Errorf("q = %+v", q)
	}
	if q.SigErrorReport != 1 || q.FxPrimaryPushback != 1 {
		t.Errorf("signals = %+v", q)
	}
}

func TestFirstMessageNegativeIsNotPushback(t *testing.T) {
	var s SessionStats
	s.StartExchange(msg(negative), exParams)
	q := s.Snapshot()
	if q.FrictionExchanges != 0 || q.SigErrorReport != 1 {
		t.Errorf("q = %+v", q)
	}
}

func TestUnionCountsAnExchangeOnce(t *testing.T) {
	var s SessionStats
	s.StartExchange(msg(), exParams)
	for range 3 {
		s.RecordToolResult(errResult(7), exParams)
	}
	s.RecordInterrupt()
	s.StartExchange(msg(negative, correction), exParams)
	q := s.Snapshot()
	if q.FrictionExchanges != 1 || q.FxToolLoop != 1 || q.FxInterrupt != 1 || q.FxPushback != 1 {
		t.Errorf("q = %+v", q)
	}
	// Primary channel: tool loop wins; the primaries sum to friction_exchanges.
	if q.FxPrimaryToolLoop != 1 || q.FxPrimaryInterrupt+q.FxPrimaryPushback != 0 {
		t.Errorf("primary = %+v", q)
	}
	if q.SigErrorReport != 1 || q.SigCorrection != 1 {
		t.Errorf("signals = %+v", q)
	}
}

func TestToolLoopRuns(t *testing.T) {
	var s SessionStats
	s.StartExchange(msg(), exParams)
	for _, r := range []ToolResult{errResult(1), errResult(1), {}, errResult(1), errResult(1), errResult(1), errResult(1), errResult(2)} {
		s.RecordToolResult(r, exParams)
	}
	q := s.Snapshot()
	// The first run (2) is broken by a success; the second reaches 3 then grows to 4.
	if q.RepeatedErrorRuns != 1 || q.RepeatedErrorCalls != 4 || q.ToolCalls != 8 || q.ToolErrors != 7 || q.FxToolLoop != 1 {
		t.Errorf("q = %+v", q)
	}
}

func TestDeniedAndInterruptedToolsBreakRunsAndMarkInterrupt(t *testing.T) {
	var s SessionStats
	s.StartExchange(msg(), exParams)
	s.RecordToolResult(errResult(1), exParams)
	s.RecordToolResult(ToolResult{IsError: true, Fingerprint: 1, Denied: true}, exParams)
	s.RecordToolResult(errResult(1), exParams)
	q := s.Snapshot()
	if q.PermissionDenials != 1 || q.FxInterrupt != 1 || q.RepeatedErrorRuns != 0 {
		t.Errorf("q = %+v", q)
	}
}

func TestExitCodes(t *testing.T) {
	var s SessionStats
	zero, one := 0, 1
	s.RecordToolResult(ToolResult{ExitCode: &zero}, exParams)
	s.RecordToolResult(ToolResult{IsError: true, ExitCode: &one}, exParams)
	if q := s.Snapshot(); q.TerminalNonzero != 1 || q.ToolCalls != 2 {
		t.Errorf("q = %+v", q)
	}
}

func TestRepeatAndErrorPaste(t *testing.T) {
	var s SessionStats
	s.StartExchange(msg(sigOf("please add validation to the signup form")), exParams)
	s.StartExchange(msg(sigOf("please add validation to the signup form!")), exParams) // repeat
	s.RecordEdit()
	s.StartExchange(msg(errPaste), exParams) // pasted error after the agent edited files
	s.StartExchange(msg(errPaste), exParams) // no edit in the previous exchange: not counted
	q := s.Snapshot()
	if q.SigRepeat != 1 || q.SigErrorPaste != 1 || q.FrictionExchanges != 2 || q.FirstFrictionExchange != 1 {
		t.Errorf("q = %+v", q)
	}
}

func TestInterruptMarkerDoesNotOpenAnExchange(t *testing.T) {
	var s SessionStats
	s.StartExchange(msg(), exParams)
	s.StartExchange(&UserFeatures{Interrupted: true}, exParams)
	q := s.Snapshot()
	if q.Exchanges != 1 || q.FxInterrupt != 1 {
		t.Errorf("q = %+v", q)
	}
}

func TestUnanalyzedExchangesStayOutOfTheDenominator(t *testing.T) {
	var s SessionStats
	s.StartExchange(nil, exParams)
	for range 3 {
		s.RecordToolResult(errResult(5), exParams)
	}
	s.StartExchange(msg(), exParams)
	s.StartExchange(msg(negative), exParams)
	q := s.Snapshot()
	if q.Exchanges != 3 || q.AnalyzedExchanges != 2 || s.UnanalyzedMessages != 1 {
		t.Errorf("q = %+v", q)
	}
	// The tool loop happened in an unanalyzed exchange; pushback is on analyzed exchange 1.
	if q.FrictionExchanges != 1 || q.FxToolLoop != 0 || q.FirstFrictionExchange != 1 || q.RepeatedErrorRuns != 1 {
		t.Errorf("q = %+v", q)
	}
}

func TestSnapshotDoesNotMutate(t *testing.T) {
	var s SessionStats
	s.StartExchange(msg(), exParams)
	s.RecordCancelled()
	a, b := s.Snapshot(), s.Snapshot()
	if a != b || a.FrictionExchanges != 1 || s.Q.FrictionExchanges != 0 {
		t.Errorf("a=%+v b=%+v state=%+v", a, b, s.Q)
	}
}

func TestQualityInvariants(t *testing.T) {
	var s SessionStats
	steps := []func(){
		func() { s.StartExchange(msg(), exParams) },
		func() { s.RecordToolResult(errResult(3), exParams) },
		func() { s.StartExchange(msg(negative), exParams) },
		func() { s.RecordCancelled() },
		func() { s.StartExchange(nil, exParams) },
		func() { s.RecordInterrupt() },
		func() { s.StartExchange(msg(correction), exParams) },
	}
	for i := 0; i < 200; i++ {
		steps[(i*7+i/3)%len(steps)]()
		q := s.Snapshot()
		if q.FrictionExchanges > q.AnalyzedExchanges || q.AnalyzedExchanges > q.Exchanges ||
			q.FxPrimaryPushback+q.FxPrimaryToolLoop+q.FxPrimaryInterrupt != q.FrictionExchanges ||
			q.FirstFrictionExchange > q.AnalyzedExchanges {
			t.Fatalf("step %d: invariant broken: %+v", i, q)
		}
	}
}
