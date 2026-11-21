package service

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harnext/harnext-agent/internal/account"
	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/identity"
	"github.com/harnext/harnext-agent/internal/parser/claudecode"
	"github.com/harnext/harnext-agent/internal/parser/copilot"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
	"github.com/harnext/harnext-agent/internal/selfupdate"
	"github.com/harnext/harnext-agent/internal/storage"
	"github.com/harnext/harnext-agent/internal/uploader"
	"github.com/harnext/harnext-agent/internal/watcher"
)

const (
	stateName            = "aggregate"
	copilotStateName     = "aggregate_copilot"
	DefaultFlushInterval = 30 * time.Second
	initialConfigTimeout = 20 * time.Second // upper bound of the background fetch; it never blocks reading
)

type RunOptions struct {
	Paths   Paths
	Version string
	// Root overrides transcript discovery with one explicit root (its parent is the config directory); for tests.
	Root string
	// ChatRoots overrides the VS Code workspaceStorage roots; for tests.
	ChatRoots []string
	// Home overrides the user home directory (Claude config discovery, git config); for tests.
	Home          string
	FlushInterval time.Duration // default DefaultFlushInterval
	UploadPeriod  time.Duration // default uploader.DefaultPeriod
	ConfigPeriod  time.Duration // default remoteconfig.DefaultPeriod
	// UpdateCheck installs a newer release and returns its version, or selfupdate.ErrNoUpdate. Nil uses the signed release
	// notice (only for release builds with trusted keys, never when HARNEXT_NO_AUTO_UPDATE is set). Tests inject one.
	UpdateCheck func(ctx context.Context) (string, error)
	// UpdateFirstDelay and UpdateInterval space the checks (zero: 2 minutes and 6 hours, jittered).
	UpdateFirstDelay time.Duration
	UpdateInterval   time.Duration
	// Backfill tuning; zero values mean the defaults (30 days, 400 sessions or 7 days, 2 s, 6 s).
	HistoryMaxAge       time.Duration
	FirstPacketSessions int
	FirstPacketWindow   time.Duration
	ConfigGrace         time.Duration
	BatchPause          time.Duration
	// Gate decides per session whether it is sent and to which account route. Nil builds the production gate: the attribution
	// gate over the signed identity document (fail closed without a valid one). Tests inject their own.
	Gate func(aggregate.Row) uploader.Decision
	// IdentityPeriod spaces the identity document refreshes (zero: identity.DefaultPeriod). The account check, report and
	// re-report periods default to 30 s, 30 min and 2 min; tests shorten them.
	IdentityPeriod     time.Duration
	AccountCheckPeriod time.Duration
	AccountReportEvery time.Duration
	AccountReportGap   time.Duration
	// Keys overrides the build-time trusted keys; for tests.
	Keys       remoteconfig.TrustedKeys
	Clock      remoteconfig.Clock // configuration expiry clock; default system clock
	HTTPClient *http.Client
	// Logf receives content-free operational messages; nil discards them.
	Logf func(format string, args ...any)
}

type sinkFunc func(watcher.ClaudeFile, claudecode.Event)

func (f sinkFunc) Handle(file watcher.ClaudeFile, ev claudecode.Event) { f(file, ev) }

type copilotSinkFunc func(watcher.FileID, copilot.Event)

func (f copilotSinkFunc) Handle(id watcher.FileID, ev copilot.Event) { f(id, ev) }

func ownTrim(s Settings) int64 {
	if s.CopilotOTelPath != "" {
		return 0
	}
	return watcher.DefaultTrimOver
}

// Run is the agent main loop. It returns nil when ctx ends and an error when it cannot
// start (not initialized, already running, unusable storage or configuration).
func Run(ctx context.Context, o RunOptions) error {
	for {
		rescan, err := runOnce(ctx, o)
		if err != nil || !rescan || ctx.Err() != nil {
			return err
		}
	}
}

// ErrRestartForUpdate is returned by Run after a new agent version was installed and the state flushed: the caller starts
// the new binary (selfupdate.Restart).
var ErrRestartForUpdate = errors.New("service: restart to run the updated agent")

// runOnce is one life of the agent. It returns rescan = true after it dropped the saved state and offsets because the
// configuration arrived after history had been read without it (friction analysis needs the text, which is not kept);
// Run then starts again and reads that history once more, now with the configuration.
func runOnce(parent context.Context, o RunOptions) (rescan bool, err error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = DefaultFlushInterval
	}
	if o.HistoryMaxAge <= 0 {
		o.HistoryMaxAge = historyMaxAge
	}
	if o.ConfigGrace <= 0 {
		o.ConfigGrace = configGrace
	}
	settings, err := o.Paths.LoadSettings()
	if err != nil {
		return false, err
	}
	token, err := o.Paths.LoadToken()
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(o.Paths.Dir, 0o700); err != nil {
		return false, errors.New("service: create directory")
	}
	lock, err := AcquireLock(o.Paths.Lock)
	if err != nil {
		return false, err
	}
	defer lock.Release()

	db, err := storage.Open(o.Paths.DB)
	if err != nil {
		return false, err
	}
	defer db.Close()
	offsets, err := LoadOffsets(db)
	if err != nil {
		return false, err
	}
	agg, err := restoreAggregator(db, offsets, logf)
	if err != nil {
		return false, err
	}
	copilotPath := o.Paths.CopilotOTelFile(settings)
	chatRoots := o.ChatRoots
	if len(chatRoots) == 0 {
		chatRoots = watcher.DefaultChatRoots()
	}
	copilotAgg, err := restoreCopilot(db, offsets, copilotPath, logf, chatRoots...)
	if err != nil {
		return false, err
	}
	chat := newChatSource(copilotAgg)

	keys := o.Keys
	if keys == nil {
		if keys, err = remoteconfig.BuildTrustedKeys(); err != nil {
			return false, err
		}
	}
	client, err := remoteconfig.NewClient(remoteconfig.ClientOptions{
		BaseURL: settings.ServerURL, Token: func() string { return token }, AgentVersion: o.Version,
		AllowInsecureLocalhost: settings.AllowInsecureLocalhost, HTTPClient: o.HTTPClient,
	})
	if err != nil {
		return false, err
	}
	mgr := remoteconfig.NewManager(remoteconfig.ManagerOptions{
		Fetcher: client, Keys: keys, Store: remoteconfig.NewStore(o.Paths.RemoteConfig), Period: o.ConfigPeriod, Clock: o.Clock,
	})

	// roots is the one discovery both the transcript watcher and the account gate use.
	home, homeErr := o.Home, error(nil)
	if home == "" {
		home, homeErr = os.UserHomeDir()
	}
	var fixedRoots []watcher.Root
	roots := func() []watcher.Root { return watcher.DiscoverClaudeRoots(home) }
	if o.Root != "" {
		fixedRoots = []watcher.Root{{Dir: filepath.Dir(o.Root), Projects: o.Root}}
		roots = func() []watcher.Root { return fixedRoots }
	} else if homeErr != nil {
		return false, homeErr
	}

	gate := o.Gate
	var ag *accountGate
	var upRef atomic.Pointer[uploader.Uploader]
	if gate == nil {
		idc, err := identity.NewClient(identity.ClientOptions{
			BaseURL: settings.ServerURL, Token: func() string { return token }, AgentVersion: o.Version,
			AllowInsecureLocalhost: settings.AllowInsecureLocalhost, HTTPClient: o.HTTPClient,
		})
		if err != nil {
			return false, err
		}
		idm := identity.NewManager(identity.ManagerOptions{
			Remote: idc, Keys: keys, Store: identity.NewStore(o.Paths.IdentityFile), Period: o.IdentityPeriod, Clock: o.Clock,
			// A connected account makes held sessions sendable: try them again at once.
			OnChange: func() {
				if u := upRef.Load(); u != nil {
					u.Rewake()
				}
			},
		})
		saved, _, err := db.LoadState(accountsStateName)
		if err != nil {
			return false, err
		}
		ag = newAccountGate(home, roots, saved, idm, len(keys) > 0, logf)
		ag.gate.ObserveAccounts() // before the first upload cycle
		gate = ag.Decide
	}

	parser := claudecode.NewParser(nil)
	// fileGate keeps the uploader from reading rows while a transcript or chat file is half read:
	// a session is sent once its file is fully processed, not once per partial state.
	fileGate := &sync.RWMutex{}
	app := &applier{mgr: mgr, parser: parser, agg: agg, cop: copilotAgg, chat: chat, logf: logf, gate: fileGate,
		unanalyzedBase: agg.Counters().UnanalyzedUserTurns}
	// A configuration that becomes active after history was read without it asks for one more reading (see runOnce).
	reanalyze := make(chan struct{}, 1)
	app.onAnalysis = func() {
		select {
		case reanalyze <- struct{}{}:
		default:
		}
	}

	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	// The configuration is fetched in the background and never blocks the upload: text read
	// while none is active is not analyzed (friction stays null; see configGrace for the P4
	// boundary). A cached configuration is applied at once; otherwise reading waits for the
	// fetch only up to the grace period, which a healthy server answers well within.
	app.apply()
	if len(keys) == 0 {
		logf("no trusted keys in this build: friction analysis is off")
	}
	if ag != nil {
		check := orDefault(o.AccountCheckPeriod, accountCheckPeriod)
		run(func() { ag.observeLoop(ctx, check) })
		if len(keys) == 0 {
			logf("no trusted keys in this build: sessions are held back (identity_pending)")
		} else {
			run(func() { _ = ag.idm.Run(ctx) })
			run(func() {
				ag.reportLoop(ctx, check, orDefault(o.AccountReportEvery, accountReportEvery), orDefault(o.AccountReportGap, accountReportGap), logf)
			})
		}
	}
	_, haveConfig := mgr.Current()
	if len(keys) > 0 {
		fetched := make(chan struct{})
		run(func() {
			if !haveConfig {
				cctx, cancel := context.WithTimeout(ctx, initialConfigTimeout)
				if err := mgr.Refresh(cctx); err != nil && ctx.Err() == nil {
					logf("initial configuration unavailable: %s", remoteconfig.CodeOf(err))
				}
				cancel()
				app.apply()
			}
			close(fetched)
			_ = mgr.Run(ctx)
		})
		if !haveConfig {
			grace := time.NewTimer(o.ConfigGrace)
			select {
			case <-fetched:
			case <-grace.C:
				logf("configuration not ready yet: reading history without waiting for it")
			case <-ctx.Done():
			}
			grace.Stop()
		}
	}

	bf := newBackfill(nil, logf, o.FirstPacketSessions, o.FirstPacketWindow)
	up, err := uploader.New(uploader.Options{
		BaseURL: settings.ServerURL, Token: func() string { return token }, AgentVersion: clip(o.Version, 32),
		Ledger: db, AllowInsecureLocalhost: settings.AllowInsecureLocalhost, HTTPClient: o.HTTPClient,
		BatchPause: o.BatchPause, OnBatch: bf.Batch, OnQueued: bf.Queued, Catchup: bf.Catchup, Backlog: func() int { return bf.Status().Remaining },
		BacklogTotal: func() int { return bf.Status().Found }, Gate: gate,
	})
	if err != nil {
		return false, err
	}
	upRef.Store(up)

	st := &statusKeeper{backfill: bf, acct: ag, paths: o.Paths, copilotPath: copilotPath, ownFile: settings.CopilotOTelPath == "", base: StatusFile{PID: os.Getpid(), Version: o.Version, StartedAt: time.Now().UTC()}}
	st.base.Config.TrustedKeys = len(keys) > 0

	var wlastErr statusErr
	claudeSrc, tailSrc := bf.source(), bf.source()
	claudeOpts := watcher.Options{
		Offsets: offsets, Parser: parser, MaxFileAge: o.HistoryMaxAge, Progress: claudeSrc, Gate: fileGate,
		Sink: sinkFunc(func(f watcher.ClaudeFile, ev claudecode.Event) { agg.AddFrom(account.DirID(f.ConfigDir), ev) }),
		OnError: func(err error) {
			wlastErr.set(err.Error())
			logf("watcher: %v", err)
		},
	}
	if o.Root != "" {
		claudeOpts.Roots = fixedRoots
	} else {
		claudeOpts.RootsFunc = roots
	}
	w, err := watcher.New(claudeOpts)
	if err != nil {
		return false, err
	}

	tailer, err := watcher.NewFileTailer(watcher.TailOptions{
		Path: copilotPath, Offsets: offsets,
		// Only the agent's own default file is emptied; a path the user set in VS Code is left alone.
		TrimOver: ownTrim(settings),
		Sink:     copilotSinkFunc(func(_ watcher.FileID, ev copilot.Event) { copilotAgg.Add(ev) }),
		OnError: func(err error) {
			wlastErr.set(err.Error())
			logf("copilot tailer: %v", err)
		},
	})
	if err != nil {
		return false, err
	}

	var chatW *watcher.ChatWatcher
	var chatSrc *bfSource
	if len(chatRoots) > 0 {
		chatSrc = bf.source()
		if chatW, err = watcher.NewChatWatcher(watcher.ChatOptions{
			Roots: chatRoots, Offsets: offsets, Sink: chat, MaxFileAge: o.HistoryMaxAge, Progress: chatSrc, Gate: fileGate,
			OnError: func(err error) {
				wlastErr.set(err.Error())
				logf("chat watcher: %v", err)
			},
		}); err != nil {
			return false, err
		}
	}

	publish := func() { st.publish(agg, copilotAgg, mgr, wlastErr.get(), logf) }
	flush := func() {
		if ag != nil {
			ag.gate.ObserveAccounts()
			if err := ag.saveTimeline(db); err != nil {
				logf("account history not saved: %v", err)
			}
		}
		pend := offsets.Pending()
		if !pend.Empty() {
			blob, err := json.Marshal(agg.Snapshot())
			if err == nil {
				err = db.SaveState(stateName, blob)
			}
			if err == nil {
				var cblob []byte
				if cblob, err = json.Marshal(copilotAgg.Snapshot()); err == nil {
					err = db.SaveState(copilotStateName, cblob)
				}
			}
			if err == nil {
				err = offsets.Commit(pend)
			}
			if err != nil {
				logf("flush failed: %v", err)
			}
		}
		app.apply()
		publish()
	}

	run(func() {
		if err := w.Run(ctx); err != nil && ctx.Err() == nil {
			logf("watcher stopped: %v", err)
		}
	})
	run(func() {
		if err := tailer.Run(ctx); err != nil && ctx.Err() == nil {
			logf("copilot tailer stopped: %v", err)
		}
	})
	// Each reader's first scan reports to the backfill; the first upload packet does not wait for
	// the slowest of them (see backfill.go).
	finished := func(ready <-chan struct{}, src *bfSource) {
		run(func() {
			select {
			case <-ready:
				src.Done()
			case <-ctx.Done():
			}
		})
	}
	finished(w.Ready(), claudeSrc)
	finished(tailer.Ready(), tailSrc)
	if chatW != nil {
		finished(chatW.Ready(), chatSrc)
		run(func() {
			if err := chatW.Run(ctx); err != nil && ctx.Err() == nil {
				logf("chat watcher stopped: %v", err)
			}
		})
	}
	run(func() {
		select {
		case <-bf.FirstPacketReady():
		case <-ctx.Done():
			return
		}
		publish()
		up.Run(ctx, app.source, o.UploadPeriod, func(r uploader.Result, err error) {
			bf.Pass(r, err)
			st.recordUpload(r, err)
			publish()
			if err != nil {
				logf("upload failed: %v", err)
			}
		})
	})
	run(func() {
		t := time.NewTicker(o.FlushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				flush()
			}
		}
	})

	// Self-update: a signed release notice for a newer version installs it and asks for a restart (see updateLoop).
	restartTo := make(chan string, 1)
	check := o.UpdateCheck
	if check == nil && os.Getenv("HARNEXT_NO_AUTO_UPDATE") == "" && selfupdate.Eligible(o.Version, keys, settings.ServerURL) {
		if exe, exeErr := os.Executable(); exeErr == nil {
			if resolved, linkErr := filepath.EvalSymlinks(exe); linkErr == nil {
				exe = resolved
			}
			uo := selfupdate.Options{ServerURL: settings.ServerURL, AllowInsecureLocalhost: settings.AllowInsecureLocalhost,
				Current: o.Version, Keys: keys, BinaryPath: exe, HTTPClient: o.HTTPClient}
			check = func(c context.Context) (string, error) { return selfupdate.Update(c, uo) }
		}
	}
	if check != nil {
		run(func() { updateLoop(ctx, check, o.UpdateFirstDelay, o.UpdateInterval, logf, restartTo) })
	}

	var updated string
	select {
	case <-ctx.Done():
	case <-reanalyze:
		rescan = true
		cancel()
	case updated = <-restartTo:
		cancel()
	}
	wg.Wait()
	if updated != "" {
		flush()
		logf("updated to %s: restarting", updated)
		return false, ErrRestartForUpdate
	}
	if rescan {
		logf("configuration arrived after history was read without it: reading that history again")
		if ag != nil {
			if err := ag.saveTimeline(db); err != nil {
				logf("account history not saved: %v", err)
			}
		}
		if err := dropReadState(db, offsets); err != nil {
			return false, err
		}
		return true, nil
	}
	flush()
	return false, nil
}

const (
	updateFirstDelay = 2 * time.Minute
	updateInterval   = 6 * time.Hour
)

// updateLoop checks for a new release shortly after start (not while the first history upload is under way) and every few
// hours. A failed check is logged and retried at the next interval; a successful install ends the loop and asks to restart.
func updateLoop(ctx context.Context, check func(context.Context) (string, error), first, every time.Duration, logf func(string, ...any), restartTo chan<- string) {
	if first <= 0 {
		first = updateFirstDelay
	}
	if every <= 0 {
		every = updateInterval
	}
	delay := first
	for {
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		v, err := check(ctx)
		switch {
		case err == nil:
			select {
			case restartTo <- v:
			default:
			}
			return
		case errors.Is(err, selfupdate.ErrNoUpdate):
		case ctx.Err() != nil:
			return
		default:
			logf("update check failed: %v", err)
		}
		delay = every + time.Duration(rand.Int63n(int64(every/10)+1)) // jitter: agents do not all ask at once
	}
}

// dropReadState forgets the saved aggregates and every offset so the next life reads all history from scratch. The upload
// ledger stays: rows that come out identical are not sent again, rows that gained friction are.
func dropReadState(db *storage.Store, offsets *OffsetStore) error {
	if err := offsets.Reset(); err != nil {
		return err
	}
	if err := db.DeleteState(stateName); err != nil {
		return err
	}
	return db.DeleteState(copilotStateName)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// restoreAggregator loads the saved snapshot. Offsets without a usable snapshot would skip
// data that was never counted, so they are discarded and transcripts are read again.
func restoreAggregator(db *storage.Store, offsets *OffsetStore, logf func(string, ...any)) (*aggregate.Aggregator, error) {
	agg := aggregate.New()
	blob, ok, err := db.LoadState(stateName)
	if err != nil {
		return nil, err
	}
	if ok {
		var snap aggregate.State
		if json.Unmarshal(blob, &snap) == nil && agg.Restore(snap) == nil {
			return agg, nil
		}
		logf("saved state is unreadable: rebuilding from transcripts")
		agg = aggregate.New()
	}
	if len(offsets.Paths()) > 0 {
		if err := offsets.Reset(); err != nil {
			return nil, err
		}
	}
	return agg, nil
}

// restoreCopilot loads the Copilot snapshot. Only the Copilot file's offset and the offsets
// under chatRoots are dropped when the snapshot cannot be trusted; Claude offsets are never touched here.
func restoreCopilot(db *storage.Store, offsets *OffsetStore, path string, logf func(string, ...any), chatRoots ...string) (*aggregate.CopilotAggregator, error) {
	agg := aggregate.NewCopilot()
	blob, ok, err := db.LoadState(copilotStateName)
	if err != nil {
		return nil, err
	}
	if ok {
		var snap aggregate.CopilotState
		if json.Unmarshal(blob, &snap) == nil && agg.Restore(snap) == nil {
			return agg, nil
		}
		logf("saved copilot state is unreadable: rebuilding from the telemetry file")
		agg = aggregate.NewCopilot()
	}
	if _, has := offsets.Get(path); has {
		if err := offsets.Delete(path); err != nil {
			return nil, err
		}
	}
	for _, r := range chatRoots {
		if r == "" {
			continue
		}
		if err := offsets.DeletePrefix(filepath.Clean(r) + string(filepath.Separator)); err != nil {
			return nil, err
		}
	}
	return agg, nil
}

// applier keeps the parser and aggregator on the current verified configuration. Rows and
// the version they were scored with are read under the same lock, so they always match.
type applier struct {
	mgr    *remoteconfig.Manager
	parser *claudecode.Parser
	agg    *aggregate.Aggregator
	cop    *aggregate.CopilotAggregator
	chat   *chatSource
	logf   func(string, ...any)
	gate   *sync.RWMutex // write-locked while rows are read; see fileGate
	// onAnalysis is called when analysis turns on after text was already read without it; unanalyzedBase is the
	// unanalyzed-turn count restored from disk (only turns read by this life count).
	onAnalysis     func()
	unanalyzedBase int64
	mu             sync.Mutex
	last           *remoteconfig.Config
	version        int64 // version of the configuration the aggregator currently uses; 0 = none
}

func (a *applier) apply() {
	cfg, ok := a.mgr.Current()
	if !ok {
		cfg = nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if cfg == a.last {
		return
	}
	a.last = cfg
	if cfg != nil {
		ex, err := analyzer.Compile(cfg)
		if err == nil {
			wasOff := a.version == 0
			a.parser.SetExtractor(ex)
			a.agg.SetAnalyzer(ex)
			a.cop.SetAnalyzer(ex)
			a.chat.setExtractor(ex)
			a.version = cfg.ConfigVersion
			a.logf("configuration v%d active", cfg.ConfigVersion)
			if wasOff && a.onAnalysis != nil && (a.agg.Counters().UnanalyzedUserTurns > a.unanalyzedBase || a.chat.unanalyzed.Load() > 0) {
				a.onAnalysis()
			}
			return
		}
		a.logf("configuration could not be compiled")
	} else {
		a.logf("no valid configuration: friction analysis paused")
	}
	a.parser.SetExtractor(nil)
	a.agg.SetAnalyzer(nil)
	a.cop.SetAnalyzer(nil)
	a.chat.setExtractor(nil)
	a.version = 0
}

// source returns the rows and the configuration version that scored them.
func (a *applier) source() ([]aggregate.Row, int64) {
	if a.gate != nil {
		a.gate.Lock()
		defer a.gate.Unlock()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rows := a.agg.Rows()
	if a.cop != nil {
		rows = append(rows, a.cop.Rows()...)
	}
	return rows, a.version
}

type statusErr struct {
	mu sync.Mutex
	s  string
}

func (e *statusErr) set(s string) { e.mu.Lock(); e.s = s; e.mu.Unlock() }
func (e *statusErr) get() string  { e.mu.Lock(); defer e.mu.Unlock(); return e.s }

type statusKeeper struct {
	backfill *backfill
	acct     *accountGate // nil when a test gate replaced the account gate
	paths    Paths
	// copilotPath is only stat-ed; it is never written to the status file.
	copilotPath string
	ownFile     bool
	mu          sync.Mutex
	base        StatusFile
	upload      UploadStatus
}

func (s *statusKeeper) recordUpload(r uploader.Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.upload.LastAttempt = now
	s.upload.Accepted, s.upload.Rejected, s.upload.Deferred = r.Accepted, r.Rejected, r.Deferred
	s.upload.Invalid, s.upload.Unchanged = r.Invalid, r.Unchanged
	s.upload.Skipped, s.upload.Held = r.Skipped, r.Held
	if err != nil {
		s.upload.LastError = errCode(err)
		return
	}
	s.upload.LastError = ""
	s.upload.LastSuccess = now
}

func errCode(err error) string {
	var ue *uploader.Error
	if errors.As(err, &ue) {
		return string(ue.Code)
	}
	return "error"
}

func (s *statusKeeper) publish(agg *aggregate.Aggregator, cop *aggregate.CopilotAggregator, mgr *remoteconfig.Manager, watcherErr string, logf func(string, ...any)) {
	c := agg.Counters()
	var skipped int64
	for _, n := range c.Skipped {
		skipped += n
	}
	copRows := cop.Rows() // also refreshes the folder split self-check counters
	cc := cop.Counters()
	var cskipped int64
	for _, n := range cc.Skipped {
		cskipped += n
	}
	var chatSkipped int64
	for _, n := range cc.ChatSkipped {
		chatSkipped += n
	}
	fi, ferr := os.Lstat(s.copilotPath)
	ms := mgr.Status()
	var gate GateSummary
	if s.acct != nil {
		gate = s.acct.Summary()
	}
	s.mu.Lock()
	out := s.base
	out.Identity, out.Accounts = gate.Identity, gate.Accounts
	out.Copilot = CopilotStatus{
		Sessions: aggregate.CountSessions(copRows), UniqueSpans: cc.UniqueSpans, SkippedLines: cskipped,
		BackgroundSpans: cc.Ignored["background_chat"], File: ferr == nil && fi.Mode().IsRegular(),
		OwnFile:      s.ownFile,
		ChatRequests: cc.ChatRequests, ChatSkippedLines: chatSkipped,
		TracesBound: cc.TracesBound, TracesUnbound: cc.TracesUnbound, TraceChecks: cc.TraceChecks, TraceMatches: cc.TraceMatches,
	}
	if out.Copilot.File {
		out.Copilot.FileBytes = fi.Size()
	}
	for _, cs := range cop.Snapshot().Sessions {
		if len(cs.Requests) > 0 {
			out.Copilot.ChatSessions++
		}
	}
	out.UpdatedAt = time.Now().UTC()
	out.Sessions = aggregate.CountSessions(agg.Rows())
	out.UniqueResponses, out.SkippedLines = c.UniqueResponses, skipped
	out.AnalyzedTurns, out.UnanalyzedTurns = c.AnalyzedUserTurns, c.UnanalyzedUserTurns
	out.WatcherLastError = watcherErr
	out.Config.Loaded, out.Config.Version, out.Config.ExpiresAt = ms.HasConfig, ms.ConfigVersion, ms.ExpiresAt
	out.Config.LastCheck, out.Config.LastError, out.Config.UpgradeRequired = ms.LastCheck, string(ms.LastError), ms.UpgradeRequired
	out.Upload = s.upload
	if s.backfill != nil {
		out.Backfill = s.backfill.Status()
	}
	s.mu.Unlock()
	if err := s.paths.WriteStatus(out); err != nil {
		logf("status not written: %v", err)
	}
}
