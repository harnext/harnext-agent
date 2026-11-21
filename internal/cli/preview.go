package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/harnext/harnext-agent/internal/service"
	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/uploader"
)

func previewCmd(env Env) *cobra.Command {
	var sessions int
	var all bool
	c := &cobra.Command{
		Use:   "preview",
		Short: "Print the data the agent would send to the server (sends nothing)",
		Long: "Prints the exact request body the agent uploads for your newest sessions, as JSON on standard output.\n" +
			"It reads only the agent's saved state, makes no network call and changes nothing.\n" +
			"What is not in this output is not sent: no prompts, no replies, no file contents, no full paths.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := os.Stat(env.Paths.DB); err != nil {
				fmt.Fprintln(env.Err, "No data yet: nothing would be sent.")
				return nil
			}
			db, err := storage.Open(env.Paths.DB)
			if err != nil {
				return err
			}
			defer db.Close()
			rows, err := service.PreviewRows(db)
			if err != nil {
				return err
			}
			view, err := gateFor(env, db)
			if err != nil {
				return err
			}
			gate := env.Gate
			if view != nil {
				gate = view.Decide
			}
			var configVersion int64
			if st, err := env.Paths.ReadStatus(); err == nil && st.Config.Loaded {
				configVersion = st.Config.Version
			}
			limit := sessions
			if all {
				limit = 0
			}
			res := uploader.Preview(rows, uploader.PreviewOptions{
				AgentVersion: env.Version, ConfigVersion: configVersion, Gate: gate, MaxSessions: limit,
			})
			if res.Total == 0 {
				if view != nil && len(rows) > 0 {
					fmt.Fprintln(env.Err, "Nothing would be sent right now: no session passes the account check.")
					printAttribution(env.Err, view.Summary())
					return nil
				}
				fmt.Fprintln(env.Err, "No data yet: nothing would be sent.")
				return nil
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, res.Body, "", "  "); err != nil {
				return err
			}
			fmt.Fprintf(env.Err, "Showing %d of %d sessions that would be sent (newest first). Nothing was sent.\n", res.Sessions, res.Total)
			if res.Invalid > 0 {
				fmt.Fprintf(env.Err, "%d sessions are outside the contract limits and would never be sent.\n", res.Invalid)
			}
			fmt.Fprintln(env.Out, pretty.String())
			if view != nil {
				printAttribution(env.Err, view.Summary())
			}
			return nil
		},
	}
	c.Flags().IntVar(&sessions, "sessions", 1, "how many of the newest sessions to show")
	c.Flags().BoolVar(&all, "all", false, "show every session (can be long)")
	return c
}
