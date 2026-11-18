package uploader

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
)

func rowEndingAt(id string, ended time.Time) aggregate.Row {
	r := row(id)
	r.EndedAt = ended
	r.StartedAt = ended.Add(-time.Hour)
	return r
}

func sessionIDs(body map[string]any) []string {
	var out []string
	for _, s := range body["sessions"].([]any) {
		out = append(out, s.(map[string]any)["session_external_id"].(string))
	}
	return out
}

// The most recent sessions go first, and a session's own rows keep their order.
func TestSyncSendsNewestSessionsFirst(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u, _ := newUploader(t, ts.URL, newLedger())

	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var rows []aggregate.Row
	for i := 0; i < 5; i++ { // ids ascend with time, the input order is ascending too
		rows = append(rows, rowEndingAt(fmt.Sprintf("s%d", i), base.Add(time.Duration(i)*24*time.Hour)))
	}
	if _, err := u.Sync(context.Background(), rows, 0); err != nil {
		t.Fatal(err)
	}
	want := []string{"s4", "s3", "s2", "s1", "s0"}
	if got := sessionIDs(s.bodies[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSyncReportsBatchesAndRemaining(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	var seen []Result
	var sleeps []time.Duration
	u, err := New(Options{
		BaseURL: ts.URL, Token: func() string { return "t" }, AgentVersion: "0.1.0", Ledger: newLedger(), AllowInsecureLocalhost: true,
		Sleep:   func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
		OnBatch: func(r Result) { seen = append(seen, r) },
	})
	if err != nil {
		t.Fatal(err)
	}
	var rows []aggregate.Row
	for i := 0; i < MaxSessionsPerPost+50; i++ {
		rows = append(rows, row(fmt.Sprintf("s%04d", i)))
	}
	res, err := u.Sync(context.Background(), rows, 0)
	if err != nil || res.Accepted != MaxSessionsPerPost+50 || res.Remaining != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(seen) != 2 || seen[0].Accepted != MaxSessionsPerPost || seen[0].Remaining != 50 || seen[1].Remaining != 0 {
		t.Fatalf("batches seen = %+v", seen)
	}
	if len(sleeps) != 1 || sleeps[0] != DefaultBatchPause {
		t.Fatalf("pauses = %v, want one of %s", sleeps, DefaultBatchPause)
	}
}

// While history is still being read, a finished pass is followed by the batch pause, not the
// upload period; afterwards the normal period applies.
func TestRunCatchupUsesBatchPause(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	catchup := []bool{true, true, false}
	u, err := New(Options{
		BaseURL: ts.URL, Token: func() string { return "t" }, AgentVersion: "0.1.0", Ledger: newLedger(), AllowInsecureLocalhost: true,
		BatchPause: 3 * time.Second,
		Rand:       func() float64 { return 0 },
		Catchup: func() bool {
			c := catchup[0]
			catchup = catchup[1:]
			return c
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			if len(waits) == 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	u.Run(ctx, func() ([]aggregate.Row, int64) { return []aggregate.Row{row("a")}, 0 }, time.Minute, nil)
	want := []time.Duration{3 * time.Second, 3 * time.Second, time.Minute}
	if !reflect.DeepEqual(waits, want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}
