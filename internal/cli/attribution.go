package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/harnext/harnext-agent/internal/service"
	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/uploader"
)

// reasonOrder is the display order of the gate's skip reasons; unknown reasons follow alphabetically.
var reasonOrder = []string{"no_account", "copilot_account_unknown", "identity_pending", "no_matching_account", "account_unbound"}

// gateFor returns the gate for preview and stats. It is nil only when a test substituted Env.Gate without a PreviewGate.
func gateFor(env Env, db *storage.Store) (service.GateView, error) {
	if env.PreviewGate != nil {
		return env.PreviewGate(db)
	}
	if env.Gate != nil {
		return nil, nil
	}
	return service.NewPreviewGate(env.Paths, db, service.PreviewGateOptions{})
}

// printAttribution writes the account lines; it prints masked labels only, never an e-mail, a GitHub name or a hash. Accounts on
// this machine that match no Harnext account are only counted.
func printAttribution(w io.Writer, s service.GateSummary) {
	ac := s.Accounts
	if len(ac.Harnext) == 0 {
		return
	}
	fmt.Fprintf(w, "Accounts:  %s on this machine\n", count(len(ac.Harnext), "Harnext account"))
	matchedClaude := false
	for _, h := range ac.Harnext {
		fmt.Fprintf(w, "  %s  identity %s\n", h.ID, identityLine(h.Identity))
		if h.ClaudeMatched {
			matchedClaude = true
			if label := safeLabel(h.ClaudeLabel); label != "" {
				fmt.Fprintf(w, "            Claude Code: %s\n", label)
			} else {
				fmt.Fprintln(w, "            Claude Code: matched")
			}
		} else {
			fmt.Fprintln(w, "            Claude Code: no signed-in account matches this Harnext account")
		}
		if h.CopilotSelected {
			fmt.Fprintln(w, "            Copilot:     GitHub account selected")
		} else {
			fmt.Fprintln(w, "            Copilot:     no GitHub account selected")
		}
		if h.LastReportError != "" {
			fmt.Fprintf(w, "            Report:      the last account report failed (%s)\n", h.LastReportError)
		}
	}
	fmt.Fprintf(w, "Found:     %s signed in, %s found for Copilot\n", count(ac.ClaudeFound, "Claude Code account"), count(ac.CopilotLogins, "GitHub account"))
	fmt.Fprintf(w, "Sessions:  %s\n", accountsLine(ac))
	if ac.ClaudeFound > 0 && !matchedClaude {
		fmt.Fprintln(w, "Hint:      Claude Code is signed in with an address that is not the e-mail of a Harnext account here; its usage stays on this machine.")
		fmt.Fprintln(w, "           Sign in to Harnext with that e-mail (run `harnext-agent login` with its token) to collect it.")
	}
	if ac.CopilotLogins > 0 {
		sel := false
		for _, h := range ac.Harnext {
			sel = sel || h.CopilotSelected
		}
		if !sel {
			fmt.Fprintln(w, "Hint:      choose your GitHub account for Copilot in the Harnext panel under Agents.")
		}
	}
}

func identityLine(id service.IdentityStatus) string {
	switch id.State {
	case service.IdentityOK:
		return fmt.Sprintf("ok (%s connected, valid until %s)", count(id.Bindings, "account"), id.ExpiresAt.Local().Format("2006-01-02 15:04"))
	case service.IdentityExpired:
		return "expired (sessions stay on this machine until it is renewed)"
	case service.IdentityNoTrustedKeys:
		return "no trusted keys (this build cannot verify identities; sessions stay on this machine)"
	default:
		s := "pending (no verified identity yet; sessions stay on this machine)"
		if id.LastError != "" {
			s += "; last error: " + id.LastError
		}
		return s
	}
}

func accountsLine(ac service.AccountsStatus) string {
	reasons := make([]string, 0, len(ac.Skipped))
	seen := map[string]bool{}
	for _, r := range reasonOrder {
		if n := ac.Skipped[r]; n > 0 {
			reasons = append(reasons, fmt.Sprintf("%s %d", r, n))
		}
		seen[r] = true
	}
	var rest []string
	for r, n := range ac.Skipped {
		if !seen[r] && n > 0 {
			rest = append(rest, fmt.Sprintf("%s %d", r, n))
		}
	}
	sort.Strings(rest)
	reasons = append(reasons, rest...)
	line := count(ac.Sendable, "session") + " ready to send"
	if len(reasons) > 0 {
		line += "; kept on this machine: " + strings.Join(reasons, ", ")
	}
	return line
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// safeLabel passes only masked labels: one with an '@' must contain '*', so a plain address is never shown.
func safeLabel(s string) string {
	if strings.Contains(s, "@") && !strings.Contains(s, "*") {
		return ""
	}
	return s
}

// feedGate runs every saved session through the gate so its summary is current.
func feedGate(db *storage.Store, view service.GateView) error {
	rows, err := service.PreviewRows(db)
	if err != nil {
		return err
	}
	uploader.Preview(rows, uploader.PreviewOptions{Gate: view.Decide, MaxSessions: 1})
	return nil
}
