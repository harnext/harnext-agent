// Package cli implements the agent's command line: init, run, status, stats, preview, scan and stop.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/selfupdate"
	"github.com/harnext/harnext-agent/internal/service"
	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/svcmgr"
	"github.com/harnext/harnext-agent/internal/uploader"
)

// Env carries everything the commands need, so tests can substitute it.
type Env struct {
	Paths   service.Paths
	Version string
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	// ServerDefault is the compiled-in server URL; init requires --server when empty.
	ServerDefault string
	// ServiceOptions overrides the service manager options; for tests.
	ServiceOptions func() (svcmgr.Options, error)
	// Gate decides which sessions are sent (run) or shown (preview). Nil is the production gate: the account gate over the signed
	// identity (preview and stats build it from the saved state, without a request).
	Gate func(aggregate.Row) uploader.Decision
	// PreviewGate overrides the gate view that preview and stats use; for tests.
	PreviewGate func(db *storage.Store) (service.GateView, error)
}

// New builds the root command.
func New(env Env) *cobra.Command {
	root := &cobra.Command{
		Use:           "harnext-agent",
		Short:         "Harnext local agent: collects token and cost metrics of AI coding tools",
		Version:       env.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// A literal, not {{.Version}}: keeps the output independent of struct reflection.
	root.SetVersionTemplate("harnext-agent " + strings.ReplaceAll(env.Version, "{{", "") + "\n")
	root.SetOut(env.Out)
	root.SetErr(env.Err)
	root.AddCommand(initCmd(env), logoutCmd(env), runCmd(env), statusCmd(env), statsCmd(env), previewCmd(env), stopCmd(env), serviceCmd(env), copilotCmd(env), scanCmd(env))
	return root
}

func initCmd(env Env) *cobra.Command {
	var token, server string
	var tokenStdin, devLocalhost bool
	c := &cobra.Command{
		Use:     "init",
		Aliases: []string{"login"},
		Short:   "Add a Harnext account (its agent token) and store the server address",
		Long: "Stores the agent token (file mode 0600) and the server address. Run it once for every Harnext account you use on\n" +
			"this machine (for example a personal and a company account): each account collects the usage of the tool accounts\n" +
			"that sign in with its e-mail. Adding a token that is already stored changes nothing.\n" +
			"Prefer --token-stdin or the HARNEXT_TOKEN variable over --token: command line arguments\n" +
			"are visible to other local users and stay in shell history.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if tokenStdin {
				line, err := bufio.NewReader(env.In).ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return errors.New("could not read the token from standard input")
				}
				token = strings.TrimSpace(line)
			}
			if token == "" {
				token = os.Getenv("HARNEXT_TOKEN")
			}
			if server == "" {
				server = env.ServerDefault
			}
			if server == "" {
				return errors.New("--server is required (this build has no default server)")
			}
			if err := service.ValidateToken(token); err != nil {
				return errors.New("the token is missing or malformed (expected hxt_...)")
			}
			if err := env.Paths.SaveSettings(service.Settings{ServerURL: server, AllowInsecureLocalhost: devLocalhost}); err != nil {
				return err
			}
			acct, err := env.Paths.AddAccount(token)
			switch {
			case errors.Is(err, service.ErrTooManyAccounts):
				return fmt.Errorf("this machine already holds %d accounts: remove one with `harnext-agent logout <id>` first", service.MaxAccounts)
			case err != nil:
				return err
			}
			fmt.Fprintf(env.Out, "Saved account %s. Start the agent with: harnext-agent run\n", acct.ID)
			if _, running := service.RunningPID(env.Paths.Lock); running {
				fmt.Fprintln(env.Out, "The agent is already running: restart it to use the new settings (stop, then run).")
			}
			return nil
		},
	}
	c.Flags().StringVar(&token, "token", "", "agent token (visible in the process list; prefer --token-stdin)")
	c.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the token from standard input")
	c.Flags().StringVar(&server, "server", "", "server address, https://host")
	c.Flags().BoolVar(&devLocalhost, "dev-allow-insecure-localhost", false, "development only: allow http:// for localhost")
	return c
}

func logoutCmd(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "logout [account-id]",
		Short: "Remove a Harnext account from this machine",
		Long: "Removes one Harnext account (its token and stored identity). The id is shown by `harnext-agent status`; it may be\n" +
			"left out when there is only one account. Data already sent stays in Harnext.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prefix := ""
			if len(args) == 1 {
				prefix = args[0]
				if prefix == "" {
					return errors.New("account id is empty")
				}
			} else if list, err := env.Paths.LoadAccounts(); err == nil && len(list) > 1 {
				return errors.New("more than one account is stored: name the one to remove (see `harnext-agent status`)")
			}
			acct, err := env.Paths.RemoveAccount(prefix)
			switch {
			case errors.Is(err, service.ErrNotInitialized):
				return errors.New("no account is stored on this machine")
			case errors.Is(err, service.ErrNoSuchAccount):
				return errors.New("no stored account has that id (see `harnext-agent status`)")
			case errors.Is(err, service.ErrAmbiguousAccount):
				return errors.New("that id matches more than one account: type more of it")
			case err != nil:
				return err
			}
			fmt.Fprintf(env.Out, "Removed account %s.\n", acct.ID)
			if _, running := service.RunningPID(env.Paths.Lock); running {
				fmt.Fprintln(env.Out, "The agent is running: restart it to stop using that account (stop, then run).")
			}
			return nil
		},
	}
}

func runCmd(env Env) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the agent in the foreground (the service manager runs this)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			lg := log.New(env.Err, "", log.LstdFlags|log.LUTC)
			exe, exeErr := os.Executable()
			if exeErr == nil {
				if resolved, linkErr := filepath.EvalSymlinks(exe); linkErr == nil {
					exe = resolved
				}
				selfupdate.CleanupOld(exe) // Windows keeps the replaced binary aside until the next start
			}
			err := service.Run(ctx, service.RunOptions{
				Paths: env.Paths, Version: env.Version, Gate: env.Gate,
				Logf: func(f string, a ...any) { lg.Printf(f, a...) },
			})
			switch {
			case errors.Is(err, service.ErrRestartForUpdate):
				// A newer agent was installed: start it in place of this process.
				if exeErr != nil {
					return errors.New("updated, but the agent binary could not be located: restart the agent")
				}
				return selfupdate.Restart(exe, os.Args)
			case errors.Is(err, service.ErrNotInitialized):
				return errors.New("not initialized: run `harnext-agent init` first")
			case errors.Is(err, service.ErrAlreadyRunning):
				return errors.New("the agent is already running")
			}
			return err
		},
	}
}
