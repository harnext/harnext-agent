package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/parser/copilot"
	"github.com/harnext/harnext-agent/internal/watcher"
)

const sentinel = "SENTINEL_PROMPT_TEXT"

func fixtureLines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash("../../testdata/copilot/otel-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if l != "" {
			lines = append(lines, l+"\n")
		}
	}
	return lines
}

type wireTotals struct {
	sessions            int
	in, out, cache, tur int64
}

// lastTotals sums the newest uploaded row of every vscode_copilot (session, model); the
// uploader re-sends only changed rows.
func lastTotals(t *testing.T, rec *ingestRecorder) (wireTotals, map[string]any) {
	t.Helper()
	latest := map[string]map[string]any{}
	var first map[string]any
	for _, b := range rec.all() {
		var env struct {
			Sessions []map[string]any `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(b), &env); err != nil {
			t.Fatal(err)
		}
		for _, s := range env.Sessions {
			if s["tool_name"] != "vscode_copilot" {
				continue
			}
			if first == nil {
				first = s
			}
			latest[s["session_external_id"].(string)] = s
		}
	}
	var w wireTotals
	for _, s := range latest {
		w.sessions++
		for _, u := range s["usage"].([]any) {
			m := u.(map[string]any)
			w.in += int64(m["input_tokens"].(float64))
			w.out += int64(m["output_tokens"].(float64))
			w.cache += int64(m["cache_read_tokens"].(float64))
		}
		w.tur += int64(s["turn_count"].(float64))
	}
	return w, first
}

func TestCopilotEndToEnd(t *testing.T) {
	h := newHarness(t)
	otel := filepath.Join(t.TempDir(), "telemetry", "otel.jsonl")
	if err := os.MkdirAll(filepath.Dir(otel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.paths.SaveSettings(Settings{ServerURL: h.srv.URL, AllowInsecureLocalhost: true, CopilotOTelPath: otel}); err != nil {
		t.Fatal(err)
	}
	lines := fixtureLines(t)
	last := len(lines) - 1
	if !strings.Contains(lines[last], sentinel) {
		t.Fatal("fixture must end with the sentinel line")
	}

	stop := h.start(t)
	eventually(t, "agent running without a copilot file", func() bool {
		s, err := h.paths.ReadStatus()
		return err == nil && !s.Copilot.File
	})
	appendFile(t, otel, strings.Join(lines[:last], ""))
	eventually(t, "first copilot upload", func() bool {
		w, _ := lastTotals(t, h.rec)
		return w.sessions == 1 && w.in == 700 && w.out == 250 && w.cache == 800 && w.tur == 2
	})
	appendFile(t, otel, lines[last])
	eventually(t, "upload after append", func() bool {
		w, _ := lastTotals(t, h.rec)
		return w.sessions == 2 && w.in == 740 && w.out == 254 && w.cache == 800 && w.tur == 3
	})
	_, sess := lastTotals(t, h.rec)
	if sess["project"] != "unknown" || sess["topic"] != "other" || sess["quality"] == nil {
		t.Fatalf("session = %v", sess)
	}
	for _, b := range h.rec.all() {
		if strings.Contains(b, sentinel) || strings.Contains(b, testToken) || strings.Contains(b, otel) {
			t.Fatalf("body leaks content, token or path: %s", b)
		}
	}
	eventually(t, "copilot status", func() bool {
		s, err := h.paths.ReadStatus()
		return err == nil && s.Copilot.File && s.Copilot.Sessions == 2 && s.Copilot.UniqueSpans == 3 && s.Copilot.BackgroundSpans == 1
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(h.paths.Status); strings.Contains(string(raw), otel) {
		t.Fatal("status file contains the telemetry path")
	}

	// Restart with the same home: the same lines are not counted twice and nothing is re-sent.
	before := len(h.rec.all())
	stop = h.start(t)
	eventually(t, "restarted status", func() bool {
		s, err := h.paths.ReadStatus()
		return err == nil && s.Copilot.UniqueSpans == 3 && s.Copilot.Sessions == 2
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if w, _ := lastTotals(t, h.rec); w.in != 740 || w.out != 254 || w.tur != 3 {
		t.Fatalf("totals after restart = %+v", w)
	}
	if n := len(h.rec.all()) - before; n != 0 {
		t.Fatalf("uploads after restart = %d, want 0", n)
	}
}

func fixtureAggregator(t *testing.T) *aggregate.CopilotAggregator {
	t.Helper()
	agg := aggregate.NewCopilot()
	for _, l := range fixtureLines(t) {
		agg.Add(copilot.ParseLine([]byte(l)))
	}
	return agg
}

func TestRestoreCopilotKeepsClaudeOffsets(t *testing.T) {
	db, _ := openDB(t)
	o, _ := LoadOffsets(db)
	const cop = "/x/copilot.jsonl"
	_ = o.Set("/x/claude.jsonl", watcher.FileState{Offset: 5})
	_ = o.Set(cop, watcher.FileState{Offset: 7})
	var msgs []string
	logf := func(f string, a ...any) { msgs = append(msgs, f) }

	// No state: only the Copilot offset goes.
	agg, err := restoreCopilot(db, o, cop, logf)
	if err != nil || len(agg.Rows()) != 0 {
		t.Fatalf("err=%v rows=%d", err, len(agg.Rows()))
	}
	if _, ok := o.Get(cop); ok {
		t.Fatal("copilot offset kept without state")
	}
	if _, ok := o.Get("/x/claude.jsonl"); !ok {
		t.Fatal("claude offset dropped")
	}

	// Corrupt state: same.
	_ = o.Set(cop, watcher.FileState{Offset: 7})
	_ = db.SaveState(copilotStateName, []byte("{nope"))
	if _, err := restoreCopilot(db, o, cop, logf); err != nil || len(msgs) == 0 {
		t.Fatalf("err=%v msgs=%v", err, msgs)
	}
	if _, ok := o.Get(cop); ok {
		t.Fatal("copilot offset kept with corrupt state")
	}
	if _, ok := o.Get("/x/claude.jsonl"); !ok {
		t.Fatal("claude offset dropped")
	}

	// Valid state keeps the offset and the totals.
	blob, _ := json.Marshal(fixtureAggregator(t).Snapshot())
	_ = db.SaveState(copilotStateName, blob)
	_ = o.Set(cop, watcher.FileState{Offset: 7})
	agg, err = restoreCopilot(db, o, cop, logf)
	if err != nil || len(agg.Rows()) != 2 {
		t.Fatalf("err=%v rows=%d", err, len(agg.Rows()))
	}
	if _, ok := o.Get(cop); !ok {
		t.Fatal("copilot offset dropped despite valid state")
	}
}

// A missing Claude snapshot resets every offset, Copilot's included; the Copilot file is then
// read again and its span dedup must keep the restored totals unchanged.
func TestCopilotReplayAfterClaudeReset(t *testing.T) {
	db, _ := openDB(t)
	o, _ := LoadOffsets(db)
	const cop = "/x/copilot.jsonl"
	_ = o.Set(cop, watcher.FileState{Offset: 7})
	ref := fixtureAggregator(t)
	blob, _ := json.Marshal(ref.Snapshot())
	_ = db.SaveState(copilotStateName, blob)
	logf := func(string, ...any) {}

	if _, err := restoreAggregator(db, o, logf); err != nil {
		t.Fatal(err)
	}
	if _, ok := o.Get(cop); ok {
		t.Fatal("expected the claude reset to clear every offset")
	}
	agg, err := restoreCopilot(db, o, cop, logf)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range fixtureLines(t) {
		agg.Add(copilot.ParseLine([]byte(l)))
	}
	got, want := agg.Rows(), ref.Rows()
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("row %d changed after replay: %+v vs %+v", i, got[i], want[i])
		}
	}
}

func TestOldStatusFileWithoutCopilotIsReadable(t *testing.T) {
	p := PathsIn(t.TempDir())
	if err := os.WriteFile(p.Status, []byte(`{"pid":1,"version":"0.1.0","sessions":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := p.ReadStatus()
	if err != nil || s.Sessions != 2 || s.Copilot != (CopilotStatus{}) {
		t.Fatalf("status = %+v, err = %v", s, err)
	}
}
