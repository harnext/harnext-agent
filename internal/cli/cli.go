// Package cli implements the agent's command line: init, run, status, stats, preview and stop.
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

	"github.com/harnext/harnext-agent/internal/selfupdate"
	"github.com/harnext/harnext-agent/internal/service"
	"github.com/harnext/harnext-agent/internal/svcmgr"
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
	root.AddCommand(initCmd(env), runCmd(env), statusCmd(env), statsCmd(env), previewCmd(env), stopCmd(env), serviceCmd(env), copilotCmd(env))
	return root
}

func initCmd(env Env) *cobra.Command {
	var token, server string
	var tokenStdin, devLocalhost bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Store the agent token and server address",
		Long: "Stores the agent token (file mode 0600) and the server address.\n" +
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
			if err := env.Paths.SaveToken(token); err != nil {
				return err
			}
			fmt.Fprintln(env.Out, "Saved. Start the agent with: harnext-agent run")
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
				Paths: env.Paths, Version: env.Version,
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
