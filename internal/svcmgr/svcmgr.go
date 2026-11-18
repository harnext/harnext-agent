// Package svcmgr installs the agent as a per-user background service: a launchd LaunchAgent
// on macOS, a systemd user unit on Linux and a logon Task Scheduler task on Windows. No elevated
// privileges are involved and the definitions hold no secrets (the token stays in the agent directory).
package svcmgr

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

const (
	Label       = "com.harnext.agent"
	UnitName    = "harnext-agent.service"
	TaskName    = "HarnextAgent"
	maxCmdOut   = 300
	maxPathLen  = 1024
	logDirMacOS = "Library/Logs/harnext"
)

// Runner runs an external command and returns its trimmed combined output.
type Runner func(name string, args ...string) (string, error)

type Options struct {
	GOOS     string
	Exe      string // absolute path of the agent binary
	UserHome string
	UID      int
	// HarnextHome is exported to the service as HARNEXT_HOME when non-empty; unsupported on Windows
	// because schtasks cannot pass environment variables.
	HarnextHome string
	Run         Runner
}

// Defaults fills the options from the running process.
func Defaults(goos string) (Options, error) {
	exe, err := os.Executable()
	if err != nil {
		return Options{}, errors.New("svcmgr: cannot locate the agent binary")
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Options{}, errors.New("svcmgr: cannot locate the home directory")
	}
	return Options{GOOS: goos, Exe: exe, UserHome: home, UID: os.Getuid(), HarnextHome: os.Getenv("HARNEXT_HOME"), Run: execRunner}, nil
}

func execRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if len(s) > maxCmdOut {
		s = s[:maxCmdOut]
	}
	return s, err
}

// File is a generated service definition.
type File struct {
	Path        string // empty on Windows: the task lives in Task Scheduler, not in a file
	Content     string // on Windows: the task command line
	Mode        os.FileMode
	Description string // human-readable location when Path is empty
}

var ErrUnsupported = errors.New("svcmgr: services are supported on macOS, Linux and Windows only")

func isWindowsAbs(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}

func validateExe(goos, p string) error {
	abs := filepath.IsAbs(p)
	if goos == "windows" {
		abs = isWindowsAbs(p)
	}
	if !abs || len(p) > maxPathLen {
		return errors.New("svcmgr: the binary path must be absolute")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return errors.New("svcmgr: the binary path contains control characters")
		}
		if goos == "windows" && strings.ContainsRune(`"%&|<>^`, r) {
			return errors.New("svcmgr: the binary path contains characters that are not allowed")
		}
	}
	return nil
}

var plistTmpl = template.Must(template.New("plist").Funcs(template.FuncMap{"x": html.EscapeString}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{x .Exe}}</string>
		<string>run</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardErrorPath</key>
	<string>{{x .Log}}</string>
	<key>StandardOutPath</key>
	<string>{{x .Log}}</string>
{{- if .Home}}
	<key>EnvironmentVariables</key>
	<dict>
		<key>HARNEXT_HOME</key>
		<string>{{x .Home}}</string>
	</dict>
{{- end}}
</dict>
</plist>
`))

var unitTmpl = template.Must(template.New("unit").Parse(`[Unit]
Description=Harnext agent
After=network-online.target

[Service]
Type=simple
ExecStart={{.Exec}}
Restart=on-failure
RestartSec=30
NoNewPrivileges=true
PrivateTmp=true
{{- if .Home}}
Environment={{.Home}}
{{- end}}

[Install]
WantedBy=default.target
`))

// systemdQuote quotes one word of a unit file; % starts a specifier and \ and " are escapes.
// In ExecStart a $ starts a variable and must be doubled; Environment= does not expand it.
func systemdQuote(s string, exec bool) string {
	pairs := []string{`\`, `\\`, `"`, `\"`, `%`, `%%`}
	if exec {
		pairs = append(pairs, `$`, `$$`)
	}
	return `"` + strings.NewReplacer(pairs...).Replace(s) + `"`
}

// Render returns the service definition for o.GOOS.
func Render(o Options) (File, error) {
	if err := validateExe(o.GOOS, o.Exe); err != nil {
		return File{}, err
	}
	if o.GOOS == "windows" {
		if o.HarnextHome != "" {
			return File{}, errors.New("svcmgr: a custom HARNEXT_HOME is not supported for the Windows service; unset it")
		}
		return File{Content: `"` + o.Exe + `" run`, Description: "Task Scheduler task " + TaskName}, nil
	}
	if strings.IndexFunc(o.HarnextHome, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return File{}, errors.New("svcmgr: HARNEXT_HOME contains control characters")
	}
	var buf bytes.Buffer
	switch o.GOOS {
	case "darwin":
		data := map[string]string{
			"Label": Label, "Exe": o.Exe, "Home": o.HarnextHome,
			"Log": filepath.Join(o.UserHome, logDirMacOS, "agent.log"),
		}
		if err := plistTmpl.Execute(&buf, data); err != nil {
			return File{}, errors.New("svcmgr: render")
		}
		return File{Path: filepath.Join(o.UserHome, "Library", "LaunchAgents", Label+".plist"), Content: buf.String(), Mode: 0o644}, nil
	case "linux":
		data := map[string]string{"Exec": systemdQuote(o.Exe, true) + " run"}
		if o.HarnextHome != "" {
			data["Home"] = systemdQuote("HARNEXT_HOME="+o.HarnextHome, false)
		}
		if err := unitTmpl.Execute(&buf, data); err != nil {
			return File{}, errors.New("svcmgr: render")
		}
		return File{Path: filepath.Join(o.UserHome, ".config", "systemd", "user", UnitName), Content: buf.String(), Mode: 0o644}, nil
	}
	return File{}, ErrUnsupported
}

func runStep(o Options, name string, args ...string) error {
	out, err := o.Run(name, args...)
	if err != nil {
		if out != "" {
			return fmt.Errorf("svcmgr: %s failed: %s", name, out)
		}
		return fmt.Errorf("svcmgr: %s failed", name)
	}
	return nil
}

// Install writes the definition and starts the service.
func Install(o Options) (File, error) {
	f, err := Render(o)
	if err != nil {
		return File{}, err
	}
	if o.GOOS == "windows" {
		// ONLOGON + LIMITED runs in the user session without elevation.
		if err := runStep(o, "schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", TaskName, "/TR", f.Content, "/RL", "LIMITED"); err != nil {
			return f, err
		}
		return f, runStep(o, "schtasks", "/Run", "/TN", TaskName)
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return File{}, errors.New("svcmgr: create service directory")
	}
	if o.GOOS == "darwin" {
		if err := os.MkdirAll(filepath.Join(o.UserHome, logDirMacOS), 0o700); err != nil {
			return File{}, errors.New("svcmgr: create log directory")
		}
	}
	if err := os.WriteFile(f.Path, []byte(f.Content), f.Mode); err != nil {
		return File{}, errors.New("svcmgr: write service file")
	}
	switch o.GOOS {
	case "darwin":
		domain := fmt.Sprintf("gui/%d", o.UID)
		_, _ = o.Run("launchctl", "bootout", domain+"/"+Label) // not loaded yet is fine
		if err := runStep(o, "launchctl", "bootstrap", domain, f.Path); err != nil {
			return f, err
		}
	case "linux":
		if err := runStep(o, "systemctl", "--user", "daemon-reload"); err != nil {
			return f, err
		}
		if err := runStep(o, "systemctl", "--user", "enable", "--now", UnitName); err != nil {
			return f, err
		}
	}
	return f, nil
}

// Uninstall stops the service and removes its definition. A missing service is not an error.
func Uninstall(o Options) (File, error) {
	if o.GOOS == "windows" {
		o.HarnextHome = "" // irrelevant for removal
		f, err := Render(o)
		if err != nil {
			return File{}, err
		}
		// Query first: schtasks messages are localized, so a missing task is detected by exit status.
		if _, err := o.Run("schtasks", "/Query", "/TN", TaskName); err != nil {
			return f, nil
		}
		_, _ = o.Run("schtasks", "/End", "/TN", TaskName)
		return f, runStep(o, "schtasks", "/Delete", "/F", "/TN", TaskName)
	}
	f, err := Render(o)
	if err != nil {
		return File{}, err
	}
	switch o.GOOS {
	case "darwin":
		_, _ = o.Run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", o.UID, Label))
	case "linux":
		_, _ = o.Run("systemctl", "--user", "disable", "--now", UnitName)
	}
	if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return f, errors.New("svcmgr: remove service file")
	}
	if o.GOOS == "linux" {
		_, _ = o.Run("systemctl", "--user", "daemon-reload")
	}
	return f, nil
}
