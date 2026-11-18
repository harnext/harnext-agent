package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/harnext/harnext-agent/internal/service"
	"github.com/harnext/harnext-agent/internal/vscodecfg"
	"github.com/harnext/harnext-agent/internal/watcher"
)

// Package-level so tests never touch the real VS Code settings or need a real terminal.
var (
	vscodeRoots = vscodecfg.Roots
	isTerminal  = func(r io.Reader) bool {
		f, ok := r.(*os.File)
		if !ok {
			return false
		}
		fi, err := f.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
)

func copilotCmd(env Env) *cobra.Command {
	var path string
	var useDefault, apply, revert, yes, force, restart bool
	c := &cobra.Command{
		Use:   "copilot",
		Short: "Show or set the VS Code Copilot telemetry file and the settings to enable it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("path") && useDefault {
				return errors.New("use either --path or --default, not both")
			}
			if apply && revert {
				return errors.New("use either --apply or --revert, not both")
			}
			if revert && (cmd.Flags().Changed("path") || useDefault) {
				return errors.New("--revert cannot be combined with --path or --default")
			}
			settings, err := env.Paths.LoadSettings()
			initialized := err == nil
			before := settings.CopilotOTelPath
			changing := cmd.Flags().Changed("path") || useDefault
			switch {
			case errors.Is(err, service.ErrNotInitialized):
				if changing {
					return errors.New("not initialized: run `harnext-agent init` first")
				}
			case err != nil:
				return err
			}
			if changing {
				if useDefault {
					path = ""
				}
				if (!useDefault && path == "") || service.ValidateCopilotOTelPath(path) != nil {
					return errors.New("--path must be an absolute path without control characters")
				}
				settings.CopilotOTelPath = path
				if err := env.Paths.SaveSettings(settings); err != nil {
					return err
				}
			}
			switch {
			case revert:
				return revertVSCode(env, settings)
			case apply:
				if err := applyVSCode(env, settings, initialized, changing, yes, force); err != nil {
					return err
				}
				return afterSettingChange(env, before, restart)
			}
			if err := printCopilotBlock(env, env.Paths.CopilotOTelFile(settings)); err != nil {
				return err
			}
			fmt.Fprintln(env.Out, "Run `harnext-agent copilot --apply` to configure it automatically.")
			for _, p := range vscodecfg.Targets(vscodeRoots()) {
				if d, err := vscodecfg.Read(p); err == nil {
					warnCapture(env, d)
				}
			}
			return afterSettingChange(env, before, restart)
		},
	}
	c.Flags().StringVar(&path, "path", "", "absolute path of the telemetry file to read")
	c.Flags().BoolVar(&useDefault, "default", false, "go back to the default telemetry file")
	c.Flags().BoolVar(&apply, "apply", false, "write the settings into the VS Code settings.json (a backup is kept)")
	c.Flags().BoolVar(&revert, "revert", false, "remove the settings written by --apply (the backup is left untouched)")
	c.Flags().BoolVar(&yes, "yes", false, "with --apply: do not ask for confirmation")
	c.Flags().BoolVar(&force, "force", false, "with --apply: overwrite a different exporterType")
	c.Flags().BoolVar(&restart, "restart", false, "restart the agent (through the service) if the setting it reads changed")
	return c
}

// afterSettingChange covers the running agent, which reads its settings only at start.
func afterSettingChange(env Env, before string, restart bool) error {
	cur, err := env.Paths.LoadSettings()
	if err != nil || cur.CopilotOTelPath == before {
		return nil
	}
	if _, running := service.RunningPID(env.Paths.Lock); !running {
		return nil
	}
	switch {
	case !restart:
		fmt.Fprintln(env.Out, "The running agent reads this setting at start: run `harnext-agent service install` to restart it.")
	case !serviceInstalled(env):
		fmt.Fprintln(env.Out, "No service is installed, so the agent was not restarted: run `harnext-agent service install` to install and restart it, or `harnext-agent stop` and `harnext-agent run`.")
	default:
		if _, _, err := installService(env); err != nil {
			return err
		}
		fmt.Fprintln(env.Out, "Restarted the agent.")
	}
	return nil
}

func warnCapture(env Env, d vscodecfg.Doc) {
	if vals, err := vscodecfg.Inspect(d); err == nil && vals[vscodecfg.KeyCapture] == true {
		fmt.Fprintf(env.Out, "WARNING: %s has captureContent enabled: prompts are written to the telemetry file. Turn it off.\n", d.Path)
	}
}

type vscodePlan struct {
	doc     vscodecfg.Doc
	text    string
	changes []vscodecfg.Change
}

func applyVSCode(env Env, settings service.Settings, initialized, explicit, yes, force bool) error {
	w := env.Out
	file := env.Paths.CopilotOTelFile(settings)
	paths := vscodecfg.Targets(vscodeRoots())
	if len(paths) == 0 {
		// The installer passes --yes for everyone who left Copilot on; without VS Code this is not an error.
		if yes {
			fmt.Fprintln(w, "VS Code not found, so Copilot tracking was skipped. After installing VS Code, run: harnext-agent copilot --apply --restart")
			return nil
		}
		fmt.Fprintln(w, "No VS Code settings folder found; add the lines manually.")
		return printCopilotBlock(env, file)
	}
	docs := make([]vscodecfg.Doc, 0, len(paths))
	var failed int
	manual := false
	for _, p := range paths {
		d, err := vscodecfg.Read(p)
		if err != nil {
			reportUnusable(env, p, err, &manual, &failed)
			continue
		}
		docs = append(docs, d)
	}

	valid := func(p string) bool { return p != "" && filepath.IsAbs(p) && service.ValidateCopilotOTelPath(p) == nil }
	effective, adopted := file, ""
	if !explicit {
		for _, d := range docs {
			if vals, err := vscodecfg.Inspect(d); err == nil {
				if s, ok := vals[vscodecfg.KeyOutfile].(string); ok && valid(s) {
					adopted = s
					break
				}
			}
		}
		if adopted != "" {
			effective = adopted
		}
	}
	accept := valid
	if explicit {
		accept = func(p string) bool { return p == effective }
	}

	var plans, pending []vscodePlan
	for _, d := range docs {
		warnCapture(env, d)
		text, changes, err := vscodecfg.Apply(d, vscodecfg.Want{Outfile: effective, Force: force, AcceptOutfile: accept})
		if err != nil {
			reportUnusable(env, d.Path, err, &manual, &failed)
			continue
		}
		pl := vscodePlan{d, text, changes}
		for _, c := range changes {
			switch {
			case c.Key == vscodecfg.KeyExporter && c.Action == "kept":
				fmt.Fprintf(w, "WARNING: %s: your VS Code sends telemetry to a collector; the agent reads a file — set exporterType to file or run with --force\n", d.Path)
			case c.Key == vscodecfg.KeyOutfile && c.Action == "updated" && c.Old != nil:
				fmt.Fprintf(w, "WARNING: %s: the existing outfile is not a usable absolute path and was replaced\n", d.Path)
			case c.Key == vscodecfg.KeyOutfile && c.Action == "kept" && c.Old != effective:
				fmt.Fprintf(w, "WARNING: %s: its outfile differs from the file the agent reads (%s)\n", d.Path, effective)
			}
		}
		plans = append(plans, pl)
		if vscodecfg.Changed(changes) {
			pending = append(pending, pl)
		}
	}

	settingsChange := adopted != "" && adopted != file
	if len(pending) == 0 && !settingsChange {
		for _, pl := range plans {
			fmt.Fprintf(w, "Already configured: %s\n", pl.doc.Path)
		}
		return finishApply(env, manual, failed, effective, 0)
	}
	if settingsChange && !initialized {
		return errors.New("not initialized: run `harnext-agent init` first")
	}
	if !yes {
		if !isTerminal(env.In) {
			if err := printCopilotBlock(env, effective); err != nil {
				return err
			}
			fmt.Fprintln(w, "Nothing was written: re-run with --apply --yes.")
			return nil
		}
		if len(pending) > 0 {
			names := make([]string, len(pending))
			for i, pl := range pending {
				names[i] = pl.doc.Path
			}
			fmt.Fprintf(w, "Update VS Code settings at %s? [Y/n] ", strings.Join(names, ", "))
			line, _ := bufio.NewReader(env.In).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "" && a != "y" && a != "yes" {
				fmt.Fprintln(w, "Nothing was written.")
				return nil
			}
		}
	}

	if settingsChange {
		settings.CopilotOTelPath = adopted
		if err := env.Paths.SaveSettings(settings); err != nil {
			return err
		}
		fmt.Fprintf(w, "Agent now reads the outfile already set in VS Code: %s\n", adopted)
	}
	wrote := 0
	for _, pl := range plans {
		if !vscodecfg.Changed(pl.changes) {
			fmt.Fprintf(w, "Already configured: %s\n", pl.doc.Path)
			continue
		}
		backup, err := vscodecfg.Write(pl.doc, pl.text)
		switch {
		case err != nil:
			fmt.Fprintf(env.Err, "Could not write %s: %v\n", pl.doc.Path, err)
			failed++
		case backup == "":
			fmt.Fprintf(w, "Created %s\n", pl.doc.Path)
			wrote++
		default:
			fmt.Fprintf(w, "Updated %s (backup: %s)\n", pl.doc.Path, backup)
			wrote++
		}
	}
	return finishApply(env, manual, failed, effective, wrote)
}

func reportUnusable(env Env, path string, err error, manual *bool, failed *int) {
	if errors.Is(err, vscodecfg.ErrUnsafe) || errors.Is(err, vscodecfg.ErrTooLarge) {
		fmt.Fprintf(env.Out, "Left %s untouched: it cannot be edited safely (%v).\n", path, err)
		*manual = true
		return
	}
	fmt.Fprintf(env.Err, "Could not read %s: %v\n", path, err)
	*failed++
}

func finishApply(env Env, manual bool, failed int, file string, wrote int) error {
	if manual {
		fmt.Fprintln(env.Out, "Add these lines to the file(s) above manually:")
		if err := printCopilotBlock(env, file); err != nil {
			return err
		}
	}
	if wrote > 0 {
		fmt.Fprintln(env.Out, "Reload the VS Code window (Cmd/Ctrl+Shift+P → Developer: Reload Window), then send one Copilot chat message.")
	}
	if failed > 0 {
		return errors.New("some VS Code settings files could not be updated")
	}
	return nil
}

func revertVSCode(env Env, settings service.Settings) error {
	w := env.Out
	file := env.Paths.CopilotOTelFile(settings)
	paths := vscodecfg.Targets(vscodeRoots())
	if len(paths) == 0 {
		fmt.Fprintln(w, "No VS Code settings folder found; nothing to revert.")
		return nil
	}
	var failed int
	for _, p := range paths {
		d, err := vscodecfg.Read(p)
		var text string
		var changes []vscodecfg.Change
		if err == nil {
			text, changes, err = vscodecfg.Remove(d, vscodecfg.RemoveWant{Outfile: file})
		}
		if err != nil {
			if errors.Is(err, vscodecfg.ErrUnsafe) || errors.Is(err, vscodecfg.ErrTooLarge) {
				fmt.Fprintf(w, "Left %s untouched: it cannot be edited safely; remove the github.copilot.chat.otel.* lines manually.\n", p)
			} else {
				fmt.Fprintf(env.Err, "Could not read %s: %v\n", p, err)
				failed++
			}
			continue
		}
		for _, c := range changes {
			if c.Action == "kept" {
				fmt.Fprintf(w, "Kept %s in %s: its value differs from what harnext wrote\n", c.Key, p)
			}
		}
		if !vscodecfg.Changed(changes) {
			fmt.Fprintf(w, "Nothing to revert: %s\n", p)
			continue
		}
		backup, err := vscodecfg.Write(d, text)
		if err != nil {
			fmt.Fprintf(env.Err, "Could not write %s: %v\n", p, err)
			failed++
			continue
		}
		fmt.Fprintf(w, "Reverted %s (backup left untouched: %s)\n", p, backup)
	}
	if failed > 0 {
		return errors.New("some VS Code settings files could not be reverted")
	}
	return nil
}

func printCopilotBlock(env Env, file string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(file); err != nil {
		return errors.New("could not encode the telemetry path")
	}
	quoted := strings.TrimSpace(buf.String())
	w := env.Out
	fmt.Fprintf(w, "Telemetry file: %s\n\n", file)
	fmt.Fprintln(w, "Add these lines to the VS Code settings.json:")
	fmt.Fprintln(w, `  "github.copilot.chat.otel.enabled": true,`)
	fmt.Fprintln(w, `  "github.copilot.chat.otel.exporterType": "file",`)
	fmt.Fprintf(w, "  \"github.copilot.chat.otel.outfile\": %s\n\n", quoted)
	fmt.Fprintln(w, "Do not enable captureContent: the file would then contain prompts and stay on disk (the agent never reads content).")
	return nil
}

// printCopilotStatus skips silently when the status file predates the copilot section.
func printCopilotStatus(env Env, st service.StatusFile) {
	data, err := os.ReadFile(env.Paths.Status)
	if err != nil {
		return
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	if _, ok := raw["copilot"]; !ok {
		return
	}
	w := env.Out
	c := st.Copilot
	if !c.File {
		fmt.Fprintln(w, "Copilot:   telemetry file not found yet — run `harnext-agent copilot` to enable")
		return
	}
	fmt.Fprintf(w, "Copilot:   %d sessions, %d spans, %d background spans, %d unreadable lines skipped\n", c.Sessions, c.UniqueSpans, c.BackgroundSpans, c.SkippedLines)
	fmt.Fprintf(w, "           chat history: %d conversations, %d requests, %d unreadable lines skipped\n", c.ChatSessions, c.ChatRequests, c.ChatSkippedLines)
	if c.TracesBound+c.TracesUnbound > 0 {
		// How reliably telemetry traces were tied to chat requests (the folder split of Copilot tokens).
		fmt.Fprintf(w, "           folders: %d traces tied to a request, %d without one; credits of %d/%d checked requests match\n",
			c.TracesBound, c.TracesUnbound, c.TraceMatches, c.TraceChecks)
	}
	// The agent empties only its own default file; a file the user pointed VS Code at is never touched.
	if !c.OwnFile && c.FileBytes > watcher.DefaultTrimOver {
		fmt.Fprintf(w, "           telemetry file is %d MB and keeps growing; the agent doesn't trim files outside its folder. You can delete it while VS Code is closed.\n", c.FileBytes>>20)
	}
}
