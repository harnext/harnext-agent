package aggregate

import "testing"

func TestEstimateRequestTokens(t *testing.T) {
	p := DefaultEstimateParams
	cases := []struct {
		name              string
		prompt, comp      int64
		rounds            int
		read, create, out int64
	}{
		{"unknown prompt", 0, 50, 3, 0, 0, 0},
		{"single round", 1000, 10, 1, 200, 800, 10},
		{"three rounds", 1000, 10, 3, 1240, 800, 10},
		{"rounds unknown count as one", 1000, 10, 0, 200, 800, 10},
	}
	for _, c := range cases {
		got := EstimateRequestTokens(c.prompt, c.comp, c.rounds, p)
		want := TokenEstimate{CacheRead: c.read, CacheCreation: c.create, Output: c.out}
		if got != want {
			t.Errorf("%s: got %+v want %+v", c.name, got, want)
		}
	}
}

// Every round's whole prompt is plain input + cache read + cache write, so the three add up to the sum of the round prompts.
func TestEstimateAddsUpToTheRoundPrompts(t *testing.T) {
	const prompt, rounds = 90_000, 12
	est := EstimateRequestTokens(prompt, 100, rounds, DefaultEstimateParams)
	s := 0.2 * float64(prompt)
	first := s + 0.2*(float64(prompt)-s)
	var sum float64
	for r := 0; r < rounds; r++ {
		sum += first + (float64(prompt)-first)*float64(r)/float64(rounds-1)
	}
	total := float64(est.Input + est.CacheRead + est.CacheCreation)
	if d := total - sum; d > 2 || d < -2 {
		t.Errorf("total %f, sum of round prompts %f", total, sum)
	}
	if est.Input != 0 {
		t.Errorf("plain input = %d, want 0", est.Input)
	}
}

func TestEstimateBoundsTheLoop(t *testing.T) {
	a := EstimateRequestTokens(1_000_000, 10, 1_000_000_000, DefaultEstimateParams)
	b := EstimateRequestTokens(1_000_000, 10, maxEstimateRounds, DefaultEstimateParams)
	if a != b {
		t.Errorf("rounds must be capped: %+v vs %+v", a, b)
	}
}
