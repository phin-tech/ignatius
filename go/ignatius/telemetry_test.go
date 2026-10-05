package ignatius

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The global providers delegate only once, so one recording provider is installed
// for the whole test binary and each test reads what happened since it started.
var (
	obsOnce sync.Once
	recSpan *tracetest.SpanRecorder
	recRead *sdkmetric.ManualReader
)

type observer struct {
	t    *testing.T
	mark int
	rm   *metricdata.ResourceMetrics // collected once: delta temporality drains on every Collect
}

func observe(t *testing.T) *observer {
	obsOnce.Do(func() {
		recSpan = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recSpan)))
		recRead = sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(
			func(sdkmetric.InstrumentKind) metricdata.Temporality { return metricdata.DeltaTemporality }))
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(recRead)))
	})
	var discard metricdata.ResourceMetrics
	_ = recRead.Collect(context.Background(), &discard) // reset the delta window
	return &observer{t: t, mark: len(recSpan.Ended())}
}

// next starts a new collection window: metrics read after it cover only what ran since.
func (o *observer) next() { o.rm = nil }

func (o *observer) spans() []sdktrace.ReadOnlySpan { return recSpan.Ended()[o.mark:] }

func (o *observer) spansNamed(name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range o.spans() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// point is one metric data point flattened for assertions.
type point struct {
	attrs map[string]string
	value float64 // sum value, or the histogram's sum
	count uint64  // histogram observations
}

func (o *observer) metric(name string) []point {
	if o.rm == nil {
		o.rm = &metricdata.ResourceMetrics{}
		if err := recRead.Collect(context.Background(), o.rm); err != nil {
			o.t.Fatal(err)
		}
	}
	var out []point
	for _, sm := range o.rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			flat := func(s attribute.Set) map[string]string {
				mm := map[string]string{}
				for _, kv := range s.ToSlice() {
					mm[string(kv.Key)] = kv.Value.Emit()
				}
				return mm
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					out = append(out, point{attrs: flat(p.Attributes), value: float64(p.Value)})
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					out = append(out, point{attrs: flat(p.Attributes), value: p.Value})
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					out = append(out, point{attrs: flat(p.Attributes), value: p.Sum, count: p.Count})
				}
			}
		}
	}
	return out
}

func attrMap(kvs []attribute.KeyValue) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func total(ps []point, match map[string]string) (sum float64, n int) {
next:
	for _, p := range ps {
		for k, v := range match {
			if p.attrs[k] != v {
				continue next
			}
		}
		sum += p.value
		n++
	}
	return
}

func cascadeReg() Registry {
	return Registry{
		"cheap": &Priced{Inner: &scripted{usage: Usage{InputTokens: 10, OutputTokens: 2},
			ans: map[string]Answer{"q": {Type: "noul", Noul: f(0.55)}}}, InPerMTok: 100_000},
		"smart": &scripted{ans: map[string]Answer{"q": {Type: "noul", Noul: f(0.99)}}},
	}
}

func TestRunEmitsASpanTreeWithPerQuestionDecisions(t *testing.T) {
	o := observe(t)
	plan := Plan{Mode: ModeCascade, Tiers: []Tier{{Model: "cheap", Threshold: &Threshold{Default: f(0.5)}}, {Model: "smart"}}}
	routed, err := Run(WithRouteLabel(context.Background(), "tiered"), cascadeReg(), noulReq(), plan)
	if err != nil || !routed.OK {
		t.Fatal(err, routed)
	}

	run := o.spansNamed("ignatius.run")
	tiers := o.spansNamed("ignatius.cascade.tier")
	calls := o.spansNamed("ignatius.model.call")
	if len(run) != 1 || len(tiers) != 2 || len(calls) != 2 {
		t.Fatalf("want 1 run, 2 tier and 2 call spans, got %d/%d/%d", len(run), len(tiers), len(calls))
	}
	// the tree: run -> tier -> model call
	for _, tr := range tiers {
		if tr.Parent().SpanID() != run[0].SpanContext().SpanID() {
			t.Error("a tier span must be a child of the run span")
		}
	}
	for i, c := range calls {
		if c.Parent().SpanID() != tiers[i].SpanContext().SpanID() {
			t.Errorf("model call %d must be a child of its tier span", i)
		}
	}
	ra := attrMap(run[0].Attributes())
	if ra["ignatius.mode"].AsString() != ModeCascade || !ra["ignatius.ok"].AsBool() || ra["ignatius.questions"].AsInt64() != 1 {
		t.Errorf("run attrs: %v", ra)
	}
	ca := attrMap(calls[0].Attributes())
	if ca["ignatius.model"].AsString() != "cheap" || ca["ignatius.usage.input_tokens"].AsInt64() != 10 || !ca["ignatius.ok"].AsBool() {
		t.Errorf("call attrs: %v", ca)
	}

	// tier 0 decided: confidence 0.1 < threshold 0.5 -> escalated; tier 1 is final
	ev := tiers[0].Events()
	if len(ev) != 1 || ev[0].Name != "ignatius.decision" {
		t.Fatalf("tier 0 events: %+v", ev)
	}
	da := attrMap(ev[0].Attributes)
	if da["ignatius.question.id"].AsString() != "q" || !da["ignatius.escalated"].AsBool() ||
		da["ignatius.threshold"].AsFloat64() != 0.5 || diff(da["ignatius.confidence"].AsFloat64(), 0.1) > 1e-9 {
		t.Errorf("decision event: %v", da)
	}
	if _, has := attrMap(tiers[1].Events()[0].Attributes)["ignatius.threshold"]; has {
		t.Error("the last tier has no threshold")
	}
}

func TestFailedCallSpanCarriesTheKindNeverTheMessage(t *testing.T) {
	o := observe(t)
	const secret = "SECRET-FROM-UPSTREAM-BODY"
	reg := Registry{"m": &scripted{err: &CallError{Kind: KindHTTP, Message: "500: " + secret, Status: 500}}}
	routed, _ := Run(context.Background(), reg, noulReq(), Plan{Mode: ModeSingle, Model: "m"})
	if routed.OK || !strings.Contains(routed.Failures[0].Error.Message, secret) {
		t.Fatal("setup: the caller's own response should still carry the message")
	}
	span := o.spansNamed("ignatius.model.call")[0]
	if span.Status().Code != codes.Error || span.Status().Description != KindHTTP {
		t.Errorf("status = %+v, want Error with the kind as description", span.Status())
	}
	if attrMap(span.Attributes())["ignatius.error.kind"].AsString() != KindHTTP {
		t.Error("error kind attribute missing")
	}
	for _, s := range o.spans() {
		if strings.Contains(fmt.Sprint(s.Attributes(), s.Events(), s.Status(), s.Name()), secret) {
			t.Fatalf("span %q leaked an upstream error body", s.Name())
		}
	}
	for _, p := range o.metric("ignatius.model.failures") {
		if strings.Contains(fmt.Sprint(p.attrs), secret) {
			t.Error("metric attributes leaked an upstream error body")
		}
	}
}

func TestMetricsRecordedByTheLibrary(t *testing.T) {
	o := observe(t)
	ctx := WithRouteLabel(context.Background(), "tiered")
	plan := Plan{Mode: ModeCascade, Tiers: []Tier{{Model: "cheap", Threshold: &Threshold{Default: f(0.5)}}, {Model: "smart"}}}
	if _, err := Run(ctx, cascadeReg(), noulReq(), plan); err != nil {
		t.Fatal(err)
	}
	// a failure
	Run(ctx, Registry{"bad": &scripted{err: &CallError{Kind: KindTimeout, Message: "slow"}}}, noulReq(), Plan{Mode: ModeSingle, Model: "bad"})

	if _, n := total(o.metric("ignatius.model.call.duration"), map[string]string{"model": "cheap", "ok": "true"}); n != 1 {
		t.Errorf("duration for cheap: %d points", n)
	}
	if _, n := total(o.metric("ignatius.model.call.duration"), map[string]string{"model": "bad", "ok": "false"}); n != 1 {
		t.Error("a failed call is still timed, with ok=false")
	}
	if v, _ := total(o.metric("ignatius.model.failures"), map[string]string{"model": "bad", "kind": KindTimeout}); v != 1 {
		t.Errorf("failure counter = %v", v)
	}
	// the cascade escalated the one question at tier 0, under the route label from the context
	if v, _ := total(o.metric("ignatius.cascade.escalations"), map[string]string{"route": "tiered", "tier": "0", "model": "cheap"}); v != 1 {
		t.Errorf("escalations = %v", v)
	}
	if v, _ := total(o.metric("ignatius.cost"), map[string]string{"model": "cheap"}); diff(v, 1.0) > 1e-9 { // 10 tokens * $0.1
		t.Errorf("cost = %v, want 1.0", v)
	}
	if _, n := total(o.metric("ignatius.answer.confidence"), map[string]string{"model": "smart", "question_type": "noul"}); n != 1 {
		t.Error("confidence histogram missing")
	}
}

func TestCacheAndDisagreementMetrics(t *testing.T) {
	o := observe(t)
	ctx := WithRouteLabel(context.Background(), "vote")
	cached := &Cached{Inner: &scripted{}, C: NewCache(CacheConfig{}, nil), Alias: "m"}
	reg := Registry{"m": cached}
	for i := 0; i < 2; i++ {
		Run(ctx, reg, noulReq(), Plan{Mode: ModeSingle, Model: "m"})
	}
	if v, _ := total(o.metric("ignatius.cache.questions"), map[string]string{"model": "m"}); v != 1 {
		t.Errorf("cached questions = %v, want 1 (the second run)", v)
	}
	o.next()

	a := Answer{Type: "choice", Choice: "x", Probabilities: map[string]float64{"x": 1}}
	b := Answer{Type: "choice", Choice: "y", Probabilities: map[string]float64{"y": 1}}
	two := Registry{"a": &scripted{ans: map[string]Answer{"c": a}}, "b": &scripted{ans: map[string]Answer{"c": b}}}
	req := Request{State: "s", Questions: map[string]Question{"c": {Type: "choice"}}}
	Run(ctx, two, req, Plan{Mode: ModeFanOut, Models: []string{"a", "b"}})
	if v, _ := total(o.metric("ignatius.fanout.disagreement"), map[string]string{"route": "vote", "question_type": "choice"}); v != 1 {
		t.Errorf("disagreement = %v", v)
	}
	o.next()
	agree := Registry{"a": &scripted{ans: map[string]Answer{"c": a}}, "b": &scripted{ans: map[string]Answer{"c": a}}}
	Run(ctx, agree, req, Plan{Mode: ModeFanOut, Models: []string{"a", "b"}})
	if v, _ := total(o.metric("ignatius.fanout.disagreement"), nil); v != 0 {
		t.Errorf("agreement must not count, got %v", v)
	}
}

func TestDifferByQuestionType(t *testing.T) {
	n := func(p float64) Answer { return Answer{Type: "noul", Noul: f(p)} }
	s := func(v float64) Answer { return Answer{Type: "score", Score: f(v)} }
	c := func(v string) Answer { return Answer{Type: "choice", Choice: v} }
	for name, tc := range map[string]struct {
		typ  string
		as   []Answer
		want bool
	}{
		"choice differs":         {"choice", []Answer{c("a"), c("b")}, true},
		"choice agrees":          {"choice", []Answer{c("a"), c("a"), c("a")}, false},
		"noul across the line":   {"noul", []Answer{n(0.9), n(0.1)}, true},
		"noul same side":         {"noul", []Answer{n(0.9), n(0.6)}, false},
		"noul both no":           {"noul", []Answer{n(0.1), n(0.4)}, false},
		"score different levels": {"score", []Answer{s(1.2), s(2.6)}, true},
		"score same level":       {"score", []Answer{s(1.9), s(2.4)}, false},
	} {
		if got := differ(tc.typ, tc.as); got != tc.want {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
}

func TestUnlabeledRouteHasABoundedDefault(t *testing.T) {
	if routeLabel(context.Background()) != "unlabeled" || routeLabel(WithRouteLabel(context.Background(), "")) != "unlabeled" {
		t.Error("no label must fall back to a fixed value")
	}
}

func TestPropagateTraceIsOptIn(t *testing.T) {
	o := observe(t)
	_ = o
	var got []string
	srv := newHeaderSpy(&got)
	defer srv.Close()
	run := func(propagate bool) {
		reg := Registry{"m": &SystemOne{BaseURL: srv.URL, Model: "x", PropagateTrace: propagate}}
		ctx, span := otel.Tracer("test").Start(context.Background(), "caller")
		defer span.End()
		Run(ctx, reg, noulReq(), Plan{Mode: ModeSingle, Model: "m"})
	}
	run(false)
	run(true)
	if len(got) != 2 || got[0] != "" {
		t.Fatalf("default must send no traceparent, got %q", got)
	}
	if !strings.HasPrefix(got[1], "00-") || len(got[1]) != 55 {
		t.Errorf("opt-in must send a W3C traceparent, got %q", got[1])
	}
}

func TestCascadeTierFailureSpansCarryTheKindNeverTheMessage(t *testing.T) {
	o := observe(t)
	const secret = "SECRET-IN-A-TIER-ERROR"
	reg := Registry{
		"cheap": &scripted{err: &CallError{Kind: KindHTTP, Message: "503: " + secret, Status: 503}},
		"smart": &scripted{ans: map[string]Answer{"q": {Type: "noul", Noul: f(0.99)}}},
	}
	routed, _ := Run(context.Background(), reg, noulReq(), Plan{Mode: ModeCascade, Tiers: []Tier{{Model: "cheap"}, {Model: "smart"}}})
	if !routed.OK || routed.Trace[0].Error == nil {
		t.Fatal("setup: tier 0 should fail and the cascade should still answer")
	}
	tier0 := o.spansNamed("ignatius.cascade.tier")[0]
	if tier0.Status().Code != codes.Error || tier0.Status().Description != KindHTTP {
		t.Errorf("tier span status = %+v, want Error with the kind", tier0.Status())
	}
	for _, s := range o.spans() {
		if strings.Contains(fmt.Sprint(s.Attributes(), s.Events(), s.Status(), s.Name()), secret) {
			t.Fatalf("span %q leaked an upstream error body", s.Name())
		}
	}
}
