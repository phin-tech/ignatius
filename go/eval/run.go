package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
)

// Candidate is a route to judge: a name to show and the plan it resolves to.
type Candidate struct {
	Name string
	Plan ignatius.Plan
}

// Options tunes a run. The zero value is usable.
type Options struct {
	Concurrency int           // items in flight at once; default 4
	ItemTimeout time.Duration // one item through one plan; default 30s
	Sweep       bool          // also sweep the threshold of each cascade (SPEC 15.4); costs one solo run per tier model
	Misses      int           // wrong answers kept per candidate, to look at; default 10, -1 for none
	Seed        int64         // for the paired bootstrap; default 1
	// Progress, if set, is called as work completes (units of one item through one plan).
	Progress func(done, total int)
}

func (o *Options) defaults() {
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.ItemTimeout <= 0 {
		o.ItemTimeout = 30 * time.Second
	}
	if o.Misses == 0 {
		o.Misses = 10
	}
	if o.Seed == 0 {
		o.Seed = 1
	}
}

// observation is what one judged question came to under one plan.
type observation struct {
	judged     bool
	answered   bool
	correct    bool
	qtype      string
	model      string   // who gave the final answer
	confidence *float64 // of the final answer
	pTop       *float64 // its top probability
	absErr     *float64 // a score's distance from the gold level
	gold, got  string   // for looking at a miss
}

// itemResult is everything kept of one item run through one plan (never the content).
type itemResult struct {
	obs       map[string]observation
	routed    ignatius.Routed
	wall      time.Duration
	judgedQs  int
	memberObs map[string][]observation // fan-out: each member's own answers, judged
	err       error
}

// Run runs every item through every candidate, judges the answers, and returns the report. It returns an
// error if the context ends or a plan cannot run at all (an invalid plan is the same error for every item).
func Run(ctx context.Context, reg ignatius.Registry, items []Item, cands []Candidate, opts Options) (*Report, error) {
	opts.defaults()
	if err := preflight(items, cands); err != nil {
		return nil, err
	}
	solo := soloModels(cands, opts.Sweep)
	total := len(items) * (len(cands) + len(solo))
	var done int
	var doneMu sync.Mutex
	tick := func() {
		if opts.Progress == nil {
			return
		}
		doneMu.Lock()
		done++
		d := done
		doneMu.Unlock()
		opts.Progress(d, total)
	}

	results := make([][]itemResult, len(cands))
	for ci, c := range cands {
		res, err := runPlan(ctx, reg, items, c.Plan, opts, tick)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Name, err)
		}
		results[ci] = res
	}
	var soloResults map[string][]itemResult
	if len(solo) > 0 {
		soloResults = map[string][]itemResult{}
		for _, m := range solo {
			res, err := runPlan(ctx, reg, items, ignatius.Plan{Mode: ignatius.ModeSingle, Model: m}, opts, tick)
			if err != nil {
				return nil, fmt.Errorf("sweep of %s: %w", m, err)
			}
			soloResults[m] = res
		}
	}
	return buildReport(items, cands, results, soloResults, opts), nil
}

func preflight(items []Item, cands []Candidate) error {
	if len(items) == 0 {
		return errors.New("no items")
	}
	if len(cands) == 0 {
		return errors.New("no routes to judge")
	}
	seen := map[string]bool{}
	for _, c := range cands {
		if seen[c.Name] {
			return fmt.Errorf("route %q is listed twice", c.Name)
		}
		seen[c.Name] = true
		if c.Plan.Mode == ignatius.ModeFanOut && (c.Plan.Reduce == "" || c.Plan.Reduce == "none") {
			return fmt.Errorf("%s: a fan-out must reduce to one answer (vote, mean or most_confident) to be judged", c.Name)
		}
	}
	return nil
}

// soloModels is the distinct models of the cascades, when a sweep was asked for.
func soloModels(cands []Candidate, sweep bool) []string {
	if !sweep {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		if c.Plan.Mode != ignatius.ModeCascade {
			continue
		}
		for _, t := range c.Plan.Tiers {
			if !seen[t.Model] {
				seen[t.Model] = true
				out = append(out, t.Model)
			}
		}
	}
	return out
}

func runPlan(ctx context.Context, reg ignatius.Registry, items []Item, plan ignatius.Plan, opts Options, tick func()) ([]itemResult, error) {
	out := make([]itemResult, len(items))
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := range items {
		select {
		case sem <- struct{}{}:
		case <-runCtx.Done():
		}
		if runCtx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := runItem(runCtx, reg, items[i], plan, opts.ItemTimeout)
			if err != nil {
				errOnce.Do(func() { firstErr = err; cancel() })
				return
			}
			out[i] = r
			tick()
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func runItem(ctx context.Context, reg ignatius.Registry, it Item, plan ignatius.Plan, timeout time.Duration) (itemResult, error) {
	ictx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ictx = ignatius.WithRouteLabel(ictx, "eval")
	start := time.Now()
	routed, err := ignatius.Run(ictx, reg, it.Request(), plan)
	wall := time.Since(start)
	if err != nil {
		// A plan that cannot run is a problem with the plan, not with the item: stop the evaluation.
		return itemResult{}, err
	}
	r := itemResult{routed: routed, wall: wall, obs: map[string]observation{}}
	for qid, g := range it.gold {
		q := it.Questions[qid]
		r.judgedQs++
		o := observation{judged: true, qtype: q.Type, gold: g.show(q.Type), got: "(no answer)"}
		if a, ok := routed.Answers[qid]; ok {
			o = judge(q, g, a)
		}
		r.obs[qid] = o
	}
	if plan.Mode == ignatius.ModeFanOut {
		r.memberObs = map[string][]observation{}
		for _, res := range routed.Results {
			for qid, g := range it.gold {
				o := observation{judged: true, qtype: it.Questions[qid].Type, gold: g.show(it.Questions[qid].Type), got: "(no answer)"}
				if a, ok := res.Answers[qid]; ok {
					o = judge(it.Questions[qid], g, a)
				}
				r.memberObs[res.Model] = append(r.memberObs[res.Model], o)
			}
		}
	}
	return r, nil
}

// judge compares one answer with its gold.
func judge(q ignatius.Question, g goldValue, a ignatius.Answer) observation {
	none := observation{judged: true, qtype: q.Type, gold: g.show(q.Type), got: "(no answer)"}
	o := observation{judged: true, qtype: q.Type, model: a.Model, confidence: a.Confidence, gold: g.show(q.Type)}
	switch q.Type {
	case "choice":
		if a.Choice == "" {
			return none
		}
		o.answered, o.correct, o.got = true, a.Choice == g.text, a.Choice
		o.pTop = maxProb(a.Probabilities)
	case "noul":
		if a.Noul == nil {
			return none
		}
		o.answered, o.correct = true, (*a.Noul >= 0.5) == g.yes
		o.got = yesNo(*a.Noul >= 0.5)
		p := math.Max(*a.Noul, 1-*a.Noul)
		o.pTop = &p
	case "score":
		if a.Score == nil {
			return none
		}
		o.answered, o.correct = true, math.Round(*a.Score) == g.level
		o.got = fmt.Sprintf("%.1f", *a.Score)
		e := math.Abs(*a.Score - g.level)
		o.absErr = &e
		o.pTop = maxProb(a.Probabilities)
	}
	return o
}

func maxProb(p map[string]float64) *float64 {
	if len(p) == 0 {
		return nil
	}
	m := math.Inf(-1)
	for _, v := range p {
		m = math.Max(m, v)
	}
	return &m
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// show is the gold answer as text.
func (g goldValue) show(qtype string) string {
	switch qtype {
	case "noul":
		return yesNo(g.yes)
	case "score":
		return fmt.Sprintf("%.0f", g.level)
	}
	return g.text
}
