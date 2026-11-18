package aggregate

import (
	"cmp"
	"fmt"
	"math/bits"
	"slices"
	"time"
)

const (
	// maxActiveMinutes bounds the distinct minutes kept per session; later new minutes are skipped and counted.
	maxActiveMinutes = 20_000
	// activeGapMinutes is the longest gap between two points that still counts as continuous work.
	activeGapMinutes = 5
)

// MinuteWord holds the minutes W*64 .. W*64+63 (since the Unix epoch) as bits of B.
type MinuteWord struct {
	W int64  `json:"w"`
	B uint64 `json:"b"`
}

// MinuteSet is the set of UTC minutes in which a session had an event, as sorted sparse bit words. Adding is idempotent and
// order independent, so replays and late events are safe. It holds minute numbers only, no identifiers or text.
type MinuteSet struct {
	Words []MinuteWord `json:"w,omitempty"`
	n     int          // number of set bits; rebuilt by check after a JSON round trip
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func (m *MinuteSet) count() int {
	if m.n == 0 && len(m.Words) > 0 {
		for _, w := range m.Words {
			m.n += bits.OnesCount64(w.B)
		}
	}
	return m.n
}

// Add records the minute of t; a zero time adds nothing. It returns false only when the minute is new and the cap is reached.
func (m *MinuteSet) Add(t time.Time) bool {
	if t.IsZero() {
		return true
	}
	minute := floorDiv(t.Unix(), 60)
	w := floorDiv(minute, 64)
	bit := uint64(1) << uint(minute-w*64)
	i, found := slices.BinarySearchFunc(m.Words, w, func(e MinuteWord, w int64) int { return cmp.Compare(e.W, w) })
	if found {
		if m.Words[i].B&bit != 0 {
			return true
		}
		if m.count() >= maxActiveMinutes {
			return false
		}
		m.Words[i].B |= bit
		m.n++
		return true
	}
	if m.count() >= maxActiveMinutes {
		return false
	}
	m.Words = slices.Insert(m.Words, i, MinuteWord{W: w, B: bit})
	m.n++
	return true
}

// Seconds is the active time: the first minute counts 60 s; a later minute d minutes after the previous one adds d*60 s when
// d <= 5 and only its own 60 s otherwise. A nil or empty set is 0.
func (m *MinuteSet) Seconds() int64 {
	if m == nil {
		return 0
	}
	var total, prev int64
	first := true
	for _, w := range m.Words {
		for b := w.B; b != 0; b &= b - 1 {
			minute := w.W*64 + int64(bits.TrailingZeros64(b))
			switch {
			case first:
				total += 60
			case minute-prev <= activeGapMinutes:
				total += (minute - prev) * 60
			default:
				total += 60
			}
			prev, first = minute, false
		}
	}
	return total
}

func (m *MinuteSet) clone() *MinuteSet {
	if m == nil {
		return nil
	}
	return &MinuteSet{Words: slices.Clone(m.Words), n: m.n}
}

// check validates a restored set (words strictly increasing, none empty, within the cap) and rebuilds the bit count.
func (m *MinuteSet) check() error {
	n := 0
	for i, w := range m.Words {
		if w.B == 0 || (i > 0 && m.Words[i-1].W >= w.W) {
			return fmt.Errorf("aggregate: state: invalid active minutes")
		}
		n += bits.OnesCount64(w.B)
	}
	if n > maxActiveMinutes {
		return fmt.Errorf("aggregate: state: too many active minutes")
	}
	m.n = n
	return nil
}
