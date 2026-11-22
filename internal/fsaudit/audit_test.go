package fsaudit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// audited lists the standard library calls that read files, discover files, read the environment, start programs or open a
// network connection. Every call in production code must be listed in allowed below with the reason, so a new way to look at
// the user's machine cannot be added without this test failing and a reviewer reading the reason.
var audited = map[string][]string{
	"os":            {"Open", "OpenFile", "ReadFile", "ReadDir", "Stat", "Lstat", "Getenv", "LookupEnv", "Environ"},
	"io/ioutil":     {"ReadFile", "ReadDir"},
	"path/filepath": {"Walk", "WalkDir", "Glob"},
	"os/exec":       {"Command", "CommandContext"},
	"net/http":      {"NewRequest", "NewRequestWithContext", "Get", "Post", "PostForm", "Head"},
	"net":           {"Dial", "DialTimeout", "Listen"},
}

type use struct{ file, call string }

// allowed maps a production file to the audited calls it makes and why. Paths are relative to the agent module. Files that
// hold user content are read in exactly four places: the watchers (transcripts, chat sessions, the Copilot telemetry file),
// project (git remote), account (the Claude account e-mail, hashed at once) and copilotid (the GitHub account Copilot uses, read from VS Code's state database; the numeric id is hashed at once). Everything else is the agent's own state, the service manager or the update check.
var allowed = map[use]string{
	// User content: the only places the agent reads files that the user's tools wrote.
	{"internal/watcher/watcher.go", "filepath.WalkDir"}: "lists *.jsonl transcripts under each Claude config directory's projects folder (~/.claude/projects, ~/.claude-work/projects ...) and the Copilot telemetry file's directory; contents are read only by followFile",
	{"internal/watcher/watcher.go", "os.Open"}:          "followFile: opens one *.jsonl transcript, chat session or telemetry file found by the watchers and reads its new lines",
	{"internal/watcher/watcher.go", "os.Lstat"}:         "skips symlinks and non-regular files before a transcript is read",
	{"internal/watcher/watcher.go", "os.Stat"}:          "checks that a transcript root (a config directory's projects folder) exists",
	{"internal/watcher/discover.go", "os.ReadDir"}:      "lists the names directly under the home directory only to find Claude Code config directories (.claude*); nothing else in it is read",
	{"internal/watcher/discover.go", "os.Lstat"}:        "checks that <config directory>/projects is a real directory (not a symlink) before it is watched; the directory is not listed here",
	{"internal/watcher/discover.go", "os.Getenv"}:       "CLAUDE_CONFIG_DIR only, the agent's own copy of Claude Code's config directory setting; no other variable is read",
	{"internal/watcher/tail.go", "os.Lstat"}:            "checks that the Copilot telemetry file is a regular file before it is followed",
	{"internal/watcher/chat.go", "os.ReadDir"}:          "lists <workspaceStorage>/<hash>/chatSessions/ only; names are matched, nothing else in a workspace is listed",
	{"internal/watcher/chat.go", "os.Lstat"}:            "checks that chatSessions is a real directory and that a session file is a regular file",
	{"internal/service/settings.go", "os.Open"}:         "readLimited: size-capped read of the agent's own files and of a workspace.json (its folder is reduced to a project identity)",
	{"internal/project/project.go", "os.Open"}:          "readSmall: size-capped read of .git (worktree pointer), .git/commondir and .git/config only, to find the git remote; credentials in it are dropped",
	{"internal/project/project.go", "os.Stat"}:          "looks up whether a project marker name (package.json, go.mod, .git ...) exists; the file is never opened",

	{"internal/account/account.go", "os.Lstat"}:         "refuses symlinks and non-regular files before the Claude global config is read",
	{"internal/account/account.go", "os.Open"}:          "ClaudeEmail: streams .claude.json and keeps only oauthAccount.emailAddress; the e-mail is hashed at once and never stored or logged",
	{"internal/attribution/gate.go", "os.Lstat"}:        "size and mtime of a Claude global config (.claude.json), only to skip parsing it again while it is unchanged; the file is not opened here",
	{"internal/storage/storage.go", "os.OpenFile"}:      "creates the agent's local database file (mode 0600)",
	{"internal/remoteconfig/store.go", "os.Open"}:       "reads the cached signed configuration the agent wrote",
	{"internal/copilotid/reader.go", "os.Lstat"}:        "checks that VS Code's state database (and its write-ahead log) is a regular file before a private copy is made",
	{"internal/copilotid/reader.go", "os.Open"}:         "reads a copy source: the editor's state database, only to copy it; the keys read from the copy are the GitHub user name, its numeric id (hashed at once), the Copilot plan and the list of user names that used Copilot Chat; tokens in the OS keychain are never touched",
	{"internal/copilotid/reader.go", "os.OpenFile"}:     "creates the private temporary copy (0600, exclusive) that is queried read-only and deleted at once",
	{"internal/identity/store.go", "os.Open"}:           "reads the cached signed identity document the agent wrote",
	{"internal/service/lock_unix.go", "os.OpenFile"}:    "single-instance lock file in the agent's directory",
	{"internal/service/lock_unix.go", "os.Open"}:        "reads the PID from the agent's own lock file",
	{"internal/service/lock_windows.go", "os.OpenFile"}: "single-instance lock file in the agent's directory",
	{"internal/service/lock_windows.go", "os.Open"}:     "reads the PID from the agent's own lock file",
	{"internal/service/run.go", "os.Lstat"}:             "size and existence of the Copilot telemetry file for the status line",
	{"internal/cli/copilot.go", "os.ReadFile"}:          "reads the agent's own status file",
	{"internal/cli/preview.go", "os.Stat"}:              "checks that the agent's database exists",
	{"internal/cli/status.go", "os.Stat"}:               "checks that the agent's database exists",
	{"internal/cli/service.go", "os.Stat"}:              "checks whether the agent's service file exists",

	// `scan`: asks for consent on the terminal because standard input is a pipe under `curl | sh`.
	{"internal/cli/tty_unix.go", "os.Open"}:    "opens /dev/tty only to read the user's y/n answers before anything is sent; nothing else is read from it",
	{"internal/cli/tty_windows.go", "os.Open"}: "opens the console input (CONIN$) only to read the user's y/n answers before anything is sent",

	// VS Code settings: only on `copilot --apply` / `--revert`, with the user's confirmation.
	{"internal/vscodecfg/settings.go", "os.Stat"}:     "looks for the VS Code user directory that holds settings.json",
	{"internal/vscodecfg/settings.go", "os.ReadFile"}: "reads settings.json to add the Copilot telemetry keys, backed up first; reverted by --revert",
	{"internal/vscodecfg/settings.go", "os.Lstat"}:    "refuses to write through a symlink",
	{"internal/vscodecfg/settings.go", "os.OpenFile"}: "creates the settings.json backup",

	// Environment: names the agent itself defines. The environment is never read for secrets.
	{"internal/cli/cli.go", "os.Getenv"}:             "HARNEXT_TOKEN: the agent token given to `init`",
	{"internal/service/run.go", "os.Getenv"}:         "HARNEXT_NO_AUTO_UPDATE switch",
	{"internal/service/settings.go", "os.Getenv"}:    "HARNEXT_HOME: where the agent keeps its own files",
	{"internal/svcmgr/svcmgr.go", "os.Getenv"}:       "HARNEXT_HOME for the service definition",
	{"internal/selfupdate/environ.go", "os.Environ"}: "hands the unchanged environment to the restarted new agent binary; never read or sent",

	// Other programs.
	{"internal/svcmgr/svcmgr.go", "exec.Command"}:                "launchctl, systemctl and schtasks to install or remove the service",
	{"internal/selfupdate/selfupdate.go", "exec.CommandContext"}: "runs the downloaded, signature-checked new binary with --help before it replaces the old one",
	{"internal/selfupdate/restart_windows.go", "exec.Command"}:   "starts the updated agent on Windows",
	{"internal/cli/stop_windows.go", "exec.Command"}:             "schtasks and taskkill to stop the agent on Windows",

	// Network: the server calls and nothing else.
	{"internal/scan/send.go", "http.NewRequestWithContext"}: "`scan`: one POST of the card numbers to /api/v1/cards/drafts, only after the user confirmed the exact body",

	{"internal/uploader/uploader.go", "http.NewRequestWithContext"}:     "POST usage numbers to /api/v1/ingest",
	{"internal/remoteconfig/client.go", "http.NewRequestWithContext"}:   "GET the signed configuration from /api/v1/agent-config",
	{"internal/identity/client.go", "http.NewRequestWithContext"}:       "GET the signed identity document from /api/v1/agent-identity and POST the masked account report (hash, masked label, session count) to /api/v1/agent-accounts",
	{"internal/selfupdate/selfupdate.go", "http.NewRequestWithContext"}: "GET the signed release notice and the release package",
}

func TestEveryFileAccessIsListedAndJustified(t *testing.T) {
	found := scan(t)
	var unlisted, stale []string
	for u := range found {
		if _, ok := allowed[u]; !ok {
			unlisted = append(unlisted, u.file+": "+u.call)
		}
	}
	for u := range allowed {
		if !found[u] {
			stale = append(stale, u.file+": "+u.call)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	if len(unlisted) > 0 {
		t.Errorf("calls that are not in the allow list (add them with the reason, or remove them):\n  %s", strings.Join(unlisted, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("allow list entries that no longer exist (remove them):\n  %s", strings.Join(stale, "\n  "))
	}
}

// scan returns the audited calls of every non-test Go file of the agent module.
func scan(t *testing.T) map[use]bool {
	t.Helper()
	root := filepath.Join("..", "..")
	found := map[use]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "bin" || n == "dist" || n == "testdata" || n == "fsaudit" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		names := map[string]string{} // local package name -> import path
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if _, ok := audited[p]; !ok {
				continue
			}
			name := filepath.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			names[name] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			p, ok := names[id.Name]
			if !ok || id.Obj != nil { // a local variable that shadows the package name
				return true
			}
			for _, fn := range audited[p] {
				if sel.Sel.Name == fn {
					found[use{rel, filepath.Base(p) + "." + fn}] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestDumpInventory prints the current inventory; run with HARNEXT_DUMP_FSAUDIT=1 to prepare allow list entries.
func TestDumpInventory(t *testing.T) {
	if os.Getenv("HARNEXT_DUMP_FSAUDIT") == "" {
		t.Skip("set HARNEXT_DUMP_FSAUDIT=1 to print the inventory")
	}
	var lines []string
	for u := range scan(t) {
		lines = append(lines, fmt.Sprintf("%s: %s", u.file, u.call))
	}
	sort.Strings(lines)
	t.Log("\n" + strings.Join(lines, "\n"))
}
