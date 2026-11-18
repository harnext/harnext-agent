package analyzer

import (
	"reflect"
	"testing"
)

// Every int64 field of Quality except the position FirstFrictionExchange must be listed in additive().
func TestQualityAdditiveCoversAllFields(t *testing.T) {
	var q Quality
	rt := reflect.TypeOf(q)
	want := 0
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.Int64 {
			t.Fatalf("Quality.%s is not int64; Add/Sub/SplitStart assume int64 counters", f.Name)
		}
		if f.Name != "FirstFrictionExchange" {
			want++
		}
	}
	if got := len(q.additive()); got != want {
		t.Fatalf("additive() lists %d fields, Quality has %d counters: update additive() and SplitStart", got, want)
	}
	seen := map[*int64]bool{}
	for _, p := range q.additive() {
		if seen[p] {
			t.Fatal("additive() lists a field twice")
		}
		seen[p] = true
	}
}

func filled(base int64) Quality {
	var q Quality
	for i, p := range q.additive() {
		*p = base + int64(i)
	}
	q.FirstFrictionExchange = 5
	return q
}

func TestQualityAddSubSplit(t *testing.T) {
	a, b := filled(10), filled(1)
	d := a.Sub(b)
	for i, p := range d.additive() {
		if *p != 9 {
			t.Fatalf("Sub field %d = %d, want 9", i, *p)
		}
	}
	if d.FirstFrictionExchange != 0 {
		t.Fatal("Sub must not carry FirstFrictionExchange")
	}
	sum := b
	sum.Add(d)
	if sum.Sub(a) != (Quality{}) {
		t.Fatal("b + (a-b) != a")
	}
	closing, opening := SplitStart(d)
	back := closing
	back.Add(opening)
	if back != d {
		t.Fatalf("closing + opening != d:\n%+v\n%+v", back, d)
	}
	// The split puts the channels where StartExchange writes them.
	var probe Quality
	probe.FrictionExchanges, probe.FxPushback, probe.FxPrimaryPushback, probe.SigRepeat, probe.SigErrorPaste = 1, 1, 1, 1, 1
	probe.Exchanges, probe.AnalyzedExchanges, probe.SigErrorReport, probe.SigCorrection, probe.SigPositiveAck = 1, 1, 1, 1, 1
	c, o := SplitStart(probe)
	if c.Exchanges != 0 || c.SigErrorReport != 0 || o.FrictionExchanges != 0 || o.SigRepeat != 0 || o.SigErrorPaste != 0 {
		t.Fatalf("split misplaced counters: closing=%+v opening=%+v", c, o)
	}
	if c.FrictionExchanges != 1 || c.SigRepeat != 1 || c.SigErrorPaste != 1 || o.Exchanges != 1 || o.SigPositiveAck != 1 {
		t.Fatalf("split lost counters: closing=%+v opening=%+v", c, o)
	}
}
