package copilotchat

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/dirpath"
)

// MaxLineBytes bounds one patch line; longer lines are skipped without decoding.
const MaxLineBytes = 8 << 20

const (
	maxFieldBytes = 1024
	maxModelLen   = 128
	maxToolLen    = 64
	maxIndex      = 1 << 24
	// maxEpochMs is 2100-01-01; larger timestamps are treated as absent.
	maxEpochMs = 4102444800000
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Parse interprets one patch line and reports zero or more events through emit. It is
// stateless, never panics and never returns an error; unusable input is reported as
// Skipped with a fixed reason code. ex may be nil, in which case no text is decoded.
func Parse(line []byte, ex analyzer.Extractor, emit func(Event)) {
	defer func() {
		if recover() != nil {
			emit(Skipped{Reason: ReasonInternal})
		}
	}()

	if len(line) > MaxLineBytes {
		emit(Skipped{Reason: ReasonLineTooLong})
		return
	}
	line = bytes.TrimSpace(bytes.TrimPrefix(line, utf8BOM))
	if len(line) == 0 {
		emit(Skipped{Reason: ReasonEmpty})
		return
	}

	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		var typeErr *json.UnmarshalTypeError
		if asTypeErr(err, &typeErr) {
			emit(Skipped{Reason: ReasonBadShape})
			return
		}
		emit(Skipped{Reason: ReasonInvalidJSON})
		return
	}

	if !env.Kind.ok {
		emit(Skipped{Reason: ReasonBadShape})
		return
	}
	switch env.Kind.n {
	case 0:
		parseSnapshot(env.V, ex, emit)
	case 1:
		parseSet(&env, ex, emit)
	case 2:
		if len(env.K) == 3 && isStr(env.K[0], "requests") && isStr(env.K[2], "response") {
			parseResponse(&env, ex, emit)
			return
		}
		parseAppend(&env, ex, emit)
	default:
		emit(Skipped{Reason: ReasonBadShape})
	}
}

func asTypeErr(err error, target **json.UnmarshalTypeError) bool {
	te, ok := err.(*json.UnmarshalTypeError)
	if ok {
		*target = te
	}
	return ok
}

type envelope struct {
	Kind num               `json:"kind"`
	K    []json.RawMessage `json:"k"`
	I    num               `json:"i"`
	V    rawJSON           `json:"v"`
}

func parseSnapshot(v []byte, ex analyzer.Extractor, emit func(Event)) {
	if len(v) == 0 || v[0] != '{' {
		emit(Skipped{Reason: ReasonBadShape})
		return
	}
	var snap struct {
		Requests []rawJSON `json:"requests"`
	}
	_ = json.Unmarshal(v, &snap)
	if len(snap.Requests) == 0 {
		emit(Ignored{Type: "snapshot_meta"})
		return
	}
	for _, r := range snap.Requests {
		emit(decodeRequest(r, -1, ex))
	}
}

func parseAppend(env *envelope, ex analyzer.Extractor, emit func(Event)) {
	if !pathIs(env.K, "requests") {
		emit(Ignored{Type: "other_path"})
		return
	}
	if len(env.V) == 0 || env.V[0] != '[' {
		emit(Skipped{Reason: ReasonBadShape})
		return
	}
	base := -1
	if env.I.ok && env.I.n >= 0 && env.I.n <= maxIndex {
		base = int(env.I.n)
	}
	var items []rawJSON
	_ = json.Unmarshal(env.V, &items)
	if len(items) == 0 {
		emit(Ignored{Type: "empty_append"})
		return
	}
	for j, r := range items {
		at := -1
		if base >= 0 {
			at = base + j
		}
		emit(decodeRequest(r, at, ex))
	}
}

func parseSet(env *envelope, ex analyzer.Extractor, emit func(Event)) {
	k := env.K
	if len(k) != 3 || !isStr(k[0], "requests") {
		emit(Ignored{Type: "other_path"})
		return
	}
	var field string
	switch {
	case isStr(k[2], "modelState"):
		field = "modelState"
	case isStr(k[2], "result"):
		field = "result"
	case isStr(k[2], "copilotCredits"), isStr(k[2], "promptTokens"), isStr(k[2], "completionTokens"):
		field = "metric"
	case isStr(k[2], "contentReferences"), isStr(k[2], "editedFileEvents"):
		idx, err := strconv.Atoi(string(k[1]))
		if err != nil || idx < 0 || idx > maxIndex {
			emit(Skipped{Reason: ReasonBadShape})
			return
		}
		ev := RequestFiles{Index: idx}
		if isStr(k[2], "contentReferences") {
			ev.N = appendRefFiles(&ev.Files, ev.N, env.V)
		} else {
			ev.N = appendEditedFiles(&ev.Files, ev.N, env.V)
		}
		if ev.N == 0 {
			emit(Ignored{Type: "other_path"})
			return
		}
		emit(ev)
		return
	case isStr(k[2], "response"):
		parseResponse(env, ex, emit)
		return
	default:
		emit(Ignored{Type: "other_path"})
		return
	}
	idx, err := strconv.Atoi(string(k[1]))
	if err != nil || idx < 0 || idx > maxIndex {
		emit(Skipped{Reason: ReasonBadShape})
		return
	}
	if field == "metric" {
		var v fnum
		_ = json.Unmarshal(env.V, &v)
		if !v.ok || v.f < 0 || v.f > MaxMetricValue {
			emit(Skipped{Reason: ReasonBadShape})
			return
		}
		kind := MetricCredits
		switch {
		case isStr(k[2], "promptTokens"):
			kind = MetricPrompt
		case isStr(k[2], "completionTokens"):
			kind = MetricCompletion
		}
		emit(RequestMetric{Index: idx, Kind: kind, Value: v.f})
		return
	}
	if field == "modelState" {
		var ms modelState
		_ = json.Unmarshal(env.V, &ms)
		if !ms.ok() {
			emit(Skipped{Reason: ReasonBadShape})
			return
		}
		emit(RequestState{Index: idx, State: int(ms.Value.n), CompletedAt: msTime(ms.CompletedAt)})
		return
	}
	model, tools, n, rounds := extractResult(env.V)
	emit(RequestTools{Index: idx, Model: model, Tools: tools, ToolCount: n, Rounds: rounds})
}

// request declares only the fields we use; message is kept raw so its text is decoded
// only on demand.
type request struct {
	RequestID  lenStr     `json:"requestId"`
	Timestamp  num        `json:"timestamp"`
	ModelID    lenStr     `json:"modelId"`
	Message    rawJSON    `json:"message"`
	ModelState modelState `json:"modelState"`
	Hidden     lenBool    `json:"hiddenFromTranscript"`
	Credits    fnum       `json:"copilotCredits"`
	Prompt     fnum       `json:"promptTokens"`
	Completion fnum       `json:"completionTokens"`
	Result     rawJSON    `json:"result"`
	Response   rawJSON    `json:"response"`

	// Files the request touched: editedFileEvents and contentReferences are read for the folder part of their uri only.
	EditedFiles rawJSON `json:"editedFileEvents"`
	ContentRefs rawJSON `json:"contentReferences"`
}

func decodeRequest(raw []byte, at int, ex analyzer.Extractor) RequestStart {
	rs := RequestStart{At: at}
	if len(raw) == 0 || raw[0] != '{' {
		return rs
	}
	var r request
	_ = json.Unmarshal(raw, &r)

	rs.RequestID = validIdent(string(r.RequestID), maxFieldBytes)
	rs.Timestamp = msTime(r.Timestamp)
	rs.Hidden = bool(r.Hidden)
	rs.Model = cleanModel(string(r.ModelID))
	if r.ModelState.ok() {
		rs.HasState = true
		rs.State = int(r.ModelState.Value.n)
		rs.CompletedAt = msTime(r.ModelState.CompletedAt)
	}
	plausible := func(v fnum) bool { return v.ok && v.f >= 0 && v.f <= MaxMetricValue }
	if plausible(r.Credits) {
		rs.HasCredits, rs.Credits = true, r.Credits.f
	}
	if plausible(r.Prompt) {
		rs.HasPrompt, rs.PromptTokens = true, int64(r.Prompt.f)
	}
	if plausible(r.Completion) {
		rs.HasCompletion, rs.CompletionTokens = true, int64(r.Completion.f)
	}
	if len(r.Result) > 0 {
		model, tools, n, rounds := extractResult(r.Result)
		if model != "" {
			rs.Model = model
		}
		rs.Tools, rs.ToolCount, rs.Rounds = tools, n, rounds
	}
	if ex != nil && !rs.Hidden {
		rs.Features, rs.Analyzed = userFeatures(ex, r.Message)
	}
	rs.Calls, rs.CallCount = extractToolCalls(r.Response, ex != nil)
	rs.FileCount = appendResponseFiles(&rs.Files, 0, r.Response)
	rs.FileCount = appendEditedFiles(&rs.Files, rs.FileCount, r.EditedFiles)
	rs.FileCount = appendRefFiles(&rs.Files, rs.FileCount, r.ContentRefs)
	return rs
}

// parseResponse handles a set or append of a request's response parts (k = requests/<i>/response).
// The analyzer is only used to decide whether error text may be fingerprinted.
func parseResponse(env *envelope, ex analyzer.Extractor, emit func(Event)) {
	idx, err := strconv.Atoi(string(env.K[1]))
	if err != nil || idx < 0 || idx > maxIndex {
		emit(Skipped{Reason: ReasonBadShape})
		return
	}
	ev := RequestToolCalls{Index: idx}
	ev.Calls, ev.N = extractToolCalls(env.V, ex != nil)
	files := RequestFiles{Index: idx}
	files.N = appendResponseFiles(&files.Files, 0, env.V)
	if ev.N == 0 && files.N == 0 {
		emit(Ignored{Type: "other_path"}) // response parts without completed tool calls or files
		return
	}
	if ev.N > 0 {
		emit(ev)
	}
	if files.N > 0 {
		emit(files)
	}
}

// uriObj reads only the scheme and the path of a VS Code URI object; anything else (a string, a symbol with a range) is ignored.
type uriObj struct {
	scheme, fsPath, path string
}

func (u *uriObj) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] != '{' {
		return nil
	}
	var raw struct {
		Scheme lenStr `json:"scheme"`
		FsPath lenStr `json:"fsPath"`
		Path   lenStr `json:"path"`
	}
	_ = json.Unmarshal(b, &raw)
	u.scheme, u.fsPath, u.path = string(raw.Scheme), string(raw.FsPath), string(raw.Path)
	return nil
}

var windowsURIPath = regexp.MustCompile(`^/[A-Za-z]:/`)

// file returns the local file path of a file: URI; "" for other schemes (remote workspaces), relative or missing paths.
func (u uriObj) file() string {
	if u.scheme != "" && u.scheme != "file" {
		return ""
	}
	p := u.fsPath
	if p == "" {
		p = u.path
		if windowsURIPath.MatchString(p) {
			p = p[1:]
		}
	}
	return p
}

// addFile appends the folder of uri's file (deduplicated within the event); n is the new count.
func addFile(dst *[MaxFiles]FileRef, n int, u uriObj, edit bool) int {
	path := u.file()
	dir := dirpath.Dir(path)
	if dir == "" || n >= MaxFiles {
		return n
	}
	h := dirpath.Hash(path)
	for i := 0; i < n; i++ {
		if dst[i].Hash == h && dst[i].Edit == edit {
			return n
		}
	}
	dst[n] = FileRef{Dir: dir, Hash: h, Edit: edit}
	return n + 1
}

// appendResponseFiles reads only kind, uri, isEdit and inlineReference of the response parts: text edits and edit code blocks
// are edits, code blocks without isEdit and inline references are reads. Everything else in a part (text, edits, messages) is skipped.
func appendResponseFiles(dst *[MaxFiles]FileRef, n int, raw []byte) int {
	if len(raw) == 0 || raw[0] != '[' {
		return n
	}
	var parts []struct {
		Kind            lenStr  `json:"kind"`
		URI             uriObj  `json:"uri"`
		IsEdit          lenBool `json:"isEdit"`
		InlineReference uriObj  `json:"inlineReference"`
	}
	_ = json.Unmarshal(raw, &parts)
	for i := range parts {
		p := &parts[i]
		switch p.Kind {
		case "textEditGroup":
			n = addFile(dst, n, p.URI, true)
		case "codeblockUri":
			n = addFile(dst, n, p.URI, bool(p.IsEdit))
		case "inlineReference":
			n = addFile(dst, n, p.InlineReference, false)
		}
	}
	return n
}

// appendEditedFiles reads the uri of editedFileEvents entries.
func appendEditedFiles(dst *[MaxFiles]FileRef, n int, raw []byte) int {
	if len(raw) == 0 || raw[0] != '[' {
		return n
	}
	var evs []struct {
		URI uriObj `json:"uri"`
	}
	_ = json.Unmarshal(raw, &evs)
	for i := range evs {
		n = addFile(dst, n, evs[i].URI, true)
	}
	return n
}

// appendRefFiles reads the reference of contentReferences entries (files given to the model as context).
func appendRefFiles(dst *[MaxFiles]FileRef, n int, raw []byte) int {
	if len(raw) == 0 || raw[0] != '[' {
		return n
	}
	var refs []struct {
		Reference uriObj `json:"reference"`
	}
	_ = json.Unmarshal(raw, &refs)
	for i := range refs {
		n = addFile(dst, n, refs[i].Reference, false)
	}
	return n
}

// extractToolCalls reads only the identifying and outcome fields of tool invocation parts.
// invocationMessage, pastTenseMessage, results and terminal output are skipped by the decoder.
func extractToolCalls(raw []byte, fingerprint bool) (calls [MaxToolCalls]ToolCall, n int) {
	if len(raw) == 0 || raw[0] != '[' {
		return calls, 0
	}
	var parts []struct {
		Kind          lenStr  `json:"kind"`
		ToolCallID    lenStr  `json:"toolCallId"`
		ToolID        lenStr  `json:"toolId"`
		IsComplete    lenBool `json:"isComplete"`
		ResultError   rawJSON `json:"resultError"`
		ResultDetails struct {
			IsError lenBool `json:"isError"`
		} `json:"resultDetails"`
		ToolSpecificData struct {
			TerminalCommandState struct {
				ExitCode num `json:"exitCode"`
			} `json:"terminalCommandState"`
		} `json:"toolSpecificData"`
	}
	// Element type errors are ignored on purpose; valid elements are still decoded.
	_ = json.Unmarshal(raw, &parts)
	for i := range parts {
		p := &parts[i]
		if p.Kind != "toolInvocationSerialized" || !bool(p.IsComplete) || p.ToolCallID == "" {
			continue
		}
		if n >= MaxToolCalls {
			break
		}
		c := ToolCall{IDHash: fnv64(string(p.ToolCallID)), ToolID: validIdent(string(p.ToolID), maxToolLen)}
		re := bytes.TrimSpace(p.ResultError)
		errText := len(re) > 2 && re[0] == '"'
		c.IsError = bool(p.ResultDetails.IsError) || errText || string(re) == "true"
		if ec := p.ToolSpecificData.TerminalCommandState.ExitCode; ec.ok && ec.n >= -1<<31 && ec.n < 1<<31 {
			c.HasExit, c.ExitCode = true, int(ec.n)
		}
		if c.IsError && fingerprint {
			switch {
			case errText:
				c.ErrorHash = analyzer.ErrorFingerprint(re)
			case c.HasExit:
				c.ErrorHash = fnv64(c.ToolID + "#" + strconv.Itoa(c.ExitCode))
			default:
				c.ErrorHash = fnv64(c.ToolID + "#error")
			}
		}
		calls[n] = c
		n++
	}
	return calls, n
}

func fnv64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// userFeatures decodes message.text only for the duration of the call; the text is not
// stored or returned.
func userFeatures(ex analyzer.Extractor, msg []byte) (analyzer.UserFeatures, bool) {
	if len(msg) == 0 || msg[0] != '{' {
		return analyzer.UserFeatures{}, false
	}
	var m struct {
		Text strVal `json:"text"`
	}
	_ = json.Unmarshal(msg, &m)
	if !m.Text.ok {
		return analyzer.UserFeatures{}, false
	}
	return ex.UserFeatures(m.Text.s), true
}

// extractResult reads only the resolved model, tool names and the number of rounds of a result value.
func extractResult(raw []byte) (model string, tools [MaxTools]string, n, rounds int) {
	if len(raw) == 0 || raw[0] != '{' {
		return
	}
	var res struct {
		Metadata struct {
			ResolvedModel lenStr `json:"resolvedModel"`
			Rounds        []struct {
				ToolCalls []struct {
					Name lenStr `json:"name"`
				} `json:"toolCalls"`
			} `json:"toolCallRounds"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &res)
	model = cleanModel(string(res.Metadata.ResolvedModel))
	rounds = min(len(res.Metadata.Rounds), MaxRounds)
	for _, round := range res.Metadata.Rounds {
		for _, tc := range round.ToolCalls {
			if n >= MaxTools {
				return
			}
			if name := validIdent(string(tc.Name), maxToolLen); name != "" {
				tools[n] = name
				n++
			}
		}
	}
	return
}

type modelState struct {
	Value       num
	CompletedAt num
	isObj       bool
}

func (m *modelState) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] != '{' {
		return nil
	}
	var raw struct {
		Value       num `json:"value"`
		CompletedAt num `json:"completedAt"`
	}
	_ = json.Unmarshal(b, &raw)
	m.Value, m.CompletedAt, m.isObj = raw.Value, raw.CompletedAt, true
	return nil
}

func (m *modelState) ok() bool {
	return m.isObj && m.Value.ok && m.Value.n >= 0 && m.Value.n <= maxKnownStateValue
}

func msTime(n num) time.Time {
	if !n.ok || n.n <= 0 || n.n > maxEpochMs {
		return time.Time{}
	}
	return time.UnixMilli(n.n).UTC()
}

// cleanModel keeps the part after the last "/" if it is a short model identifier.
func cleanModel(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	if s == "" || len(s) > maxModelLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !identByte(c) && c != ':' && c != '+' {
			return ""
		}
	}
	return s
}

// validIdent returns s if it consists only of [A-Za-z0-9._-] and fits the limit, else "".
func validIdent(s string, limit int) string {
	if s == "" || len(s) > limit {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if !identByte(s[i]) {
			return ""
		}
	}
	return s
}

func identByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
}

func isStr(raw json.RawMessage, want string) bool {
	return len(raw) == len(want)+2 && raw[0] == '"' && string(raw[1:len(raw)-1]) == want
}

func pathIs(k []json.RawMessage, want string) bool {
	return len(k) == 1 && isStr(k[0], want)
}

// lenStr ignores non-string values and oversized strings instead of failing the line.
type lenStr string

func (s *lenStr) UnmarshalJSON(b []byte) error {
	if len(b) < 2 || b[0] != '"' || len(b)-2 > maxFieldBytes {
		return nil
	}
	var v string
	if json.Unmarshal(b, &v) == nil {
		*s = lenStr(v)
	}
	return nil
}

// strVal records whether the value was a JSON string.
type strVal struct {
	s  string
	ok bool
}

func (v *strVal) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		v.ok = json.Unmarshal(b, &v.s) == nil
	}
	return nil
}

// lenBool ignores non-boolean values.
type lenBool bool

func (v *lenBool) UnmarshalJSON(b []byte) error {
	*v = string(b) == "true"
	return nil
}

// num is a lenient integer: non-numeric values leave ok false instead of failing the line.
// fnum is a finite JSON number kept as a float (credits are fractional); anything else leaves ok false.
type fnum struct {
	f  float64
	ok bool
}

func (v *fnum) UnmarshalJSON(b []byte) error {
	if f, err := strconv.ParseFloat(string(b), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		v.f, v.ok = f, true
	}
	return nil
}

type num struct {
	n  int64
	ok bool
}

func (v *num) UnmarshalJSON(b []byte) error {
	if n, err := strconv.ParseInt(string(b), 10, 64); err == nil {
		v.n, v.ok = n, true
		return nil
	}
	if f, err := strconv.ParseFloat(string(b), 64); err == nil && f >= 0 && f < math.MaxInt64/2 {
		v.n, v.ok = int64(f), true
	}
	return nil
}

// rawJSON captures a value as a slice of the input without copying or decoding it.
type rawJSON []byte

func (r *rawJSON) UnmarshalJSON(b []byte) error {
	*r = b
	return nil
}
