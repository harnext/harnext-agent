package scan

import (
	"cmp"
	"math"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/project"
)

const (
	maxTokens    = 100_000_000_000
	maxRequests  = 1_000_000_000
	maxCredits   = 1e9
	maxModelLen  = 100
	maxProject   = 128
	maxSessions  = 1_000_000
	periodDays   = 30
	unknownModel = "unknown"
)

// UsageGroup is the total of one (tool, model, speed, prompt floor) key over the period.
type UsageGroup struct {
	Tool        string
	Model       string
	Speed       string
	PromptFloor int64

	InputTokens           int64
	OutputTokens          int64
	CacheReadTokens       int64
	CacheCreationTokens   int64
	CacheCreation1hTokens int64
	WebSearchRequests     int64
	WebFetchRequests      int64

	// CopilotCredits is set only for Copilot groups that have credits.
	CopilotCredits *float64
	// TokensEstimated is true when any row of the group has estimated token counts.
	TokensEstimated bool
}

// TotalTokens is input, output and both cache kinds together.
func (g UsageGroup) TotalTokens() int64 {
	return addSat(addSat(g.InputTokens, g.OutputTokens), addSat(g.CacheReadTokens, g.CacheCreationTokens))
}

// ProjectTotal is the token total of one project identity (git "owner/repo" or a home-shortened folder).
type ProjectTotal struct {
	Project string
	Tokens  int64
}

// Summary is what a scan found; From and To are UTC days (YYYY-MM-DD), both inclusive.
type Summary struct {
	From       string
	To         string
	ActiveDays int
	Sessions   int
	Usage      []UsageGroup   // largest first
	Projects   []ProjectTotal // largest first
}

// DefaultResolve resolves a working directory to its project identity the way the agent's uploader does.
func DefaultResolve() func(cwd string) string {
	r := project.DefaultResolver()
	return func(cwd string) string { return r.Resolve(cwd).Project }
}

// Build folds rows into a Summary of the 30 days ending at now (UTC day). Rows outside that period are ignored. resolve
// maps a working directory to a project name; a result that is still an absolute path is cut to its last element.
func Build(rows []aggregate.Row, now time.Time, resolve func(cwd string) string) Summary {
	from := WindowStart(now, periodDays*24*time.Hour).Format(time.DateOnly)
	to := now.UTC().Format(time.DateOnly)
	s := Summary{From: from, To: to, Usage: []UsageGroup{}, Projects: []ProjectTotal{}}

	type key struct {
		tool, model, speed string
		floor              int64
	}
	groups := map[key]*UsageGroup{}
	days := map[string]struct{}{}
	sessions := map[string]struct{}{}
	byProject := map[string]int64{}
	names := map[string]string{}
	for _, r := range rows {
		if r.Day < from || r.Day > to {
			continue
		}
		days[r.Day] = struct{}{}
		sessions[r.Tool+"\x00"+r.SessionID] = struct{}{}

		k := key{r.Tool, clipModel(r.Model), r.Speed, r.PromptFloor}
		g := groups[k]
		if g == nil {
			g = &UsageGroup{Tool: k.tool, Model: k.model, Speed: k.speed, PromptFloor: k.floor}
			groups[k] = g
		}
		g.InputTokens = addSat(g.InputTokens, r.InputTokens)
		g.OutputTokens = addSat(g.OutputTokens, r.OutputTokens)
		g.CacheReadTokens = addSat(g.CacheReadTokens, r.CacheReadTokens)
		g.CacheCreationTokens = addSat(g.CacheCreationTokens, r.CacheCreationTokens)
		g.CacheCreation1hTokens = addSat(g.CacheCreation1hTokens, r.CacheCreation1hTokens)
		g.WebSearchRequests = min(maxRequests, addSat(g.WebSearchRequests, r.WebSearchRequests))
		g.WebFetchRequests = min(maxRequests, addSat(g.WebFetchRequests, r.WebFetchRequests))
		if r.Tool == aggregate.ToolVSCodeCopilot {
			if c := r.CopilotCredits; c != nil && !math.IsNaN(*c) && !math.IsInf(*c, 0) && *c >= 0 {
				sum := *c
				if g.CopilotCredits != nil {
					sum += *g.CopilotCredits
				}
				v := math.Round(min(sum, maxCredits)*1e6) / 1e6
				g.CopilotCredits = &v
			}
			g.TokensEstimated = g.TokensEstimated || r.TokensEstimated
		}

		name, ok := names[r.Cwd]
		if !ok {
			name = safeProject(resolve(r.Cwd))
			names[r.Cwd] = name
		}
		byProject[name] = addSat(byProject[name], rowTokens(r))
	}

	s.ActiveDays = min(len(days), periodDays+1)
	s.Sessions = min(len(sessions), maxSessions)
	for _, g := range groups {
		g.CacheCreation1hTokens = min(g.CacheCreation1hTokens, g.CacheCreationTokens)
		s.Usage = append(s.Usage, *g)
	}
	slices.SortFunc(s.Usage, compareUsage)
	for p, n := range byProject {
		s.Projects = append(s.Projects, ProjectTotal{Project: p, Tokens: n})
	}
	slices.SortFunc(s.Projects, func(a, b ProjectTotal) int {
		return cmp.Or(cmp.Compare(b.Tokens, a.Tokens), strings.Compare(a.Project, b.Project))
	})
	return s
}

// compareUsage orders groups by tokens, largest first; ties by key so the order is deterministic.
func compareUsage(a, b UsageGroup) int {
	return cmp.Or(
		cmp.Compare(b.TotalTokens(), a.TotalTokens()),
		cmp.Compare(a.Tool, b.Tool), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Speed, b.Speed),
		cmp.Compare(a.PromptFloor, b.PromptFloor),
	)
}

func rowTokens(r aggregate.Row) int64 {
	return addSat(addSat(r.InputTokens, r.OutputTokens), addSat(r.CacheReadTokens, r.CacheCreationTokens))
}

// addSat adds non-negative counts and saturates at the contract's token limit; negative inputs count as zero.
func addSat(a, b int64) int64 {
	return min(maxTokens, max(a, 0)+max(b, 0))
}

func clipModel(m string) string {
	if m == "" {
		return unknownModel
	}
	return clipBytes(m, maxModelLen)
}

// safeProject keeps a project name from carrying an absolute path or an overlong value.
func safeProject(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || (len(p) >= 2 && p[1] == ':') {
		p = path.Base(strings.ReplaceAll(p, `\`, "/"))
		if p == "/" || p == "." || strings.HasSuffix(p, ":") {
			p = ""
		}
	}
	if p == "" {
		return project.Unknown
	}
	return clipBytes(p, maxProject)
}

// clipBytes cuts s to at most n bytes without splitting a character.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
