package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

	"github.com/phin-tech/ignatius/go/ignatius"
)

// One recording provider for the whole test binary (global providers delegate only
// once); each test reads what happened since it started.
var (
	obsOnce sync.Once
	recSpan *tracetest.SpanRecorder
	recRead *sdkmetric.ManualReader
)

type observer struct {
	t    *testing.T
	mark int
	rm   *metricdata.ResourceMetrics // collected once per window: delta temporality drains on Collect
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
	_ = recRead.Collect(context.Background(), &discard)
	return &observer{t: t, mark: len(recSpan.Ended())}
}

func (o *observer) next()                          { o.rm = nil }
func (o *observer) spans() []sdktrace.ReadOnlySpan { return recSpan.Ended()[o.mark:] }

func (o *observer) spanNamed(name string) sdktrace.ReadOnlySpan {
	for _, s := range o.spans() {
		if s.Name() == name {
			return s
		}
	}
	o.t.Fatalf("no span named %q among %d spans", name, len(o.spans()))
	return nil
}

type point struct {
	attrs map[string]string
	value float64
	count uint64
}

func (o *observer) collect() *metricdata.ResourceMetrics {
	if o.rm == nil {
		o.rm = &metricdata.ResourceMetrics{}
		if err := recRead.Collect(context.Background(), o.rm); err != nil {
			o.t.Fatal(err)
		}
	}
	return o.rm
}

func flat(s attribute.Set) map[string]string {
	m := map[string]string{}
	for _, kv := range s.ToSlice() {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

func (o *observer) metric(name string) []point {
	var out []point
	for _, sm := range o.collect().ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					out = append(out, point{flat(p.Attributes), float64(p.Value), 0})
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					out = append(out, point{flat(p.Attributes), p.Sum, p.Count})
				}
			}
		}
	}
	return out
}

// allTelemetryText is every string any span or metric carried: names, attribute
// keys and values, events, statuses. Used to assert something never appears.
func (o *observer) allTelemetryText() string {
	var b strings.Builder
	for _, s := range o.spans() {
		fmt.Fprint(&b, s.Name(), s.Attributes(), s.Status(), s.Resource())
		for _, e := range s.Events() {
			fmt.Fprint(&b, e.Name, e.Attributes)
		}
	}
	for _, sm := range o.collect().ScopeMetrics {
		for _, m := range sm.Metrics {
			fmt.Fprint(&b, m.Name, m.Description, m.Unit)
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					fmt.Fprint(&b, p.Attributes.ToSlice())
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					fmt.Fprint(&b, p.Attributes.ToSlice())
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					fmt.Fprint(&b, p.Attributes.ToSlice())
				}
			}
		}
	}
	return b.String()
}

func logTo(s *Server) *bytes.Buffer {
	var buf bytes.Buffer
	s.SetLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	return &buf
}

func postWith(s *Server, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

const (
	incomingTrace = "0af7651916cd43dd8448eb211c80319c"
	incomingSpan  = "b7ad6b7169203331"
	traceparent   = "00-" + incomingTrace + "-" + incomingSpan + "-01"
)

func TestRequestSpanContinuesTheCallersTrace(t *testing.T) {
	o := observe(t)
	cheap, smart := newFake(t, unsure), newFake(t, sure)
	s := setup(t, map[string]*fake{"cheap": cheap, "smart": smart}, nil, nil)

	rec := postWith(s, "/v1/systemone", l1("cascade:cheap@0.5>smart"), map[string]string{"traceparent": traceparent})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	req := o.spanNamed("ignatius.request")
	if req.SpanContext().TraceID().String() != incomingTrace || req.Parent().SpanID().String() != incomingSpan {
		t.Errorf("the request must continue the caller's trace: trace=%s parent=%s", req.SpanContext().TraceID(), req.Parent().SpanID())
	}
	run := o.spanNamed("ignatius.run")
	if run.Parent().SpanID() != req.SpanContext().SpanID() {
		t.Error("the library's run span must be a child of the request span")
	}
	a := map[string]attribute.Value{}
	for _, kv := range req.Attributes() {
		a[string(kv.Key)] = kv.Value
	}
	if a["ignatius.layer"].AsString() != "L1" || a["ignatius.route"].AsString() != "inline" || a["ignatius.mode"].AsString() != "cascade" ||
		!a["ignatius.ok"].AsBool() || a["http.response.status_code"].AsInt64() != 200 || a["ignatius.client"].AsString() != "anonymous" {
		t.Errorf("request span attrs: %v", a)
	}
	var body struct {
		Ignatius struct {
			RequestID string `json:"request_id"`
		} `json:"ignatius"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if a["ignatius.request_id"].AsString() == "" || a["ignatius.request_id"].AsString() != rec.Header().Get("X-Request-Id") ||
		body.Ignatius.RequestID != rec.Header().Get("X-Request-Id") {
		t.Errorf("one request id must tie the span, header and body together: span=%q header=%q body=%q",
			a["ignatius.request_id"].AsString(), rec.Header().Get("X-Request-Id"), body.Ignatius.RequestID)
	}
}

func TestErrorResponsesStillGetARequestIDAndAnErrorSpan(t *testing.T) {
	o := observe(t)
	sick := newFake(t, sure)
	sick.status = 503
	s := setup(t, map[string]*fake{"sick": sick}, nil, nil)
	rec := do(s, "POST", "/v1/systemone", l1("sick"), "") // upstream fails: the Jev contract makes this a 502
	if rec.Code != 502 || rec.Header().Get("X-Request-Id") == "" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if req := o.spanNamed("ignatius.request"); req.Status().Code != codes.Error {
		t.Errorf("a 5xx request span must be marked as an error, got %+v", req.Status())
	}
	if rec := do(s, "POST", "/v1/systemone", l1("nope"), ""); rec.Code != 422 || rec.Header().Get("X-Request-Id") == "" {
		t.Errorf("a 422 still carries a request id: %d %v", rec.Code, rec.Header())
	}
}

func TestRequestMetricsUseBoundedLabels(t *testing.T) {
	o := observe(t)
	cheap := newFake(t, sure)
	s := setupClients2(t, cheap)
	postWith(s, "/v1/systemone", l1("cheap"), map[string]string{"Authorization": "Bearer ka"})
	postWith(s, "/v1/systemone", l1("not-a-model"), map[string]string{"Authorization": "Bearer ka"}) // 422
	postWith(s, "/v1/systemone", l1("cheap"), map[string]string{"Authorization": "Bearer wrong"})    // 401
	for i := 0; i < 3; i++ {
		postWith(s, "/v1/systemone", l1("cheap"), map[string]string{"Authorization": "Bearer kb"}) // kb: burst of 1
	}

	reqs := o.metric("ignatius.requests")
	find := func(want map[string]string) float64 {
		var sum float64
	next:
		for _, p := range reqs {
			for k, v := range want {
				if p.attrs[k] != v {
					continue next
				}
			}
			sum += p.value
		}
		return sum
	}
	if find(map[string]string{"layer": "L1", "route": "cheap", "mode": "single", "ok": "true", "client": "a", "status_class": "2xx"}) != 1 {
		t.Errorf("successful request not counted with its labels: %+v", reqs)
	}
	if find(map[string]string{"route": "unknown", "client": "a", "status_class": "4xx", "ok": "false"}) != 1 {
		t.Error("an unknown model must be labeled route=unknown, never the client's text")
	}
	if find(map[string]string{"client": "none", "status_class": "4xx"}) != 1 {
		t.Error("a failed login has no client")
	}
	if find(map[string]string{"client": "b", "status_class": "4xx"}) != 2 { // 3 requests, burst of 1 => 2 limited
		t.Errorf("rate-limited requests should be counted: %+v", reqs)
	}
	var limited float64
	for _, p := range o.metric("ignatius.rate_limited") {
		if p.attrs["client"] == "b" {
			limited += p.value
		}
	}
	if limited != 2 {
		t.Errorf("rate_limited = %v, want 2", limited)
	}
	dur := o.metric("ignatius.request.duration")
	if len(dur) == 0 || dur[0].count == 0 {
		t.Error("request duration histogram is empty")
	}
}

// setupClients2: client a is unlimited, client b has a burst of exactly one.
func setupClients2(t *testing.T, f *fake) *Server {
	t.Helper()
	cfg := Config{Models: map[string]ignatius.ModelConfig{"cheap": {Provider: "systemone", BaseURL: f.URL}},
		Clients: []ClientConfig{{Name: "a", KeyEnv: "KA"}, {Name: "b", KeyEnv: "KB", RateLimitPerMinute: 1, Burst: 1}}}
	cfg.applyDefaults()
	env := func(k string) string { return map[string]string{"KA": "ka", "KB": "kb"}[k] }
	reg, _ := ignatius.BuildRegistry(cfg.Models, env)
	s, err := New(cfg, reg, env)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOneStructuredLogLinePerRequest(t *testing.T) {
	observe(t)
	up := newFake(t, sure)
	s := setup(t, map[string]*fake{"a": up}, nil, nil)
	buf := logTo(s)
	postWith(s, "/v1/systemone", l1("a"), map[string]string{"traceparent": traceparent})
	do(s, "POST", "/v1/systemone", l1("nope"), "")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one line per request, got %d: %s", len(lines), buf)
	}
	var first, second map[string]any
	json.Unmarshal([]byte(lines[0]), &first)
	json.Unmarshal([]byte(lines[1]), &second)
	if first["msg"] != "request" || first["route"] != "a" || first["mode"] != "single" || first["ok"] != true ||
		first["status"].(float64) != 200 || first["models"] != "a" || first["layer"] != "L1" || first["client"] != "anonymous" {
		t.Errorf("first line: %v", first)
	}
	if first["trace_id"] != incomingTrace {
		t.Errorf("the log line must carry the trace id so it leads to its trace, got %v", first["trace_id"])
	}
	if first["request_id"] == "" || first["request_id"] == second["request_id"] {
		t.Error("each request gets its own id")
	}
	if second["level"] != "INFO" || second["route"] != "unknown" || second["status"].(float64) != 422 {
		t.Errorf("a client mistake is INFO, labeled unknown: %v", second)
	}
}

func TestFailuresLogAtWarnWithKindsOnly(t *testing.T) {
	observe(t)
	const secret = "SECRET-IN-UPSTREAM-ERROR"
	sick := newFake(t, sure)
	sick.status, sick.body = 500, `{"error":"`+secret+`"}`
	s := setup(t, map[string]*fake{"sick": sick}, nil, nil)
	buf := logTo(s)
	do(s, "POST", "/v1/systemone", l1("sick"), "")
	var line map[string]any
	json.Unmarshal(buf.Bytes(), &line)
	if line["level"] != "WARN" || line["failure_kinds"] != "http" || strings.Contains(buf.String(), secret) {
		t.Errorf("a failed request logs at WARN with the error kind only: %s", buf)
	}
}

// The central guarantee (SPEC 10.1): nothing a caller sent, nothing a model
// answered, and no upstream error body ever reaches a span, a metric, a log line
// or the status page.
func TestNoRequestContentReachesAnyTelemetry(t *testing.T) {
	o := observe(t)
	const content = "SENTINEL-CONTENT-8f3a"
	const modelText = "SENTINEL-MODELSTR-77c1"
	good := newFake(t, `{"q":{"type":"choice","choice":"`+content+`","probabilities":{"`+content+`":0.9,"other":0.1}}}`)
	bad := newFake(t, sure)
	bad.status, bad.body = 500, `{"error":"echoing your request: `+content+`"}`
	s := setup(t, map[string]*fake{"good": good, "bad": bad}, func(c *Config) {
		c.Routes = map[string]map[string]any{"safe": {"mode": "cascade", "tiers": []any{"bad", "good"}}}
	}, nil)
	logs := logTo(s)

	body := func(model string) string {
		q := map[string]any{"q": map[string]any{"type": "choice", "instructions": "Is " + content + " the answer?", "criteria": []string{content, "other"}}}
		b, _ := json.Marshal(map[string]any{"state": map[string]any{"text": content, "n": 1}, "model": model, "questions": q})
		return string(b)
	}
	for _, model := range []string{"good", "safe", "fan-out:good,bad|vote", "cascade:bad>good", "bad",
		modelText, "fan-out:" + modelText + ",good", "cascade:good@0.5>" + modelText} {
		do(s, "POST", "/v1/systemone", body(model), "")
	}
	l2 := `{"request":{"state":"` + content + `","questions":{"q":{"type":"choice","instructions":"` + content + `","criteria":["` + content + `"]}}},"plan":{"mode":"fan_out","models":["good","bad"]}}`
	do(s, "POST", "/v1/route", l2, "")

	stats := do(s, "GET", "/v1/stats", "", "").Body.String()
	if !strings.Contains(stats, `"calls"`) {
		t.Fatal("setup: stats should be readable")
	}
	for name, haystack := range map[string]string{
		"spans and metrics": o.allTelemetryText(),
		"log lines":         logs.String(),
	} {
		for _, secret := range []string{content, modelText} {
			if strings.Contains(haystack, secret) {
				t.Errorf("%s leaked %q:\n%.600s", name, secret, haystack)
			}
		}
	}
	// the status page stores structure only. A model string is a route selector the
	// admin sees, but request content, answers and upstream error bodies never are.
	if strings.Contains(stats, content) {
		t.Errorf("stats leaked request content or an upstream error body: %.600s", stats)
	}
}

func TestRouteLabelCardinalityStaysBounded(t *testing.T) {
	o := observe(t)
	up := newFake(t, sure)
	s := setup(t, map[string]*fake{"a": up, "b": newFake(t, sure)}, func(c *Config) {
		c.Routes = map[string]map[string]any{"named": {"mode": "single", "model": "a"}}
	}, nil)
	for i := 0; i < 40; i++ { // a client sprays distinct strings
		do(s, "POST", "/v1/systemone", l1(fmt.Sprintf("garbage-%d", i)), "")
		do(s, "POST", "/v1/systemone", l1(fmt.Sprintf("cascade:a@0.%d>b", i%10)), "")
		do(s, "POST", "/v1/systemone", l1(fmt.Sprintf("fan-out:a,b|mean%d", i)), "")
	}
	do(s, "POST", "/v1/systemone", l1("named"), "")
	do(s, "POST", "/v1/systemone", l1("a"), "")

	allowed := map[string]bool{"named": true, "a": true, "b": true, "inline": true, "unknown": true, "plan": true}
	seen := map[string]bool{}
	for _, name := range []string{"ignatius.requests", "ignatius.request.duration", "ignatius.cascade.escalations", "ignatius.fanout.disagreement"} {
		for _, p := range o.metric(name) {
			if r, ok := p.attrs["route"]; ok {
				seen[r] = true
				if !allowed[r] {
					t.Errorf("%s has an unbounded route label %q", name, r)
				}
			}
		}
	}
	if !seen["inline"] || !seen["unknown"] || !seen["named"] {
		t.Errorf("expected inline, unknown and named routes among the labels, saw %v", seen)
	}
}

func TestTraceparentIsOnlyForwardedToModelsThatOptIn(t *testing.T) {
	o := observe(t)
	hosted, internal := newFake(t, sure), newFake(t, sure)
	hm, im := ignatius.ModelConfig{Provider: "systemone", BaseURL: hosted.URL}, ignatius.ModelConfig{Provider: "systemone", BaseURL: internal.URL, PropagateTrace: true}
	cfg := Config{Models: map[string]ignatius.ModelConfig{"hosted": hm, "internal": im}}
	cfg.applyDefaults()
	reg, _ := ignatius.BuildRegistry(cfg.Models, func(string) string { return "" })
	s, _ := New(cfg, reg, func(string) string { return "" })

	postWith(s, "/v1/systemone", l1("hosted"), map[string]string{"traceparent": traceparent})
	postWith(s, "/v1/systemone", l1("internal"), map[string]string{"traceparent": traceparent})
	if hosted.trace != "" {
		t.Errorf("a third-party hosted model must not receive our trace ids by default, got %q", hosted.trace)
	}
	if !strings.HasPrefix(internal.trace, "00-"+incomingTrace+"-") {
		t.Errorf("an opted-in model continues the same trace: %q", internal.trace)
	}
	// the span it received as parent is our client span for that call
	var callSpan string
	for _, sp := range o.spans() {
		if sp.Name() == "ignatius.model.call" && sp.Parent().IsValid() {
			for _, kv := range sp.Attributes() {
				if kv.Key == "ignatius.model" && kv.Value.AsString() == "internal" {
					callSpan = sp.SpanContext().SpanID().String()
				}
			}
		}
	}
	if callSpan == "" || !strings.Contains(internal.trace, callSpan) {
		t.Errorf("the forwarded parent should be our model-call span %q, got %q", callSpan, internal.trace)
	}
}

func TestStatsErrorMessagesDropUpstreamBodies(t *testing.T) {
	const secret = "SECRET-BODY-ECHO"
	bad := newFake(t, sure)
	bad.status, bad.body = 502, `{"detail":"`+secret+`"}`
	s := setup(t, map[string]*fake{"bad": bad}, nil, nil)
	rec := do(s, "POST", "/v1/systemone", l1("bad"), "")
	if !strings.Contains(rec.Body.String(), secret) {
		t.Fatal("setup: the caller's own 502 body keeps the detail, which is theirs to see")
	}
	stats := do(s, "GET", "/v1/stats", "", "").Body.String()
	if strings.Contains(stats, secret) || !strings.Contains(stats, "HTTP 502") {
		t.Errorf("stored errors must keep the kind and status only: %.400s", stats)
	}
	for kind, want := range map[string]string{ignatius.KindDecode: "invalid response body", ignatius.KindHTTP: "HTTP 404"} {
		got := safeError(&ignatius.ErrorBody{Kind: kind, Message: "404: " + secret})
		if got.Message != want || strings.Contains(got.Message, secret) {
			t.Errorf("safeError(%s) = %q, want %q", kind, got.Message, want)
		}
	}
	if safeError(&ignatius.ErrorBody{Kind: ignatius.KindTimeout, Message: "deadline exceeded"}).Message != "deadline exceeded" {
		t.Error("kinds with no content risk keep their message")
	}
	if safeError(nil) != nil {
		t.Error("nil stays nil")
	}
}

func TestMetricsEndpointIsAdminOnly(t *testing.T) {
	up := newFake(t, sure)
	s, _, _ := setupClients(t, []ClientConfig{{Name: "plain", KeyEnv: "KP"}, {Name: "ops", KeyEnv: "KO", Admin: true}},
		map[string]string{"KP": "kp", "KO": "ko"}, nil)
	_ = up
	s.SetMetricsHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ignatius_requests_total 1\n") }))
	for auth, want := range map[string]int{"": 403, "Bearer nope": 401, "Bearer kp": 403, "Bearer ko": 200} {
		if got := do(s, "GET", "/metrics", "", auth).Code; got != want {
			t.Errorf("GET /metrics with %q: %d want %d", auth, got, want)
		}
	}
	s2 := setup(t, map[string]*fake{"a": newFake(t, sure)}, nil, nil)
	s2.SetMetricsHandler(nil) // telemetry off: no /metrics route
	if do(s2, "GET", "/metrics", "", "").Code != 404 {
		t.Error("with no metrics handler there is no /metrics")
	}
}

func TestOnlyAuthenticatedCallersCanSteerTheTrace(t *testing.T) {
	o := observe(t)
	up := newFake(t, sure)
	s, _, _ := setupClients(t, []ClientConfig{{Name: "app", KeyEnv: "KA"}}, map[string]string{"KA": "ka"}, nil)
	_ = up
	buf := logTo(s)
	tp := map[string]string{"traceparent": traceparent}

	// no credentials, and a wrong key: both must get a fresh root, not the caller's trace
	for name, auth := range map[string]string{"no credentials": "", "wrong key": "Bearer nope"} {
		h := map[string]string{"traceparent": traceparent}
		if auth != "" {
			h["Authorization"] = auth
		}
		before := len(o.spans())
		rec := postWith(s, "/v1/systemone", l1("cheap"), h)
		if rec.Code != 403 && rec.Code != 401 {
			t.Fatalf("%s: %d", name, rec.Code)
		}
		req := o.spans()[before]
		if req.SpanContext().TraceID().String() == incomingTrace || req.Parent().IsValid() {
			t.Errorf("%s: an unauthenticated traceparent must be ignored (trace=%s parent=%v)", name, req.SpanContext().TraceID(), req.Parent())
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(line, incomingTrace) {
			t.Errorf("an unauthenticated caller planted its trace id in our logs: %s", line)
		}
	}

	// an authenticated caller does continue its trace
	tp["Authorization"] = "Bearer ka"
	before := len(o.spans())
	if rec := postWith(s, "/v1/systemone", l1("cheap"), tp); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var req sdktrace.ReadOnlySpan
	for _, sp := range o.spans()[before:] {
		if sp.Name() == "ignatius.request" {
			req = sp
		}
	}
	if req == nil || req.SpanContext().TraceID().String() != incomingTrace || req.Parent().SpanID().String() != incomingSpan {
		t.Error("an authenticated caller's traceparent must be continued")
	}
	if !strings.Contains(buf.String(), incomingTrace) {
		t.Error("and its trace id belongs in the log line")
	}
}
