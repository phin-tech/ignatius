package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Like the library, the gateway uses only the OpenTelemetry API: a no-op unless
// the telemetry package (or an embedding app) installs providers.
//
// Privacy rule (SPEC 10.1): spans, metrics and logs carry structure, timings,
// config-defined names, counts and error kinds. Never state, question text,
// criteria, answer values or error messages. Metric attributes are bounded: the
// route label is a configured route or alias name, or the literals "inline",
// "plan" and "unknown", never client-supplied text.

const instrumentationName = "github.com/phin-tech/ignatius/go/gateway"

var gt = func() struct {
	meter       metric.Meter
	tracer      trace.Tracer
	requests    metric.Int64Counter
	duration    metric.Float64Histogram
	rateLimited metric.Int64Counter
} {
	m := otel.Meter(instrumentationName)
	var i struct {
		meter       metric.Meter
		tracer      trace.Tracer
		requests    metric.Int64Counter
		duration    metric.Float64Histogram
		rateLimited metric.Int64Counter
	}
	i.meter = m
	i.tracer = otel.Tracer(instrumentationName)
	i.requests, _ = m.Int64Counter("ignatius.requests", metric.WithDescription("Routing requests handled."))
	i.duration, _ = m.Float64Histogram("ignatius.request.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of routing requests."),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30))
	i.rateLimited, _ = m.Int64Counter("ignatius.rate_limited", metric.WithDescription("Requests rejected by a client's rate limit."))
	return i
}()

// reqInfo is filled in by a handler as it learns things, and read by the wrapper
// when the request finishes.
type reqInfo struct {
	requestID string
	client    string
	route     string // bounded label, see the privacy rule above
	mode      string
	ok        bool
	models    []string
	failures  []string // error kinds only
}

type infoKey struct{}

func infoFrom(ctx context.Context) *reqInfo {
	if v, _ := ctx.Value(infoKey{}).(*reqInfo); v != nil {
		return v
	}
	return &reqInfo{} // a handler called without the wrapper (tests): writes go nowhere
}

// statusWriter remembers the response status.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// routeLabel is the bounded label for a model/route string.
func (s *Server) routeLabel(model string) string {
	name, kind := s.router.Classify(model)
	switch kind {
	case "route", "profile", "alias": // all config-defined, so a bounded label
		return name
	case "inline":
		return "inline"
	}
	return "unknown"
}

// traced wraps a routing handler with a server span (continuing any incoming W3C
// traceparent), a request id, metrics and one structured log line.
func (s *Server) traced(layer string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only a caller we have authenticated may steer our trace. Otherwise anyone could
		// send "traceparent: ...-01" to force every request to be sampled whatever
		// traces_sample_ratio says, or to plant a trace id of their choosing in our logs.
		// An unrecognised caller gets a fresh root trace instead.
		ctx := r.Context()
		if c, _ := s.lookup(r); c != nil {
			ctx = propagation.TraceContext{}.Extract(ctx, propagation.HeaderCarrier(r.Header))
		}
		ctx, span := gt.tracer.Start(ctx, "ignatius.request", trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("ignatius.layer", layer)))
		defer span.End()

		info := &reqInfo{requestID: requestID(), client: "none", route: "unknown", mode: "none"}
		ctx = context.WithValue(ctx, infoKey{}, info)
		w.Header().Set("X-Request-Id", info.requestID)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		next(sw, r.WithContext(ctx))

		elapsed := time.Since(start)
		class := statusClass(sw.status)
		span.SetAttributes(
			attribute.String("ignatius.request_id", info.requestID), attribute.String("ignatius.client", info.client),
			attribute.String("ignatius.route", info.route), attribute.String("ignatius.mode", info.mode),
			attribute.Bool("ignatius.ok", info.ok), attribute.Int("http.response.status_code", sw.status))
		if sw.status >= 500 {
			span.SetStatus(codes.Error, class)
		}
		common := []attribute.KeyValue{attribute.String("layer", layer), attribute.String("route", info.route), attribute.String("mode", info.mode)}
		gt.requests.Add(ctx, 1, metric.WithAttributes(append(common,
			attribute.Bool("ok", info.ok), attribute.String("client", info.client), attribute.String("status_class", class))...))
		gt.duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(common...))
		s.logRequest(ctx, layer, info, sw.status, elapsed)
	}
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	}
	return "2xx"
}

// logRequest writes the one structured line per request. Fields are the same
// non-content set as the span, plus the trace id so a log line leads to its trace.
func (s *Server) logRequest(ctx context.Context, layer string, info *reqInfo, status int, elapsed time.Duration) {
	level := slog.LevelInfo
	if status >= 500 || (status < 400 && !info.ok) {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("request_id", info.requestID), slog.String("layer", layer), slog.String("client", info.client),
		slog.String("route", info.route), slog.String("mode", info.mode), slog.Bool("ok", info.ok),
		slog.Int("status", status), slog.Int64("duration_ms", elapsed.Milliseconds()),
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()))
	}
	if len(info.models) > 0 {
		attrs = append(attrs, slog.String("models", strings.Join(info.models, ",")))
	}
	if len(info.failures) > 0 {
		attrs = append(attrs, slog.String("failure_kinds", strings.Join(info.failures, ",")))
	}
	s.log.LogAttrs(ctx, level, "request", attrs...)
}

// noteRouted records what a finished run did, without any content.
func (info *reqInfo) noteRouted(routed ignatius.Routed) {
	info.mode, info.ok = routed.Mode, routed.OK
	if len(routed.Trace) > 0 { // a cascade's trace is the true call order
		for _, h := range routed.Trace {
			info.models = append(info.models, h.Model)
		}
	} else {
		for _, r := range routed.Results {
			info.models = append(info.models, r.Model)
		}
		for _, f := range routed.Failures {
			info.models = append(info.models, f.Model)
		}
	}
	for _, f := range routed.Failures {
		info.failures = append(info.failures, f.Error.Kind)
	}
}

// safeError strips what could echo request content out of an error before it is
// stored for display: an upstream HTTP error body, or a JSON decode message.
func safeError(e *ignatius.ErrorBody) *ignatius.ErrorBody {
	if e == nil {
		return nil
	}
	out := *e
	switch e.Kind {
	case ignatius.KindHTTP:
		status, _, _ := strings.Cut(e.Message, ":") // "503: <body snippet>"
		out.Message = "HTTP " + strings.TrimSpace(status)
	case ignatius.KindDecode:
		out.Message = "invalid response body"
	}
	return &out
}

// SetLogger sets the logger for the per-request log line (default slog.Default()).
// Call it before serving.
func (s *Server) SetLogger(l *slog.Logger) {
	s.log = l
	if s.writer != nil {
		s.writer.log = l
	}
	if s.auditor != nil {
		s.auditor.log = l
	}
	s.events.SetLogger(l)
}

// SetMetricsHandler serves GET /metrics (Prometheus text) behind admin auth. Call
// it before serving.
func (s *Server) SetMetricsHandler(h http.Handler) {
	if h == nil {
		return
	}
	s.mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.authorize(w, r)
		if !ok {
			return
		}
		if !c.admin {
			writeErr(w, http.StatusForbidden, "admin_required", "this key is valid but not an admin key", nil)
			return
		}
		h.ServeHTTP(w, r)
	})
}
