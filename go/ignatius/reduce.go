package ignatius

import (
	"math"
	"sort"
)

// reduce merges fan-out results into one Answer per question (SPEC 6.0).
// A question with no successful contributor is absent from the output.
func reduce(kind string, questions map[string]Question, results []Result) map[string]Answer {
	out := map[string]Answer{}
	for id, q := range questions {
		var as []Answer
		for _, r := range results {
			if a, ok := r.Answers[id]; ok && a.Type == q.Type {
				as = append(as, a)
			}
		}
		if len(as) == 0 {
			continue
		}
		if kind == "most_confident" {
			best := as[0]
			for _, a := range as[1:] {
				if confValue(a) > confValue(best) {
					best = a
				}
			}
			out[id] = best
			continue
		}
		contributors := make([]string, len(as))
		for i, a := range as {
			contributors[i] = a.Model
		}
		var merged Answer
		switch q.Type {
		case "choice":
			merged = mergeChoice(kind, as)
		case "noul":
			merged = Answer{Type: "noul", Noul: meanOf(as, func(a Answer) *float64 { return a.Noul })}
		case "score":
			merged = Answer{Type: "score", Score: meanOf(as, func(a Answer) *float64 { return a.Score }),
				Probabilities: meanProbs(as), Legend: as[0].Legend}
		default:
			continue
		}
		merged.Model, merged.Contributors = "ensemble", contributors
		merged.Confidence = deriveConfidence(merged)
		out[id] = merged
	}
	return out
}

func meanOf(as []Answer, get func(Answer) *float64) *float64 {
	sum, n := 0.0, 0
	for _, a := range as {
		if v := get(a); v != nil {
			sum += *v
			n++
		}
	}
	if n == 0 {
		return nil
	}
	m := sum / float64(n)
	return &m
}

func meanProbs(as []Answer) map[string]float64 {
	out := map[string]float64{}
	for _, a := range as {
		for k, p := range a.Probabilities {
			out[k] += p / float64(len(as))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func argmax(p map[string]float64) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic ties: lexicographically smallest wins
	best, bv := "", math.Inf(-1)
	for _, k := range keys {
		if p[k] > bv {
			best, bv = k, p[k]
		}
	}
	return best
}

func mergeChoice(kind string, as []Answer) Answer {
	if kind == "mean" {
		p := meanProbs(as)
		return Answer{Type: "choice", Choice: argmax(p), Probabilities: p}
	}
	// vote: plurality; ties go to the larger summed probability, then the name.
	counts, psum := map[string]int{}, map[string]float64{}
	shares := map[string]float64{}
	for _, a := range as {
		counts[a.Choice]++
		psum[a.Choice] += a.Probabilities[a.Choice]
		for k := range a.Probabilities {
			shares[k] = 0
		}
	}
	for c := range counts {
		shares[c] = 0
	}
	score := map[string]float64{}
	for c, n := range counts {
		shares[c] = float64(n) / float64(len(as))
		score[c] = float64(n)*1e6 + psum[c] // count dominates; probability breaks ties
	}
	return Answer{Type: "choice", Choice: argmax(score), Probabilities: shares}
}
