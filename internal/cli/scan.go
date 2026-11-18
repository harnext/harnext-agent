package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/harnext/harnext-agent/internal/scan"
)

// Package-level so tests never read the real tool records or need a real terminal.
var (
	scanOptions = func() scan.Options { return scan.Options{} }
	scanTTY     = openTTY
)

const noTTYHint = "Run again with --yes to create a card without prompts (it prints exactly what it sends), or use --json to look at the data first."

func scanCmd(env Env) *cobra.Command {
	var server string
	var noCard, asJSON, withFolders, yes, devLocalhost bool
	c := &cobra.Command{
		Use:   "scan",
		Short: "Summarize your last 30 days of AI coding usage; optionally make a shareable card",
		Long: "Reads the Claude Code and VS Code Copilot history on this computer once and prints a summary. It needs no account,\n" +
			"installs nothing and stores nothing. Only numbers are ever sent, never prompts, replies or code, and only after you\n" +
			"confirm the exact data. Use --json to see that data without any network call.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if server == "" {
				server = env.ServerDefault
			}
			client := scan.Client{BaseURL: server, UserAgent: "harnext-agent/" + env.Version, AllowInsecureLocalhost: devLocalhost}
			wantCard := !asJSON && !noCard
			origin := ""
			if wantCard {
				if server == "" {
					return errors.New("--server is required (this build has no default server)")
				}
				var err error
				if origin, err = client.Origin(); err != nil {
					return errors.New("the server address must be https://host (http only for localhost with --dev-allow-insecure-localhost)")
				}
			}

			fmt.Fprintln(env.Err, "Reading the last 30 days of Claude Code and VS Code Copilot history on this computer (nothing is sent yet)...")
			opts := scanOptions()
			if opts.Now.IsZero() {
				opts.Now = time.Now()
			}
			rows, err := scan.Collect(ctx, opts)
			if err != nil {
				return errors.New("the scan was cancelled")
			}
			summary := scan.Build(rows, opts.Now, scan.DefaultResolve())
			if summary.Sessions == 0 {
				fmt.Fprintln(env.Err, "No Claude Code or VS Code Copilot history found in the last 30 days.")
				return nil
			}
			if asJSON {
				body, _ := cardBody(summary, env.Version, withFolders)
				fmt.Fprintln(env.Out, string(body))
				return nil
			}
			scan.Render(env.Out, summary)
			if noCard {
				return nil
			}
			return offerCard(ctx, env, client, origin, summary, withFolders, yes)
		},
	}
	c.Flags().BoolVar(&noCard, "no-card", false, "only print the summary; do not offer a shareable card")
	c.Flags().BoolVar(&asJSON, "json", false, "print only the card data as JSON; no network, no questions")
	c.Flags().BoolVar(&withFolders, "with-folders", false, "include your top project names in the card data")
	c.Flags().BoolVar(&yes, "yes", false, "create the card without questions (the data to send is still printed first)")
	c.Flags().StringVar(&server, "server", "", "server address, https://host")
	c.Flags().BoolVar(&devLocalhost, "dev-allow-insecure-localhost", false, "development only: allow http:// for localhost")
	return c
}

// cardBody is the exact request body: indented for reading, or compact when indenting would exceed the size limit.
func cardBody(s scan.Summary, version string, folders bool) ([]byte, scan.CardPayload) {
	p := s.CardPayload(version, folders)
	b, _ := json.Marshal(p)
	var pretty bytes.Buffer
	if json.Indent(&pretty, b, "", "  ") == nil && pretty.Len() <= scan.MaxBodyBytes {
		return pretty.Bytes(), p
	}
	return b, p
}

func offerCard(ctx context.Context, env Env, client scan.Client, origin string, s scan.Summary, withFolders, yes bool) error {
	w := env.Err
	folders := withFolders
	var answers *bufio.Reader
	if !yes {
		tty, err := scanTTY()
		if err != nil {
			fmt.Fprintln(w, noTTYHint)
			return nil
		}
		defer tty.Close()
		answers = bufio.NewReader(tty)
		fmt.Fprintln(w)
		if !askYes(answers, w, "Create a shareable card from this summary? Only numbers are sent, no prompts, replies or code. [y/N]") {
			return nil
		}
		if !withFolders && len(s.Projects) > 0 {
			folders = askYes(answers, w, "Also send your top project names so you can choose to show them on the card? [y/N]")
		}
	}

	body, p := cardBody(s, env.Version, folders)
	fmt.Fprintln(w, "\nData to send:")
	fmt.Fprintln(w, string(body))
	if p.Dropped > 0 {
		fmt.Fprintf(w, "(%d small usage rows were left out to fit the size limit.)\n", p.Dropped)
	}
	if !yes {
		if !askYes(answers, w, "Send exactly this to "+origin+"? [y/N]") {
			fmt.Fprintln(w, "Nothing was sent.")
			return nil
		}
	}

	d, err := scan.CreateDraft(ctx, client, body)
	if err != nil {
		return errors.New(scanErrorText(err))
	}
	fmt.Fprintln(env.Out, "Open this private link to finish: "+d.ManageURL)
	fmt.Fprintln(env.Out, "It expires "+d.ExpiresAt+". Anyone with this link can publish or discard your card, so keep it to yourself. Nothing is public until you publish.")
	return nil
}

// askYes accepts only a complete line of y or yes; an empty line, end of input or anything else is no.
func askYes(in *bufio.Reader, w io.Writer, question string) bool {
	fmt.Fprint(w, question+" ")
	line, err := in.ReadString('\n')
	if err != nil {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// scanErrorText gives fixed messages only: nothing from the response, the URL or the request is ever shown.
func scanErrorText(err error) string {
	var se *scan.Error
	if !errors.As(err, &se) {
		if errors.Is(err, context.Canceled) {
			return "The request was cancelled."
		}
		return "Could not reach the server."
	}
	switch se.Code {
	case scan.CodeRateLimited:
		return "Too many requests from this network. Try again in an hour."
	case scan.CodeRejected:
		return "The server rejected this summary."
	case scan.CodeUnavailable:
		return "The service is not available right now. Try again later."
	case scan.CodeNetwork:
		return "Could not reach the server."
	case scan.CodeRedirect:
		return "Unexpected redirect; refusing to follow."
	case scan.CodeBadURL:
		return "The server address is not valid."
	}
	return fmt.Sprintf("Unexpected response from the server (HTTP %d).", se.Status)
}
