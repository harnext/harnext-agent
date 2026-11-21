package watcher

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Root is one Claude Code config directory and its transcript tree.
type Root struct {
	Dir      string // config directory, e.g. ~/.claude or ~/.claude-work
	Projects string // <Dir>/projects
}

// ClaudeRoot returns the root of a config directory.
func ClaudeRoot(configDir string) Root {
	return Root{Dir: configDir, Projects: filepath.Join(configDir, "projects")}
}

// DiscoverClaudeRoots finds the Claude Code config directories of a user: every directory
// directly under home whose name starts with ".claude" and that holds a projects directory,
// plus CLAUDE_CONFIG_DIR if the agent's own environment sets it. The agent runs as a service
// without the user's shell variables, so the file system is the main source. Symlinks and
// unreadable entries are ignored; the result is sorted by directory.
func DiscoverClaudeRoots(home string) []Root {
	var roots []Root
	add := func(dir string) {
		r := ClaudeRoot(dir)
		if fi, err := os.Lstat(r.Projects); err == nil && fi.IsDir() {
			roots = append(roots, r)
		}
	}
	if home != "" {
		entries, _ := os.ReadDir(home) // a partial listing is still useful
		for _, e := range entries {
			// IsDir is false for symlinks.
			if e.IsDir() && strings.HasPrefix(e.Name(), ".claude") {
				add(filepath.Join(home, e.Name()))
			}
		}
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); filepath.IsAbs(dir) {
		add(dir)
	}
	roots = normalizeRoots(roots)
	slices.SortFunc(roots, func(a, b Root) int { return strings.Compare(a.Dir, b.Dir) })
	return roots
}

// normalizeRoots cleans paths, drops incomplete roots and keeps the first of equal ones.
func normalizeRoots(in []Root) []Root {
	var out []Root
	seen := map[string]bool{}
	for _, r := range in {
		if r.Dir == "" || r.Projects == "" {
			continue
		}
		r = Root{Dir: filepath.Clean(r.Dir), Projects: filepath.Clean(r.Projects)}
		if !seen[r.Projects] {
			seen[r.Projects] = true
			out = append(out, r)
		}
	}
	return out
}
