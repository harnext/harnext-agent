package watcher

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	chatSessionsDir = "chatSessions"
	maxSessionIDLen = 200
)

// DefaultChatRoots returns the VS Code and VS Code Insiders workspaceStorage roots.
// Roots that do not exist are still returned.
func DefaultChatRoots() []string {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	var roots []string
	for _, edition := range []string{"Code", "Code - Insiders"} {
		roots = append(roots, filepath.Join(cfg, edition, "User", "workspaceStorage"))
	}
	return roots
}

// ChatFile identifies one chat session file.
type ChatFile struct {
	SessionID    string // file name without extension
	WorkspaceDir string // <root>/<workspaceHash>
	ID           FileID
}

// ChatSink receives raw lines. HandleLine is called synchronously and must not call back
// into the watcher; line is reused after the call, so copy it to keep it.
type ChatSink interface {
	HandleLine(f ChatFile, line []byte, tooLong bool)
}

type ChatOptions struct {
	Roots        []string      // workspaceStorage roots, normally DefaultChatRoots(); may not exist yet
	Offsets      OffsetStore   // required
	Sink         ChatSink      // required
	ScanInterval time.Duration // full-scan safety net; default DefaultScanInterval
	MaxLineBytes int           // longer lines are delivered with tooLong; default DefaultMaxLineBytes
	// MaxFileAge skips files whose mtime is older (no offset is created); 0 means unlimited.
	MaxFileAge time.Duration
	// Progress, if set, observes the first scan of Run only (see ScanProgress).
	Progress ScanProgress
	// Gate, if set, is read-locked while one file is being read (see Options.Gate).
	Gate *sync.RWMutex
	// OnError receives operational errors without paths or file content.
	OnError func(error)
}

// ChatWatcher follows <root>/<workspaceHash>/chatSessions/*.jsonl at that fixed depth.
type ChatWatcher struct {
	opts      ChatOptions
	mu        sync.Mutex // serializes all file processing
	lr        *lineReader
	ready     chan struct{}
	readyOnce sync.Once
}

func NewChatWatcher(opts ChatOptions) (*ChatWatcher, error) {
	if len(opts.Roots) == 0 || opts.Offsets == nil || opts.Sink == nil {
		return nil, errors.New("watcher: Roots, Offsets and Sink are required")
	}
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = DefaultScanInterval
	}
	if opts.MaxLineBytes <= 0 {
		opts.MaxLineBytes = DefaultMaxLineBytes
	}
	roots := make([]string, 0, len(opts.Roots))
	for _, r := range opts.Roots {
		if r != "" {
			roots = append(roots, filepath.Clean(r))
		}
	}
	if len(roots) == 0 {
		return nil, errors.New("watcher: Roots, Offsets and Sink are required")
	}
	opts.Roots = roots
	w := &ChatWatcher{opts: opts, ready: make(chan struct{})}
	w.lr = newLineReader(opts.MaxLineBytes, w.report)
	return w, nil
}

// Ready is closed once Run has installed its watches and finished the initial scan.
func (w *ChatWatcher) Ready() <-chan struct{} { return w.ready }

// ScanOnce reads new complete lines of every chat session file and forgets state of files
// that no longer exist. Missing roots are tolerated; it returns an error only if ctx ends.
func (w *ChatWatcher) ScanOnce(ctx context.Context) error {
	return w.scan(ctx, nil, nil)
}

// chatCandidate is a discovered session file with its modification time.
type chatCandidate struct {
	path  string
	cf    ChatFile
	mtime time.Time
}

// scan reads every session file within MaxFileAge, newest first (LIFO). prog, if non-nil,
// observes the scan.
func (w *ChatWatcher) scan(ctx context.Context, watch func(dir string), prog ScanProgress) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	found := map[string]struct{}{}
	complete := true
	var cands []chatCandidate
	for _, root := range w.opts.Roots {
		if !w.scanRoot(ctx, root, watch, found, &cands) {
			complete = false
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.opts.MaxFileAge > 0 {
		cands = slices.DeleteFunc(cands, func(c chatCandidate) bool { return time.Since(c.mtime) > w.opts.MaxFileAge })
	}
	slices.SortFunc(cands, func(a, b chatCandidate) int {
		if c := b.mtime.Compare(a.mtime); c != 0 {
			return c
		}
		return strings.Compare(a.path, b.path)
	})
	if prog != nil {
		prog.Found(len(cands))
	}
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.processFile(ctx, c.path, c.cf, c.mtime)
		if prog != nil {
			prog.File(c.mtime)
		}
	}
	// A partial walk cannot tell deleted files from unreadable ones.
	if complete {
		for _, p := range w.opts.Offsets.Paths() {
			if _, ok := found[p]; !ok && w.underRoot(p) {
				w.forget(p)
			}
		}
	}
	return nil
}

func (w *ChatWatcher) underRoot(path string) bool {
	for _, r := range w.opts.Roots {
		if strings.HasPrefix(path, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// scanRoot reports whether the walk of root was complete; a missing root is incomplete
// (state is kept) but silent.
func (w *ChatWatcher) scanRoot(ctx context.Context, root string, watch func(string), found map[string]struct{}, cands *[]chatCandidate) bool {
	if watch != nil {
		watch(root)
	}
	wsEntries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			w.report("walk", err)
		}
		return false
	}
	complete := true
	for _, ws := range wsEntries {
		if ctx.Err() != nil {
			return false
		}
		if !ws.IsDir() { // symlinks report as non-directories
			continue
		}
		if !w.scanWorkspace(ctx, filepath.Join(root, ws.Name()), watch, found, cands) {
			complete = false
		}
	}
	return complete
}

// scanWorkspace lists the session files of one workspace without reading them.
func (w *ChatWatcher) scanWorkspace(ctx context.Context, wsDir string, watch func(string), found map[string]struct{}, cands *[]chatCandidate) bool {
	if watch != nil {
		watch(wsDir)
	}
	dir := filepath.Join(wsDir, chatSessionsDir)
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true
		}
		w.report("walk", err)
		return false
	}
	if !fi.IsDir() {
		return true
	}
	if watch != nil {
		watch(dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		w.report("walk", err)
		return false
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return false
		}
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		cf, ok := w.classify(path)
		if !ok {
			continue
		}
		found[path] = struct{}{}
		info, err := e.Info()
		if err != nil {
			w.report("stat", err)
			continue
		}
		*cands = append(*cands, chatCandidate{path: path, cf: cf, mtime: info.ModTime()})
	}
	return true
}

// classify accepts only <root>/<workspaceHash>/chatSessions/<validID>.jsonl.
func (w *ChatWatcher) classify(path string) (ChatFile, bool) {
	for _, root := range w.opts.Roots {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 || parts[1] != chatSessionsDir || parts[0] == "" || parts[0] == ".." || parts[0] == "." {
			continue
		}
		id, ok := strings.CutSuffix(parts[2], ".jsonl")
		if !ok || !validSessionID(id) {
			return ChatFile{}, false
		}
		return ChatFile{SessionID: id, WorkspaceDir: filepath.Join(root, parts[0])}, true
	}
	return ChatFile{}, false
}

func validSessionID(id string) bool {
	if len(id) == 0 || len(id) > maxSessionIDLen || strings.Contains(id, "..") {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// processFile requires w.mu.
func (w *ChatWatcher) processFile(ctx context.Context, path string, cf ChatFile, mtime time.Time) {
	if w.opts.MaxFileAge > 0 && time.Since(mtime) > w.opts.MaxFileAge {
		return
	}
	if g := w.opts.Gate; g != nil {
		g.RLock()
		defer g.RUnlock()
	}
	followFile(ctx, path, w.opts.Offsets, w.lr, w.report, func(id FileID, line []byte, tooLong bool) {
		cf.ID = id
		w.opts.Sink.HandleLine(cf, line, tooLong)
	})
}

// Run follows the roots with fsnotify and re-scans periodically until ctx ends.
func (w *ChatWatcher) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return sanitize("fsnotify", err)
	}
	defer fsw.Close()

	watched := map[string]bool{}
	failed := map[string]bool{}
	watch := func(dir string) {
		if watched[dir] {
			return
		}
		if err := fsw.Add(dir); err != nil {
			// A missing directory is expected; other failures are reported once per path.
			if !errors.Is(err, fs.ErrNotExist) && !failed[dir] {
				failed[dir] = true
				w.report("watch", err)
			}
			return
		}
		watched[dir] = true
		delete(failed, dir)
	}
	scan := func(prog ScanProgress) {
		// Watches are installed ahead of each directory read, so nothing created meanwhile is missed.
		if err := w.scan(ctx, watch, prog); err != nil && ctx.Err() == nil {
			w.report("scan", err)
		}
	}

	scan(w.opts.Progress)
	w.readyOnce.Do(func() { close(w.ready) })

	ticker := time.NewTicker(w.opts.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
				delete(watched, ev.Name)
			}
			w.handleEvent(ctx, ev, func() { scan(nil) })
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.report("fsnotify", err)
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				scan(nil)
			}
		case <-ticker.C:
			scan(nil)
		}
	}
}

func (w *ChatWatcher) handleEvent(ctx context.Context, ev fsnotify.Event, scan func()) {
	path := ev.Name
	switch {
	case ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename):
		if _, ok := w.classify(path); ok {
			w.mu.Lock()
			w.forget(path)
			w.mu.Unlock()
		}
	case ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write):
		fi, err := os.Lstat(path)
		if err != nil {
			return
		}
		switch {
		case fi.IsDir() && ev.Has(fsnotify.Create):
			// fsnotify is not recursive; a full discovery watches and reads the new directory.
			scan()
		case fi.Mode().IsRegular():
			if cf, ok := w.classify(path); ok {
				w.mu.Lock()
				w.processFile(ctx, path, cf, fi.ModTime())
				w.mu.Unlock()
			}
		}
	}
}

func (w *ChatWatcher) forget(path string) {
	if err := w.opts.Offsets.Delete(path); err != nil {
		w.report("offsets", err)
	}
}

func (w *ChatWatcher) report(op string, err error) {
	if w.opts.OnError != nil {
		w.opts.OnError(sanitize(op, err))
	}
}
