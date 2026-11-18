// Package scan builds a one-off usage summary of the last 30 days from the local Claude Code and VS Code Copilot
// records: nothing is stored, no agent, token or network is involved, and no content analysis runs (no friction).
package scan

import (
	"context"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/parser/claudecode"
	"github.com/harnext/harnext-agent/internal/parser/copilot"
	"github.com/harnext/harnext-agent/internal/service"
	"github.com/harnext/harnext-agent/internal/watcher"
)

// DefaultWindow is the period a scan covers.
const DefaultWindow = 30 * 24 * time.Hour

// Options select what Collect reads; zero values mean the machine's defaults.
type Options struct {
	Now    time.Time     // default time.Now()
	Window time.Duration // default DefaultWindow
	// ClaudeRoot is the transcript root; empty means the watcher default.
	ClaudeRoot string
	// CopilotOTelPath is the Copilot telemetry file; empty means the agent's configured or default file.
	CopilotOTelPath string
	// ChatRoots are VS Code workspaceStorage roots; empty means the watcher defaults.
	ChatRoots []string
}

type claudeSink struct{ agg *aggregate.Aggregator }

func (s claudeSink) Handle(_ watcher.FileID, ev claudecode.Event) { s.agg.Add(ev) }

type copilotSink struct{ agg *aggregate.CopilotAggregator }

func (s copilotSink) Handle(_ watcher.FileID, ev copilot.Event) { s.agg.Add(ev) }

// WindowStart is the first UTC day a scan of the given moment and window covers.
func WindowStart(now time.Time, window time.Duration) time.Time {
	d := now.UTC().Add(-window)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}

// Collect reads the local records once (no watching) and returns the usage rows inside the window. Offsets live in memory
// only and the telemetry file is never trimmed, so nothing is written to disk. Unreadable files are skipped silently;
// the only error is the cancellation of ctx.
func Collect(ctx context.Context, o Options) ([]aggregate.Row, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	start := WindowStart(o.Now, o.Window)
	// Files untouched since the start of the first day cannot hold rows of the window.
	maxAge := max(time.Since(start), time.Nanosecond)
	offsets := watcher.NewMemoryOffsets()
	ignore := func(error) {} // errors carry no content but would not help a one-off summary

	claude := aggregate.New()
	root := o.ClaudeRoot
	if root == "" {
		root, _ = watcher.DefaultRoot()
	}
	if root != "" {
		if w, err := watcher.New(watcher.Options{
			Root: root, Offsets: offsets, Parser: claudecode.NewParser(nil), MaxFileAge: maxAge,
			Sink: claudeSink{claude}, OnError: ignore,
		}); err == nil {
			_ = w.ScanOnce(ctx) // a missing root is normal (tool not installed); only cancellation matters
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cop := aggregate.NewCopilot()
	if path := o.copilotPath(); path != "" {
		if t, err := watcher.NewFileTailer(watcher.TailOptions{Path: path, Offsets: offsets, Sink: copilotSink{cop}, OnError: ignore}); err == nil {
			_ = t.ScanOnce(ctx)
		}
	}
	roots := o.ChatRoots
	if len(roots) == 0 {
		roots = watcher.DefaultChatRoots()
	}
	if len(roots) > 0 {
		if cw, err := watcher.NewChatWatcher(watcher.ChatOptions{
			Roots: roots, Offsets: offsets, Sink: service.NewChatSink(cop), MaxFileAge: maxAge, OnError: ignore,
		}); err == nil {
			_ = cw.ScanOnce(ctx)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	day := start.Format(time.DateOnly)
	var rows []aggregate.Row
	for _, r := range append(claude.Rows(), cop.Rows()...) {
		if r.Day >= day {
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// copilotPath is the telemetry file the agent would read: the explicit option, else the path in the agent's own
// settings if it is installed, else the default file inside the agent directory.
func (o Options) copilotPath() string {
	if o.CopilotOTelPath != "" {
		return o.CopilotOTelPath
	}
	dir, err := service.Home()
	if err != nil {
		return ""
	}
	p := service.PathsIn(dir)
	settings, _ := p.LoadSettings() // not initialized: zero settings select the default file
	return p.CopilotOTelFile(settings)
}
