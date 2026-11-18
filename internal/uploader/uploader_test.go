package uploader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/project"
)

type memLedger struct {
	mu sync.Mutex
	m  map[string]string
}

func newLedger() *memLedger { return &memLedger{m: map[string]string{}} }

func (l *memLedger) UploadedDigest(k string) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.m[k]
	return d, ok, nil
}

func (l *memLedger) MarkUploaded(d map[string]string, _ time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, v := range d {
		l.m[k] = v
	}
	return nil
}

// withParts gives the row its session's single root folder (turns and quality of the session), as the aggregators do.
func withParts(r aggregate.Row, turns int64) aggregate.Row {
	r.Parts = []aggregate.FolderPart{{Path: "", Turns: turns, Quality: r.Quality}}
	return r
}

func row(id string) aggregate.Row {
	return withParts(aggregate.Row{
		SessionID: id, Tool: aggregate.ToolClaudeCode, Model: "claude-sonnet-4", Day: "2026-11-01", Speed: "standard", PromptFloor: 0,
		Totals:    aggregate.Totals{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 5, CacheCreation1hTokens: 2},
		StartedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
		EndedAt:   time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC),
		Cwd:       "/work/app", GitBranch: "main",
		Topic: "feature", DetectorVersion: 1, SignalCoverage: aggregate.ClaudeCodeCoverage,
		Quality: analyzer.Quality{
			Exchanges: 3, AnalyzedExchanges: 3, FrictionExchanges: 1, FxPushback: 1, FxPrimaryPushback: 1,
			SigErrorReport: 1, ToolCalls: 4, ToolErrors: 1, Responses: 3, FirstFrictionExchange: 2,
		},
	}, 3)
}

type server struct {
	t       *testing.T
	handler func(w http.ResponseWriter, body map[string]any)
	mu      sync.Mutex
	bodies  []map[string]any
	auths   []string
}

func (s *server) start() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Errorf("bad body: %v", err)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.mu.Unlock()
		if r.URL.Path != "/api/v1/ingest" || r.Method != http.MethodPost {
			s.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		s.handler(w, body)
	}))
}

func okHandler(w http.ResponseWriter, body map[string]any) {
	n := len(body["sessions"].([]any))
	fmt.Fprintf(w, `{"accepted":%d,"rejected":0,"errors":[]}`, n)
}

func newUploader(t *testing.T, url string, l Ledger) (*Uploader, *[]time.Duration) {
	t.Helper()
	var sleeps []time.Duration
	u, err := New(Options{
		BaseURL: url, Token: func() string { return "hxt_secret-token" }, AgentVersion: "0.1.0",
		Ledger: l, AllowInsecureLocalhost: true,
		Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return u, &sleeps
}

func TestSyncSendsContractShapeAndSkipsUnchanged(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	l := newLedger()
	u, _ := newUploader(t, ts.URL, l)

	res, err := u.Sync(context.Background(), []aggregate.Row{row("a"), row("b")}, 0)
	if err != nil || res.Accepted != 2 || res.Batches != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if s.auths[0] != "Bearer hxt_secret-token" {
		t.Fatalf("auth header = %q", s.auths[0])
	}
	b := s.bodies[0]
	if b["payload_version"] != float64(5) || b["agent_version"] != "0.1.0" {
		t.Fatalf("envelope = %v", b)
	}
	want := []string{"quality", "project", "session_external_id", "started_at", "ended_at", "tool_name", "topic", "turn_count", "usage", "folders"}
	sort.Strings(want)
	first := b["sessions"].([]any)[0].(map[string]any)
	var got []string
	for k := range first {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session keys = %v\nwant %v", got, want)
	}
	wantUsage := []string{
		"cache_creation_1h_tokens", "cache_creation_tokens", "cache_read_tokens", "day", "input_tokens", "model",
		"output_tokens", "prompt_floor", "speed", "web_fetch_requests", "web_search_requests",
	}
	got = got[:0]
	for k := range first["usage"].([]any)[0].(map[string]any) {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(wantUsage, ",") {
		t.Fatalf("usage keys = %v\nwant %v", got, wantUsage)
	}

	// Same content again: nothing is sent. A changed row is sent alone.
	changed := row("b")
	changed.OutputTokens++
	res, err = u.Sync(context.Background(), []aggregate.Row{row("a"), changed}, 0)
	if err != nil || res.Unchanged != 1 || res.Accepted != 1 || len(s.bodies) != 2 {
		t.Fatalf("res=%+v err=%v bodies=%d", res, err, len(s.bodies))
	}
	res, _ = u.Sync(context.Background(), []aggregate.Row{row("a"), changed}, 0)
	if res.Unchanged != 2 || len(s.bodies) != 2 {
		t.Fatalf("res=%+v bodies=%d", res, len(s.bodies))
	}
}

func TestInvalidRowsAreNeverSent(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())

	old := row("old")
	old.StartedAt = time.Date(2023, 12, 31, 0, 0, 0, 0, time.UTC)
	backwards := row("back")
	backwards.EndedAt = backwards.StartedAt.Add(-time.Second)
	long := row("long")
	long.EndedAt = long.StartedAt.Add(91 * 24 * time.Hour)
	huge := row("huge")
	huge.InputTokens = 100_000_000_001
	badCache := row("cache")
	badCache.CacheCreation1hTokens = 99
	noModel := row("nomodel")
	noModel.Model = ""
	longID := row(strings.Repeat("x", 201))
	otherTool := row("tool")
	otherTool.Tool = "unknown_tool"
	badFriction := row("fr")
	badFriction.Quality.FrictionExchanges = 9 // more friction exchanges than analyzed exchanges

	// Empty cwd and an unknown topic are repaired, not dropped.
	repaired := row("ok")
	repaired.Cwd, repaired.Topic, repaired.GitBranch = "", "", strings.Repeat("b", 201)

	res, err := u.Sync(context.Background(), []aggregate.Row{old, backwards, long, huge, badCache, noModel, longID, otherTool, badFriction, repaired}, 0)
	if err != nil || res.Invalid != 9 || res.Accepted != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	sess := s.bodies[0]["sessions"].([]any)[0].(map[string]any)
	if _, hasBranch := sess["git_branch"]; sess["project"] != "unknown" || sess["topic"] != "other" || hasBranch {
		t.Fatalf("repaired row = %v", sess)
	}
}

func TestOnlyTheProjectIdentityLeavesTheMachine(t *testing.T) {
	prev := resolveProject
	resolveProject = project.NewResolver("/Users/someone").Resolve
	defer func() { resolveProject = prev }()

	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())
	r := row("p")
	r.Cwd, r.GitBranch = "/Users/someone/clients/acme-bank/api", "feature/ACME-1234-secret"
	if _, err := u.Sync(context.Background(), []aggregate.Row{r}, 0); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s.bodies[0])
	sess := s.bodies[0]["sessions"].([]any)[0].(map[string]any)
	if sess["project"] != "~/clients/acme-bank/api" {
		t.Fatalf("project = %v", sess["project"])
	}
	for _, leak := range []string{"someone", "ACME-1234", "feature/"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("payload leaked %q: %s", leak, raw)
		}
	}
}

func TestCopilotRowIsAcceptedWithoutFriction(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())
	r := row("c")
	r.Tool, r.Cwd, r.Topic, r.Quality, r.DetectorVersion = aggregate.ToolVSCodeCopilot, "", "", analyzer.Quality{}, 0
	r.CacheCreationTokens, r.CacheCreation1hTokens = 0, 0
	res, err := u.Sync(context.Background(), []aggregate.Row{r}, 0)
	if err != nil || res.Accepted != 1 || res.Invalid != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	sess := s.bodies[0]["sessions"].([]any)[0].(map[string]any)
	if sess["tool_name"] != "vscode_copilot" || sess["project"] != "unknown" || sess["topic"] != "other" || sess["quality"] == nil {
		t.Fatalf("session = %v", sess)
	}
}

func copilotRow(id string, credits float64) aggregate.Row {
	r := row(id)
	r.Tool = aggregate.ToolVSCodeCopilot
	r.CacheCreationTokens, r.CacheCreation1hTokens = 0, 0
	r.CopilotCredits = &credits
	return r
}

func TestCopilotCreditsOnTheWire(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	l := newLedger()
	u, _ := newUploader(t, ts.URL, l)

	if _, err := u.Sync(context.Background(), []aggregate.Row{copilotRow("c", 2.01016), copilotRow("z", 0), row("cl")}, 0); err != nil {
		t.Fatal(err)
	}
	by := map[string]map[string]any{}
	for _, v := range s.bodies[0]["sessions"].([]any) {
		m := v.(map[string]any)
		by[m["session_external_id"].(string)] = m
	}
	use := func(id string) map[string]any { return by[id]["usage"].([]any)[0].(map[string]any) }
	if use("c")["copilot_credits"] != 2.01016 {
		t.Fatalf("copilot_credits = %v", use("c")["copilot_credits"])
	}
	if v, ok := use("z")["copilot_credits"]; !ok || v != float64(0) {
		t.Fatalf("zero credits = %v (present=%v), want 0", v, ok)
	}
	if _, ok := use("cl")["copilot_credits"]; ok {
		t.Fatal("claude row must not carry copilot_credits")
	}

	// A changed credit sum re-sends the row; an unchanged one does not.
	res, err := u.Sync(context.Background(), []aggregate.Row{copilotRow("c", 2.01016), copilotRow("z", 1.5), row("cl")}, 0)
	if err != nil || res.Accepted != 1 || res.Unchanged != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestInvalidCopilotCreditsAreNeverSent(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())
	claude := row("claude")
	v := 1.0
	claude.CopilotCredits = &v
	rows := []aggregate.Row{
		copilotRow("neg", -0.1), copilotRow("big", 1e9+1), copilotRow("nan", math.NaN()), copilotRow("inf", math.Inf(1)),
		claude, copilotRow("max", 1e9),
	}
	res, err := u.Sync(context.Background(), rows, 0)
	if err != nil || res.Invalid != 5 || res.Accepted != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestQualityOnTheWire(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())
	clean := row("clean")
	clean.Quality = analyzer.Quality{Exchanges: 2, AnalyzedExchanges: 2, Responses: 2}
	if _, err := u.Sync(context.Background(), []aggregate.Row{row("n"), clean}, 0); err != nil {
		t.Fatal(err)
	}
	sessions := s.bodies[0]["sessions"].([]any)
	q := sessions[0].(map[string]any)["quality"].(map[string]any)
	if q["analyzed_exchanges"] != float64(3) || q["friction_exchanges"] != float64(1) || q["first_friction_exchange"] != float64(2) ||
		q["signal_coverage"] != float64(aggregate.ClaudeCodeCoverage) || q["detector_version"] != float64(1) {
		t.Fatalf("quality = %v", q)
	}
	if len(q) != 26 {
		t.Fatalf("quality has %d fields: %v", len(q), q)
	}
	// No friction: first_friction_exchange is an explicit null, not 0.
	q2 := sessions[1].(map[string]any)["quality"].(map[string]any)
	if v, ok := q2["first_friction_exchange"]; !ok || v != nil {
		t.Fatalf("first_friction_exchange = %v (present=%v), want explicit null", v, ok)
	}
}

func TestQualityInvariantsDropTheRow(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())
	var rows []aggregate.Row
	for i, mut := range []func(*analyzer.Quality){
		func(q *analyzer.Quality) { q.Exchanges = -1 },
		func(q *analyzer.Quality) { q.AnalyzedExchanges = 99 },
		func(q *analyzer.Quality) { q.FxPrimaryToolLoop = 1 },
		func(q *analyzer.Quality) { q.FirstFrictionExchange = 0 },
		func(q *analyzer.Quality) { q.ToolErrors = 99 },
	} {
		r := row(fmt.Sprintf("bad%d", i))
		mut(&r.Quality)
		rows = append(rows, r)
	}
	res, err := u.Sync(context.Background(), rows, 0)
	if err != nil || res.Invalid != len(rows) || res.Accepted != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestPartialRejection(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, _ map[string]any) {
		fmt.Fprint(w, `{"accepted":1,"rejected":2,"errors":[{"index":1,"code":"out_of_range"},{"index":2,"code":"future_timestamp"},{"index":99,"code":"x"}]}`)
	}}
	ts := s.start()
	defer ts.Close()
	l := newLedger()
	u, _ := newUploader(t, ts.URL, l)
	res, err := u.Sync(context.Background(), []aggregate.Row{row("a"), row("b"), row("c")}, 0)
	if err != nil || res.Accepted != 1 || res.Rejected != 1 || res.Deferred != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// Accepted and permanently rejected rows are remembered; the future one is retried.
	if len(l.m) != 2 {
		t.Fatalf("ledger = %v", l.m)
	}
}

func TestAllRowsRejectedWith400(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"accepted":0,"rejected":1,"errors":[{"index":0,"code":"invalid_session"}]}`)
	}}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())
	res, err := u.Sync(context.Background(), []aggregate.Row{row("a")}, 0)
	if err != nil || res.Rejected != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestErrorStatuses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		hdr    map[string]string
		code   Code
		retry  int
	}{
		{"unauthorized", 401, `{"error":"unauthorized"}`, nil, CodeUnauthorized, 0},
		{"rate limited", 429, `{"error":"rate_limited"}`, map[string]string{"Retry-After": "60"}, CodeTransient, 60},
		{"huge retry-after is capped", 429, `{}`, map[string]string{"Retry-After": "999999"}, CodeTransient, 3600},
		{"server error", 503, `oops`, nil, CodeTransient, 0},
		{"bad payload", 400, `{"error":"invalid_payload"}`, nil, CodeRejected, 0},
		{"unsupported version", 400, `{"error":"unsupported_payload_version"}`, nil, CodeRejected, 0},
		{"too large", 413, `{"error":"payload_too_large"}`, nil, CodeRejected, 0},
		{"not json", 200, `<html>`, nil, CodeMalformed, 0},
		{"400 without errors", 400, `{}`, nil, CodeRejected, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &server{t: t, handler: func(w http.ResponseWriter, _ map[string]any) {
				for k, v := range c.hdr {
					w.Header().Set(k, v)
				}
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
			}}
			ts := s.start()
			defer ts.Close()
			l := newLedger()
			u, _ := newUploader(t, ts.URL, l)
			_, err := u.Sync(context.Background(), []aggregate.Row{row("a")}, 0)
			var e *Error
			if !errors.As(err, &e) || e.Code != c.code || e.RetryAfterSeconds != c.retry {
				t.Fatalf("err = %#v, want code %s retry %d", err, c.code, c.retry)
			}
			if len(l.m) != 0 {
				t.Fatalf("ledger written on failure: %v", l.m)
			}
			if strings.Contains(err.Error(), "hxt_") || strings.Contains(err.Error(), "127.0.0.1") {
				t.Fatalf("error leaks token or URL: %v", err)
			}
		})
	}
}

func TestBatchingAndResumeAfterFailure(t *testing.T) {
	calls := 0
	s := &server{t: t}
	s.handler = func(w http.ResponseWriter, b map[string]any) {
		calls++
		if calls == 2 {
			w.WriteHeader(503)
			return
		}
		okHandler(w, b)
	}
	ts := s.start()
	defer ts.Close()
	l := newLedger()
	u, sleeps := newUploader(t, ts.URL, l)

	rows := make([]aggregate.Row, 450)
	for i := range rows {
		rows[i] = row(fmt.Sprintf("s%03d", i))
	}
	res, err := u.Sync(context.Background(), rows, 0)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeTransient || res.Accepted != 200 || len(l.m) != 200 {
		t.Fatalf("res=%+v err=%v ledger=%d", res, err, len(l.m))
	}
	// Resume: the first 200 are not sent again; the remaining 250 go out in two batches.
	res, err = u.Sync(context.Background(), rows, 0)
	if err != nil || res.Unchanged != 200 || res.Accepted != 250 || res.Batches != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(*sleeps) == 0 || (*sleeps)[0] != DefaultBatchPause {
		t.Fatalf("batch pause not applied: %v", *sleeps)
	}
	for _, b := range s.bodies {
		if n := len(b["sessions"].([]any)); n > MaxSessionsPerPost {
			t.Fatalf("batch of %d sessions", n)
		}
	}
}

func TestBodySizeLimitSplitsBatches(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, err := New(Options{
		BaseURL: ts.URL, Token: func() string { return "t" }, AgentVersion: "0.1.0", Ledger: newLedger(),
		AllowInsecureLocalhost: true, MaxBodyBytes: 1000,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := u.Sync(context.Background(), []aggregate.Row{row("a"), row("b"), row("c"), row("d")}, 0)
	if err != nil || res.Accepted != 4 || res.Batches < 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("redirect target was contacted")
	}))
	defer other.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())
	_, err := u.Sync(context.Background(), []aggregate.Row{row("a")}, 0)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeRejected {
		t.Fatalf("err = %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	tok := func() string { return "t" }
	ok := Options{Token: tok, AgentVersion: "0.1.0", Ledger: newLedger()}
	for name, c := range map[string]struct {
		url      string
		insecure bool
		wantErr  bool
	}{
		"https":                  {"https://app.example.com", false, false},
		"https prefix":           {"https://app.example.com/harnext/", false, false},
		"http remote":            {"http://app.example.com", true, true},
		"http localhost no flag": {"http://localhost:3000", false, true},
		"http localhost flag":    {"http://localhost:3000", true, false},
		"http 127.0.0.1 flag":    {"http://127.0.0.1:3000", true, false},
		"user info":              {"https://u:p@app.example.com", false, true},
		"query":                  {"https://app.example.com?x=1", false, true},
		"ftp":                    {"ftp://app.example.com", false, true},
		"empty":                  {"", false, true},
	} {
		o := ok
		o.BaseURL, o.AllowInsecureLocalhost = c.url, c.insecure
		_, err := New(o)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", name, err, c.wantErr)
		}
	}
	if _, err := New(Options{BaseURL: "https://a.b", AgentVersion: "0.1.0", Ledger: newLedger()}); err == nil {
		t.Error("missing Token accepted")
	}
	if _, err := New(Options{BaseURL: "https://a.b", Token: tok, AgentVersion: "0.1.0"}); err == nil {
		t.Error("missing Ledger accepted")
	}
	if _, err := New(Options{BaseURL: "https://a.b", Token: tok, Ledger: newLedger()}); err == nil {
		t.Error("missing AgentVersion accepted")
	}
}

func TestNoTokenSendsNothing(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, err := New(Options{BaseURL: ts.URL, Token: func() string { return "" }, AgentVersion: "0.1.0",
		Ledger: newLedger(), AllowInsecureLocalhost: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = u.Sync(context.Background(), []aggregate.Row{row("a")}, 0)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeNoToken || len(s.bodies) != 0 {
		t.Fatalf("err=%v bodies=%d", err, len(s.bodies))
	}
}

func TestNextWaitBackoff(t *testing.T) {
	u, _ := New(Options{BaseURL: "https://a.b", Token: func() string { return "t" }, AgentVersion: "1",
		Ledger: newLedger(), Rand: func() float64 { return 0.5 }})
	period := 5 * time.Minute
	f := 0
	transient := &Error{Code: CodeTransient}
	var got []time.Duration
	for i := 0; i < 5; i++ {
		got = append(got, u.nextWait(period, transient, &f))
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, period, period}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waits = %v, want %v", got, want)
		}
	}
	if w := u.nextWait(period, &Error{Code: CodeTransient, RetryAfterSeconds: 600}, &f); w != 10*time.Minute {
		t.Fatalf("Retry-After ignored: %v", w)
	}
	if w := u.nextWait(period, &Error{Code: CodeUnauthorized}, &f); w != period {
		t.Fatalf("unauthorized wait = %v", w)
	}
	if w := u.nextWait(period, nil, &f); w != period+15*time.Second || f != 0 {
		t.Fatalf("success wait = %v failures=%d", w, f)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	u, err := New(Options{BaseURL: ts.URL, Token: func() string { return "t" }, AgentVersion: "0.1.0",
		Ledger: newLedger(), AllowInsecureLocalhost: true,
		Sleep: func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() },
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	reports := 0
	go func() {
		u.Run(ctx, func() ([]aggregate.Row, int64) { return []aggregate.Row{row("a")}, 0 }, time.Minute,
			func(Result, error) { reports++ })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if reports != 1 {
		t.Fatalf("reports = %d", reports)
	}
}

func TestConfigVersionInEnvelopeAndDigest(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())
	rows := []aggregate.Row{row("a")}

	// No configuration: the field is absent, not 0 and not null.
	if _, err := u.Sync(context.Background(), rows, 0); err != nil {
		t.Fatal(err)
	}
	if _, present := s.bodies[0]["config_version"]; present {
		t.Fatalf("config_version sent without configuration: %v", s.bodies[0])
	}

	// A new version re-sends unchanged rows so the server learns which configuration scored them.
	res, err := u.Sync(context.Background(), rows, 7)
	if err != nil || res.Accepted != 1 || len(s.bodies) != 2 {
		t.Fatalf("res=%+v err=%v bodies=%d", res, err, len(s.bodies))
	}
	if s.bodies[1]["config_version"] != float64(7) {
		t.Fatalf("config_version = %v, want 7", s.bodies[1]["config_version"])
	}
	// Same version and content: nothing to send.
	if res, _ = u.Sync(context.Background(), rows, 7); res.Unchanged != 1 || len(s.bodies) != 2 {
		t.Fatalf("res=%+v bodies=%d", res, len(s.bodies))
	}
	// Negative values are treated as none.
	if _, err := u.Sync(context.Background(), []aggregate.Row{row("b")}, -3); err != nil {
		t.Fatal(err)
	}
	if _, present := s.bodies[2]["config_version"]; present {
		t.Fatalf("negative version sent: %v", s.bodies[2])
	}
}

// Rows of one session travel as one entry: usage rows in order, the turn count taken from the folders. Any invalid
// usage row (bad day, speed, floor, duplicate key) or too many rows drops the whole session.
func TestSessionGroupsUsageRows(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())

	a1 := row("a")
	a2 := row("a")
	a2.Day, a2.Speed, a2.PromptFloor, a2.WebSearchRequests = "2026-11-02", "fast", 200_000, 4
	other := row("b")
	badDay, badSpeed, badFloor, dup := row("d"), row("s"), row("f"), row("x")
	badDay.Day = "2026-11-31"
	badSpeed.Speed = "turbo"
	badFloor.PromptFloor = -2
	many := make([]aggregate.Row, 0, 401)
	for i := range 401 {
		r := row("many")
		r.Model = fmt.Sprintf("m-%d", i)
		many = append(many, r)
	}
	rows := append([]aggregate.Row{a1, other, a2, badDay, badSpeed, badFloor, dup, dup}, many...)
	res, err := u.Sync(context.Background(), rows, 0)
	if err != nil || res.Accepted != 2 || res.Invalid != 5 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	sessions := s.bodies[0]["sessions"].([]any)
	first := sessions[0].(map[string]any)
	usage := first["usage"].([]any)
	if first["session_external_id"] != "a" || first["turn_count"] != float64(3) || len(usage) != 2 {
		t.Fatalf("session a = %v", first)
	}
	second := usage[1].(map[string]any)
	if second["day"] != "2026-11-02" || second["speed"] != "fast" || second["prompt_floor"] != float64(200_000) || second["web_search_requests"] != float64(4) {
		t.Fatalf("usage row = %v", second)
	}
}

func TestToUsageTokensEstimated(t *testing.T) {
	credits := 2.0
	row := aggregate.Row{Tool: aggregate.ToolVSCodeCopilot, Model: "gpt-5.1", Day: "2026-11-01", Speed: "unknown", PromptFloor: -1, CopilotCredits: &credits, TokensEstimated: true}
	u, ok := toUsage(row)
	if !ok || !u.TokensEstimated {
		t.Fatalf("copilot history row: %+v ok=%v", u, ok)
	}
	b, _ := json.Marshal(u)
	if !strings.Contains(string(b), `"tokens_estimated":true`) {
		t.Errorf("flag missing from the wire row: %s", b)
	}
	row.TokensEstimated = false
	u, _ = toUsage(row)
	if b, _ := json.Marshal(u); strings.Contains(string(b), "tokens_estimated") {
		t.Errorf("measured rows must not carry the flag: %s", b)
	}
	// Only Copilot rows with credits may be estimated; the server anchors the estimate to them.
	row.TokensEstimated = true
	row.CopilotCredits = nil
	if _, ok := toUsage(row); ok {
		t.Error("an estimated row without credits must be rejected")
	}
	row.CopilotCredits, row.Tool = &credits, aggregate.ToolClaudeCode
	if _, ok := toUsage(row); ok {
		t.Error("only Copilot rows may be estimated")
	}
}

// The backlog count travels as a bare header on every ingest request (the panel's "history is syncing" ring); no body field.
func TestBacklogHeaderIsSentOnEveryRequest(t *testing.T) {
	var mu sync.Mutex
	var got []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("X-Harnext-Backlog"))
		mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		okHandler(w, body)
	}))
	defer ts.Close()
	remaining := 41
	u, err := New(Options{
		BaseURL: ts.URL, Token: func() string { return "hxt_secret-token" }, AgentVersion: "0.1.0",
		Ledger: newLedger(), AllowInsecureLocalhost: true, Backlog: func() int { return remaining },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Sync(context.Background(), []aggregate.Row{row("a")}, 0); err != nil {
		t.Fatal(err)
	}
	remaining = -3 // never negative on the wire
	if _, err := u.Sync(context.Background(), []aggregate.Row{row("b")}, 0); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "41" || got[1] != "0" {
		t.Fatalf("headers = %v", got)
	}
}

func TestNoBacklogHeaderWithoutAProvider(t *testing.T) {
	var header, total string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("X-Harnext-Backlog")
		total = r.Header.Get("X-Harnext-Backlog-Total")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		okHandler(w, body)
	}))
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())
	if _, err := u.Sync(context.Background(), []aggregate.Row{row("a")}, 0); err != nil || header != "" || total != "" {
		t.Fatalf("headers %q %q err %v", header, total, err)
	}
}

// The total (sessions found in the last 30 days) travels next to the remaining count so the panel can show a percentage.
func TestBacklogTotalHeader(t *testing.T) {
	var got []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-Harnext-Backlog-Total"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		okHandler(w, body)
	}))
	defer ts.Close()
	total := 305
	u, err := New(Options{
		BaseURL: ts.URL, Token: func() string { return "hxt_secret-token" }, AgentVersion: "0.1.0",
		Ledger: newLedger(), AllowInsecureLocalhost: true, BacklogTotal: func() int { return total },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Sync(context.Background(), []aggregate.Row{row("a")}, 0); err != nil {
		t.Fatal(err)
	}
	total = -1 // never negative on the wire
	if _, err := u.Sync(context.Background(), []aggregate.Row{row("b")}, 0); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "305" || got[1] != "0" {
		t.Fatalf("headers = %v", got)
	}
}

// After a token change usage of earlier days is dropped before anything is sent; a session left without usage is not sent and
// the others keep the days from the switch day on (the switch day itself is included).
func TestNotBeforeDayDropsEarlierUsage(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, err := New(Options{
		BaseURL: ts.URL, Token: func() string { return "hxt_secret-token" }, AgentVersion: "0.1.0",
		Ledger: newLedger(), AllowInsecureLocalhost: true, NotBeforeDay: "2026-11-05",
	})
	if err != nil {
		t.Fatal(err)
	}
	day := func(r aggregate.Row, d string) aggregate.Row { r.Day = d; return r }
	rows := []aggregate.Row{
		day(row("old"), "2026-11-01"),
		day(row("spans"), "2026-11-04"),
		day(row("spans"), "2026-11-05"),
		day(row("new"), "2026-11-06"),
	}
	res, err := u.Sync(context.Background(), rows, 0)
	if err != nil || res.Accepted != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	sent := map[string]int{}
	for _, sess := range s.bodies[0]["sessions"].([]any) {
		m := sess.(map[string]any)
		sent[m["session_external_id"].(string)] = len(m["usage"].([]any))
	}
	if len(sent) != 2 || sent["spans"] != 1 || sent["new"] != 1 {
		t.Fatalf("sent = %v", sent)
	}
	if got := dropBeforeDay(rows, ""); len(got) != len(rows) {
		t.Fatalf("empty day must keep all rows, kept %d", len(got))
	}
}

// A session in several folders: the session-level turn count, quality and usage are the sums over the folders, the main
// folder is the session's project_path, and every folder (also one without usage) travels with its own counters.
func TestSessionSplitsIntoFoldersThatAddUp(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())

	fq := func(a, f, first int64) analyzer.Quality {
		return analyzer.Quality{Exchanges: a, AnalyzedExchanges: a, FrictionExchanges: f, FxPushback: f, FxPrimaryPushback: f, Responses: a, FirstFrictionExchange: first}
	}
	mk := func(folder, model string, out int64) aggregate.Row {
		r := row("multi")
		r.Folder, r.Model, r.Totals = folder, model, aggregate.Totals{OutputTokens: out}
		r.Quality = fq(10, 1, 3)
		r.MainFolder = "apps/web"
		r.Parts = []aggregate.FolderPart{
			{Path: "", Turns: 1, Quality: fq(1, 0, 0)},
			{Path: "apps/web", Turns: 6, Quality: fq(6, 0, 0)},
			{Path: "docs", Turns: 3, Quality: fq(3, 1, 2)},
			{Path: "empty", Turns: 0, Quality: analyzer.Quality{}},
		}
		return r
	}
	rows := []aggregate.Row{mk("", "m", 5), mk("apps/web", "m", 60), mk("apps/web", "n", 7), mk("docs", "m", 30)}
	res, err := u.Sync(context.Background(), rows, 0)
	if err != nil || res.Accepted != 1 || res.Invalid != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	sess := s.bodies[0]["sessions"].([]any)[0].(map[string]any)
	if sess["turn_count"] != float64(10) || sess["project_path"] != "apps/web" {
		t.Fatalf("session = turn_count %v project_path %v", sess["turn_count"], sess["project_path"])
	}
	usage := sess["usage"].([]any)
	if len(usage) != 2 {
		t.Fatalf("session usage = %v", usage)
	}
	var sumM, sumN float64
	for _, x := range usage {
		row := x.(map[string]any)
		switch row["model"] {
		case "m":
			sumM = row["output_tokens"].(float64)
		case "n":
			sumN = row["output_tokens"].(float64)
		}
	}
	if sumM != 95 || sumN != 7 {
		t.Fatalf("session usage totals m=%v n=%v", sumM, sumN)
	}
	folders := sess["folders"].([]any)
	var paths []string
	var turns float64
	for _, x := range folders {
		f := x.(map[string]any)
		if f["path"] == nil {
			paths = append(paths, "<root>")
		} else {
			paths = append(paths, f["path"].(string))
		}
		turns += f["turn_count"].(float64)
		if _, ok := f["usage"].([]any); !ok {
			t.Fatalf("folder %v: usage must be an array even when empty", f["path"])
		}
		if _, ok := f["quality"].(map[string]any)["signal_coverage"]; !ok {
			t.Fatalf("folder %v quality has no signal_coverage", f["path"])
		}
	}
	if strings.Join(paths, ",") != "<root>,apps/web,docs,empty" || turns != 10 {
		t.Fatalf("folders = %v turns=%v", paths, turns)
	}
	// Folders are relative to the project: no absolute path anywhere inside them.
	if strings.Contains(fmt.Sprint(folders), "/work") || strings.Contains(fmt.Sprint(sess["project_path"]), "/") && strings.HasPrefix(fmt.Sprint(sess["project_path"]), "/") {
		t.Fatalf("an absolute path leaked into the folders: %v", folders)
	}
}

// The folder list must match the session: a main folder that is not in the list, a duplicate or an absolute folder drops the session.
func TestSessionWithBrokenFoldersIsInvalid(t *testing.T) {
	bad := map[string]func(*aggregate.Row){
		"main folder not listed": func(r *aggregate.Row) { r.MainFolder = "nope" },
		"duplicate folder":       func(r *aggregate.Row) { r.Parts = append(r.Parts, r.Parts[0]) },
		"absolute folder": func(r *aggregate.Row) {
			r.Parts[0].Path, r.MainFolder, r.Folder = "/home/me/app", "/home/me/app", "/home/me/app"
		},
		"dot segment": func(r *aggregate.Row) { r.Parts[0].Path, r.MainFolder, r.Folder = "a/../b", "a/../b", "a/../b" },
		"backslash":   func(r *aggregate.Row) { r.Parts[0].Path, r.MainFolder, r.Folder = `a\b`, `a\b`, `a\b` },
		"usage row of a folder that is not listed": func(r *aggregate.Row) { r.Folder = "ghost" },
		"no folders":     func(r *aggregate.Row) { r.Parts = nil },
		"negative turns": func(r *aggregate.Row) { r.Parts[0].Turns = -1 },
		"too many folders": func(r *aggregate.Row) {
			for i := 0; i < maxFoldersPerSess; i++ {
				r.Parts = append(r.Parts, aggregate.FolderPart{Path: fmt.Sprintf("f%d", i)})
			}
		},
	}
	for name, mutate := range bad {
		r := row("x")
		mutate(&r)
		if _, ok := toSession([]aggregate.Row{r}); ok {
			t.Errorf("%s: session accepted", name)
		}
	}
	if _, ok := toSession([]aggregate.Row{row("x")}); !ok {
		t.Fatal("the unmodified row must be valid")
	}
}

// The wire format is the contract: contracts/testdata/ingest.v5.vector.json is produced by this code (UPDATE_CONTRACT=1 to
// rewrite it) and the server's contract test parses the same file with the server's schema, so a drift on either
// side fails a test on its own side.
func TestIngestV5VectorMatchesWire(t *testing.T) {
	fq := func(a, f, first int64) analyzer.Quality {
		return analyzer.Quality{Exchanges: a, AnalyzedExchanges: a, FrictionExchanges: f, FxPushback: f, FxPrimaryPushback: f, ToolCalls: a, Responses: a, FirstFrictionExchange: first}
	}
	web := fq(6, 0, 0)
	docs := fq(4, 1, 2)
	sessionQ := fq(10, 1, 8)
	parts := []aggregate.FolderPart{{Path: "apps/web", Turns: 6, Quality: web}, {Path: "docs", Turns: 4, Quality: docs}}
	claude := func(folder string, out int64) aggregate.Row {
		r := row("claude-multi")
		r.Folder, r.Totals, r.Quality, r.Parts, r.MainFolder = folder, aggregate.Totals{InputTokens: 1, OutputTokens: out}, sessionQ, parts, "apps/web"
		return r
	}
	credits := func(c float64) *float64 { return &c }
	cop := func(folder string, c float64, estimated bool) aggregate.Row {
		r := row("copilot-history")
		r.Tool, r.Model, r.Speed, r.PromptFloor, r.Folder = aggregate.ToolVSCodeCopilot, "gpt-5.1", "unknown", -1, folder
		r.CopilotCredits, r.TokensEstimated = credits(c), estimated
		r.Totals = aggregate.Totals{InputTokens: 100, OutputTokens: 10}
		r.SignalCoverage = aggregate.CopilotCoverage
		r.Quality = fq(2, 0, 0)
		r.Parts = []aggregate.FolderPart{{Path: "", Turns: 1, Quality: fq(1, 0, 0)}, {Path: "services/api", Turns: 1, Quality: fq(1, 0, 0)}}
		r.MainFolder = ""
		return r
	}
	rows := []aggregate.Row{claude("apps/web", 600), claude("docs", 400), cop("", 1.25, true), cop("services/api", 0.5, true)}
	old := resolveProject
	resolveProject = func(string) project.Identity { return project.Identity{Project: "acme/app"} }
	defer func() { resolveProject = old }()
	prepared, invalid := prepare(rows, 7)
	if invalid != 0 || len(prepared) != 2 {
		t.Fatalf("prepared=%d invalid=%d", len(prepared), invalid)
	}
	sessions := make([]json.RawMessage, 0, len(prepared))
	for _, p := range prepared {
		sessions = append(sessions, p.body)
	}
	got, err := json.MarshalIndent(map[string]any{"payload_version": PayloadVersion, "agent_version": "0.3.0", "config_version": 7, "sessions": sessions}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := "../../contracts/testdata/ingest.v5.vector.json"
	if os.Getenv("UPDATE_CONTRACT") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Fatalf("%s is out of date; run UPDATE_CONTRACT=1 go test ./internal/uploader -run TestIngestV5Vector\n%s", path, got)
	}
}
