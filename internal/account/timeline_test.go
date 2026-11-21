package account

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var (
	hA = Hash("a@example.test")
	hB = Hash("b@example.test")
	t0 = time.Unix(1_000_000, 0)
)

func at(sec int64) time.Time { return time.Unix(1_000_000+sec, 0) }

func TestDirID(t *testing.T) {
	a := DirID("/home/u/.claude")
	if len(a) != 16 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("unexpected id %q", a)
	}
	if a != DirID("/home/u/.claude/") || a != DirID("/home/u/x/../.claude") {
		t.Error("equivalent paths must give the same id")
	}
	if a == DirID("/home/u/.claude-b") {
		t.Error("different paths must differ")
	}
	if strings.Contains(a, "claude") {
		t.Error("id contains the path")
	}
}

func TestTimelineHashAt(t *testing.T) {
	cases := []struct {
		name    string
		observe []struct {
			hash string
			sec  int64
		}
		query int64
		want  string
		ok    bool
	}{
		{"nothing observed", nil, 0, "", false},
		{"single hash, later", []struct {
			hash string
			sec  int64
		}{{hA, 100}}, 500, hA, true},
		{"single hash, before first", []struct {
			hash string
			sec  int64
		}{{hA, 100}}, 10, hA, true},
		{"same hash repeated, before first", []struct {
			hash string
			sec  int64
		}{{hA, 100}, {hA, 200}}, 10, hA, true},
		{"two hashes, before first is unknown", []struct {
			hash string
			sec  int64
		}{{hA, 100}, {hB, 200}}, 10, "", false},
		{"A B A, before first is unknown", []struct {
			hash string
			sec  int64
		}{{hA, 100}, {hB, 200}, {hA, 300}}, 10, "", false},
		{"between entries", []struct {
			hash string
			sec  int64
		}{{hA, 100}, {hB, 200}}, 150, hA, true},
		{"exactly at a change", []struct {
			hash string
			sec  int64
		}{{hA, 100}, {hB, 200}}, 200, hB, true},
		{"after last", []struct {
			hash string
			sec  int64
		}{{hA, 100}, {hB, 200}}, 900, hB, true},
		{"out of order observation is clamped", []struct {
			hash string
			sec  int64
		}{{hA, 100}, {hB, 50}}, 150, hB, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tl Timeline
			for _, o := range c.observe {
				tl.Observe("dir", o.hash, at(o.sec))
			}
			got, ok := tl.HashAt("dir", at(c.query))
			if got != c.want || ok != c.ok {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestTimelineObserveAppendsOnlyOnChange(t *testing.T) {
	var tl Timeline
	tl.Observe("d", hA, at(1))
	tl.Observe("d", hA, at(2))
	tl.Observe("d", hB, at(3))
	tl.Observe("d", hB, at(4))
	tl.Observe("", hA, at(5))
	tl.Observe("d", "", at(6))
	if n := len(tl.dirs["d"]); n != 2 {
		t.Errorf("entries = %d, want 2", n)
	}
	if _, ok := tl.HashAt("other", at(10)); ok {
		t.Error("unknown dir must not resolve")
	}
}

func TestTimelineSnapshotRestore(t *testing.T) {
	var tl Timeline
	tl.Observe("d1", hA, at(100))
	tl.Observe("d1", hB, at(200))
	tl.Observe("d2", hA, at(300))
	snap := tl.Snapshot()

	var m map[string]any
	if err := json.Unmarshal(snap, &m); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snap), "@") {
		t.Error("snapshot contains an e-mail")
	}

	var back Timeline
	if err := back.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if got, ok := back.HashAt("d1", at(150)); !ok || got != hA {
		t.Errorf("d1@150 = (%q, %v)", got, ok)
	}
	if got, ok := back.HashAt("d1", at(250)); !ok || got != hB {
		t.Errorf("d1@250 = (%q, %v)", got, ok)
	}
	if got, ok := back.HashAt("d2", at(0)); !ok || got != hA {
		t.Errorf("d2 single hash = (%q, %v)", got, ok)
	}
	if string(back.Snapshot()) != string(snap) {
		t.Error("snapshot is not stable across restore")
	}
}

func TestTimelineRestoreDropsBadInput(t *testing.T) {
	var tl Timeline
	tl.Observe("old", hA, at(1))
	if err := tl.Restore([]byte("not json")); err == nil {
		t.Error("want an error for broken input")
	}
	if _, ok := tl.HashAt("old", at(2)); ok {
		t.Error("state must be empty after a failed restore")
	}
	bad := `{"v":1,"dirs":{"d":[{"h":"a@example.test","s":1},{"h":"` + hB + `","s":9},{"h":"` + hA + `","s":5}],"e":[{"h":"zz","s":1}]}}`
	if err := tl.Restore([]byte(bad)); err != nil {
		t.Fatal(err)
	}
	if got, ok := tl.HashAt("d", time.Unix(0, 0)); ok {
		t.Errorf("two hashes before the first entry must be unknown: got %q", got)
	}
	if got, ok := tl.HashAt("d", time.Unix(6, 0)); !ok || got != hA {
		t.Errorf("d@6 = (%q, %v)", got, ok)
	}
	if _, ok := tl.HashAt("e", t0); ok {
		t.Error("dir with only invalid entries must be dropped")
	}
	var empty Timeline
	if string(empty.Snapshot()) != `{"v":1,"dirs":{}}` {
		t.Errorf("empty snapshot = %s", empty.Snapshot())
	}
}
