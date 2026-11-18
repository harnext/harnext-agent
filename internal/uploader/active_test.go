package uploader

import (
	"strconv"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
)

func TestActiveSecondsOnTheWire(t *testing.T) {
	// row() spans one hour, so the server limit is 3600 + 3600.
	tests := []struct {
		name   string
		active int64
		want   int64 // 0 = the field is absent
	}{
		{"no timed event: absent, never a fake 0", 0, 0},
		{"negative is dropped", -5, 0},
		{"normal", 1800, 1800},
		{"at the span plus one hour", 7200, 7200},
		{"above the span plus one hour is clipped", 9000, 7200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := row("a")
			r.ActiveSeconds = tc.active
			s, ok := toSession([]aggregate.Row{r})
			if !ok || s.ActiveSeconds != tc.want {
				t.Fatalf("ok=%v ActiveSeconds=%d, want %d", ok, s.ActiveSeconds, tc.want)
			}
			p, invalid := prepare([]aggregate.Row{r}, 0)
			if invalid != 0 || len(p) != 1 {
				t.Fatalf("prepared=%d invalid=%d", len(p), invalid)
			}
			if has := contains(p[0].body, `"active_seconds"`); has != (tc.want > 0) {
				t.Errorf("field present = %v for %s", has, p[0].body)
			}
			if tc.want > 0 && !contains(p[0].body, `"active_seconds":`+strconv.FormatInt(tc.want, 10)) {
				t.Errorf("body lacks the value: %s", p[0].body)
			}
		})
	}
}

func TestActiveSecondsSpanUsesWholeSeconds(t *testing.T) {
	r := row("a")
	r.EndedAt = r.StartedAt.Add(1500 * time.Millisecond) // floor(1.5) + 3600
	r.ActiveSeconds = 3602
	if s, ok := toSession([]aggregate.Row{r}); !ok || s.ActiveSeconds != 3601 {
		t.Fatalf("ok=%v ActiveSeconds=%d, want 3601", ok, s.ActiveSeconds)
	}
}

func TestActiveSecondsWeekLimit(t *testing.T) {
	r := row("a")
	r.EndedAt = r.StartedAt.Add(80 * 24 * time.Hour)
	r.ActiveSeconds = 10_000_000
	if s, ok := toSession([]aggregate.Row{r}); !ok || s.ActiveSeconds != 604800 {
		t.Fatalf("ok=%v ActiveSeconds=%d, want 604800", ok, s.ActiveSeconds)
	}
}

// Sessions are re-sent once after the agent starts producing the value: the digest covers the body.
func TestActiveSecondsChangesTheDigest(t *testing.T) {
	digest := func(active int64) string {
		r := row("a")
		r.ActiveSeconds = active
		p, _ := prepare([]aggregate.Row{r}, 3)
		return p[0].digest
	}
	if digest(0) == digest(600) || digest(600) == digest(601) {
		t.Error("the digest does not cover active_seconds")
	}
	if digest(600) != digest(600) {
		t.Error("the digest is not stable")
	}
}
