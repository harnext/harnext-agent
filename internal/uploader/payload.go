package uploader

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/project"
)

// Limits of the ingest contract (contracts/ingest.v1.schema.json plus the rules JSON Schema
// cannot express); rows outside them are never sent.
const (
	// 6: required per-session account_hash (64 lowercase hex of the tool account); the server routes the session to a workspace by it.
	// 5: required folders[]: the session-level turn_count, quality and
	// usage are the sums over the folders and the server checks it. 4: one entry per session with a usage list (model × UTC day ×
	// speed × prompt floor); money is never sent or stored. 3: session quality counters.
	PayloadVersion     = 6
	MaxSessionsPerPost = 200
	maxUsageRows       = 400
	maxFoldersPerSess  = 64
	maxToolRequests    = 1_000_000_000
	maxPromptFloor     = 10_000_000_000
	maxTokenCount      = 100_000_000_000
	maxTurnCount       = 1_000_000
	maxCopilotCredits  = 1e9
	maxSessionDuration = 90 * 24 * time.Hour
	// maxActiveSeconds and activeSlack are the server's limits on active_seconds (the value may exceed the session span by slack).
	maxActiveSeconds   = 7 * 24 * 60 * 60
	activeSlack        = 3600
	maxSessionIDLen    = 200
	maxModelLen        = 100
	maxFolderLen       = 512
	maxQualityCount    = 10_000_000
	maxCoverage        = 1<<5 - 1
	maxDetectorVersion = 1_000_000
)

// resolveProject turns a working directory into the identity that is sent; package-level so tests can replace it.
var resolveProject = project.DefaultResolver().Resolve

var minStartedAt = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

var topics = map[string]bool{
	"feature": true, "bugfix": true, "refactor": true, "test": true, "docs": true,
	"devops": true, "data": true, "explore": true, "other": true,
}

type session struct {
	SessionExternalID string      `json:"session_external_id"`
	ToolName          string      `json:"tool_name"`
	AccountHash       string      `json:"account_hash"`
	Project           string      `json:"project"`
	ProjectPath       *string     `json:"project_path,omitempty"`
	Topic             string      `json:"topic"`
	Quality           wireQuality `json:"quality"`
	TurnCount         int64       `json:"turn_count"`
	StartedAt         string      `json:"started_at"`
	EndedAt           string      `json:"ended_at"`
	// ActiveSeconds is omitted when the session has no timed event; 0 is never sent.
	ActiveSeconds int64 `json:"active_seconds,omitempty"`
	// Usage is the session total (the sum of the folders' usage per key); Folders carries the split.
	Usage   []usageRow `json:"usage"`
	Folders []folder   `json:"folders"`
}

// folder is one folder of the project: its requests, the counters of those requests (numbered inside the folder) and
// their usage. Path is null at the project root; it is never an absolute path (the aggregator reduces it to a folder
// inside the project, aggregate.CollapsePath).
type folder struct {
	Path      *string     `json:"path"`
	TurnCount int64       `json:"turn_count"`
	Quality   wireQuality `json:"quality"`
	Usage     []usageRow  `json:"usage"`
}

type usageRow struct {
	Model                 string `json:"model"`
	Day                   string `json:"day"`
	Speed                 string `json:"speed"`
	PromptFloor           int64  `json:"prompt_floor"`
	InputTokens           int64  `json:"input_tokens"`
	OutputTokens          int64  `json:"output_tokens"`
	CacheReadTokens       int64  `json:"cache_read_tokens"`
	CacheCreationTokens   int64  `json:"cache_creation_tokens"`
	CacheCreation1hTokens int64  `json:"cache_creation_1h_tokens"`
	WebSearchRequests     int64  `json:"web_search_requests"`
	WebFetchRequests      int64  `json:"web_fetch_requests"`
	// Omitted for Claude Code; the server treats absent as null.
	CopilotCredits *float64 `json:"copilot_credits,omitempty"`
	// TokensEstimated is sent only when true (Copilot history rows): the token totals are an estimate and the server scales
	// them to the credits. Absent means measured.
	TokensEstimated bool `json:"tokens_estimated,omitempty"`
}

// wireQuality is the uploaded counter set. first_friction_exchange is null when the session
// had no friction (the outer field shadows the embedded one of the same JSON name).
type wireQuality struct {
	analyzer.Quality
	FirstFrictionExchange *int64 `json:"first_friction_exchange"`
	SignalCoverage        int    `json:"signal_coverage"`
	DetectorVersion       int    `json:"detector_version"`
}

func toWireQuality(q analyzer.Quality, coverage, detector int) (wireQuality, bool) {
	w := wireQuality{Quality: q, SignalCoverage: coverage, DetectorVersion: detector}
	if q.FirstFrictionExchange > 0 {
		v := q.FirstFrictionExchange
		w.FirstFrictionExchange = &v
	}
	counts := []int64{q.Exchanges, q.AnalyzedExchanges, q.FrictionExchanges, q.FxPushback, q.FxToolLoop, q.FxInterrupt,
		q.FxPrimaryPushback, q.FxPrimaryToolLoop, q.FxPrimaryInterrupt, q.SigErrorReport, q.SigCorrection, q.SigRepeat,
		q.SigErrorPaste, q.SigPositiveAck, q.ToolCalls, q.ToolErrors, q.TerminalNonzero, q.RepeatedErrorRuns,
		q.RepeatedErrorCalls, q.Responses, q.CancelledResponses, q.InterruptedTools, q.PermissionDenials, q.FirstFrictionExchange}
	for _, n := range counts {
		if n < 0 || n > maxQualityCount {
			return w, false
		}
	}
	ok := q.FrictionExchanges <= q.AnalyzedExchanges && q.AnalyzedExchanges <= q.Exchanges &&
		q.FxPrimaryPushback+q.FxPrimaryToolLoop+q.FxPrimaryInterrupt == q.FrictionExchanges &&
		q.FxPushback <= q.FrictionExchanges && q.FxToolLoop <= q.FrictionExchanges && q.FxInterrupt <= q.FrictionExchanges &&
		q.FirstFrictionExchange <= q.AnalyzedExchanges && (q.FirstFrictionExchange == 0) == (q.FrictionExchanges == 0) &&
		q.ToolErrors <= q.ToolCalls && q.CancelledResponses <= q.Responses &&
		coverage >= 0 && coverage <= maxCoverage && detector >= 0 && detector <= maxDetectorVersion
	return w, ok
}

// prepared is a valid session with its ledger key and content digest.
type prepared struct {
	key    string
	route  string // the Harnext account (token) the session is uploaded with
	digest string
	body   []byte
}

// rowKey identifies an uploaded session (all of its usage rows travel together).
func rowKey(r aggregate.Row) string {
	return r.Tool + "\x00" + r.SessionID
}

var speeds = map[string]bool{"standard": true, "fast": true, "unknown": true}

// toUsage maps one aggregate row's usage key and totals; ok is false when it would break the contract.
func toUsage(r aggregate.Row) (usageRow, bool) {
	u := usageRow{
		Model: r.Model, Day: r.Day, Speed: r.Speed, PromptFloor: r.PromptFloor,
		InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CacheReadTokens: r.CacheReadTokens,
		CacheCreationTokens: r.CacheCreationTokens, CacheCreation1hTokens: r.CacheCreation1hTokens,
		WebSearchRequests: r.WebSearchRequests, WebFetchRequests: r.WebFetchRequests,
		TokensEstimated: r.TokensEstimated,
	}
	if u.TokensEstimated && (r.Tool != aggregate.ToolVSCodeCopilot || r.CopilotCredits == nil) {
		return usageRow{}, false
	}
	if len(u.Model) < 1 || len(u.Model) > maxModelLen || !speeds[u.Speed] || u.PromptFloor < -1 || u.PromptFloor > maxPromptFloor {
		return usageRow{}, false
	}
	if _, err := time.Parse(time.DateOnly, u.Day); err != nil {
		return usageRow{}, false
	}
	for _, n := range []int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens, u.CacheCreation1hTokens} {
		if n < 0 || n > maxTokenCount {
			return usageRow{}, false
		}
	}
	if u.CacheCreation1hTokens > u.CacheCreationTokens || u.WebSearchRequests < 0 || u.WebSearchRequests > maxToolRequests ||
		u.WebFetchRequests < 0 || u.WebFetchRequests > maxToolRequests {
		return usageRow{}, false
	}
	if c := r.CopilotCredits; c != nil {
		if r.Tool != aggregate.ToolVSCodeCopilot || math.IsNaN(*c) || math.IsInf(*c, 0) || *c < 0 || *c > maxCopilotCredits {
			return usageRow{}, false
		}
		v := math.Round(*c*1e6) / 1e6
		u.CopilotCredits = &v
	}
	return u, true
}

// validFolderPath: the folder is relative to the project, uses forward slashes and never holds an empty or dot segment.
func validFolderPath(p string) bool {
	if p == "" || len(p) > maxFolderLen || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.Contains(p, `\`) ||
		(len(p) >= 2 && p[1] == ':') {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func usageKey(u usageRow) string {
	return u.Model + "\x00" + u.Day + "\x00" + u.Speed + "\x00" + strconv.FormatInt(u.PromptFloor, 10)
}

// addUsage folds o into t (same usage key): token and request sums, Copilot credits and the estimate flag.
func addUsage(t *usageRow, o usageRow) {
	t.InputTokens += o.InputTokens
	t.OutputTokens += o.OutputTokens
	t.CacheReadTokens += o.CacheReadTokens
	t.CacheCreationTokens += o.CacheCreationTokens
	t.CacheCreation1hTokens += o.CacheCreation1hTokens
	t.WebSearchRequests += o.WebSearchRequests
	t.WebFetchRequests += o.WebFetchRequests
	t.TokensEstimated = t.TokensEstimated || o.TokensEstimated
	if o.CopilotCredits != nil {
		sum := *o.CopilotCredits
		if t.CopilotCredits != nil {
			sum += *t.CopilotCredits
		}
		v := math.Round(sum*1e6) / 1e6
		t.CopilotCredits = &v
	}
}

// toSession maps the rows of one session (same tool and id, session fields repeated on each, one row per folder and usage
// key) to the wire shape; ok is false when any part would break the contract. The session-level turn_count, quality and
// usage are the sums over the folders; the server verifies this.
func toSession(rows []aggregate.Row) (session, bool) {
	r := rows[0]
	s := session{
		SessionExternalID: r.SessionID,
		ToolName:          r.Tool,
		Topic:             r.Topic,
	}
	// Only the project identity leaves the machine: never the full path, the user name or the branch.
	id := resolveProject(r.Cwd)
	s.Project = id.Project
	if !topics[s.Topic] {
		s.Topic = "other"
	}
	if (r.Tool != aggregate.ToolClaudeCode && r.Tool != aggregate.ToolVSCodeCopilot) || len(s.SessionExternalID) < 1 || len(s.SessionExternalID) > maxSessionIDLen ||
		len(s.Project) < 1 || len(s.Project) > maxFolderLen || len(r.Parts) < 1 || len(r.Parts) > maxFoldersPerSess {
		return session{}, false
	}
	q, ok := toWireQuality(r.Quality, r.SignalCoverage, r.DetectorVersion)
	if !ok {
		return session{}, false
	}
	s.Quality = q
	if r.StartedAt.IsZero() || r.EndedAt.Before(r.StartedAt) || r.StartedAt.Before(minStartedAt) ||
		r.EndedAt.Sub(r.StartedAt) > maxSessionDuration {
		return session{}, false
	}
	s.StartedAt = r.StartedAt.UTC().Format(time.RFC3339Nano)
	s.EndedAt = r.EndedAt.UTC().Format(time.RFC3339Nano)
	s.ActiveSeconds = max(0, min(r.ActiveSeconds, maxActiveSeconds, int64(r.EndedAt.Sub(r.StartedAt)/time.Second)+activeSlack))

	// Folders in the order of Parts (sorted by path); every folder of the session is sent, even one without usage.
	byPath := make(map[string]int, len(r.Parts))
	for _, p := range r.Parts {
		if _, dup := byPath[p.Path]; dup || (p.Path != "" && !validFolderPath(p.Path)) || p.Turns < 0 {
			return session{}, false
		}
		fq, ok := toWireQuality(p.Quality, r.SignalCoverage, r.DetectorVersion)
		if !ok {
			return session{}, false
		}
		f := folder{TurnCount: p.Turns, Quality: fq, Usage: []usageRow{}}
		if p.Path != "" {
			path := p.Path
			f.Path = &path
		}
		byPath[p.Path] = len(s.Folders)
		s.Folders = append(s.Folders, f)
		s.TurnCount += p.Turns
	}
	if s.TurnCount < 0 || s.TurnCount > maxTurnCount {
		return session{}, false
	}
	main, ok := byPath[r.MainFolder]
	if !ok {
		return session{}, false
	}
	if p := s.Folders[main].Path; p != nil {
		path := *p
		s.ProjectPath = &path
	}

	total := map[string]int{}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		u, ok := toUsage(row)
		fi, known := byPath[row.Folder]
		k := row.Folder + "\x00" + usageKey(u)
		if !ok || !known || seen[k] {
			return session{}, false
		}
		seen[k] = true
		f := &s.Folders[fi]
		f.Usage = append(f.Usage, u)
		if i, ok := total[usageKey(u)]; ok {
			addUsage(&s.Usage[i], u)
			continue
		}
		total[usageKey(u)] = len(s.Usage)
		s.Usage = append(s.Usage, u)
	}
	for _, f := range s.Folders {
		if len(f.Usage) > maxUsageRows {
			return session{}, false
		}
	}
	if len(s.Usage) < 1 || len(s.Usage) > maxUsageRows {
		return session{}, false
	}
	return s, true
}

// validAccountHash: 64 lowercase hex characters.
func validAccountHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		if c := h[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// groupRows collects rows by session (tool and id) in order of first appearance.
func groupRows(rows []aggregate.Row) [][]aggregate.Row {
	idx := map[string]int{}
	var out [][]aggregate.Row
	for _, r := range rows {
		k := rowKey(r)
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], r)
	}
	return out
}

// prepare returns the valid sessions in order of first appearance, the number of sessions dropped as invalid and the number
// the gate held back. The gate decides once per session on its first row; its AccountHash goes into the body and its RouteID
// (the Harnext account whose token uploads it) into the ledger key, so a session routed elsewhere is sent again. The digest covers configVersion too, so sessions are
// re-sent when the configuration that produced their counters changes. A nil gate sends nothing.
func prepare(rows []aggregate.Row, configVersion int64, gate func(aggregate.Row) Decision) (out []prepared, invalid, skipped int) {
	for _, group := range groupRows(rows) {
		r := group[0]
		var d Decision
		if gate != nil {
			d = gate(r)
		}
		if !d.Send {
			skipped++
			continue
		}
		s, ok := toSession(group)
		if !ok || !validAccountHash(d.AccountHash) {
			invalid++
			continue
		}
		s.AccountHash = d.AccountHash
		body, err := json.Marshal(s)
		if err != nil {
			invalid++
			continue
		}
		h := sha256.New()
		h.Write(body)
		var v [8]byte
		binary.BigEndian.PutUint64(v[:], uint64(max(configVersion, 0)))
		h.Write(v[:])
		out = append(out, prepared{key: rowKey(r) + "\x00" + d.RouteID, route: d.RouteID, digest: hex.EncodeToString(h.Sum(nil)[:16]), body: body})
	}
	return out, invalid, skipped
}
