// Package gitid finds the git user.email that applies to a working directory by reading git's config files in git's own
// order; it never runs git. The address is only handed to the caller (which hashes it at once): it is never logged, never
// stored and never part of an error.
package gitid

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/harnext/harnext-agent/internal/project"
)

const (
	cacheTTL        = 60 * time.Second
	maxFileSize     = 256 << 10
	maxIncludeDepth = 10
	maxFiles        = 64 // total config files read per resolution, so include fan-out stays bounded
	maxCacheEntries = 256
)

// stamp is what a config file looked like when it was read; a change invalidates the cached answer.
type stamp struct {
	path   string
	exists bool
	mod    int64
	size   int64
}

type entry struct {
	email  string
	ok     bool
	at     time.Time
	stamps []stamp
}

// Resolver resolves and caches git user.email values per repository.
type Resolver struct {
	home     string
	realHome string
	xdg      string
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

// NewResolver reads XDG_CONFIG_HOME once; home is where ~/.gitconfig lives and the only place a symlinked config may point into.
func NewResolver(home string) *Resolver {
	r := &Resolver{now: time.Now, cache: map[string]entry{}}
	if strings.TrimSpace(home) != "" {
		r.home = filepath.Clean(home)
		r.realHome = r.home
		if real, err := filepath.EvalSymlinks(r.home); err == nil {
			r.realHome = real
		}
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		r.xdg = filepath.Clean(x)
	}
	return r
}

// Email returns the lowercased user.email in effect for dir; ok is false when none is set or anything is unreadable.
func (r *Resolver) Email(dir string) (email string, ok bool) {
	defer func() {
		if recover() != nil {
			email, ok = "", false
		}
	}()
	var root, gitDir string
	if strings.TrimSpace(dir) != "" {
		root, gitDir = project.FindRepo(filepath.Clean(dir))
	}
	key := root + "\x00" + gitDir
	now := r.now()

	r.mu.Lock()
	e, hit := r.cache[key]
	r.mu.Unlock()
	if hit && e.fresh(now) {
		return e.email, e.ok
	}

	s := &state{r: r, gitDir: gitDir, stack: map[string]bool{}}
	s.resolve()
	e = entry{email: s.email, ok: s.email != "", at: now, stamps: s.stamps}

	r.mu.Lock()
	if len(r.cache) >= maxCacheEntries {
		r.cache = map[string]entry{}
	}
	r.cache[key] = e
	r.mu.Unlock()
	return e.email, e.ok
}

func (e entry) fresh(now time.Time) bool {
	if age := now.Sub(e.at); age < 0 || age >= cacheTTL {
		return false
	}
	for _, st := range e.stamps {
		if cur, _ := lstat(st.path); cur != st {
			return false
		}
	}
	return true
}

func lstat(path string) (stamp, os.FileInfo) {
	info, err := os.Lstat(path)
	if err != nil {
		return stamp{path: path}, nil
	}
	return stamp{path: path, exists: true, mod: info.ModTime().UnixNano(), size: info.Size()}, info
}

// state is one resolution: the winning e-mail so far and the files it depended on.
type state struct {
	r      *Resolver
	gitDir string
	email  string
	files  int
	stamps []stamp
	stack  map[string]bool
}

// resolve applies the files in git's precedence order; a later value overrides an earlier one.
func (s *state) resolve() {
	r := s.r
	xdg := r.xdg
	if xdg == "" && r.home != "" {
		xdg = filepath.Join(r.home, ".config")
	}
	if xdg != "" {
		s.file(filepath.Join(xdg, "git", "config"), 0)
	}
	if r.home != "" {
		s.file(filepath.Join(r.home, ".gitconfig"), 0)
	}
	if s.gitDir != "" {
		s.file(filepath.Join(project.CommonDir(s.gitDir), "config"), 0)
	}
}

func (s *state) file(path string, depth int) {
	if s.files >= maxFiles {
		return
	}
	s.files++
	data, real, ok := s.read(path)
	if !ok || s.stack[real] {
		return
	}
	s.stack[real] = true
	defer delete(s.stack, real)
	s.parse(string(data), path, depth)
}

// read returns a regular file of at most maxFileSize bytes. A symlink is followed only when its target is a regular file
// inside the home directory, so a config cannot be pointed at arbitrary files.
func (s *state) read(path string) (data []byte, real string, ok bool) {
	st, info := lstat(path)
	s.stamps = append(s.stamps, st)
	if info == nil {
		return nil, "", false
	}
	real = path
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil || !s.r.insideHome(target) {
			return nil, "", false
		}
		var tst stamp
		tst, info = lstat(target)
		s.stamps = append(s.stamps, tst)
		if info == nil {
			return nil, "", false
		}
		real = target
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileSize {
		return nil, "", false
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, "", false
	}
	defer f.Close()
	data, err = io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil || len(data) > maxFileSize {
		return nil, "", false
	}
	return data, real, true
}

func (r *Resolver) insideHome(p string) bool {
	if r.realHome == "" {
		return false
	}
	rel, err := filepath.Rel(r.realHome, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
