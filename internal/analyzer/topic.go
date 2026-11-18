package analyzer

import (
	"encoding/json"
	"math"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

const (
	// MaxSignalSlots bounds the distinct extension / tool keys remembered per session.
	MaxSignalSlots = 48
	// signalCountCap bounds how often one key can contribute to a topic score, so tool
	// volume (e.g. 200 reads) does not drown out everything else.
	signalCountCap = 10
)

// SignalKey is FNV-1a 64 of a short validated identifier (extension suffix or tool name).
// It keeps plain strings out of session state; it is not a security measure.
func SignalKey(s string) uint64 {
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint64(s[i])) * prime
	}
	return h
}

// SignalTable is a small fixed-size counter table keyed by SignalKey. When full, new keys
// are only counted in Dropped.
type SignalTable struct {
	Keys    [MaxSignalSlots]uint64
	Counts  [MaxSignalSlots]uint32
	Used    uint8
	Dropped uint32
}

// Add counts one occurrence of key.
func (t *SignalTable) Add(key uint64) {
	for i := 0; i < int(t.Used); i++ {
		if t.Keys[i] == key {
			t.Counts[i] = satAdd(t.Counts[i], 1)
			return
		}
	}
	if int(t.Used) < MaxSignalSlots {
		t.Keys[t.Used], t.Counts[t.Used] = key, 1
		t.Used++
		return
	}
	t.Dropped = satAdd(t.Dropped, 1)
}

// Count returns the number of occurrences of key.
func (t *SignalTable) Count(key uint64) uint32 {
	for i := 0; i < int(t.Used); i++ {
		if t.Keys[i] == key {
			return t.Counts[i]
		}
	}
	return 0
}

type signalTableJSON struct {
	Entries [][2]uint64 `json:"e,omitempty"`
	Dropped uint32      `json:"d,omitempty"`
}

// MarshalJSON writes only the used slots.
func (t SignalTable) MarshalJSON() ([]byte, error) {
	out := signalTableJSON{Dropped: t.Dropped}
	for i := 0; i < int(t.Used); i++ {
		out.Entries = append(out.Entries, [2]uint64{t.Keys[i], uint64(t.Counts[i])})
	}
	return json.Marshal(out)
}

// UnmarshalJSON tolerates oversized input by truncating to the capacity.
func (t *SignalTable) UnmarshalJSON(b []byte) error {
	var in signalTableJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	*t = SignalTable{Dropped: in.Dropped}
	for _, e := range in.Entries {
		if int(t.Used) >= MaxSignalSlots {
			t.Dropped = satAdd(t.Dropped, 1)
			continue
		}
		t.Keys[t.Used], t.Counts[t.Used] = e[0], uint32(min(e[1], math.MaxUint32))
		t.Used++
	}
	return nil
}

// TopicSignals are the per-session counters behind ClassifyTopic. They hold hashed keys and
// numbers only: never a path, command, tool input or message text.
type TopicSignals struct {
	Ext  SignalTable `json:"ext"`  // file-name suffixes of tool_use file_path inputs
	Tool SignalTable `json:"tool"` // tool_use names
	// Keyword holds per-label keyword points in thousandths, indexed by LabelIndex.
	Keyword [NumLabels]uint32 `json:"keyword"`
}

// AddKeywords folds the keyword scores of one user message.
func (s *TopicSignals) AddKeywords(scores [NumLabels]uint32) {
	for i, v := range scores {
		s.Keyword[i] = satAdd(s.Keyword[i], v)
	}
}

// ClassifyTopic picks a label from cfg.Labels. Layers: (1) file suffix counts x weight,
// (2) tool name counts x weight, (3) keyword points collected from user messages. Counts are
// capped per key. The highest total wins; ties go to the label listed first; if the best
// total is not above 0 or below min_score the fallback is returned. The result is always
// one of cfg.Labels ("" only if cfg.Labels is empty).
func ClassifyTopic(sig TopicSignals, cfg TopicConfig) string {
	if len(cfg.Labels) == 0 {
		return ""
	}
	pos := func(label string) int {
		for i, l := range cfg.Labels {
			if l == label {
				return i
			}
		}
		return -1
	}
	// Integer thousandths make the sum independent of map iteration order.
	scores := make([]int64, len(cfg.Labels))
	apply := func(table *SignalTable, signals map[string][]remoteconfig.LabelWeight) {
		for key, ws := range signals {
			n := min(table.Count(SignalKey(key)), signalCountCap)
			if n == 0 {
				continue
			}
			for _, w := range ws {
				if p := pos(w.Label); p >= 0 {
					scores[p] += int64(n) * milli(w.Weight)
				}
			}
		}
	}
	apply(&sig.Ext, cfg.ExtensionSignals)
	apply(&sig.Tool, cfg.ToolSignals)
	for p, l := range cfg.Labels {
		if i := LabelIndex(l); i >= 0 {
			scores[p] += int64(sig.Keyword[i])
		}
	}

	best := -1
	for p, s := range scores {
		if best < 0 || s > scores[best] {
			best = p
		}
	}
	if best >= 0 && scores[best] > 0 && scores[best] >= milli(cfg.MinScore) {
		return cfg.Labels[best]
	}
	if pos(cfg.Fallback) >= 0 {
		return cfg.Fallback
	}
	return cfg.Labels[0]
}

func milli(v float64) int64 { return int64(math.Round(v * 1000)) }
