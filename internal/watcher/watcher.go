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

// Sink receives every parsed event, including Skipped and Ignored ones.
// Handle is called synchronously from the watcher and must not call back into it.
type Sink interface {
	Handle(file FileID, ev claudecode.Event)
}

type Options struct {
	Root         string        // transcript root, normally DefaultRoot()
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
	*lineReader            // embedded so lineBuf stays reachable as w.lineBuf
	ready       chan struct{}
	readyOnce   sync.Once
}

func New(opts Options) (*Watcher, error) {
	if opts.Root == "" || opts.Offsets == nil || opts.Sink == nil {
		return nil, errors.New("watcher: Root, Offsets and Sink are required")
	}
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

// ScanOnce walks the root, reads new complete lines of every transcript and forgets
// state of files that no longer exist. It returns an error only if the root is unreadable
// or ctx ends.
func (w *Watcher) ScanOnce(ctx context.Context) error { return w.scanOnce(ctx, nil) }

func (w *Watcher) scanOnce(ctx context.Context, prog ScanProgress) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := os.Stat(w.opts.Root); err != nil {
		return sanitize("root", err)
	}
	found := map[string]struct{}{}
	complete := w.scanTree(ctx, w.opts.Root, found, prog)
	if err := ctx.Err(); err != nil {
		return err
	}
	// A partial walk cannot tell deleted files from unreadable ones.
	if complete {
		for _, p := range w.opts.Offsets.Paths() {
			// The store may be shared with other watchers; only this root's entries are ours.
			if _, ok := found[p]; !ok && strings.HasPrefix(p, w.opts.Root+string(filepath.Separator)) {
				w.forget(p)
			}
		}
	}
	return nil
}

// Run follows the root with fsnotify and re-scans periodically until ctx ends.
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
		w.watchTree(fsw, watched, w.opts.Root)
		err := w.scanOnce(ctx, prog)
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
			w.scanTree(ctx, path, nil, nil)
			w.mu.Unlock()
		case fi.Mode().IsRegular() && isTranscript(path):
			w.mu.Lock()
			w.processFile(ctx, path)
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

// transcriptFile is a discovered file with its modification time.
type transcriptFile struct {
	path  string
	mtime time.Time
}

// newestFirst orders by mtime, newest first; equal times by path so the order is stable.
func newestFirst(a, b transcriptFile) int {
	if c := b.mtime.Compare(a.mtime); c != 0 {
		return c
	}
	return strings.Compare(a.path, b.path)
}

// scanTree reads every transcript under dir within MaxFileAge, newest first (LIFO: the
// history that matters most reaches the aggregator first), and reports whether the walk was
// complete. found, if non-nil, collects every visited transcript path, old ones included, so
// their stored offsets are kept. prog, if non-nil, observes the scan.
func (w *Watcher) scanTree(ctx context.Context, dir string, found map[string]struct{}, prog ScanProgress) bool {
	files, complete := w.collect(ctx, dir, found)
	if w.opts.MaxFileAge > 0 {
		files = slices.DeleteFunc(files, func(f transcriptFile) bool { return time.Since(f.mtime) > w.opts.MaxFileAge })
	}
	slices.SortFunc(files, newestFirst)
	if prog != nil {
		prog.Found(len(files))
	}
	for _, f := range files {
		if ctx.Err() != nil {
			return false
		}
		w.processFile(ctx, f.path)
		if prog != nil {
			prog.File(f.mtime)
		}
	}
	return complete
}

// collect walks dir and lists its transcripts without reading them.
func (w *Watcher) collect(ctx context.Context, dir string, found map[string]struct{}) (files []transcriptFile, complete bool) {
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
			files = append(files, transcriptFile{path: path, mtime: info.ModTime()})
		}
		return nil
	})
	return files, complete
}

// processFile reads the complete lines appended since the stored offset. w.mu must be held.
func (w *Watcher) processFile(ctx context.Context, path string) {
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
		w.opts.Sink.Handle(id, ev)
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
