package ignatius

import "math"

// deriveConfidence implements SPEC 1.1.
//   - noul: abs(p-0.5)*2 (providers send no separate confidence for noul)
//   - choice/score: the provider's confidence if present, else max(probabilities)
//   - nil if nothing can be derived
func deriveConfidence(a Answer) *float64 {
	if a.Type == "noul" {
		if a.Noul == nil {
			return nil
		}
		v := math.Abs(*a.Noul-0.5) * 2
		return &v
	}
	if a.Confidence != nil {
		return a.Confidence
	}
	if len(a.Probabilities) == 0 {
		return nil
	}
	max := math.Inf(-1)
	for _, p := range a.Probabilities {
		max = math.Max(max, p)
	}
	return &max
}

// normalize stamps the producing alias and derives confidence.
func normalize(model string, wire map[string]Answer) map[string]Answer {
	out := make(map[string]Answer, len(wire))
	for id, a := range wire {
		a.Model = model
		a.Confidence = deriveConfidence(a)
		out[id] = a
	}
	return out
}

func confValue(a Answer) float64 {
	if a.Confidence == nil {
		return -1
	}
	return *a.Confidence
}
