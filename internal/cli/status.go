package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/service"
	"github.com/harnext/harnext-agent/internal/storage"
)

func statusCmd(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the agent runs and what it last did",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := env.Out
			fmt.Fprintf(w, "Version:   %s\n", env.Version)
			settings, err := env.Paths.LoadSettings()
			switch {
			case errors.Is(err, service.ErrNotInitialized):
				fmt.Fprintln(w, "Setup:     not initialized (run `harnext-agent init`)")
				return nil
			case err != nil:
				return err
			}
			if u, err := url.Parse(settings.ServerURL); err == nil {
				fmt.Fprintf(w, "Server:    %s\n", u.Host)
			}
			if _, err := env.Paths.LoadToken(); err != nil {
				fmt.Fprintln(w, "Token:     missing or invalid (run `harnext-agent init`)")
			} else {
				fmt.Fprintln(w, "Token:     stored")
			}
			pid, running := service.RunningPID(env.Paths.Lock)
			if !running {
				fmt.Fprintln(w, "Agent:     not running")
			} else if pid > 0 {
				fmt.Fprintf(w, "Agent:     running (PID %d)\n", pid)
			} else {
				fmt.Fprintln(w, "Agent:     running")
			}
			st, err := env.Paths.ReadStatus()
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(w, "Activity:  no data yet")
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "Updated:   %s\n", ago(st.UpdatedAt))
			fmt.Fprintf(w, "Tracked:   %d sessions, %d responses, %d unreadable lines skipped\n", st.Sessions, st.UniqueResponses, st.SkippedLines)
			fmt.Fprintf(w, "Analysis:  %s\n", configLine(st.Config))
			fmt.Fprintf(w, "Upload:    %s\n", uploadLine(st.Upload))
			if st.Backfill.Found > 0 || !st.Backfill.FirstPacketAt.IsZero() {
				fmt.Fprintf(w, "Backfill:  %s\n", strings.TrimPrefix(st.Backfill.Line(), "backfill: "))
			}
			printCopilotStatus(env, st)
			if st.Config.UpgradeRequired {
				fmt.Fprintln(w, "Notice:    the server asks for a newer agent version")
			}
			return nil
		},
	}
}

func configLine(c service.ConfigStatus) string {
	switch {
	case !c.TrustedKeys:
		return "off (this build has no trusted keys; scores are not computed)"
	case c.Loaded:
		return fmt.Sprintf("on (configuration v%d, valid until %s)", c.Version, c.ExpiresAt.Format("2006-01-02"))
	case c.LastError != "":
		return "off (no valid configuration; last error: " + c.LastError + ")"
	default:
		return "off (no valid configuration yet)"
	}
}

func uploadLine(u service.UploadStatus) string {
	if u.LastAttempt.IsZero() {
		return "no attempt yet"
	}
	s := fmt.Sprintf("last attempt %s: %d sent, %d unchanged, %d rejected, %d invalid", ago(u.LastAttempt), u.Accepted, u.Unchanged, u.Rejected, u.Invalid)
	if u.LastError != "" {
		s += "; error: " + u.LastError
	}
	if !u.LastSuccess.IsZero() {
		s += "; last success " + ago(u.LastSuccess)
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String() + " ago"
}

func statsCmd(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show your own token totals per project folder (stays on this machine)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := os.Stat(env.Paths.DB); err != nil {
				fmt.Fprintln(env.Out, "No data yet.")
				return nil
			}
			db, err := storage.Open(env.Paths.DB)
			if err != nil {
				return err
			}
			defer db.Close()
			blob, ok, err := db.LoadState("aggregate")
			if err != nil {
				return err
			}
			rows, err := rowsFromSnapshot(blob, ok)
			if err != nil {
				return err
			}
			printStats(env, rows)
			return nil
		},
	}
}

func printStats(env Env, rows []aggregate.Row) {
	type sum struct {
		sessions map[string]struct{}
		aggregate.Totals
	}
	byFolder := map[string]*sum{}
	var all aggregate.Totals
	allSessions := map[string]struct{}{}
	for _, r := range rows {
		folder := r.Cwd
		if folder == "" {
			folder = "(unknown)"
		}
		s := byFolder[folder]
		if s == nil {
			s = &sum{sessions: map[string]struct{}{}}
			byFolder[folder] = s
		}
		s.sessions[r.SessionID] = struct{}{}
		allSessions[r.SessionID] = struct{}{}
		add(&s.Totals, r.Totals)
		add(&all, r.Totals)
	}
	if len(byFolder) == 0 {
		fmt.Fprintln(env.Out, "No data yet.")
		return
	}
	folders := make([]string, 0, len(byFolder))
	for f := range byFolder {
		folders = append(folders, f)
	}
	sort.Slice(folders, func(i, j int) bool {
		a, b := byFolder[folders[i]], byFolder[folders[j]]
		if a.OutputTokens != b.OutputTokens {
			return a.OutputTokens > b.OutputTokens
		}
		return folders[i] < folders[j]
	})
	tw := tabwriter.NewWriter(env.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tSESSIONS\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE")
	for _, f := range folders {
		s := byFolder[f]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\n", f, len(s.sessions), s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheCreationTokens)
	}
	fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%d\t%d\t%d\n", len(allSessions), all.InputTokens, all.OutputTokens, all.CacheReadTokens, all.CacheCreationTokens)
	tw.Flush()
}

func add(dst *aggregate.Totals, t aggregate.Totals) {
	dst.InputTokens += t.InputTokens
	dst.OutputTokens += t.OutputTokens
	dst.CacheReadTokens += t.CacheReadTokens
	dst.CacheCreationTokens += t.CacheCreationTokens
	dst.CacheCreation1hTokens += t.CacheCreation1hTokens
}

// terminateAgent is a variable so tests never signal a real process.
var terminateAgent = terminate

// stopAgent stops the running agent; stopped is false when none was running.
func stopAgent(env Env) (stopped bool, err error) {
	pid, running := service.RunningPID(env.Paths.Lock)
	if !running {
		return false, nil
	}
	if pid <= 0 {
		return false, errors.New("the agent is running but its PID is unknown")
	}
	if err := terminateAgent(pid, 10*time.Second, func() bool {
		_, still := service.RunningPID(env.Paths.Lock)
		return still
	}); err != nil {
		return false, err
	}
	return true, nil
}

func stopCmd(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stopped, err := stopAgent(env)
			if err != nil {
				return err
			}
			if !stopped {
				fmt.Fprintln(env.Out, "The agent is not running.")
				return nil
			}
			fmt.Fprintln(env.Out, "Stopped. If a service manager runs the agent, it may start it again.")
			return nil
		},
	}
}
