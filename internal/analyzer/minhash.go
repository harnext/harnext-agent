package analyzer

import (
	"math"
	"unicode/utf8"
)

// SigSize is the number of MinHash slots used to estimate the similarity of two consecutive
// user messages (repeat detection). 64 slots give a standard error of about 0.06 on Jaccard.
const SigSize = 64

// minRepeatRunes: very short messages ("continue", "devam") are often repeated on purpose.
const minRepeatRunes = 12

// maxSigRunes bounds the sketch input; a repeated request is recognizable from its start.
const maxSigRunes = 4096

// Signature is a MinHash sketch of a message's character 3-grams. It is derived from text but
// is not text: it cannot be reversed. It stays on the machine (memory and the local state
// file) and is never part of the upload payload.
type Signature struct {
	Slots [SigSize]uint64 `json:"s"`
	// OK is false when the message was too short to compare.
	OK bool `json:"ok"`
}

var sigSeeds = func() (s [SigSize]uint64) {
	x := uint64(0x9E3779B97F4A7C15)
	for i := range s {
		x = splitmix(x)
		s[i] = x
	}
	return s
}()

func splitmix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// signature builds the sketch from folded, cleaned text (runs of spaces collapsed).
func signature(folded string) Signature {
	var sig Signature
	runes := make([]rune, 0, len(folded))
	space := true
	for _, r := range folded {
		if len(runes) >= maxSigRunes {
			break
		}
		isSpace := r == ' ' || r == '\n' || r == '\t' || r == '\r'
		if isSpace {
			if space {
				continue
			}
			r = ' '
		}
		space = isSpace
		runes = append(runes, r)
	}
	if len(runes) > 0 && runes[len(runes)-1] == ' ' {
		runes = runes[:len(runes)-1]
	}
	if len(runes) < minRepeatRunes {
		return sig
	}
	for i := range sig.Slots {
		sig.Slots[i] = math.MaxUint64
	}
	var buf [12]byte
	for i := 0; i+3 <= len(runes); i++ {
		n := 0
		for _, r := range runes[i : i+3] {
			n += utf8.EncodeRune(buf[n:], r)
		}
		base := uint64(14695981039346656037) // FNV-1a, inlined to avoid an allocation per 3-gram
		for _, c := range buf[:n] {
			base ^= uint64(c)
			base *= 1099511628211
		}
		for j := range sig.Slots {
			if v := splitmix(base ^ sigSeeds[j]); v < sig.Slots[j] {
				sig.Slots[j] = v
			}
		}
	}
	sig.OK = true
	return sig
}

// merge combines the sketches of two parts of one message (MinHash of a union = slot-wise min).
func (s *Signature) merge(o Signature) {
	if !o.OK {
		return
	}
	if !s.OK {
		*s = o
		return
	}
	for i := range s.Slots {
		s.Slots[i] = min(s.Slots[i], o.Slots[i])
	}
}

// similarity estimates the Jaccard similarity of two sketches; 0 when either is unusable.
func similarity(a, b Signature) float64 {
	if !a.OK || !b.OK {
		return 0
	}
	eq := 0
	for i := range a.Slots {
		if a.Slots[i] == b.Slots[i] {
			eq++
		}
	}
	return float64(eq) / SigSize
}
