//go:build realdata && !windows

package watcher

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/claudecode"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

// Cost of the first scan of your own transcripts (the last 30 days, as the agent reads them at start). Read-only; it prints sizes,
// times and counts, never paths or content. Run each test in its own process, because the peak memory is a per-process maximum:
//
//	go test -tags realdata -count=1 -run TestMeasureScanPlain -v ./internal/watcher
//	HARNEXT_REALDATA_CONFIG=/tmp/cfg.json go test -tags realdata -count=1 -run TestMeasureScanAnalyzed -v ./internal/watcher
//
// The configuration file is the JSON payload of the server's agent configuration; see TestRealDataAnalysis.
func TestMeasureScanPlain(t *testing.T) { measureScan(t, nil) }

func TestMeasureScanAnalyzed(t *testing.T) {
	cfgPath := os.Getenv("HARNEXT_REALDATA_CONFIG")
	if cfgPath == "" {
		t.Skip("HARNEXT_REALDATA_CONFIG is not set")
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal("cannot read the configuration file")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	env := remoteconfig.Envelope{
		Payload: base64.RawURLEncoding.EncodeToString(raw),
		Sig:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, raw)),
		KID:     "realdata-throwaway",
	}
	cfg, err := remoteconfig.Verify(env, remoteconfig.TrustedKeys{env.KID: pub}, time.Now(), 0)
	if err != nil {
		t.Fatalf("configuration rejected by the production validator: %s", remoteconfig.CodeOf(err))
	}
	ex, err := analyzer.Compile(cfg)
	if err != nil {
		t.Fatal("configuration does not compile")
	}
	measureScan(t, ex)
}

func measureScan(t *testing.T, ex analyzer.Extractor) {
	root, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	const window = 30 * 24 * time.Hour
	var files int
	var bytesRead int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil && time.Since(info.ModTime()) <= window {
			files++
			bytesRead += info.Size()
		}
		return nil
	})
	if files == 0 {
		t.Skip("no transcripts in the last 30 days")
	}

	agg := aggregate.New()
	var p *claudecode.Parser
	if ex != nil {
		agg.SetAnalyzer(ex)
		p = claudecode.NewParser(ex)
	}
	sink := newSink(agg)
	w, err := New(Options{Roots: []Root{rootOf(root)}, Offsets: NewMemoryOffsets(), Sink: sink, Parser: p, MaxFileAge: window, OnError: func(error) {}})
	if err != nil {
		t.Fatal(err)
	}

	baseRSS := maxRSSBytes(t)
	cpu0 := cpuSeconds(t)
	start := time.Now()
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	wall := time.Since(start)
	cpu := cpuSeconds(t) - cpu0
	peakRSS := maxRSSBytes(t)

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	mb := float64(bytesRead) / (1 << 20)
	t.Logf("analysis=%v os=%s/%s cpus=%d go=%s", ex != nil, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	t.Logf("input: %d transcripts, %.1f MB, %d lines handled, %d sessions", files, mb, sink.count(), aggregate.CountSessions(agg.Rows()))
	t.Logf("wall=%s cpu=%.2fs throughput=%.1f MB/s", wall.Round(time.Millisecond), cpu, mb/wall.Seconds())
	t.Logf("peak_rss_before=%.0f MB peak_rss_after=%.0f MB heap_after_gc=%.0f MB",
		float64(baseRSS)/(1<<20), float64(peakRSS)/(1<<20), float64(ms.HeapAlloc)/(1<<20))
}

func cpuSeconds(t *testing.T) float64 {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatal(err)
	}
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

// maxRSSBytes is the peak resident memory of this process; ru_maxrss is in bytes on macOS and in kilobytes on Linux.
func maxRSSBytes(t *testing.T) int64 {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss)
	}
	return int64(ru.Maxrss) * 1024
}
