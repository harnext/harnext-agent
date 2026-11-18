package aggregate

// Request-based folder split.
//
// Every request (one real user message and all the work until the next one) belongs to ONE folder of the
// project: the folder where most of its files were edited, else read, else the working directory. The tokens,
// the request count and the friction counters of the request are written to that folder, so the folders add up
// to the session and the session to the project.
//
// Only the FOLDER part of a file path is read locally (product-owner decision of 2026-11-15); it is turned into a
// folder INSIDE the project (relative to the repository or marker root, collapsed by CollapsePath) before it is
// stored in a FolderState or leaves the machine. Absolute folders exist only in the open request's Dirs (local state,
// at most maxDirsPerRequest per request) until the request closes.

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/project"
)

const (
	// maxFolders bounds the folders of one session; requests beyond it go to the project root (path "").
	maxFolders = 64
	// maxFolderPath bounds a folder path (the contract's limit of project_path).
	maxFolderPath = 512
	// maxDirsPerRequest bounds the distinct folders counted for one request; later ones are ignored.
	maxDirsPerRequest = 8
)

// ResolveFunc maps a directory to its project identity (internal/project.Resolver.Resolve). Aggregators use it to
// decide whether a file belongs to the session's project and to find its path inside the project.
type ResolveFunc func(dir string) project.Identity

var defaultResolver = project.DefaultResolver()

// firstLevelContainers hold one project each; below them the folder is the first TWO path parts ("apps/web").
var firstLevelContainers = map[string]bool{
	"apps": true, "packages": true, "services": true, "libs": true, "modules": true, "projects": true, "cmd": true,
}

// CollapsePath reduces a path inside a project (forward slashes, no leading slash) to its working unit: the first
// folder, or the first two under apps, packages, services, libs, modules, projects and cmd. "" is the project root.
func CollapsePath(rel string) string {
	rel = strings.Trim(strings.ReplaceAll(rel, `\`, "/"), "/")
	if rel == "" || rel == "." {
		return ""
	}
	var parts []string
	for _, p := range strings.Split(rel, "/") {
		if p != "" && p != "." && p != ".." {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	n := 1
	if len(parts) > 1 && firstLevelContainers[parts[0]] {
		n = 2
	}
	out := strings.Join(parts[:n], "/")
	if len(out) > maxFolderPath {
		return ""
	}
	return out
}

// DirCount is one absolute folder the open request edited or read, with how many times. It lives only in the open
// request and only until the request closes.
type DirCount struct {
	Dir  string `json:"dir"`
	Edit int32  `json:"edit,omitempty"`
	Read int32  `json:"read,omitempty"`
}

func addDir(dirs []DirCount, dir string, edit bool) []DirCount {
	if dir == "" || len(dir) > 1024 {
		return dirs
	}
	i := slices.IndexFunc(dirs, func(d DirCount) bool { return d.Dir == dir })
	if i < 0 {
		if len(dirs) >= maxDirsPerRequest {
			return dirs
		}
		dirs = append(dirs, DirCount{Dir: dir})
		i = len(dirs) - 1
	}
	if edit {
		dirs[i].Edit++
	} else {
		dirs[i].Read++
	}
	return dirs
}

// CwdFolder is the working directory's folder inside its project ("" at the root or when unknown).
func CwdFolder(resolve ResolveFunc, cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	return CollapsePath(resolve(cwd).Path)
}

// RequestFolder decides the folder of one request: the folder with the most edited files, else the most read
// files (ties go to the smaller path, the project root being the smallest), else the working directory's folder.
// Files outside the session's project (another repository, /tmp) are not counted.
func RequestFolder(resolve ResolveFunc, cwd string, dirs []DirCount) string {
	base := CwdFolder(resolve, cwd)
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	cwdID := resolve(cwd)
	edits, reads := map[string]int32{}, map[string]int32{}
	for _, d := range dirs {
		folder, ok := folderInProject(resolve, cwd, cwdID, d.Dir)
		if !ok {
			continue
		}
		if d.Edit > 0 {
			edits[folder] += d.Edit
		}
		if d.Read > 0 {
			reads[folder] += d.Read
		}
	}
	if f, ok := argmax(edits); ok {
		return f
	}
	if f, ok := argmax(reads); ok {
		return f
	}
	return base
}

// folderInProject returns the collapsed folder of dir inside the session's project; ok is false for folders of another
// project. A folder under the working directory always belongs to the project, which also covers projects without
// a repository or marker file (each folder is its own project for the resolver there).
func folderInProject(resolve ResolveFunc, cwd string, cwdID project.Identity, dir string) (string, bool) {
	id := resolve(dir)
	if id.Project == cwdID.Project && id.Project != project.Unknown {
		return CollapsePath(id.Path), true
	}
	if rel, err := filepath.Rel(filepath.Clean(cwd), filepath.Clean(dir)); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		if rel == "." {
			rel = ""
		}
		return CollapsePath(filepath.ToSlash(filepath.Join(filepath.FromSlash(cwdID.Path), rel))), true
	}
	return "", false
}

// argmax returns the key with the highest count; ties go to the smaller key ("" is the smallest).
func argmax(m map[string]int32) (string, bool) {
	best, bestN, ok := "", int32(0), false
	for k, n := range m {
		if n > bestN || (n == bestN && ok && k < best) {
			best, bestN, ok = k, n, true
		}
	}
	return best, ok
}

// UsageState is one usage key's tokens inside a folder or an open request.
type UsageState struct {
	Totals
	WebSearchRequests int64 `json:"web_search_requests,omitempty"`
	WebFetchRequests  int64 `json:"web_fetch_requests,omitempty"`
}

func (u *UsageState) add(o *UsageState) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheCreationTokens += o.CacheCreationTokens
	u.CacheCreation1hTokens += o.CacheCreation1hTokens
	u.WebSearchRequests += o.WebSearchRequests
	u.WebFetchRequests += o.WebFetchRequests
}

// Request is the open request: the counters, tokens and folders gathered since the last real user message. It is
// folded into a FolderState when the next request starts (or, virtually, whenever rows are built).
type Request struct {
	// Q holds the counter change of the request, taken from the analyzer by difference (see Ledger notes in
	// aggregate.go): its own signals and in-flight counters, plus the closing part (friction, repeat, error paste)
	// that the NEXT message writes to it.
	Q     analyzer.Quality       `json:"q"`
	Turns int64                  `json:"turns,omitempty"`
	Usage map[string]*UsageState `json:"usage,omitempty"`
	Dirs  []DirCount             `json:"dirs,omitempty"`
}

func (r *Request) usage(key string) *UsageState {
	if r.Usage == nil {
		r.Usage = map[string]*UsageState{}
	}
	u := r.Usage[key]
	if u == nil {
		u = &UsageState{}
		r.Usage[key] = u
	}
	return u
}

func (r *Request) empty() bool {
	return r.Q.IsZero() && r.Turns == 0 && len(r.Usage) == 0
}

// FolderState is the sum of the requests that belong to one folder of the session.
type FolderState struct {
	Usage map[string]*UsageState `json:"usage,omitempty"`
	// Turns counts the user messages (requests) of the folder.
	Turns int64 `json:"turns,omitempty"`
	// Q are the folder's counters. Exchanges and friction positions are numbered inside the folder:
	// Q.FirstFrictionExchange is the position among the folder's analyzed requests.
	Q analyzer.Quality `json:"q"`
}

// merge adds a request to the folder.
func (f *FolderState) merge(r Request) {
	f.Turns += r.Turns
	f.Q.Add(r.Q)
	if r.Q.FrictionExchanges > 0 && f.Q.FirstFrictionExchange == 0 {
		// The request is the newest analyzed one of the folder, so its position is the folder's analyzed count.
		f.Q.FirstFrictionExchange = f.Q.AnalyzedExchanges
	}
	for k, u := range r.Usage {
		if f.Usage == nil {
			f.Usage = map[string]*UsageState{}
		}
		d := f.Usage[k]
		if d == nil {
			d = &UsageState{}
			f.Usage[k] = d
		}
		d.add(u)
	}
}

func (f *FolderState) clone() *FolderState {
	c := *f
	c.Usage = make(map[string]*UsageState, len(f.Usage))
	for k, u := range f.Usage {
		uc := *u
		c.Usage[k] = &uc
	}
	return &c
}

// FolderPart is one folder of a session as the uploader needs it: the request count and the counters.
type FolderPart struct {
	// Path is the folder inside the project ("" = project root).
	Path    string
	Turns   int64
	Quality analyzer.Quality
}

// folderTarget returns the folder an additional request lands in: name, or the project root when the session
// already has the maximum number of folders.
func folderTarget(folders map[string]*FolderState, name string) string {
	if _, ok := folders[name]; ok || name == "" {
		return name
	}
	n := len(folders)
	if _, ok := folders[""]; ok {
		n--
	}
	if n >= maxFolders-1 { // the root takes the last place
		return ""
	}
	return name
}

// mainFolderOf is the folder with the most requests (ties: smaller path, the root first) among those that carry usage rows
// (hasUsage; all folders when none does). Sessions count only in their main folder, so the sessions of the folders add up to the
// sessions of the project, and a session never counts in a folder that has no data of it.
func mainFolderOf(parts []FolderPart, hasUsage map[string]bool) string {
	pick := func(only bool) (string, bool) {
		best := -1
		for i, p := range parts {
			if only && !hasUsage[p.Path] {
				continue
			}
			if best < 0 || p.Turns > parts[best].Turns || (p.Turns == parts[best].Turns && p.Path < parts[best].Path) {
				best = i
			}
		}
		if best < 0 {
			return "", false
		}
		return parts[best].Path, true
	}
	if m, ok := pick(true); ok {
		return m
	}
	m, _ := pick(false)
	return m
}

// partsOf lists the folders sorted by path.
func partsOf(folders map[string]*FolderState) []FolderPart {
	parts := make([]FolderPart, 0, len(folders))
	for name, f := range folders {
		parts = append(parts, FolderPart{Path: name, Turns: f.Turns, Quality: f.Q})
	}
	slices.SortFunc(parts, func(a, b FolderPart) int { return strings.Compare(a.Path, b.Path) })
	return parts
}

// track runs f, which may change st, and returns the change of the additive counters.
func track(st *analyzer.SessionStats, f func()) analyzer.Quality {
	before := st.Q
	f()
	return st.Q.Sub(before)
}
