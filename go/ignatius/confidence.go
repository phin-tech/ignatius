package ignatius

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Confidence policy (SPEC 1.2). Providers report a confidence in different ways and the numbers
// are not comparable: Jev's formula is undisclosed, Decis documents one formula for every engine
// but keeps each engine's own in its native field, and a provider may send none, in which case
// 1.1 falls back to max(probabilities), on a different scale again. A model's `confidence`
// setting chooses what its cascade and fan-out thresholds are measured against.
const (
	// ConfidenceProvider (the default) trusts the provider's number: 1.1 as it always was.
	ConfidenceProvider = "provider"
	// ConfidenceDerived computes confidence from the answer's own probabilities with one
	// documented formula, whichever provider answered, so thresholds are on one scale across
	// tiers. The provider's number is kept in Answer.NativeConfidence.
	ConfidenceDerived = "derived"
)

func validConfidencePolicy(p string) bool {
	return p == "" || p == ConfidenceProvider || p == ConfidenceDerived
}

// derivedConfidence is the chance-corrected concentration of a distribution, TypeSafe's
// published formulas (and the ones Decis uses):
//
//	choice, K options:  (p_max - 1/K) / (1 - 1/K)
//	score,  n levels:   max(0, 1 - sum_i p_i*|i - m| / MAD_uniform), m the most likely level,
//	                    MAD_uniform = (1/n) * sum_i |i - (n-1)/2|
//
// 0 is no better than a uniform guess and 1 is certain. Noul has its own derivation (1.1) and
// is not handled here. It returns nil when the answer has no usable probabilities, so the
// provider's number (or the 1.1 fallback) stands.
func derivedConfidence(a Answer) *float64 {
	if len(a.Probabilities) == 0 {
		return nil
	}
	switch a.Type {
	case "choice":
		k := float64(len(a.Probabilities))
		pmax := math.Inf(-1)
		for _, p := range a.Probabilities {
			pmax = math.Max(pmax, p)
		}
		if k < 2 { // one option: nothing to choose between
			one := 1.0
			return &one
		}
		return clamp01((pmax - 1/k) / (1 - 1/k))
	case "score":
		type level struct {
			i int
			p float64
		}
		levels := make([]level, 0, len(a.Probabilities))
		for key, p := range a.Probabilities {
			i, err := strconv.Atoi(key)
			if err != nil { // levels are indices; anything else cannot be placed on the scale
				return nil
			}
			levels = append(levels, level{i, p})
		}
		sort.Slice(levels, func(x, y int) bool { return levels[x].i < levels[y].i })
		n := float64(len(levels))
		m := levels[0]
		for _, l := range levels {
			if l.p > m.p { // the first of equals wins, so the result does not depend on map order
				m = l
			}
		}
		var spread, mad float64
		for idx, l := range levels {
			spread += l.p * math.Abs(float64(l.i-m.i))
			mad += math.Abs(float64(idx) - (n-1)/2)
		}
		mad /= n
		if mad == 0 { // a single level
			one := 1.0
			return &one
		}
		return clamp01(1 - spread/mad)
	}
	return nil
}

func clamp01(v float64) *float64 {
	v = math.Max(0, math.Min(1, v))
	return &v
}

// withConfidence applies a model's confidence policy to the raw answers of its backend. It sits
// directly on the provider, below pricing, the breaker and the cache, so what they store has the
// policy applied. It copies the answers: the backend may reuse its map.
type withConfidence struct {
	Inner  Backend
	Policy string
}

func (c withConfidence) Call(ctx context.Context, req Request) (WireResponse, error) {
	w, err := c.Inner.Call(ctx, req)
	if err != nil || c.Policy != ConfidenceDerived {
		return w, err
	}
	answers := make(map[string]Answer, len(w.Answers))
	for id, a := range w.Answers {
		if d := derivedConfidence(a); d != nil {
			if a.Confidence != nil {
				native := *a.Confidence
				a.NativeConfidence = &native
			}
			a.Confidence = d
		}
		answers[id] = a
	}
	w.Answers = answers
	return w, nil
}

func (c withConfidence) Probe(ctx context.Context) string { return probeOf(ctx, c.Inner) }

func confidencePolicyError(alias, p string) error {
	return fmt.Errorf("model %q: confidence must be %q or %q, got %q", alias, ConfidenceProvider, ConfidenceDerived, p)
}
