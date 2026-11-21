// Package watcher discovers Claude Code transcript files, follows them and feeds parsed
// events to a Sink. Files are only ever opened read-only.
package watcher

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/harnext/harnext-agent/internal/parser/claudecode"
)

const (
	DefaultScanInterval = 60 * time.Second
	DefaultMaxLineBytes = 8 << 20
	readBufferBytes     = 64 << 10
	maxRetainedLineBuf  = 1 << 20
)

// ScanProgress observes a watcher's first scan. Files are read newest first (by mtime), so a
// caller can act on "the recent files are done" before the whole history has been read. Calls
// come from the watcher's goroutine and must not call back into it.
type ScanProgress interface {
	// Found is called once, before the first file is read, with the number of files the scan
	// will read (those within MaxFileAge).
	Found(n int)
	// File is called after each file has been read, with its mtime.
	File(mtime time.Time)
}

// ClaudeFile says where a delivered event came from. ConfigDir is the full path of the Claude
// Code config directory (e.g. ~/.claude-work); it is kept in memory only.
type ClaudeFile struct {
	ID        FileID
	ConfigDir string
}

// Sink receives every parsed event, including Skipped and Ignored ones.
// Handle is called synchronously from the watcher and must not call back into it.
type Sink interface {
	Handle(file ClaudeFile, ev claudecode.Event)
}

type Options struct {
	// Roots are the fixed transcript roots; ignored when RootsFunc is set.
	Roots []Root
	// RootsFunc, if set, is called at the start of every full scan: roots that appear later are
	// picked up, vanished ones are skipped and keep their stored offsets.
	RootsFunc    func() []Root
	Offsets      OffsetStore   // required
	Sink         Sink          // required
	ScanInterval time.Duration // full-scan safety net; default DefaultScanInterval
	MaxLineBytes int           // longer lines are reported as line_too_long; default DefaultMaxLineBytes
	// Parser parses lines; nil means plain parsing without content analysis.
	Parser *claudecode.Parser
	// MaxFileAge skips transcripts whose mtime is older (no offset is created); 0 means unlimited.
	MaxFileAge time.Duration
	// Progress, if set, observes the first scan of Run only (see ScanProgress).
	Progress ScanProgress
	// Gate, if set, is read-locked while one file is being read, so a holder of the write lock
	// never sees a file half-processed (the uploader reads rows under it).
	Gate *sync.RWMutex
	// OnError receives operational errors. They carry only an operation name and the
	// underlying system error, never paths or file content.
	OnError func(error)
}

// DefaultRoot is ~/.claude/projects.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

type Watcher struct {
	opts        Options
	mu          sync.Mutex // serializes all file processing
	roots       []Root     // roots of the last scan; guarded by mu
	*lineReader            // embedded so lineBuf stays reachable as w.lineBuf
	ready       chan struct{}
	readyOnce   sync.Once
}

func New(opts Options) (*Watcher, error) {
	if opts.Offsets == nil || opts.Sink == nil || (len(opts.Roots) == 0 && opts.RootsFunc == nil) {
		return nil, errors.New("watcher: Roots (or RootsFunc), Offsets and Sink are required")
	}
	for _, r := range opts.Roots {
		if r.Dir == "" || r.Projects == "" {
			return nil, errors.New("watcher: a Root needs Dir and Projects")
		}
	}
	opts.Roots = normalizeRoots(opts.Roots)
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = DefaultScanInterval
	}
	if opts.MaxLineBytes <= 0 {
		opts.MaxLineBytes = DefaultMaxLineBytes
	}
	w := &Watcher{opts: opts, ready: make(chan struct{})}
	w.lineReader = newLineReader(opts.MaxLineBytes, w.report)
	return w, nil
}

// Ready is closed once Run has installed its watches and finished the initial scan.
func (w *Watcher) Ready() <-chan struct{} { return w.ready }

// ScanOnce walks the roots, reads new complete lines of every transcript and forgets
// state of files that no longer exist. It returns an error only if a root is unreadable
// (the other roots are still scanned) or ctx ends.
func (w *Watcher) ScanOnce(ctx context.Context) error { return w.scanOnce(ctx, nil) }

func (w *Watcher) scanOnce(ctx context.Context, prog ScanProgress) error {
	return w.scanWith(ctx, prog, nil)
}

// scanWith is scanOnce; beforeRead, if set, gets the freshly resolved roots before any file is read.
func (w *Watcher) scanWith(ctx context.Context, prog ScanProgress, beforeRead func([]Root)) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.roots = w.currentRoots()
	if beforeRead != nil {
		beforeRead(w.roots)
	}
	found := map[string]struct{}{}
	var files []transcriptFile
	var complete []Root
	var rootErr error
	for _, r := range w.roots {
		if _, err := os.Stat(r.Projects); err != nil {
			if rootErr == nil {
				rootErr = sanitize("root", err)
			}
			continue
		}
		fl, ok := w.collect(ctx, r.Projects, r.Dir, found)
		files = append(files, fl...)
		if ok {
			complete = append(complete, r)
		}
	}
	// All roots are read together so the newest files of any account come first.
	w.readNewestFirst(ctx, files, prog)
	if err := ctx.Err(); err != nil {
		return err
	}
	// A partial walk cannot tell deleted files from unreadable ones.
	if len(complete) > 0 {
		for _, p := range w.opts.Offsets.Paths() {
			if _, ok := found[p]; ok {
				continue
			}
			// The store may be shared with other watchers; only complete roots' entries are ours.
			for _, r := range complete {
				if underDir(p, r.Projects) {
					w.forget(p)
					break
				}
			}
		}
	}
	return rootErr
}

func (w *Watcher) currentRoots() []Root {
	if w.opts.RootsFunc != nil {
		return normalizeRoots(w.opts.RootsFunc())
	}
	return w.opts.Roots
}

// rootFor returns the root that holds path. w.mu must be held.
func (w *Watcher) rootFor(path string) (Root, bool) {
	for _, r := range w.roots {
		if path == r.Projects || underDir(path, r.Projects) {
			return r, true
		}
	}
	return Root{}, false
}

func underDir(path, dir string) bool { return strings.HasPrefix(path, dir+string(filepath.Separator)) }

// Run follows the roots with fsnotify and re-scans periodically until ctx ends.
func (w *Watcher) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return sanitize("fsnotify", err)
	}
	defer fsw.Close()

	watched := map[string]bool{}
	scanFailing := false
	scan := func(prog ScanProgress) {
		// Watches first, so files created during the scan are not missed.
		err := w.scanWith(ctx, prog, func(roots []Root) {
			for _, r := range roots {
				w.watchTree(fsw, watched, r.Projects)
			}
		})
		if err != nil && ctx.Err() == nil && !scanFailing && w.opts.OnError != nil {
			w.opts.OnError(err)
		}
		scanFailing = err != nil
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
			w.handleEvent(ctx, fsw, watched, ev)
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

func (w *Watcher) handleEvent(ctx context.Context, fsw *fsnotify.Watcher, watched map[string]bool, ev fsnotify.Event) {
	path := ev.Name
	switch {
	case ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename):
		delete(watched, path)
		if isTranscript(path) {
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
			// fsnotify is not recursive; watch first, then scan what was created before the watch.
			w.watchTree(fsw, watched, path)
			w.mu.Lock()
			if r, ok := w.rootFor(path); ok {
				files, _ := w.collect(ctx, path, r.Dir, nil)
				w.readNewestFirst(ctx, files, nil)
			}
			w.mu.Unlock()
		case fi.Mode().IsRegular() && isTranscript(path):
			w.mu.Lock()
			if r, ok := w.rootFor(path); ok {
				w.processFile(ctx, path, r.Dir)
			}
			w.mu.Unlock()
		}
	}
}

func (w *Watcher) watchTree(fsw *fsnotify.Watcher, watched map[string]bool, dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() || watched[path] {
			return nil
		}
		// Marked even on failure: the periodic scan covers it and errors are not repeated.
		watched[path] = true
		if err := fsw.Add(path); err != nil {
			w.report("watch", err)
		}
		return nil
	})
}

// transcriptFile is a discovered file with its modification time and config directory.
type transcriptFile struct {
	path      string
	configDir string
	mtime     time.Time
}

// newestFirst orders by mtime, newest first; equal times by path so the order is stable.
func newestFirst(a, b transcriptFile) int {
	if c := b.mtime.Compare(a.mtime); c != 0 {
		return c
	}
	return strings.Compare(a.path, b.path)
}

// readNewestFirst reads the files within MaxFileAge, newest first (LIFO: the history that
// matters most reaches the aggregator first). prog, if non-nil, observes the scan.
func (w *Watcher) readNewestFirst(ctx context.Context, files []transcriptFile, prog ScanProgress) {
	if w.opts.MaxFileAge > 0 {
		files = slices.DeleteFunc(files, func(f transcriptFile) bool { return time.Since(f.mtime) > w.opts.MaxFileAge })
	}
	slices.SortFunc(files, newestFirst)
	if prog != nil {
		prog.Found(len(files))
	}
	for _, f := range files {
		if ctx.Err() != nil {
			return
		}
		w.processFile(ctx, f.path, f.configDir)
		if prog != nil {
			prog.File(f.mtime)
		}
	}
}

// collect walks dir and lists its transcripts without reading them, and reports whether the walk
// was complete. found, if non-nil, collects every visited transcript path, old ones included, so
// their stored offsets are kept.
func (w *Watcher) collect(ctx context.Context, dir, configDir string, found map[string]struct{}) (files []transcriptFile, complete bool) {
	complete = true
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			complete = false
			return ctx.Err()
		}
		if err != nil {
			complete = false
			w.report("walk", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// Symlinks are not followed or read.
		if d.Type().IsRegular() && isTranscript(path) {
			if found != nil {
				found[path] = struct{}{}
			}
			info, err := d.Info()
			if err != nil {
				complete = false
				w.report("stat", err)
				return nil
			}
			files = append(files, transcriptFile{path: path, configDir: configDir, mtime: info.ModTime()})
		}
		return nil
	})
	return files, complete
}

// processFile reads the complete lines appended since the stored offset. w.mu must be held.
func (w *Watcher) processFile(ctx context.Context, path, configDir string) {
	if g := w.opts.Gate; g != nil {
		g.RLock()
		defer g.RUnlock()
	}
	followFile(ctx, path, w.opts.Offsets, w.lineReader, w.report, func(id FileID, line []byte, tooLong bool) {
		var ev claudecode.Event
		if tooLong {
			ev = claudecode.Skipped{Reason: claudecode.ReasonLineTooLong}
		} else {
			ev = w.opts.Parser.ParseLine(line)
		}
		w.opts.Sink.Handle(ClaudeFile{ID: id, ConfigDir: configDir}, ev)
	})
}

// lineReader reads complete lines from file ranges; it is not safe for concurrent use.
type lineReader struct {
	reader  *bufio.Reader
	lineBuf []byte
	maxLine int
	report  func(op string, err error)
}

func newLineReader(maxLine int, report func(op string, err error)) *lineReader {
	return &lineReader{reader: bufio.NewReaderSize(nil, readBufferBytes), maxLine: maxLine, report: report}
}

// followFile delivers the complete lines of path appended since the stored offset and
// updates the offset store. A missing file drops its stored state.
func followFile(ctx context.Context, path string, offsets OffsetStore, lr *lineReader,
	report func(op string, err error), deliver func(id FileID, line []byte, tooLong bool)) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if err := offsets.Delete(path); err != nil {
				report("offsets", err)
			}
		} else {
			report("open", err)
		}
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		report("stat", err)
		return
	}
	if !fi.Mode().IsRegular() {
		return
	}

	id, size := platformFileID(fi), fi.Size()
	stored, known := offsets.Get(path)
	st := stored
	// A different inode or a shrunken file is a replaced transcript: start over.
	// Re-read lines are harmless because the aggregator deduplicates.
	if !known || st.ID != id || size < st.Size {
		st = FileState{ID: id}
	}
	if st.Offset < size {
		st.Offset = lr.readLines(ctx, f, id, st.Offset, size, deliver)
	}
	st.Size = size
	if !known || st != stored {
		if err := offsets.Set(path, st); err != nil {
			report("offsets", err)
		}
	}
}

// readLines delivers complete lines in [off, size) and returns the offset after the last one.
// A trailing line without '\n' is left for a later call; its bytes are not kept.
func (r *lineReader) readLines(ctx context.Context, f *os.File, id FileID, off, size int64,
	deliver func(id FileID, line []byte, tooLong bool)) int64 {
	r.reader.Reset(io.NewSectionReader(f, off, size-off))
	defer r.reader.Reset(nil)

	consumed := off
	lineLen := 0
	tooLong := false
	r.lineBuf = r.lineBuf[:0]
	for ctx.Err() == nil {
		chunk, err := r.reader.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull {
			if err != io.EOF {
				r.report("read", err)
			}
			break
		}
		lineLen += len(chunk)
		switch {
		case tooLong:
		case lineLen > r.maxLine:
			tooLong = true
			r.lineBuf = r.lineBuf[:0]
		default:
			r.lineBuf = append(r.lineBuf, chunk...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}

		deliver(id, r.lineBuf, tooLong)
		consumed += int64(lineLen)
		lineLen, tooLong = 0, false
		r.lineBuf = r.lineBuf[:0]
	}
	if cap(r.lineBuf) > maxRetainedLineBuf {
		r.lineBuf = nil
	}
	return consumed
}

func (w *Watcher) forget(path string) {
	if err := w.opts.Offsets.Delete(path); err != nil {
		w.report("offsets", err)
	}
}

func (w *Watcher) report(op string, err error) {
	if w.opts.OnError != nil {
		w.opts.OnError(sanitize(op, err))
	}
}

// sanitize keeps only the innermost error (e.g. "permission denied"), dropping wrapped paths.
func sanitize(op string, err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			break
		}
		err = next
	}
	return fmt.Errorf("watcher %s: %s", op, err.Error())
}

func isTranscript(path string) bool { return strings.HasSuffix(path, ".jsonl") }
