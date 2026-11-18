// Package project turns a local working directory into the readable project identity that is sent to the
// server: the git repository as "owner/repo" plus the sub path inside it, or,
// outside git, the path with the home directory replaced by "~". Full paths, the computer's user name and
// branch names never leave the machine. It only reads files (.git, .git/config); it never runs git.
package project

import (
	"bufio"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Unknown is sent when the working directory is not known.
const Unknown = "unknown"

const (
	maxLen        = 512
	maxConfigSize = 256 << 10
	maxDepth      = 64
)

// Identity is the project identity of one working directory.
type Identity struct {
	Project string
	// Path is the sub path inside the repository ("services/billing"), or "" at the root and outside git.
	Path string
}

// Resolver caches identities per directory; a repository's remote rarely changes while the agent runs.
type Resolver struct {
	home  string
	mu    sync.Mutex
	cache map[string]Identity
}

// NewResolver uses home to shorten paths outside git; an empty home leaves them as they are.
func NewResolver(home string) *Resolver {
	return &Resolver{home: filepath.Clean(home), cache: map[string]Identity{}}
}

// DefaultResolver uses the user's home directory.
func DefaultResolver() *Resolver {
	home, _ := os.UserHomeDir()
	return NewResolver(home)
}

// Resolve returns the identity of dir. It never fails: unreadable git data falls back to the shortened path.
func (r *Resolver) Resolve(dir string) Identity {
	if strings.TrimSpace(dir) == "" {
		return Identity{Project: Unknown}
	}
	dir = filepath.Clean(dir)
	r.mu.Lock()
	id, ok := r.cache[dir]
	r.mu.Unlock()
	if ok {
		return id
	}
	id = r.resolve(dir)
	r.mu.Lock()
	r.cache[dir] = id
	r.mu.Unlock()
	return id
}

func (r *Resolver) resolve(dir string) Identity {
	root, gitDir := findRepo(dir)
	if root == "" {
		// Outside git the project root is the nearest folder that looks like one; without a marker the folder itself is
		// the project. Sessions started in a sub folder then group under their project instead of becoming projects.
		root = r.findMarkerRoot(dir)
		if root == "" {
			return Identity{Project: clip(r.shorten(dir))}
		}
		return Identity{Project: clip(r.shorten(root)), Path: subPath(root, dir)}
	}
	sub := subPath(root, dir)
	if remote := remoteURL(gitDir); remote != "" {
		if name := RepoName(remote); name != "" {
			return Identity{Project: clip(name), Path: sub}
		}
	}
	// A repository without a usable remote is named by its folder, home-shortened.
	return Identity{Project: clip(r.shorten(root)), Path: sub}
}

// subPath is dir relative to root with forward slashes; "" at the root or when dir is not inside it.
func subPath(root, dir string) string {
	if rel, err := filepath.Rel(root, dir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
		return clip(filepath.ToSlash(rel))
	}
	return ""
}

// projectMarkers are names whose presence makes a folder a project root outside git: AI tool and editor workspace
// settings first, then the manifests of common ecosystems. Names are only looked up, never read.
var projectMarkers = []string{
	".claude", "CLAUDE.md", "AGENTS.md", ".cursor", ".vscode", ".github", ".gitignore",
	"package.json", "go.mod", "pyproject.toml", "Cargo.toml", "pom.xml", "build.gradle", "composer.json", "Gemfile", "app.json",
}

// findMarkerRoot walks up from dir to the nearest folder holding a project marker. It never returns the home directory
// or anything above it (a stray marker there would swallow every project below), and "" means no marker was found.
func (r *Resolver) findMarkerRoot(dir string) string {
	home := r.home
	for i := 0; i < maxDepth; i++ {
		if dir == home || isFilesystemRoot(dir) {
			return ""
		}
		for _, name := range projectMarkers {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

func isFilesystemRoot(dir string) bool {
	return filepath.Dir(dir) == dir
}

// shorten replaces the home directory prefix with "~" and uses forward slashes.
func (r *Resolver) shorten(p string) string {
	if r.home != "" && r.home != "." {
		if p == r.home {
			return "~"
		}
		if rel, err := filepath.Rel(r.home, p); err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(p)
}

// findRepo walks up from dir to the directory holding .git and returns it with the git directory
// (following a "gitdir:" file for worktrees and submodules).
func findRepo(dir string) (root, gitDir string) {
	for i := 0; i < maxDepth; i++ {
		candidate := filepath.Join(dir, ".git")
		if info, err := os.Stat(candidate); err == nil {
			if info.IsDir() {
				return dir, candidate
			}
			if target := readGitFile(candidate, dir); target != "" {
				return dir, target
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
	return "", ""
}

func readGitFile(path, base string) string {
	b, err := readSmall(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	target, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return ""
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	return filepath.Clean(target)
}

// remoteURL reads the "origin" remote (or the first remote) from the repository config. A worktree's
// git directory points to the main one through its "commondir" file.
func remoteURL(gitDir string) string {
	if b, err := readSmall(filepath.Join(gitDir, "commondir")); err == nil {
		common := strings.TrimSpace(string(b))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		gitDir = filepath.Clean(common)
	}
	b, err := readSmall(filepath.Join(gitDir, "config"))
	if err != nil {
		return ""
	}
	var first, origin, section string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if !strings.HasPrefix(section, `[remote "`) {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "url" {
			continue
		}
		value = strings.TrimSpace(value)
		if first == "" {
			first = value
		}
		if section == `[remote "origin"]` && origin == "" {
			origin = value
		}
	}
	if origin != "" {
		return origin
	}
	return first
}

// RepoName reduces a remote URL to "owner/repo": scheme, host, port, user name, password or token, query,
// fragment and the ".git" suffix are dropped; nested groups keep only their last two parts.
func RepoName(remote string) string {
	remote = strings.TrimSpace(remote)
	var p string
	switch {
	case strings.Contains(remote, "://"):
		u, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		p = u.Path
	case strings.Contains(remote, ":") && !filepath.IsAbs(remote) && !looksLikeWindowsPath(remote):
		// scp form: [user@]host:owner/repo.git
		_, p, _ = strings.Cut(remote, ":")
	default:
		// A local path remote: only its last two parts are meaningful.
		p = filepath.ToSlash(remote)
	}
	p = strings.Trim(strings.TrimSuffix(strings.Trim(p, "/"), ".git"), "/")
	parts := []string{}
	for _, s := range strings.Split(p, "/") {
		// Azure DevOps puts "_git" between the project and the repository.
		if s != "" && s != "." && s != ".." && s != "_git" {
			parts = append(parts, s)
		}
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
}

func looksLikeWindowsPath(s string) bool {
	return len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/')
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, maxConfigSize)
	n, err := f.Read(buf)
	if n == 0 && err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func clip(s string) string {
	if len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:]
}
