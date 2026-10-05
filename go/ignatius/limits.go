package ignatius

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// LimitsConfig is a model's [models.NAME.limits] table (SPEC 3.1): what one call to it can take.
// Without it a model takes any number of questions and no images, so a request with pictures
// is never sent to a model that would ignore them.
type LimitsConfig struct {
	// MaxQuestions splits a request with more questions into calls of at most this many
	// (Clef: 64). 0 means no limit.
	MaxQuestions int `toml:"max_questions" json:"max_questions"`
	// MaxImages is how many images a call may carry (Clef: 4). 0 means none: a request with
	// images fails on this model with kind "unsupported", which a cascade escalates past.
	MaxImages int `toml:"max_images" json:"max_images"`
}

// Limited enforces a model's limits. It is the outermost decorator, so a split request reaches
// the cache and the breaker as the ordinary smaller calls they already handle, and an
// unsupported request never touches the breaker.
type Limited struct {
	Inner  Backend
	Limits LimitsConfig
}

func (l Limited) Call(ctx context.Context, req Request) (WireResponse, error) {
	if n := len(req.Images); n > l.Limits.MaxImages {
		msg := fmt.Sprintf("this model takes at most %d images, the request has %d", l.Limits.MaxImages, n)
		if l.Limits.MaxImages == 0 {
			msg = fmt.Sprintf("this model takes no images, the request has %d", n)
		}
		return WireResponse{}, &CallError{Kind: KindUnsupported, Message: msg}
	}
	max := l.Limits.MaxQuestions
	if max <= 0 || len(req.Questions) <= max {
		return l.Inner.Call(ctx, req)
	}

	// Too many questions for one call: split, call the pieces together, merge. The result is all
	// or nothing, so a caller never gets half of what it asked.
	ids := make([]string, 0, len(req.Questions))
	for id := range req.Questions {
		ids = append(ids, id)
	}
	sort.Strings(ids) // the same split every time, so the cache sees the same pieces
	var chunks []Request
	for i := 0; i < len(ids); i += max {
		sub := Request{State: req.State, Images: req.Images, Questions: map[string]Question{}}
		for _, id := range ids[i:min(i+max, len(ids))] {
			sub.Questions[id] = req.Questions[id]
		}
		chunks = append(chunks, sub)
	}
	type piece struct {
		w   WireResponse
		err error
	}
	pieces := make([]piece, len(chunks))
	var wg sync.WaitGroup
	for i, c := range chunks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pieces[i].w, pieces[i].err = l.Inner.Call(ctx, c)
		}()
	}
	wg.Wait()
	out := WireResponse{Answers: map[string]Answer{}}
	for i, p := range pieces {
		if p.err != nil {
			return WireResponse{}, p.err // the first failing piece, in order
		}
		if i == 0 {
			out.Model = p.w.Model
		}
		for id, a := range p.w.Answers {
			out.Answers[id] = a
		}
		out.Usage.InputTokens += p.w.Usage.InputTokens
		out.Usage.OutputTokens += p.w.Usage.OutputTokens
		out.Cached += p.w.Cached
		if p.w.CostUSD != nil {
			sum := *p.w.CostUSD
			if out.CostUSD != nil {
				sum += *out.CostUSD
			}
			out.CostUSD = &sum
		}
	}
	return out, nil
}

func (l Limited) Probe(ctx context.Context) string { return probeOf(ctx, l.Inner) }
