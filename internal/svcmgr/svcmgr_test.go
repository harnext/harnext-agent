package svcmgr

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type call struct{ name, args string }

func recorder(fail map[string]bool) (Runner, *[]call) {
	var calls []call
	return func(name string, args ...string) (string, error) {
		c := call{name, strings.Join(args, " ")}
		calls = append(calls, c)
		if fail[name+" "+firstArg(args)] {
			return "boom detail", errors.New("exit 1")
		}
		return "", nil
	}, &calls
}

func firstArg(a []string) string {
	for _, s := range a {
		if !strings.HasPrefix(s, "--") {
			return s
		}
	}
	return ""
}

func opts(t *testing.T, goos string) Options {
	t.Helper()
	run, _ := recorder(nil)
	return Options{GOOS: goos, Exe: "/Applications/Harnext Agent/harnext-agent", UserHome: t.TempDir(), UID: 501, Run: run}
}

func TestRenderPlist(t *testing.T) {
	o := opts(t, "darwin")
	o.Exe = `/Users/a&b/<x>/harnext "agent"`
	o.HarnextHome = "/tmp/h&h"
	f, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	if f.Path != filepath.Join(o.UserHome, "Library", "LaunchAgents", "com.harnext.agent.plist") {
		t.Fatalf("path = %s", f.Path)
	}
	// Well-formed XML even with hostile characters, and they survive a round trip.
	dec := xml.NewDecoder(strings.NewReader(f.Content))
	dec.Strict = false
	var texts []string
	for {
		tok, err := dec.Token()
		if err != nil {
			if err != io.EOF {
				t.Fatalf("plist is not well-formed: %v", err)
			}
			break
		}
		if cd, ok := tok.(xml.CharData); ok {
			if s := strings.TrimSpace(string(cd)); s != "" {
				texts = append(texts, s)
			}
		}
	}
	joined := strings.Join(texts, "\n")
	for _, want := range []string{o.Exe, "run", "com.harnext.agent", o.HarnextHome} {
		if !strings.Contains(joined, want) {
			t.Fatalf("plist lacks %q after XML decoding:\n%s", want, f.Content)
		}
	}
	for _, want := range []string{"<key>RunAtLoad</key>", "<key>SuccessfulExit</key>", "<false/>", "<key>ThrottleInterval</key>"} {
		if !strings.Contains(f.Content, want) {
			t.Fatalf("plist lacks %s", want)
		}
	}
}

func TestRenderPlistWithoutHomeHasNoEnvironment(t *testing.T) {
	f, err := Render(opts(t, "darwin"))
	if err != nil || strings.Contains(f.Content, "EnvironmentVariables") {
		t.Fatalf("err=%v content=%s", err, f.Content)
	}
}

func TestRenderUnitQuoting(t *testing.T) {
	o := opts(t, "linux")
	o.Exe = `/opt/my agent/100%/$HOME/"q"`
	o.HarnextHome = `/home/a b/%h`
	f, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	if f.Path != filepath.Join(o.UserHome, ".config", "systemd", "user", "harnext-agent.service") {
		t.Fatalf("path = %s", f.Path)
	}
	for _, want := range []string{
		`ExecStart="/opt/my agent/100%%/$$HOME/\"q\"" run`,
		`Environment="HARNEXT_HOME=/home/a b/%%h"`,
		"Restart=on-failure", "NoNewPrivileges=true", "WantedBy=default.target",
	} {
		if !strings.Contains(f.Content, want) {
			t.Fatalf("unit lacks %s:\n%s", want, f.Content)
		}
	}
}

func TestRenderRejectsUnsafeInput(t *testing.T) {
	for name, mutate := range map[string]func(*Options){
		"relative exe":    func(o *Options) { o.Exe = "harnext-agent" },
		"newline in exe":  func(o *Options) { o.Exe = "/bin/a\nExecStartPre=/bin/evil" },
		"newline in home": func(o *Options) { o.HarnextHome = "/h\nEnvironment=X=1" },
		"unsupported os":  func(o *Options) { o.GOOS = "plan9" },
		"very long exe":   func(o *Options) { o.Exe = "/" + strings.Repeat("a", 2000) },
	} {
		o := opts(t, "linux")
		mutate(&o)
		if _, err := Render(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestInstallDarwin(t *testing.T) {
	o := opts(t, "darwin")
	run, calls := recorder(nil)
	o.Run = run
	f, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(f.Path); err != nil || string(b) != f.Content {
		t.Fatalf("file not written: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(o.UserHome, "Library", "Logs", "harnext")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("log dir: %v", err)
	}
	want := []call{
		{"launchctl", "bootout gui/501/com.harnext.agent"},
		{"launchctl", "bootstrap gui/501 " + f.Path},
	}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestInstallLinux(t *testing.T) {
	o := opts(t, "linux")
	run, calls := recorder(nil)
	o.Run = run
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	want := []call{{"systemctl", "--user daemon-reload"}, {"systemctl", "--user enable --now harnext-agent.service"}}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("calls = %v", *calls)
	}
}

const winExe = `C:\Users\A B\harnext\harnext-agent.exe`

func winOpts(t *testing.T) Options {
	o := opts(t, "windows")
	o.Exe = winExe
	return o
}

func TestInstallWindows(t *testing.T) {
	o := winOpts(t)
	run, calls := recorder(nil)
	o.Run = run
	f, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if f.Path != "" {
		t.Fatalf("path = %q", f.Path)
	}
	want := []call{
		{"schtasks", `/Create /F /SC ONLOGON /TN HarnextAgent /TR "C:\Users\A B\harnext\harnext-agent.exe" run /RL LIMITED`},
		{"schtasks", "/Run /TN HarnextAgent"},
	}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestWindowsRejectsUnsafeInput(t *testing.T) {
	bad := []string{
		`relative\a.exe`, `/usr/bin/a`, `C:`, `\\server\share\a.exe`,
		`C:\a"b\a.exe`, `C:\a%PATH%\a.exe`, `C:\a&b\a.exe`, `C:\a|b\a.exe`,
		`C:\a<b\a.exe`, `C:\a>b\a.exe`, `C:\a^b\a.exe`, "C:\\a\nb\\a.exe",
	}
	for _, p := range bad {
		o := winOpts(t)
		o.Exe = p
		if _, err := Render(o); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
	o := winOpts(t)
	o.HarnextHome = `D:\custom`
	if _, err := Install(o); err == nil {
		t.Error("HARNEXT_HOME accepted")
	}
}

func TestUninstallWindows(t *testing.T) {
	o := winOpts(t)
	run, calls := recorder(nil)
	o.Run = run
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 3 || (*calls)[2] != (call{"schtasks", "/Delete /F /TN HarnextAgent"}) {
		t.Fatalf("calls = %v", *calls)
	}
	// A missing task (query fails) is not an error and nothing is deleted.
	run, calls = recorder(map[string]bool{"schtasks /Query": true})
	o.Run = run
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestInstallReportsCommandFailure(t *testing.T) {
	o := opts(t, "darwin")
	run, _ := recorder(map[string]bool{"launchctl bootstrap": true})
	o.Run = run
	_, err := Install(o)
	if err == nil || !strings.Contains(err.Error(), "launchctl failed: boom detail") {
		t.Fatalf("err = %v", err)
	}
}

func TestUninstall(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		o := opts(t, goos)
		if _, err := Install(o); err != nil {
			t.Fatal(err)
		}
		run, calls := recorder(map[string]bool{"launchctl bootout": true, "systemctl disable": true})
		o.Run = run
		f, err := Uninstall(o)
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		if _, err := os.Stat(f.Path); !os.IsNotExist(err) {
			t.Fatalf("%s: file still exists", goos)
		}
		if len(*calls) == 0 {
			t.Fatalf("%s: no commands", goos)
		}
		// Uninstalling twice is fine.
		if _, err := Uninstall(o); err != nil {
			t.Fatalf("%s: second uninstall: %v", goos, err)
		}
	}
}
