package aggregate

import "math"

// Token estimate for Copilot requests that predate OTel.
//
// The chat session file keeps, per request, only the prompt and completion tokens of the LAST model round, the number of
// rounds and Copilot's own credits. The full prompt of an agent request grows round by round; each round reads the previous
// prompt from the cache and writes only the new part. The estimate rebuilds those totals from that structure:
//
//	S  = StableShare * P                  stable prefix (system prompt, tools), read from cache in round 1
//	P1 = S + FirstRoundShare * (P - S)    prompt of round 1; rounds grow linearly from P1 to P
//	cache read   = S + sum of the prompts of rounds 1..N-1
//	cache write  = P - S
//	plain input  = 0, output = C (last round)
//
// The shares were fitted on half of 800 real requests and checked on the other half against Copilot's credits (1 credit =
// 0.01 USD of API value, verified on real spans): total value error -22%, median per-request error 35%. The server
// scales the input side to the credits, so the value is exact and only the split of tokens is an estimate.
// Values are defaults; they can move to the signed configuration without changing the call sites.
type EstimateParams struct {
	StableShare     float64
	FirstRoundShare float64
}

// DefaultEstimateParams are the fitted shares.
var DefaultEstimateParams = EstimateParams{StableShare: 0.2, FirstRoundShare: 0.2}

// TokenEstimate is the estimated token split of one request, in the exporter's terms (input_tokens of OTel is the sum of
// Input, CacheRead and CacheCreation).
type TokenEstimate struct {
	Input, Output, CacheRead, CacheCreation int64
}

// maxEstimateRounds bounds the loop; longer requests are treated as this long.
const maxEstimateRounds = 2000

// EstimateRequestTokens estimates one request from its last-round prompt tokens, last-round completion tokens and number
// of rounds. It returns the zero value when the prompt is unknown (<= 0).
func EstimateRequestTokens(prompt, completion int64, rounds int, p EstimateParams) TokenEstimate {
	if prompt <= 0 {
		return TokenEstimate{}
	}
	n := min(max(rounds, 1), maxEstimateRounds)
	last := float64(prompt)
	s := math.Round(p.StableShare * last)
	first := s + p.FirstRoundShare*(last-s)
	read := s
	if n > 1 {
		// Rounds 1..N-1 are read back by the next round: arithmetic series from first to P (excluding the last round).
		read += float64(n-1) * (first + (last-first)*float64(n-2)/float64(2*(n-1)))
	}
	est := TokenEstimate{
		CacheRead:     int64(math.Round(read)),
		CacheCreation: prompt - int64(s),
		Output:        max(completion, 0),
	}
	return est
}
