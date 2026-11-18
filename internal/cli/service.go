package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/harnext/harnext-agent/internal/svcmgr"
)

func (e Env) serviceOptions() (svcmgr.Options, error) {
	if e.ServiceOptions != nil {
		return e.ServiceOptions()
	}
	return svcmgr.Defaults(runtime.GOOS)
}

func where(f svcmgr.File) string {
	if f.Path == "" {
		return f.Description
	}
	return f.Path
}

// installService stops a running agent first (a foreground agent would hold the lock and make
// the service exit at once), then writes the definition and starts the service.
func installService(env Env) (f svcmgr.File, restarted bool, err error) {
	if _, err := env.Paths.LoadSettings(); err != nil {
		return f, false, errors.New("not initialized: run `harnext-agent init` first")
	}
	if _, err := env.Paths.LoadToken(); err != nil {
		return f, false, errors.New("not initialized: run `harnext-agent init` first")
	}
	o, err := env.serviceOptions()
	if err != nil {
		return f, false, err
	}
	if restarted, err = stopAgent(env); err != nil {
		return f, false, fmt.Errorf("could not stop the running agent: %w", err)
	}
	f, err = svcmgr.Install(o)
	return f, restarted, err
}

// serviceInstalled reports whether the service definition exists; any doubt counts as not installed.
func serviceInstalled(env Env) bool {
	o, err := env.serviceOptions()
	if err != nil {
		return false
	}
	f, err := svcmgr.Render(o)
	if err != nil {
		return false
	}
	if f.Path != "" {
		_, err := os.Stat(f.Path)
		return err == nil
	}
	_, err = o.Run("schtasks", "/Query", "/TN", svcmgr.TaskName)
	return err == nil
}

func serviceCmd(env Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "service",
		Short: "Install or remove the per-user background service (launchd on macOS, systemd on Linux, Task Scheduler on Windows)",
	}
	c.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Start the agent at login and keep it running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, restarted, err := installService(env)
			if err != nil {
				return err
			}
			if restarted {
				fmt.Fprintln(env.Out, "Restarted the agent.")
				return nil
			}
			fmt.Fprintf(env.Out, "Service installed (%s) and started. Check it with: harnext-agent status\n", where(f))
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "uninstall",
		Short: "Stop the service and remove it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := env.serviceOptions()
			if err != nil {
				return err
			}
			f, err := svcmgr.Uninstall(o)
			if err != nil {
				return err
			}
			fmt.Fprintf(env.Out, "Service removed (%s). Settings, token and local data were kept.\n", where(f))
			return nil
		},
	})
	return c
}
