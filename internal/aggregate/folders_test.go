package aggregate

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/claudecode"
	"github.com/harnext/harnext-agent/internal/project"
)

// fakeResolve maps /proj/<rel> to the project "acme/app" at <rel>, /other/<rel> to "acme/other", and anything
// else to its own project (as the real resolver does for a directory without a repository or marker).
func fakeResolve(dir string) project.Identity {
	switch {
	case dir == "/proj":
		return project.Identity{Project: "acme/app"}
	case strings.HasPrefix(dir, "/proj/"):
		return project.Identity{Project: "acme/app", Path: strings.TrimPrefix(dir, "/proj/")}
	case dir == "/other":
		return project.Identity{Project: "acme/other"}
	case strings.HasPrefix(dir, "/other/"):
		return project.Identity{Project: "acme/other", Path: strings.TrimPrefix(dir, "/other/")}
	case strings.TrimSpace(dir) == "":
		return project.Identity{Project: project.Unknown}
	}
	return project.Identity{Project: "~" + dir}
}

func TestCollapsePath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""}, {".", ""}, {"/", ""},
		{"web", "web"}, {"web/src/components", "web"},
		{"apps/web/src", "apps/web"}, {"apps/web", "apps/web"}, {"apps", "apps"},
		{"packages/ui/lib/x", "packages/ui"}, {"services/billing", "services/billing"}, {"libs/a/b", "libs/a"},
		{"modules/m/n", "modules/m"}, {"projects/p/q", "projects/p"}, {"cmd/agent/main", "cmd/agent"}, {"cmd", "cmd"},
		{"internal/aggregate", "internal"}, {"src/apps/web", "src"}, {"docs", "docs"},
		{`apps\web\src`, "apps/web"}, {"/apps/web/", "apps/web"}, {"./apps//web", "apps/web"}, {"../apps", "apps"},
		{strings.Repeat("a", 600), ""},
	}
	for _, tt := range tests {
		if got := CollapsePath(tt.in); got != tt.want {
			t.Errorf("CollapsePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRequestFolderRules(t *testing.T) {
	dirs := func(items ...DirCount) []DirCount { return items }
	tests := []struct {
		name string
		cwd  string
		dirs []DirCount
		want string
	}{
		{"edits win over more reads", "/proj", dirs(DirCount{"/proj/apps/web/src", 1, 0}, DirCount{"/proj/docs", 0, 9}), "apps/web"},
		{"most edits", "/proj", dirs(DirCount{"/proj/a/x", 1, 0}, DirCount{"/proj/b/y", 2, 0}), "b"},
		{"edit tie goes to the smaller path", "/proj", dirs(DirCount{"/proj/b", 1, 0}, DirCount{"/proj/a", 1, 0}), "a"},
		{"root is the smallest", "/proj", dirs(DirCount{"/proj/a", 1, 0}, DirCount{"/proj", 1, 0}), ""},
		{"no edits: reads", "/proj", dirs(DirCount{"/proj/docs", 0, 1}, DirCount{"/proj/src/x", 0, 3}), "src"},
		{"nothing: working directory", "/proj/services/billing/internal", nil, "services/billing"},
		{"files of another project do not count", "/proj", dirs(DirCount{"/other/lib", 5, 0}), ""},
		{"working directory folder when only outside files", "/proj/web", dirs(DirCount{"/other/lib", 5, 0}), "web"},
		{"folders of one unit add up", "/proj", dirs(DirCount{"/proj/apps/web/a", 1, 0}, DirCount{"/proj/apps/web/b", 1, 0}, DirCount{"/proj/apps/api", 1, 0}), "apps/web"},
		{"unknown working directory", "", dirs(DirCount{"/proj/a", 1, 0}), ""},
		{"markerless project: files under the cwd count", "/home/me/scratch", dirs(DirCount{"/home/me/scratch/sub/x", 1, 0}), "sub"},
		{"markerless project: sibling does not", "/home/me/scratch", dirs(DirCount{"/home/me/other", 1, 0}), ""},
	}
	for _, tt := range tests {
		if got := RequestFolder(fakeResolve, tt.cwd, tt.dirs); got != tt.want {
			t.Errorf("%s: folder = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// ---- event builders ----

type sim struct {
	t   *testing.T
	a   *Aggregator
	ex  analyzer.Extractor
	n   int
	ts  time.Time
	cwd string
}

func newSim(t *testing.T) *sim {
	a := New()
	a.SetResolver(fakeResolve)
	ex := testExtractor(t)
	a.SetAnalyzer(ex)
	return &sim{t: t, a: a, ex: ex, ts: at("2026-02-01T10:00:00Z"), cwd: "/proj"}
}

func (s *sim) next() string {
	s.n++
	s.ts = s.ts.Add(time.Second)
	return fmt.Sprintf("%d", s.n)
}

// user feeds a real analyzed user message.
func (s *sim) user(text string) {
	s.a.Add(claudecode.UserTurn{SessionID: "s", UUID: "u" + s.next(), Timestamp: s.ts, Cwd: s.cwd, Analyzed: true, Features: s.ex.UserFeatures(text)})
}

func (s *sim) interrupt() {
	s.a.Add(claudecode.UserTurn{SessionID: "s", UUID: "u" + s.next(), Timestamp: s.ts, Cwd: s.cwd, Analyzed: true, Features: analyzer.UserFeatures{Interrupted: true}})
}

func (s *sim) unanalyzedUser() {
	s.a.Add(claudecode.UserTurn{SessionID: "s", UUID: "u" + s.next(), Timestamp: s.ts, Cwd: s.cwd})
}

type tool struct {
	name, dir string
}

// response feeds one assistant response with out output tokens and tool uses.
func (s *sim) response(out int64, tools ...tool) {
	id := s.next()
	e := claudecode.AssistantUsage{
		SessionID: "s", MessageID: "m" + id, RequestID: "r" + id, Model: "claude-x", Timestamp: s.ts, Cwd: s.cwd,
		Usage: claudecode.Usage{InputTokens: 1, OutputTokens: out},
	}
	for i, tl := range tools {
		e.Tools[i] = claudecode.ToolUse{ID: "t" + id + fmt.Sprint(i), Name: tl.name, Dir: tl.dir}
	}
	e.ToolCount = len(tools)
	s.a.Add(e)
}

func (s *sim) toolError(hash uint64) {
	u := claudecode.UserTurn{SessionID: "s", UUID: "tr" + s.next(), Timestamp: s.ts, Cwd: s.cwd, IsToolResult: true, Analyzed: true, IsError: true, ResultCount: 1}
	u.Results[0] = claudecode.ToolResult{IsError: true, ErrorHash: hash}
	s.a.Add(u)
}

func edit(dir string) tool { return tool{"Edit", dir} }
func read(dir string) tool { return tool{"Read", dir} }

// check asserts the invariants of the folder split and returns the rows' common view.
func (s *sim) check(label string) (parts []FolderPart, rows []Row) {
	s.t.Helper()
	rows = s.a.Rows()
	st := s.a.Snapshot().Sessions["s"]
	if st == nil || len(rows) == 0 {
		return nil, rows
	}
	snap := st.Analysis.Stats.Snapshot()
	parts = rows[0].Parts
	for _, r := range rows {
		if !reflect.DeepEqual(r.Parts, parts) {
			s.t.Fatalf("%s: rows disagree on the folder list", label)
		}
		if r.Quality != snap {
			s.t.Fatalf("%s: row quality %+v != Snapshot %+v", label, r.Quality, snap)
		}
	}
	assertFoldersSumToSession(s.t, label, parts, snap)
	return parts, rows
}

// assertFoldersSumToSession is the spike's central claim: every counter of Quality, summed over the folders,
// equals the session's SessionStats.Snapshot(); FirstFrictionExchange is a position and is checked on its own.
func assertFoldersSumToSession(t *testing.T, label string, parts []FolderPart, snap analyzer.Quality) {
	t.Helper()
	var sum analyzer.Quality
	for _, p := range parts {
		sum.Add(p.Quality)
		q := p.Quality
		if q.FrictionExchanges > q.AnalyzedExchanges || q.AnalyzedExchanges > q.Exchanges {
			t.Fatalf("%s: folder %q breaks friction <= analyzed <= exchanges: %+v", label, p.Path, q)
		}
		if (q.FirstFrictionExchange == 0) != (q.FrictionExchanges == 0) || q.FirstFrictionExchange > q.AnalyzedExchanges {
			t.Fatalf("%s: folder %q first friction %d with %d friction of %d analyzed", label, p.Path, q.FirstFrictionExchange, q.FrictionExchanges, q.AnalyzedExchanges)
		}
		if q.FxPrimaryPushback+q.FxPrimaryToolLoop+q.FxPrimaryInterrupt != q.FrictionExchanges {
			t.Fatalf("%s: folder %q primary channels do not add up: %+v", label, p.Path, q)
		}
		// The same consistency rules the uploader and the server enforce on every folder (toWireQuality, qualitySchema).
		if q.FxPushback > q.FrictionExchanges || q.FxToolLoop > q.FrictionExchanges || q.FxInterrupt > q.FrictionExchanges ||
			q.ToolErrors > q.ToolCalls || q.CancelledResponses > q.Responses {
			t.Fatalf("%s: folder %q has a subset above its total: %+v", label, p.Path, q)
		}
		for i, c := range q.Counters() {
			if *c < 0 {
				t.Fatalf("%s: folder %q counter %d is negative", label, p.Path, i)
			}
		}
	}
	want := snap
	want.FirstFrictionExchange, sum.FirstFrictionExchange = 0, 0
	rv, rw := reflect.ValueOf(sum), reflect.ValueOf(want)
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).Int() != rw.Field(i).Int() {
			t.Fatalf("%s: Σ folders %s = %d, session Snapshot = %d\nfolders=%+v", label, rv.Type().Field(i).Name, rv.Field(i).Int(), rw.Field(i).Int(), parts)
		}
	}
	if (snap.FirstFrictionExchange == 0) != (sumFirst(parts) == 0) {
		t.Fatalf("%s: session first friction %d but folders %+v", label, snap.FirstFrictionExchange, parts)
	}
}

func sumFirst(parts []FolderPart) (n int64) {
	for _, p := range parts {
		n += p.Quality.FirstFrictionExchange
	}
	return n
}

func partOf(parts []FolderPart, path string) FolderPart {
	for _, p := range parts {
		if p.Path == path {
			return p
		}
	}
	return FolderPart{Path: "<missing:" + path + ">"}
}

// ---- table-driven scenarios ----

func TestFolderSplitScenarios(t *testing.T) {
	t.Run("requests land in the folder of their edits", func(t *testing.T) {
		s := newSim(t)
		s.user("add the page")
		s.response(100, read("/proj/docs"), edit("/proj/apps/web/src"), edit("/proj/apps/web/lib"))
		s.user("now the api")
		s.response(200, edit("/proj/services/api/handlers"))
		parts, rows := s.check("two folders")
		if len(parts) != 2 || partOf(parts, "apps/web").Turns != 1 || partOf(parts, "services/api").Turns != 1 {
			t.Fatalf("parts = %+v", parts)
		}
		var web, api int64
		for _, r := range rows {
			switch r.Folder {
			case "apps/web":
				web += r.OutputTokens
			case "services/api":
				api += r.OutputTokens
			}
		}
		if web != 100 || api != 200 {
			t.Errorf("tokens web=%d api=%d", web, api)
		}
	})

	t.Run("pushback goes to the request that failed, not to the one that complains", func(t *testing.T) {
		s := newSim(t)
		s.user("first")
		s.response(10, edit("/proj/web/a"))
		s.user("this is a fixture broken thing") // the complaint opens a request in folder docs and fails request 1
		s.response(10, edit("/proj/docs/b"))
		s.user("thanks")
		s.response(10, edit("/proj/docs/b"))
		parts, _ := s.check("pushback")
		web, docs := partOf(parts, "web"), partOf(parts, "docs")
		if web.Quality.FrictionExchanges != 1 || web.Quality.FxPushback != 1 || web.Quality.FirstFrictionExchange != 1 {
			t.Errorf("web = %+v", web.Quality)
		}
		if docs.Quality.FrictionExchanges != 0 || docs.Quality.SigErrorReport != 1 || docs.Quality.Exchanges != 2 {
			t.Errorf("docs = %+v", docs.Quality)
		}
	})

	t.Run("tool loop and interrupt friction of the open request count at once", func(t *testing.T) {
		s := newSim(t)
		s.user("go")
		s.response(5, edit("/proj/web/a"))
		s.toolError(7)
		s.toolError(7) // repeat_min_turns = 2: a tool loop in the open request
		parts, _ := s.check("open loop")
		if q := partOf(parts, "web").Quality; q.FrictionExchanges != 1 || q.FxToolLoop != 1 || q.RepeatedErrorRuns != 1 || q.ToolErrors != 2 {
			t.Errorf("web = %+v", q)
		}
		s.interrupt()
		s.user("next")
		s.response(5, edit("/proj/docs/x"))
		parts, _ = s.check("closed loop")
		if q := partOf(parts, "web").Quality; q.FrictionExchanges != 1 || q.FxInterrupt != 1 || q.FxToolLoop != 1 || q.FxPrimaryToolLoop != 1 {
			t.Errorf("web after close = %+v", q)
		}
		if partOf(parts, "web").Turns != 2 { // the interrupt marker is a user message of the open request
			t.Errorf("web turns = %d", partOf(parts, "web").Turns)
		}
	})

	t.Run("a tool loop that spans two requests keeps its run, each error belongs to its own request", func(t *testing.T) {
		s := newSim(t)
		s.user("a")
		s.response(1, edit("/proj/web/a"))
		s.toolError(9)
		s.user("b")
		s.response(1, edit("/proj/docs/a"))
		s.toolError(9) // run reaches 2 in request b: the loop is request b's
		parts, _ := s.check("span")
		if q := partOf(parts, "docs").Quality; q.FxToolLoop != 1 || q.RepeatedErrorRuns != 1 || q.RepeatedErrorCalls != 2 {
			t.Errorf("docs = %+v", q)
		}
		if q := partOf(parts, "web").Quality; q.FxToolLoop != 0 || q.ToolErrors != 1 {
			t.Errorf("web = %+v", q)
		}
	})

	t.Run("a request without edits uses reads then the working directory", func(t *testing.T) {
		s := newSim(t)
		s.cwd = "/proj/services/billing/src"
		s.user("explain")
		s.response(1, read("/proj/docs/guide"))
		s.user("run it")
		s.response(1)
		parts, _ := s.check("fallbacks")
		if partOf(parts, "docs").Turns != 1 || partOf(parts, "services/billing").Turns != 1 {
			t.Errorf("parts = %+v", parts)
		}
	})

	t.Run("unanalyzed requests are counted but not scored", func(t *testing.T) {
		s := newSim(t)
		s.unanalyzedUser()
		s.response(1, edit("/proj/web/a"))
		s.unanalyzedUser()
		s.response(1, edit("/proj/docs/a"))
		parts, _ := s.check("unanalyzed")
		if q := partOf(parts, "web").Quality; q.Exchanges != 1 || q.AnalyzedExchanges != 0 {
			t.Errorf("web = %+v", q)
		}
	})

	t.Run("responses before the first message belong to the first folder they touch", func(t *testing.T) {
		s := newSim(t)
		s.response(7, edit("/proj/web/a")) // resumed transcript: no user message yet
		s.user("go")
		s.response(1, edit("/proj/docs/a"))
		parts, rows := s.check("pre-request")
		var out int64
		for _, r := range rows {
			out += r.OutputTokens
		}
		if out != 8 || partOf(parts, "web").Turns != 0 || partOf(parts, "docs").Turns != 1 {
			t.Errorf("out=%d parts=%+v", out, parts)
		}
	})

	t.Run("replayed lines change nothing", func(t *testing.T) {
		s := newSim(t)
		s.user("go")
		e := claudecode.AssistantUsage{SessionID: "s", MessageID: "m", RequestID: "r", Model: "claude-x", Timestamp: s.ts, Cwd: s.cwd, Usage: claudecode.Usage{OutputTokens: 5}}
		e.Tools[0], e.ToolCount = claudecode.ToolUse{ID: "t1", Name: "Edit", Dir: "/proj/web"}, 1
		s.a.Add(e)
		before, _ := s.check("once")
		s.a.Add(e)
		after, _ := s.check("twice")
		if !reflect.DeepEqual(before, after) {
			t.Errorf("replay changed the folders: %+v vs %+v", before, after)
		}
	})

	t.Run("at most 64 folders per session; the rest go to the root", func(t *testing.T) {
		s := newSim(t)
		for i := 0; i < 80; i++ {
			s.user("go")
			s.response(1, edit(fmt.Sprintf("/proj/f%02d/x", i)))
		}
		parts, _ := s.check("cap")
		if len(parts) != maxFolders {
			t.Fatalf("folders = %d, want %d", len(parts), maxFolders)
		}
		if partOf(parts, "").Turns != 80-(maxFolders-1) {
			t.Errorf("root turns = %d", partOf(parts, "").Turns)
		}
	})

	t.Run("main folder: most requests, ties to the smaller path", func(t *testing.T) {
		s := newSim(t)
		for _, d := range []string{"b", "b", "a", "a", "c"} {
			s.user("go")
			s.response(1, edit("/proj/"+d+"/x"))
		}
		_, rows := s.check("main")
		if rows[0].MainFolder != "a" {
			t.Errorf("main = %q", rows[0].MainFolder)
		}
	})
}

// The example of the plan: 10 requests, 5 in y, 1 in x, 2 in r, 2 in p; the only friction is in x.
func TestFolderSplitPlanExample(t *testing.T) {
	s := newSim(t)
	order := []string{"y", "y", "x", "r", "y", "p", "y", "r", "y", "p"}
	for i, d := range order {
		s.user("go on") // a complaint would be pushback on the PREVIOUS request, so x's friction is a tool loop instead

		s.response(10, edit("/proj/"+d+"/src"))
		if i == 2 { // the request in x ends in a tool loop
			s.toolError(3)
			s.toolError(3)
		}
	}
	parts, rows := s.check("plan")
	want := map[string]int64{"y": 5, "x": 1, "r": 2, "p": 2}
	var total int64
	for path, n := range want {
		if p := partOf(parts, path); p.Turns != n {
			t.Errorf("%s turns = %d, want %d", path, p.Turns, n)
		}
		total += partOf(parts, path).Turns
	}
	if total != 10 {
		t.Errorf("total = %d", total)
	}
	for _, p := range parts {
		if (p.Path == "x") != (p.Quality.FrictionExchanges > 0) {
			t.Errorf("friction in %q: %+v", p.Path, p.Quality)
		}
	}
	var out int64
	for _, r := range rows {
		out += r.OutputTokens
	}
	if out != 100 {
		t.Errorf("output = %d", out)
	}
	if rows[0].MainFolder != "y" {
		t.Errorf("main = %q", rows[0].MainFolder)
	}
}

// ---- random event sequences ----

var randomTexts = []string{
	"add a feature to the page", "this is a fixture broken thing", "please fix the crash", "all good thanks", "all good thanks",
	"add a feature to the page", "run the tests again", "explain this module",
}

var randomDirs = []string{
	"/proj/apps/web/src", "/proj/apps/web/lib", "/proj/apps/api", "/proj/services/billing/x", "/proj/docs", "/proj/internal/a/b",
	"/proj", "/other/lib", "/tmp/x", "/proj/zz",
}

func TestFolderSplitRandomSequences(t *testing.T) {
	var multi, pushback, loop, interrupt, tools, first int
	defer func() {
		// The generator must really produce what the invariants are about.
		for name, n := range map[string]int{"multi-folder sessions": multi, "pushback": pushback, "tool loop": loop, "interrupt": interrupt, "permission/tool results": tools, "first friction in a folder > 1": first} {
			if n < 10 {
				t.Errorf("random sequences produced too few %s: %d", name, n)
			}
		}
	}()
	for seed := int64(1); seed <= 150; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := newSim(t)
		var wantOut, wantTurns int64
		seenResp := map[string]bool{}
		seenUser := map[string]bool{}
		var lastResp claudecode.AssistantUsage
		var lastUser claudecode.UserTurn
		haveResp, haveUser := false, false
		steps := 40 + rng.Intn(120)
		for step := 0; step < steps; step++ {
			label := fmt.Sprintf("seed %d step %d", seed, step)
			switch k := rng.Intn(20); {
			case k < 5: // real user message
				u := claudecode.UserTurn{SessionID: "s", UUID: "u" + s.next(), Timestamp: s.ts, Cwd: s.cwd, IsSidechain: rng.Intn(10) == 0}
				switch r := rng.Intn(10); {
				case r < 6:
					u.Analyzed, u.Features = true, s.ex.UserFeatures(randomTexts[rng.Intn(len(randomTexts))])
				case r < 7: // pasted error output
					u.Analyzed, u.Features = true, s.ex.UserFeatures(randomTexts[rng.Intn(len(randomTexts))])
					u.Features.Chars, u.Features.ErrorRunes = 100, 80
				case r < 8:
					u.Analyzed, u.Features = true, analyzer.UserFeatures{Interrupted: true}
				}
				s.a.Add(u)
				if !u.IsSidechain {
					wantTurns++
				}
				lastUser, haveUser = u, true
			case k < 10: // tool result(s)
				u := claudecode.UserTurn{SessionID: "s", UUID: "tr" + s.next(), Timestamp: s.ts, Cwd: s.cwd, IsToolResult: true, Analyzed: true,
					IsSidechain: rng.Intn(8) == 0, Interrupted: rng.Intn(10) == 0}
				u.ResultCount = rng.Intn(4)
				for i := 0; i < u.ResultCount; i++ {
					u.Results[i] = claudecode.ToolResult{IsError: rng.Intn(2) == 0, ErrorHash: uint64(rng.Intn(3)), Denied: rng.Intn(12) == 0}
				}
				s.a.Add(u)
			case k < 16: // assistant response
				id := s.next()
				e := claudecode.AssistantUsage{
					SessionID: "s", MessageID: "m" + id, RequestID: "r" + id, Model: "claude-x", Timestamp: s.ts, Cwd: s.cwd,
					IsSidechain: rng.Intn(8) == 0,
					Usage:       claudecode.Usage{InputTokens: int64(rng.Intn(5)), OutputTokens: int64(rng.Intn(100)), CacheReadTokens: int64(rng.Intn(50)), WebSearchRequests: int64(rng.Intn(2))},
				}
				e.ToolCount = rng.Intn(4)
				for i := 0; i < e.ToolCount; i++ {
					name := []string{"Edit", "Write", "Read", "Bash", "MultiEdit", "NotebookEdit", "Grep"}[rng.Intn(7)]
					e.Tools[i] = claudecode.ToolUse{ID: fmt.Sprintf("t%s-%d", id, i), Name: name, Dir: randomDirs[rng.Intn(len(randomDirs))]}
				}
				s.a.Add(e)
				seenResp[e.MessageID] = true
				wantOut += e.Usage.OutputTokens
				lastResp, haveResp = e, true
			case k < 18: // replay of an earlier line
				if haveResp && rng.Intn(2) == 0 {
					s.a.Add(lastResp)
				} else if haveUser {
					s.a.Add(lastUser)
				}
			case k < 19:
				s.cwd = []string{"/proj", "/proj/apps/web", "/proj/services/billing/src", ""}[rng.Intn(4)]
			default:
				s.ts = s.ts.Add(time.Hour)
			}
			_ = seenUser
			if seed <= 30 || step == steps-1 {
				parts, rows := s.check(label)
				if len(rows) == 0 {
					continue
				}
				var out, turns int64
				for _, r := range rows {
					out += r.OutputTokens
				}
				for _, p := range parts {
					turns += p.Turns
				}
				if step == steps-1 {
					if out != wantOut {
						t.Fatalf("%s: Σ output = %d, want %d", label, out, wantOut)
					}
					if turns != wantTurns {
						t.Fatalf("%s: Σ turns = %d, want %d", label, turns, wantTurns)
					}
				}
				if len(parts) > maxFolders {
					t.Fatalf("%s: %d folders", label, len(parts))
				}
				if step == steps-1 {
					if len(parts) > 1 {
						multi++
					}
					q := rows[0].Quality
					if q.FxPushback > 0 {
						pushback++
					}
					if q.FxToolLoop > 0 {
						loop++
					}
					if q.FxInterrupt > 0 {
						interrupt++
					}
					if q.ToolCalls > 0 {
						tools++
					}
					for _, p := range parts {
						if p.Quality.FirstFrictionExchange > 1 {
							first++
						}
					}
				}
			}
		}
		// The state survives a snapshot/restore round trip and keeps producing the same rows.
		before := s.a.Rows()
		b := New()
		b.SetResolver(fakeResolve)
		b.SetAnalyzer(s.ex)
		if err := b.Restore(s.a.Snapshot()); err != nil {
			t.Fatal(err)
		}
		if after := b.Rows(); !reflect.DeepEqual(before, after) {
			t.Fatalf("seed %d: rows differ after restore", seed)
		}
	}
}
