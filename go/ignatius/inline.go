package ignatius

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNotInline means the string has no inline-route prefix (it may be a plain alias).
var ErrNotInline = errors.New("not an inline route")

// InlineGrammar is shown in error messages.
const InlineGrammar = "fan-out:a,b[|vote|mean|most_confident]  |  cascade:a[@0.5]>b[@0.7]>c"

// ParseInline parses an inline route carried in a model name (SPEC 6.1):
//
//	fan-out:jeff,jev,clef        fan_out, reducer defaults to vote
//	fan-out:jeff,jev|mean        explicit reducer
//	cascade:jeff@0.5>clef>jev    cascade with optional per-tier threshold
func ParseInline(s string, maxModels int) (Plan, error) {
	head, body, ok := strings.Cut(s, ":")
	if !ok {
		return Plan{}, ErrNotInline
	}
	var p Plan
	switch strings.ReplaceAll(head, "-", "_") {
	case ModeFanOut:
		list, reduce, _ := strings.Cut(body, "|")
		if reduce == "" {
			reduce = "vote"
		}
		models, err := splitList(list, ",")
		if err != nil {
			return Plan{}, err
		}
		p = Plan{Mode: ModeFanOut, Models: models, Reduce: reduce}
	case ModeCascade:
		parts, err := splitList(body, ">")
		if err != nil {
			return Plan{}, err
		}
		p = Plan{Mode: ModeCascade}
		for _, part := range parts {
			alias, thr, has := strings.Cut(part, "@")
			t := Tier{Model: alias}
			if has {
				v, err := strconv.ParseFloat(thr, 64)
				if err != nil || v < 0 || v > 1 {
					return Plan{}, fmt.Errorf("tier %q: threshold must be a number in [0,1]", part)
				}
				t.Threshold = &Threshold{Default: &v}
			}
			p.Tiers = append(p.Tiers, t)
		}
	default:
		return Plan{}, fmt.Errorf("unknown inline mode %q (grammar: %s)", head, InlineGrammar)
	}
	if n := len(p.Models) + len(p.Tiers); maxModels > 0 && n > maxModels {
		return Plan{}, fmt.Errorf("inline route names %d models, the limit is %d", n, maxModels)
	}
	return p, nil
}

func splitList(s, sep string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, sep) {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty model name in %q", s)
		}
		out = append(out, part)
	}
	return out, nil
}
