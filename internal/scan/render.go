package scan

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	renderWidth   = 60
	nameWidth     = 22
	barWidth      = 20
	projectWidth  = 40
	renderTopN    = 5
	estimatedMark = "~"
)

// Render writes the terminal summary: plain ASCII, no colour, fixed width, so it reads the same on every terminal.
// Project names are local-only output and are the resolved names, never full paths. There is no money and no friction.
func Render(w io.Writer, s Summary) {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("Harnext scan: last 30 days")
	line("%s", strings.Repeat("=", renderWidth))
	line("%-16s %s .. %s", "Period", s.From, s.To)
	line("%-16s %d", "Active days", s.ActiveDays)
	line("%-16s %d", "Sessions", s.Sessions)

	var total int64
	var estimated bool
	var credits float64
	var haveCredits bool
	type modelKey struct{ tool, model string }
	type modelSum struct {
		tokens    int64
		estimated bool
	}
	models := map[modelKey]*modelSum{}
	for _, g := range s.Usage {
		total = addSat(total, g.TotalTokens())
		estimated = estimated || g.TokensEstimated
		if g.CopilotCredits != nil {
			credits += *g.CopilotCredits
			haveCredits = true
		}
		k := modelKey{g.Tool, g.Model}
		m := models[k]
		if m == nil {
			m = &modelSum{}
			models[k] = m
		}
		m.tokens = addSat(m.tokens, g.TotalTokens())
		m.estimated = m.estimated || g.TokensEstimated
	}
	mark := func(est bool, n int64) string {
		if est {
			return estimatedMark + humanTokens(n)
		}
		return humanTokens(n)
	}
	line("%-16s %s", "Total tokens", mark(estimated, total))
	if haveCredits {
		line("%-16s %s", "Copilot credits", trimFloat(credits))
	}
	if len(s.Usage) == 0 {
		line("")
		line("No usage found in this period.")
		io.WriteString(w, b.String())
		return
	}

	type modelRow struct {
		modelKey
		modelSum
	}
	var rows []modelRow
	for k, m := range models {
		rows = append(rows, modelRow{k, *m})
	}
	slices.SortFunc(rows, func(a, c modelRow) int {
		switch {
		case a.tokens != c.tokens:
			if a.tokens > c.tokens {
				return -1
			}
			return 1
		case a.tool != c.tool:
			return strings.Compare(a.tool, c.tool)
		}
		return strings.Compare(a.model, c.model)
	})
	rows = rows[:min(len(rows), renderTopN)]
	line("")
	line("Models (top %d by tokens)", renderTopN)
	for _, r := range rows {
		line("  %-*s %-8s %s %9s", nameWidth, asciiClip(r.model, nameWidth), toolLabel(r.tool), bar(r.tokens, rows[0].tokens), mark(r.estimated, r.tokens))
	}

	if len(s.Projects) > 0 {
		line("")
		line("Top projects")
		for i, p := range s.Projects[:min(len(s.Projects), renderTopN)] {
			line("  %d. %-*s %9s", i+1, projectWidth, asciiClip(p.Project, projectWidth), humanTokens(p.Tokens))
		}
	}
	if estimated {
		line("")
		line("%s = estimated token count (Copilot history, scaled from credits)", estimatedMark)
	}
	io.WriteString(w, b.String())
}

func toolLabel(tool string) string {
	switch tool {
	case "claude_code":
		return "Claude"
	case "vscode_copilot":
		return "Copilot"
	}
	return "other"
}

// bar draws n relative to the largest value; any non-zero value gets at least one mark.
func bar(n, largest int64) string {
	filled := 0
	if n > 0 && largest > 0 {
		filled = max(1, int(float64(n)/float64(largest)*barWidth+0.5))
	}
	filled = min(filled, barWidth)
	return strings.Repeat("#", filled) + strings.Repeat(".", barWidth-filled)
}

// humanTokens prints 999, 1.2K, 3.4M, 5.6B.
func humanTokens(n int64) string {
	v, u := float64(n), -1
	for v >= 999.95 && u < 2 {
		v /= 1000
		u++
	}
	if u < 0 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1f%s", v, "KMB"[u:u+1])
}

func trimFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}

// asciiClip replaces anything outside printable ASCII and shortens to n characters, so widths hold on any terminal.
func asciiClip(s string, n int) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			r = '?'
		}
		out = append(out, byte(r))
	}
	if len(out) > n {
		return string(out[:n-3]) + "..."
	}
	return string(out)
}
