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

const connectHint = "Connect them in the Harnext panel under Connected Agents."

// reasonOrder is the display order of the gate's skip reasons; unknown reasons follow alphabetically.
var reasonOrder = []string{"no_account", "git_email_missing", "identity_pending", "account_unbound"}

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

// printAttribution writes the identity and account lines; it prints masked labels only, never an e-mail or a hash.
func printAttribution(w io.Writer, s service.GateSummary) {
	id, ac := s.Identity, s.Accounts
	if id.State == "" {
		return
	}
	fmt.Fprintf(w, "Identity:  %s\n", identityLine(id))
	fmt.Fprintf(w, "Accounts:  %s\n", accountsLine(ac))
	if ac.LastReportError != "" {
		fmt.Fprintf(w, "Report:    the last account report failed (%s)\n", ac.LastReportError)
	}
	var labels []string
	for _, p := range ac.Pending {
		if label := safeLabel(p.Label); label != "" {
			labels = append(labels, fmt.Sprintf("%s (%s, %s)", label, p.Tool, count(p.Sessions, "session")))
		}
	}
	if len(labels) == 0 {
		return
	}
	more := ""
	if n := ac.PendingTotal - len(labels); n > 0 {
		more = fmt.Sprintf(" and %d more", n)
	}
	fmt.Fprintf(w, "Pending:   %s%s\n", strings.Join(labels, "; "), more)
	fmt.Fprintf(w, "           %s\n", connectHint)
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
