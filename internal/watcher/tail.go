package watcher

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/harnext/harnext-agent/internal/parser/copilot"
)

// CopilotSink receives every parsed Copilot event, including Skipped and Ignored ones.
// Handle is called synchronously from the tailer and must not call back into it.
type CopilotSink interface {
	Handle(file FileID, ev copilot.Event)
}

type TailOptions struct {
	Path         string        // the single JSONL file to follow; may not exist yet
	Offsets      OffsetStore   // required
	Sink         CopilotSink   // required
	ScanInterval time.Duration // safety-net re-read; default DefaultScanInterval
	MaxLineBytes int           // longer lines are reported as line_too_long; default DefaultMaxLineBytes
	// TrimOver > 0 empties the file once it is larger than this and fully processed. Only for a file the
	// agent owns (its default telemetry file); a file at a path the user chose is never written to.
	TrimOver int64
	// OnError receives operational errors without paths or file content.
	OnError func(error)
}

// DefaultTrimOver is the size above which the agent's own Copilot telemetry file is emptied.
const DefaultTrimOver int64 = 50 << 20

// FileTailer follows one append-only JSONL file; rotation is not handled.
type FileTailer struct {
	opts      TailOptions
	mu        sync.Mutex // serializes all file processing
	lr        *lineReader
	ready     chan struct{}
	readyOnce sync.Once
}

func NewFileTailer(opts TailOptions) (*FileTailer, error) {
	if opts.Path == "" || opts.Offsets == nil || opts.Sink == nil {
		return nil, errors.New("watcher: Path, Offsets and Sink are required")
	}
	opts.Path = filepath.Clean(opts.Path)
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = DefaultScanInterval
	}
	if opts.MaxLineBytes <= 0 {
		opts.MaxLineBytes = DefaultMaxLineBytes
	}
	t := &FileTailer{opts: opts, ready: make(chan struct{})}
	t.lr = newLineReader(opts.MaxLineBytes, t.report)
	return t, nil
}

// Ready is closed once Run has installed its watch and finished the initial scan.
func (t *FileTailer) Ready() <-chan struct{} { return t.ready }

// ScanOnce reads new complete lines of the file. A missing file is not an error; its
// stored state is dropped. It returns an error only if ctx ends.
func (t *FileTailer) ScanOnce(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.process(ctx)
	return ctx.Err()
}

// process requires t.mu.
func (t *FileTailer) process(ctx context.Context) {
	fi, err := os.Lstat(t.opts.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		t.forget()
		return
	case err != nil:
		t.report("stat", err)
		return
	case !fi.Mode().IsRegular(): // symlinks are not followed or read
		return
	}
	followFile(ctx, t.opts.Path, t.opts.Offsets, t.lr, t.report, func(id FileID, line []byte, tooLong bool) {
		var ev copilot.Event
		if tooLong {
			ev = copilot.Skipped{Reason: copilot.ReasonLineTooLong}
		} else {
			ev = copilot.ParseLine(line)
		}
		t.opts.Sink.Handle(id, ev)
	})
	t.trim()
}

// trim empties the file when every complete line has been processed and it has grown past TrimOver.
// The size is checked again right before truncating so a line appended meanwhile is not lost; the
// window between that check and the truncate remains (documented risk). The file keeps its identity,
// so the stored offset is simply reset to zero. Requires t.mu.
func (t *FileTailer) trim() {
	if t.opts.TrimOver <= 0 {
		return
	}
	st, ok := t.opts.Offsets.Get(t.opts.Path)
	if !ok || st.Size <= t.opts.TrimOver || st.Offset != st.Size {
		return
	}
	fi, err := os.Lstat(t.opts.Path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != st.Size {
		return
	}
	if err := os.Truncate(t.opts.Path, 0); err != nil {
		t.report("trim", err)
		return
	}
	if err := t.opts.Offsets.Set(t.opts.Path, FileState{ID: st.ID}); err != nil {
		t.report("offsets", err)
	}
}

// Run follows the file with fsnotify on its parent directory and re-scans periodically until ctx ends.
func (t *FileTailer) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return sanitize("fsnotify", err)
	}
	defer fsw.Close()

	dir := filepath.Dir(t.opts.Path)
	watching := false
	scan := func() {
		// Watch first, so a file created during the scan is not missed.
		if !watching {
			// Failure (e.g. directory not created yet) is retried on the next scan.
			watching = fsw.Add(dir) == nil
		}
		_ = t.ScanOnce(ctx)
	}

	scan()
	t.readyOnce.Do(func() { close(t.ready) })

	ticker := time.NewTicker(t.opts.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if filepath.Clean(ev.Name) == t.opts.Path {
				t.handleEvent(ctx, ev)
			} else if ev.Name == dir && (ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
				watching = false // directory gone; re-added by a later scan
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			t.report("fsnotify", err)
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				scan()
			}
		case <-ticker.C:
			scan()
		}
	}
}

func (t *FileTailer) handleEvent(ctx context.Context, ev fsnotify.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename):
		t.forget()
	case ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write):
		t.process(ctx)
	}
}

func (t *FileTailer) forget() {
	if err := t.opts.Offsets.Delete(t.opts.Path); err != nil {
		t.report("offsets", err)
	}
}

func (t *FileTailer) report(op string, err error) {
	if t.opts.OnError != nil {
		t.opts.OnError(sanitize(op, err))
	}
}
