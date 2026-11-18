package aggregate

// Copilot rows split by folder: the same shape as Claude Code, the same method.
//
// Every chat request belongs to one folder (most edited files, else most referenced, else the workspace folder). What the
// request cost follows it:
//   - credits: Copilot's own credits per request are exact (history rows, sessions without OTel usage);
//   - tokens of the OTel era: a trace is tied to the LAST chat request that started before the trace did (request times are
//     seconds, trace times milliseconds; the request intervals do not overlap in 94.5% of cases). Traces that find no
//     request go to the conversation's main folder. The credits of a request's traces must equal the credits in the chat
//     file; the share that does is the self-check (CopilotCounters.TraceChecks/TraceMatches);
//   - counters (friction, tool calls, ...): booked to the request that opened the exchange when they arrived (see
//     copilotSession.addQ), so the folders add up to SessionStats.Snapshot().

import (
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
)

// creditMatchNano is the tolerance of the credit self-check: one millionth of a credit.
const creditMatchNano = 1000

// cpAcc is the tokens and credits (nano AIU) of one (folder, usage key).
type cpAcc struct {
	Totals
	nano int64
}

func (c *cpAcc) add(t Totals, nano int64) {
	c.InputTokens += t.InputTokens
	c.OutputTokens += t.OutputTokens
	c.CacheReadTokens += t.CacheReadTokens
	c.CacheCreationTokens += t.CacheCreationTokens
	c.CacheCreation1hTokens += t.CacheCreation1hTokens
	c.nano = satAdd(c.nano, nano, maxNanoAIU)
}

// requestFolders decides the folder of every chat request and books the session's counters to them. It returns the folder
// per request index ("" for requests that do not count), the folders (counters only; turns are set by the caller) and the
// conversation's main folder: where unbound traces and unattributed counters go.
func (a *CopilotAggregator) requestFolders(s *copilotSession) (reqFolder []string, folders map[string]*FolderState, main string) {
	folders = map[string]*FolderState{}
	reqFolder = make([]string, len(s.requests))
	var virtual analyzer.Quality
	if s.analysis != nil {
		virtual = s.analysis.Stats.VirtualFold()
	}
	count := map[string]int64{}
	virtualPlaced := false
	for i := range s.requests {
		r := &s.requests[i]
		if r.Hash == 0 {
			continue
		}
		name := folderTarget(folders, RequestFolder(a.resolve, s.cwd, r.Dirs))
		reqFolder[i] = name
		q := unpackQ(r.Q)
		if r.Hash == s.curHash {
			q.Add(virtual)
			virtualPlaced = true
		}
		f := folders[name]
		if f == nil {
			f = &FolderState{}
			folders[name] = f
		}
		f.merge(Request{Q: q})
		if !r.Hidden {
			count[name]++
		}
	}
	main = CwdFolder(a.resolve, s.cwd)
	if len(count) > 0 {
		names := make([]string, 0, len(count))
		for n := range count {
			names = append(names, n)
		}
		slices.Sort(names)
		best := names[0]
		for _, n := range names[1:] {
			if count[n] > count[best] {
				best = n
			}
		}
		main = best
	}
	main = folderTarget(folders, main)
	orphan := s.preQ
	if !virtualPlaced {
		orphan.Add(virtual)
	}
	if f := ensureFolder(folders, main); !orphan.IsZero() {
		f.merge(Request{Q: orphan})
	}
	return reqFolder, folders, main
}

func ensureFolder(folders map[string]*FolderState, name string) *FolderState {
	f := folders[name]
	if f == nil {
		f = &FolderState{}
		folders[name] = f
	}
	return f
}

// Rows returns one row per (conversation, folder, usage key), sorted by session, folder and usage key. A session without any
// valid span time keeps a zero StartedAt; the upload layer rejects such rows.
func (a *CopilotAggregator) Rows() []Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.counters.TracesBound, a.counters.TracesUnbound, a.counters.TraceChecks, a.counters.TraceMatches = 0, 0, 0, 0
	var rows []Row
	for id, s := range a.sessions {
		quality, detector, topic := a.derive(s)
		reqFolder, folders, main := a.requestFolders(s)
		if len(s.models) == 0 {
			rows = append(rows, a.historyRows(id, s, quality, detector, topic, reqFolder, folders, main)...)
			continue
		}
		rows = append(rows, a.otelRows(id, s, quality, detector, topic, reqFolder, folders, main)...)
	}
	return finishRows(rows)
}

// partsFor lists the folders with their request counts; the main folder is the one with the most requests among those that
// carry usage rows, so the session counts where it has data.
func partsFor(folders map[string]*FolderState, turns map[string]int64, usage map[string]bool) (parts []FolderPart, main string) {
	for name, f := range folders {
		parts = append(parts, FolderPart{Path: name, Turns: turns[name], Quality: f.Q})
	}
	slices.SortFunc(parts, func(x, y FolderPart) int { return strings.Compare(x.Path, y.Path) })
	return parts, mainFolderOf(parts, usage)
}

// otelRows builds the rows of a conversation that has OTel usage.
func (a *CopilotAggregator) otelRows(id string, s *copilotSession, quality analyzer.Quality, detector int, topic string,
	reqFolder []string, folders map[string]*FolderState, main string) []Row {
	// Requests by start time (seconds), later index first among equals: a trace belongs to the last request that began before it.
	type ref struct {
		t   int64
		idx int
	}
	var refs []ref
	for i, r := range s.requests {
		if r.Hash != 0 && r.Time > 0 {
			refs = append(refs, ref{r.Time, i})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].t != refs[j].t {
			return refs[i].t < refs[j].t
		}
		return refs[i].idx < refs[j].idx
	})

	usage := map[string]map[string]*cpAcc{} // folder -> usage key -> tokens and credits
	acc := func(folder, key string) *cpAcc {
		m := usage[folder]
		if m == nil {
			m = map[string]*cpAcc{}
			usage[folder] = m
		}
		c := m[key]
		if c == nil {
			c = &cpAcc{}
			m[key] = c
		}
		return c
	}
	turns := map[string]int64{}
	recorded := map[string]*cpAcc{} // per usage key: what the trace records add up to
	reqNano := map[int]int64{}
	reqBound := map[int]bool{}

	hashes := make([]uint64, 0, len(s.traceUsage))
	for h := range s.traceUsage {
		hashes = append(hashes, h)
	}
	slices.Sort(hashes)
	for _, h := range hashes {
		t := s.traceUsage[h]
		folder, bound := main, false
		if t.Start > 0 {
			sec := t.Start / 1000
			if k := sort.Search(len(refs), func(i int) bool { return refs[i].t > sec }) - 1; k >= 0 {
				idx := refs[k].idx
				folder, bound = reqFolder[idx], true
				reqBound[idx] = true
				for _, e := range t.Entries {
					reqNano[idx] = satAdd(reqNano[idx], e.NanoAIU, maxNanoAIU)
				}
			}
		}
		if bound {
			a.counters.TracesBound++
		} else {
			a.counters.TracesUnbound++
		}
		turns[folder]++
		ensureFolder(folders, folder)
		for _, e := range t.Entries {
			acc(folder, e.Key).add(e.Totals, e.NanoAIU)
			r := recorded[e.Key]
			if r == nil {
				r = &cpAcc{}
				recorded[e.Key] = r
			}
			r.add(e.Totals, e.NanoAIU)
		}
	}
	// Self-check: the credits of the traces tied to a request equal the credits Copilot wrote to the chat file.
	for idx := range reqBound {
		if want := s.requests[idx].NanoAIU; want > 0 {
			a.counters.TraceChecks++
			if d := reqNano[idx] - want; d >= -creditMatchNano && d <= creditMatchNano {
				a.counters.TraceMatches++
			}
		}
	}
	// What the model totals hold beyond the trace records (spans of traces refused by the trace cap) goes to the main folder.
	for key, m := range s.models {
		rec := recorded[key]
		if rec == nil {
			rec = &cpAcc{}
		}
		rem := Totals{
			InputTokens: max(m.InputTokens-rec.InputTokens, 0), OutputTokens: max(m.OutputTokens-rec.OutputTokens, 0),
			CacheReadTokens: max(m.CacheReadTokens-rec.CacheReadTokens, 0), CacheCreationTokens: max(m.CacheCreationTokens-rec.CacheCreationTokens, 0),
		}
		if remNano := max(m.NanoAIU-rec.nano, 0); rem != (Totals{}) || remNano > 0 {
			acc(main, key).add(rem, remNano)
		}
	}

	hasUsage := map[string]bool{}
	for folder := range usage {
		ensureFolder(folders, folder)
		hasUsage[folder] = true
	}
	parts, mainOut := partsFor(folders, turns, hasUsage)
	active := s.active.Seconds()

	var rows []Row
	for folder, byKey := range usage {
		for key, c := range byKey {
			k, ok := ParseUsageKey(key)
			if !ok {
				continue
			}
			// Copilot has no "no data" state for credits: models that cost nothing really report 0.
			credits := math.Round(float64(c.nano)/1e3) / 1e6
			day := k.Day
			if day == "" {
				day = dayOf(s.startedAt)
			}
			rows = append(rows, Row{
				SessionID: id, Tool: ToolVSCodeCopilot, Model: k.Model,
				Day: day, Speed: k.Speed, PromptFloor: k.PromptFloor,
				CopilotCredits: &credits,
				Totals:         c.Totals,
				Folder:         folder, Parts: parts, MainFolder: mainOut,
				StartedAt: s.startedAt, EndedAt: s.endedAt, ActiveSeconds: active,
				Cwd: s.cwd, ToolVersion: s.toolVersion, Topic: topic,
				Quality: quality, DetectorVersion: detector, SignalCoverage: CopilotCoverage,
			})
		}
	}
	return rows
}

// historyRows builds rows for a conversation that has no OTel usage (it predates OTel, or the OTel file is gone) from the chat
// session file: per (folder, model, day) the request count, Copilot's own credits (exact) and estimated tokens
// (EstimateRequestTokens). Requests without a prompt size use the session's average one; without any, tokens stay 0. The request
// size band is unknown (-1). Requests without a model are skipped. The server scales the estimated tokens to the credits (value
// stays exact).
func (a *CopilotAggregator) historyRows(id string, s *copilotSession, quality analyzer.Quality, detector int, topic string,
	reqFolder []string, folders map[string]*FolderState, main string) []Row {
	if s.chatStart.IsZero() {
		return nil
	}
	// Session averages for requests whose prompt size was not recorded.
	var sumP, sumC, sumN, withP int64
	for _, r := range s.requests {
		if r.Hash != 0 && !r.Hidden && r.Model != "" && r.Prompt > 0 {
			sumP += r.Prompt
			sumC += r.Completion
			sumN += int64(max(r.Rounds, 1))
			withP++
		}
	}
	type group struct {
		folder    string
		nano      int64
		est       TokenEstimate
		estimated bool
	}
	type gkey struct {
		folder string
		UsageKey
	}
	groups := map[gkey]*group{}
	turns := map[string]int64{}
	for i, r := range s.requests {
		if r.Hash == 0 || r.Hidden || r.Model == "" {
			continue
		}
		day := dayOf(s.chatStart)
		if r.Time > 0 {
			day = dayOf(time.Unix(r.Time, 0))
		}
		folder := reqFolder[i]
		k := gkey{folder, UsageKey{Model: r.Model, Day: day, Speed: copilotSpeed, PromptFloor: -1}}
		g := groups[k]
		if g == nil {
			g = &group{folder: folder}
			groups[k] = g
		}
		turns[folder]++
		g.nano = satAdd(g.nano, r.NanoAIU, maxNanoAIU)
		p, c, n := r.Prompt, r.Completion, r.Rounds
		if p <= 0 && withP > 0 {
			p, c, n = sumP/withP, sumC/withP, int(sumN/withP)
		}
		if est := EstimateRequestTokens(p, c, n, DefaultEstimateParams); est != (TokenEstimate{}) {
			g.est.Input = satAdd(g.est.Input, est.Input, maxEstimatedTokens)
			g.est.Output = satAdd(g.est.Output, est.Output, maxEstimatedTokens)
			g.est.CacheRead = satAdd(g.est.CacheRead, est.CacheRead, maxEstimatedTokens)
			g.est.CacheCreation = satAdd(g.est.CacheCreation, est.CacheCreation, maxEstimatedTokens)
			g.estimated = true
		}
	}
	if len(groups) == 0 {
		return nil
	}
	hasUsage := map[string]bool{}
	for _, g := range groups {
		hasUsage[g.folder] = true
		ensureFolder(folders, g.folder)
	}
	parts, mainOut := partsFor(folders, turns, hasUsage)
	active := s.active.Seconds()
	end := s.chatEnd
	if end.Before(s.chatStart) {
		end = s.chatStart
	}
	rows := make([]Row, 0, len(groups))
	for k, g := range groups {
		credits := math.Round(float64(g.nano)/1e3) / 1e6
		rows = append(rows, Row{
			SessionID: id, Tool: ToolVSCodeCopilot, Model: k.Model,
			Day: k.Day, Speed: k.Speed, PromptFloor: k.PromptFloor,
			CopilotCredits: &credits, TokensEstimated: g.estimated,
			Totals: Totals{InputTokens: g.est.Input, OutputTokens: g.est.Output, CacheReadTokens: g.est.CacheRead,
				CacheCreationTokens: g.est.CacheCreation},
			Folder: g.folder, Parts: parts, MainFolder: mainOut,
			StartedAt: s.chatStart, EndedAt: end, ActiveSeconds: active,
			Cwd: s.cwd, Topic: topic,
			Quality: quality, DetectorVersion: detector, SignalCoverage: CopilotCoverage,
		})
	}
	return rows
}
