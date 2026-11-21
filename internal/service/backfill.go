package service

import (
	"strconv"
	"sync"
	"time"

	"github.com/harnext/harnext-agent/internal/uploader"
	"github.com/harnext/harnext-agent/internal/watcher"
)

const (
	// historyMaxAge is how far back transcripts and chat files are read: the web app promises
	// "your last 30 days are imported". Older files never reach the queue or the offset store.
	historyMaxAge = 30 * 24 * time.Hour
	// firstPacketSessions and firstPacketWindow bound the first upload packet: it is ready once
	// that many files (one file is one session) are read or all files modified within the
	// window are, whichever comes first. Files are read newest first.
	firstPacketSessions = 400
	firstPacketWindow   = 7 * 24 * time.Hour
	// configGrace is how long the first read waits for the signed configuration when none is
	// cached. It is short on purpose: a slow or unreachable server must not delay the first upload.
	//
	// Text read while no configuration is active is not analyzed (friction is null for those sessions). When the
	// configuration arrives later, runOnce drops the saved state and offsets and the history is read once more with it
	//; rows that gain friction are uploaded again.
	configGrace = 2 * time.Second
)

// BackfillStatus is the one-line backfill picture shown in the run log and by `status`.
// Counts and times only: no paths, no content.
type BackfillStatus struct {
	// Found counts the files (sessions) within the last 30 days; Scanned those already read.
	Found   int `json:"found"`
	Scanned int `json:"scanned"`
	// Remaining sessions are not on the server yet: files not read plus rows queued for upload.
	Remaining int `json:"remaining"`
	// FirstPacketAt is when the server first accepted history (or confirmed it already had it).
	FirstPacketAt time.Time `json:"first_packet_at,omitempty"`
	// Complete is true once every file was read and a full upload pass finished after that.
	Complete bool `json:"complete"`
}

// Line is the single status line; the same text goes to the log.
func (b BackfillStatus) Line() string {
	first := "first packet not sent yet"
	if !b.FirstPacketAt.IsZero() {
		first = "first packet sent " + b.FirstPacketAt.Local().Format("2006-01-02 15:04:05")
	}
	return "backfill: " + strconv.Itoa(b.Found) + " sessions found (last 30 days), " + first + ", " + strconv.Itoa(b.Remaining) + " remaining"
}

// backfill tracks the first scan of every source, decides when the first upload packet is
// ready and keeps the numbers for the status line.
type backfill struct {
	now    func() time.Time
	logf   func(string, ...any)
	packet int
	window time.Duration

	mu            sync.Mutex
	sources       []*bfSource
	scanned       int
	queued        int // rows left in the running or last upload pass
	firstSent     time.Time
	complete      bool
	scanDone      bool
	passCoversAll bool // the running upload pass started after every scan finished
	logged        bool // the complete line was written

	first     chan struct{}
	firstOnce sync.Once
}

func newBackfill(now func() time.Time, logf func(string, ...any), packet int, window time.Duration) *backfill {
	if now == nil {
		now = time.Now
	}
	if packet <= 0 {
		packet = firstPacketSessions
	}
	if window <= 0 {
		window = firstPacketWindow
	}
	return &backfill{now: now, logf: logf, packet: packet, window: window, first: make(chan struct{})}
}

// FirstPacketReady is closed when the first upload packet can go out.
func (b *backfill) FirstPacketReady() <-chan struct{} { return b.first }

// bfSource is one reader (Claude transcripts, Copilot chat files, the telemetry file).
type bfSource struct {
	b          *backfill
	found      int
	scanned    int
	recentDone bool // every file within the window is read
	done       bool
}

var _ watcher.ScanProgress = (*bfSource)(nil)

// source registers a reader; call before it starts.
func (b *backfill) source() *bfSource {
	s := &bfSource{b: b}
	b.mu.Lock()
	b.sources = append(b.sources, s)
	b.mu.Unlock()
	return s
}

func (s *bfSource) Found(n int) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.found = n
	if n == 0 {
		s.recentDone = true
	}
	s.b.checkLocked()
}

func (s *bfSource) File(mtime time.Time) {
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	s.scanned++
	b.scanned++
	if b.now().Sub(mtime) > b.window {
		s.recentDone = true
	}
	b.checkLocked()
}

// Done marks the source's first scan as finished (also when it failed or had nothing to read).
func (s *bfSource) Done() {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.done, s.recentDone = true, true
	s.b.checkLocked()
}

func (b *backfill) checkLocked() {
	all, recent := true, true
	for _, s := range b.sources {
		all = all && s.done
		recent = recent && s.recentDone
	}
	b.scanDone = all
	if b.scanned >= b.packet || recent {
		b.firstOnce.Do(func() { close(b.first) })
	}
}

// Catchup reports whether history is still being read. The uploader asks it at the start of
// every pass; the answer is kept so a pass that began after the last file was read is known to
// have covered everything.
func (b *backfill) Catchup() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.passCoversAll = b.scanDone
	return !b.scanDone
}

// Batch records an accepted batch of the running upload pass.
func (b *backfill) Batch(r uploader.Result) {
	b.mu.Lock()
	b.queued = r.Remaining
	line := ""
	if r.Accepted > 0 && b.firstSent.IsZero() {
		line = b.firstSentLocked()
	}
	b.mu.Unlock()
	b.log(line)
}

// Queued records how many sendable sessions the running upload pass has queued. The gate's skipped and held sessions are
// not part of it: they are not on their way to the server until their account is bound.
func (b *backfill) Queued(n int) {
	b.mu.Lock()
	b.queued = n
	b.mu.Unlock()
}

// Pass records the end of an upload pass. The first packet counts as sent once the server accepted or already had history, or
// when the pass had nothing the gate kept back; a pass that only skipped or held sessions has not delivered anything yet.
func (b *backfill) Pass(r uploader.Result, err error) {
	if err != nil {
		return
	}
	b.mu.Lock()
	b.queued = 0
	line := ""
	if b.firstSent.IsZero() && (r.Accepted > 0 || r.Unchanged > 0 || r.Skipped+r.Held == 0) {
		line = b.firstSentLocked() // nothing to send: the server already has the history
	}
	if b.passCoversAll && !b.complete {
		b.complete = true
		if line == "" {
			line = b.statusLocked().Line()
		}
	}
	b.mu.Unlock()
	b.log(line)
}

func (b *backfill) firstSentLocked() string {
	b.firstSent = b.now()
	return b.statusLocked().Line()
}

func (b *backfill) log(line string) {
	if line != "" && b.logf != nil {
		b.logf("%s", line)
	}
}

// Status returns the current numbers.
func (b *backfill) Status() BackfillStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.statusLocked()
}

func (b *backfill) statusLocked() BackfillStatus {
	st := BackfillStatus{Scanned: b.scanned, FirstPacketAt: b.firstSent.UTC(), Complete: b.complete}
	if b.firstSent.IsZero() {
		st.FirstPacketAt = time.Time{}
	}
	for _, s := range b.sources {
		st.Found += s.found
	}
	st.Remaining = max(st.Found-st.Scanned, 0) + b.queued
	return st
}
