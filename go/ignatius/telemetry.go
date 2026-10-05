package ignatius

import (
	"context"
	"math"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Instrumentation uses only the OpenTelemetry API, through the global providers.
// With no provider installed (the default) every call below is a cheap no-op, and
// an embedding application controls export by installing its own (SPEC 10.1).
//
// Privacy rule: spans and metrics carry structure, timings, model names, token
// counts, confidences and error KINDS only. Never state, question text, criteria,
// answer values, or error messages (an upstream error body can echo the request).
// Metric attributes must be bounded: model aliases and route labels come from
// config, never from client-controlled text.

const instrumentationName = "github.com/phin-tech/ignatius/go/ignatius"

type routeKey struct{}

// WithRouteLabel tags the metrics recorded under ctx with a route label. The label
// MUST be bounded (a named route, an alias, "inline" or "plan"): never pass
// client-controlled text, which would make the metric's cardinality unbounded.
func WithRouteLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, routeKey{}, label)
}

func routeLabel(ctx context.Context) string {
	if l, ok := ctx.Value(routeKey{}).(string); ok && l != "" {
		return l
	}
	return "unlabeled"
}

var seconds = metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10)
var ratio = metric.WithExplicitBucketBoundaries(0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.99)

// Metric attribute sets built from bounded values (model aliases, error kinds,
// question types) are cached: building them per call allocates, and the same few
// sets repeat forever. Route labels are not cached since the caller supplies them.
var attrCache sync.Map // string -> metric.MeasurementOption

func cachedAttrs(key string, kvs ...attribute.KeyValue) metric.MeasurementOption {
	if v, ok := attrCache.Load(key); ok {
		return v.(metric.MeasurementOption)
	}
	opt := metric.WithAttributeSet(attribute.NewSet(kvs...))
	attrCache.Store(key, opt)
	return opt
}

func optModel(alias string) metric.MeasurementOption {
	return cachedAttrs("m|"+alias, attribute.String("model", alias))
}

func optModelOK(alias string, ok bool) metric.MeasurementOption {
	k := "mo|" + alias + "|f"
	if ok {
		k = "mo|" + alias + "|t"
	}
	return cachedAttrs(k, attribute.String("model", alias), attribute.Bool("ok", ok))
}

func optModelKind(alias, kind string) metric.MeasurementOption {
	return cachedAttrs("mk|"+alias+"|"+kind, attribute.String("model", alias), attribute.String("kind", kind))
}

func optModelType(alias, qtype string) metric.MeasurementOption {
	return cachedAttrs("mt|"+alias+"|"+qtype, attribute.String("model", alias), attribute.String("question_type", qtype))
}

type instruments struct {
	tracer          trace.Tracer
	callDuration    metric.Float64Histogram // ignatius.model.call.duration (s) {model, ok}
	callFailures    metric.Int64Counter     // ignatius.model.failures {model, kind}
	confidence      metric.Float64Histogram // ignatius.answer.confidence {model, question_type}
	escalations     metric.Int64Counter     // ignatius.cascade.escalations {route, tier, model}
	cachedQuestions metric.Int64Counter     // ignatius.cache.questions {model}
	cost            metric.Float64Counter   // ignatius.cost (USD) {model}
	disagreement    metric.Int64Counter     // ignatius.fanout.disagreement {route, question_type}
}

// The global providers delegate, so instruments created here keep working when an
// application installs its providers later.
var tel = func() instruments {
	m := otel.Meter(instrumentationName)
	var i instruments
	i.tracer = otel.Tracer(instrumentationName)
	i.callDuration, _ = m.Float64Histogram("ignatius.model.call.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of calls to a model backend."), seconds)
	i.callFailures, _ = m.Int64Counter("ignatius.model.failures",
		metric.WithDescription("Failed model calls by error kind."))
	i.confidence, _ = m.Float64Histogram("ignatius.answer.confidence", metric.WithUnit("1"),
		metric.WithDescription("Normalized confidence of answers by model and question type."), ratio)
	i.escalations, _ = m.Int64Counter("ignatius.cascade.escalations",
		metric.WithDescription("Questions escalated to the next cascade tier."))
	i.cachedQuestions, _ = m.Int64Counter("ignatius.cache.questions",
		metric.WithDescription("Questions served from the response cache."))
	i.cost, _ = m.Float64Counter("ignatius.cost", metric.WithUnit("usd"),
		metric.WithDescription("Spend computed from configured model prices."))
	i.disagreement, _ = m.Int64Counter("ignatius.fanout.disagreement",
		metric.WithDescription("Fan-out questions on which the responding models differed."))
	return i
}()

// instrumentedCall wraps one backend call in a client span and records its metrics.
func (r Registry) instrumentedCall(ctx context.Context, alias string, req Request,
	do func(context.Context) (Result, *Failure)) (Result, *Failure) {
	ctx, span := tel.tracer.Start(ctx, "ignatius.model.call", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	recording := span.IsRecording() // a no-op span: skip building attributes nobody will see
	if recording {
		span.SetAttributes(attribute.String("ignatius.model", alias), attribute.Int("ignatius.questions", len(req.Questions)))
	}
	start := time.Now()
	res, fail := do(ctx)
	elapsed := time.Since(start).Seconds()

	if fail != nil {
		if recording {
			span.SetAttributes(attribute.Bool("ignatius.ok", false), attribute.String("ignatius.error.kind", fail.Error.Kind))
			span.SetStatus(codes.Error, fail.Error.Kind) // the kind, never the message
		}
		tel.callFailures.Add(ctx, 1, optModelKind(alias, fail.Error.Kind))
		tel.callDuration.Record(ctx, elapsed, optModelOK(alias, false))
		return res, fail
	}
	if recording {
		span.SetAttributes(attribute.Bool("ignatius.ok", true),
			attribute.Int("ignatius.usage.input_tokens", res.Usage.InputTokens),
			attribute.Int("ignatius.usage.output_tokens", res.Usage.OutputTokens),
			attribute.Int("ignatius.cached", res.Cached))
		if res.CostUSD != nil {
			span.SetAttributes(attribute.Float64("ignatius.cost_usd", *res.CostUSD))
		}
	}
	if res.CostUSD != nil && *res.CostUSD > 0 {
		tel.cost.Add(ctx, *res.CostUSD, optModel(alias))
	}
	if res.Cached > 0 {
		tel.cachedQuestions.Add(ctx, int64(res.Cached), optModel(alias))
	}
	if !(res.Cached > 0 && res.Cached == len(res.Answers)) { // fully cached: no call happened
		tel.callDuration.Record(ctx, elapsed, optModelOK(alias, true))
	}
	for _, a := range res.Answers {
		if a.Confidence != nil {
			tel.confidence.Record(ctx, *a.Confidence, optModelType(alias, a.Type))
		}
	}
	return res, nil
}

// recordDisagreement counts fan-out questions on which the responders differed:
// different choices; a noul answered yes by some and no by others; scores that
// round to different levels.
func recordDisagreement(ctx context.Context, questions map[string]Question, results []Result) {
	if len(results) < 2 {
		return
	}
	route := attribute.String("route", routeLabel(ctx))
	for id, q := range questions {
		var seen []Answer
		for _, r := range results {
			if a, ok := r.Answers[id]; ok && a.Type == q.Type {
				seen = append(seen, a)
			}
		}
		if len(seen) < 2 || !differ(q.Type, seen) {
			continue
		}
		tel.disagreement.Add(ctx, 1, metric.WithAttributes(route, attribute.String("question_type", q.Type)))
	}
}

func differ(qtype string, as []Answer) bool {
	switch qtype {
	case "choice":
		for _, a := range as[1:] {
			if a.Choice != as[0].Choice {
				return true
			}
		}
	case "noul":
		yes, no := false, false
		for _, a := range as {
			if a.Noul == nil {
				continue
			}
			if *a.Noul >= 0.5 {
				yes = true
			} else {
				no = true
			}
		}
		return yes && no
	case "score":
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, a := range as {
			if a.Score != nil {
				lo, hi = math.Min(lo, *a.Score), math.Max(hi, *a.Score)
			}
		}
		return math.Round(lo) != math.Round(hi) && !math.IsInf(lo, 0)
	}
	return false
}
