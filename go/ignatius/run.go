package ignatius

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const defaultThreshold = 0.8

// Option customizes Run.
type Option func(*runOpts)

type runOpts struct {
	escalateIf func(id string, a Answer) bool
}

// WithEscalateIf overrides the cascade "is this answer settled" rule: when it
// returns true for an answer, the question escalates regardless of confidence.
func WithEscalateIf(f func(id string, a Answer) bool) Option {
	return func(o *runOpts) { o.escalateIf = f }
}

// Validate reports every problem with a Plan against a Registry.
func (p Plan) Validate(reg Registry) error {
	var probs []string
	has := func(alias string) {
		if _, ok := reg[alias]; !ok {
			probs = append(probs, fmt.Sprintf("unknown model %q (available: %v)", alias, reg.names()))
		}
	}
	switch p.Mode {
	case ModeSingle:
		if p.Model == "" {
			probs = append(probs, "single requires model")
		} else {
			has(p.Model)
		}
	case ModeFanOut:
		if len(p.Models) == 0 {
			probs = append(probs, "fan_out requires models")
		}
		seen := map[string]bool{}
		for _, m := range p.Models {
			has(m)
			if seen[m] {
				probs = append(probs, fmt.Sprintf("duplicate model %q", m))
			}
			seen[m] = true
		}
		switch p.Reduce {
		case "", "none", "vote", "mean", "most_confident":
		default:
			probs = append(probs, fmt.Sprintf("unknown reduce %q", p.Reduce))
		}
	case ModeCascade:
		if len(p.Tiers) == 0 {
			probs = append(probs, "cascade requires tiers")
		}
		for _, t := range p.Tiers {
			has(t.Model)
		}
	default:
		probs = append(probs, fmt.Sprintf("unknown mode %q", p.Mode))
	}
	if len(probs) > 0 {
		return &PlanError{Problems: probs}
	}
	return nil
}

// Run executes a Plan. Provider failures are data in the returned Routed; the
// error is non-nil only for an invalid Plan.
func Run(ctx context.Context, reg Registry, req Request, plan Plan, opts ...Option) (Routed, error) {
	if err := plan.Validate(reg); err != nil {
		return Routed{}, err
	}
	var o runOpts
	for _, f := range opts {
		f(&o)
	}
	if plan.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(plan.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	ctx, span := tel.tracer.Start(ctx, "ignatius.run")
	defer span.End()
	recording := span.IsRecording()
	if recording {
		span.SetAttributes(attribute.String("ignatius.mode", plan.Mode), attribute.Int("ignatius.questions", len(req.Questions)))
	}
	out := Routed{Mode: plan.Mode, Answers: map[string]Answer{}, Results: []Result{}, Failures: []Failure{}}
	switch plan.Mode {
	case ModeSingle:
		runSingle(ctx, reg, req, plan, &out)
	case ModeFanOut:
		runFanOut(ctx, reg, req, plan, &out)
	case ModeCascade:
		runCascade(ctx, reg, req, plan, o, &out)
	}
	for _, res := range out.Results {
		if res.CostUSD != nil {
			total := *res.CostUSD
			if out.CostUSD != nil {
				total += *out.CostUSD
			}
			out.CostUSD = &total
		}
	}
	if recording {
		span.SetAttributes(attribute.Bool("ignatius.ok", out.OK), attribute.Int("ignatius.results", len(out.Results)),
			attribute.Int("ignatius.failures", len(out.Failures)))
		if out.CostUSD != nil {
			span.SetAttributes(attribute.Float64("ignatius.cost_usd", *out.CostUSD))
		}
		if !out.OK {
			span.SetStatus(codes.Error, "not ok")
		}
	}
	return out, nil
}

func runSingle(ctx context.Context, reg Registry, req Request, plan Plan, out *Routed) {
	res, fail := reg.call(ctx, plan.Model, req)
	if fail != nil {
		out.Failures = append(out.Failures, *fail)
		return
	}
	out.Results = append(out.Results, res)
	out.Answers = res.Answers
	out.OK = true
}

func runFanOut(ctx context.Context, reg Registry, req Request, plan Plan, out *Routed) {
	type slot struct {
		res  Result
		fail *Failure
	}
	slots := make([]slot, len(plan.Models))
	var wg sync.WaitGroup
	for i, m := range plan.Models {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots[i].res, slots[i].fail = reg.call(ctx, m, req)
		}()
	}
	wg.Wait()                 // never fail fast: every call is bounded by its own timeout
	for _, s := range slots { // given order, not completion order
		if s.fail != nil {
			out.Failures = append(out.Failures, *s.fail)
		} else {
			out.Results = append(out.Results, s.res)
		}
	}
	min := plan.MinSuccess
	if min <= 0 {
		min = 1
	}
	recordDisagreement(ctx, req.Questions, out.Results)
	out.OK = len(out.Results) >= min
	if plan.Reduce != "" && plan.Reduce != "none" {
		out.Answers = reduce(plan.Reduce, req.Questions, out.Results)
	}
}

func (t Tier) bar(qtype string, plan Plan) float64 {
	if v, ok := t.Threshold.lookup(qtype); ok {
		return v
	}
	if v, ok := plan.Threshold.lookup(qtype); ok {
		return v
	}
	return defaultThreshold
}

func sortedIDs(m map[string]Question) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func runCascade(ctx context.Context, reg Registry, req Request, plan Plan, o runOpts, out *Routed) {
	pending := sortedIDs(req.Questions)
	best := map[string]Answer{} // highest confidence so far; ties go to the later tier
	for i, tier := range plan.Tiers {
		if len(pending) == 0 {
			break
		}
		last := i == len(plan.Tiers)-1
		sub := Request{State: req.State, Images: req.Images, Questions: map[string]Question{}}
		for _, id := range pending {
			sub.Questions[id] = req.Questions[id]
		}
		hop := Hop{Tier: i, Model: tier.Model, Questions: append([]string{}, pending...), Escalated: []string{}}
		tctx, tspan := tel.tracer.Start(ctx, "ignatius.cascade.tier")
		if tspan.IsRecording() {
			tspan.SetAttributes(attribute.Int("ignatius.tier", i), attribute.String("ignatius.model", tier.Model),
				attribute.Int("ignatius.questions", len(pending)))
		}
		endTier := func() { recordTier(ctx, tspan, hop) } // call once hop is final
		res, fail := reg.call(tctx, tier.Model, sub)
		if fail != nil {
			out.Failures = append(out.Failures, *fail)
			hop.LatencyMS, hop.Error = fail.LatencyMS, &fail.Error
			for _, id := range pending {
				hop.Detail = append(hop.Detail, HopQuestion{ID: id, Escalated: !last})
			}
			if !last {
				hop.Escalated = append(hop.Escalated, pending...)
			}
			endTier()
			out.Trace = append(out.Trace, hop)
			continue
		}
		hop.LatencyMS = res.LatencyMS
		out.Results = append(out.Results, res)
		var next []string
		for _, id := range pending {
			a, ok := res.Answers[id]
			if !ok { // a successful tier that skipped a question escalates it
				next = append(next, id)
				hop.Detail = append(hop.Detail, HopQuestion{ID: id, Escalated: !last})
				continue
			}
			if prev, seen := best[id]; !seen || confValue(a) >= confValue(prev) {
				best[id] = a
			}
			if last { // last tier never settles; the best-seen pass below decides
				next = append(next, id)
				hop.Detail = append(hop.Detail, HopQuestion{ID: id, Confidence: a.Confidence})
				continue
			}
			bar := tier.bar(a.Type, plan)
			settled := a.Confidence != nil && *a.Confidence >= bar
			if o.escalateIf != nil {
				settled = !o.escalateIf(id, a)
			}
			hop.Detail = append(hop.Detail, HopQuestion{ID: id, Confidence: a.Confidence, Threshold: &bar, Escalated: !settled})
			if settled {
				out.Answers[id] = a
			} else {
				next = append(next, id)
			}
		}
		if !last {
			hop.Escalated = append(hop.Escalated, next...)
		}
		endTier()
		out.Trace = append(out.Trace, hop)
		pending = next
	}
	for _, id := range pending { // still unsettled: best answer seen, if any
		if a, ok := best[id]; ok {
			out.Answers[id] = a
		}
	}
	out.OK = len(out.Answers) == len(req.Questions)
}

// recordTier closes a cascade tier's span with its per-question decisions and
// counts its escalations. Question ids are the caller's schema names, not content.
func recordTier(ctx context.Context, span trace.Span, h Hop) {
	defer span.End()
	if span.IsRecording() {
		span.SetAttributes(attribute.Bool("ignatius.ok", h.Error == nil), attribute.Int("ignatius.escalated", len(h.Escalated)),
			attribute.Int64("ignatius.latency_ms", h.LatencyMS))
		if h.Error != nil {
			span.SetStatus(codes.Error, h.Error.Kind) // the kind, never the message
		}
		for _, q := range h.Detail {
			attrs := []attribute.KeyValue{attribute.String("ignatius.question.id", q.ID), attribute.Bool("ignatius.escalated", q.Escalated)}
			if q.Confidence != nil {
				attrs = append(attrs, attribute.Float64("ignatius.confidence", *q.Confidence))
			}
			if q.Threshold != nil {
				attrs = append(attrs, attribute.Float64("ignatius.threshold", *q.Threshold))
			}
			span.AddEvent("ignatius.decision", trace.WithAttributes(attrs...))
		}
	}
	if n := len(h.Escalated); n > 0 {
		tel.escalations.Add(ctx, int64(n), metric.WithAttributes(
			attribute.String("route", routeLabel(ctx)), attribute.Int("tier", h.Tier), attribute.String("model", h.Model)))
	}
}
