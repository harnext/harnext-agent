package scan

import (
	"encoding/json"
	"slices"

	"github.com/harnext/harnext-agent/internal/aggregate"
)

// Limits of the card contract; the byte limit is enforced by the server on the request body.
const (
	CardPayloadVersion = 1
	maxCardBytes       = 32768
	maxCardUsage       = 100
	maxCardFolders     = 5
	maxAgentVersion    = 32
)

// CardPayload is the card.v1 body. It carries only numbers and, optionally, project names.
type CardPayload struct {
	PayloadVersion int          `json:"payload_version"`
	AgentVersion   string       `json:"agent_version"`
	Period         CardPeriod   `json:"period"`
	ActiveDays     int          `json:"active_days"`
	Sessions       int          `json:"sessions"`
	Usage          []CardUsage  `json:"usage"`
	Folders        []CardFolder `json:"folders,omitempty"`

	// Dropped is the number of usage rows left out to fit the limits; it is not part of the wire body.
	Dropped int `json:"-"`
}

type CardPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// CardUsage has no day: the card holds period totals per (tool, model, speed, prompt floor).
type CardUsage struct {
	ToolName              string   `json:"tool_name"`
	Model                 string   `json:"model"`
	Speed                 string   `json:"speed"`
	PromptFloor           int64    `json:"prompt_floor"`
	InputTokens           int64    `json:"input_tokens"`
	OutputTokens          int64    `json:"output_tokens"`
	CacheReadTokens       int64    `json:"cache_read_tokens"`
	CacheCreationTokens   int64    `json:"cache_creation_tokens"`
	CacheCreation1hTokens int64    `json:"cache_creation_1h_tokens"`
	WebSearchRequests     int64    `json:"web_search_requests"`
	WebFetchRequests      int64    `json:"web_fetch_requests"`
	CopilotCredits        *float64 `json:"copilot_credits,omitempty"`
	TokensEstimated       bool     `json:"tokens_estimated,omitempty"`
}

type CardFolder struct {
	Project     string `json:"project"`
	TotalTokens int64  `json:"total_tokens"`
}

// CardPayload builds the card body. With more usage rows than the contract allows (or a body over the size limit) the
// rows with the fewest tokens are dropped, ties by key, so the result is deterministic. Without includeFolders the
// folders key is absent.
func (s Summary) CardPayload(agentVersion string, includeFolders bool) CardPayload {
	if agentVersion == "" {
		agentVersion = "dev"
	}
	p := CardPayload{
		PayloadVersion: CardPayloadVersion,
		AgentVersion:   clipBytes(agentVersion, maxAgentVersion),
		Period:         CardPeriod{From: s.From, To: s.To},
		ActiveDays:     s.ActiveDays,
		Sessions:       s.Sessions,
		Usage:          []CardUsage{},
	}
	groups := slices.Clone(s.Usage)
	slices.SortFunc(groups, compareUsage)
	for _, g := range groups {
		u := CardUsage{
			ToolName: g.Tool, Model: g.Model, Speed: g.Speed, PromptFloor: g.PromptFloor,
			InputTokens: g.InputTokens, OutputTokens: g.OutputTokens, CacheReadTokens: g.CacheReadTokens,
			CacheCreationTokens: g.CacheCreationTokens, CacheCreation1hTokens: min(g.CacheCreation1hTokens, g.CacheCreationTokens),
			WebSearchRequests: g.WebSearchRequests, WebFetchRequests: g.WebFetchRequests,
		}
		if g.Tool == aggregate.ToolVSCodeCopilot && g.CopilotCredits != nil {
			c := *g.CopilotCredits
			u.CopilotCredits = &c
			u.TokensEstimated = g.TokensEstimated
		}
		p.Usage = append(p.Usage, u)
	}
	if includeFolders {
		for _, f := range s.Projects[:min(len(s.Projects), maxCardFolders)] {
			p.Folders = append(p.Folders, CardFolder{Project: f.Project, TotalTokens: f.Tokens})
		}
	}

	keep := len(p.Usage)
	if keep > maxCardUsage {
		keep = maxCardUsage
	}
	for keep > 0 {
		p.Usage = p.Usage[:keep]
		if b, err := json.Marshal(p); err == nil && len(b) <= maxCardBytes {
			break
		}
		keep--
	}
	p.Usage = p.Usage[:keep]
	p.Dropped = len(groups) - keep
	return p
}
