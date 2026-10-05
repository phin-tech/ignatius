// Package telemetry installs the OpenTelemetry SDK for the gateway: OTLP export
// of traces and metrics over HTTP/protobuf, and a Prometheus scrape handler.
//
// It is the only place the SDK is imported. The ignatius library and the gateway
// use only the OpenTelemetry API, so an embedding application can install its own
// providers instead, and nothing here runs unless Setup is called with something
// to export to (SPEC 10.1).
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Config is the [telemetry] table. Everything is off unless OTLPEndpoint is set
// or Prometheus is true.
//
// Environment variables: the endpoint and protocol come only from this table
// (OTEL_EXPORTER_OTLP_ENDPOINT is ignored). Exporter settings this table leaves
// unset still come from the standard variables, for example request headers from
// OTEL_EXPORTER_OTLP_HEADERS. OTEL_RESOURCE_ATTRIBUTES adds resource attributes.
type Config struct {
	ServiceName string `toml:"service_name"`
	// OTLPEndpoint is the collector's base URL, e.g. http://otel-collector:4318.
	// /v1/traces and /v1/metrics are appended. Empty disables OTLP export.
	OTLPEndpoint string `toml:"otlp_endpoint"`
	// OTLPProtocol must be empty or "http/protobuf". gRPC is not supported yet.
	OTLPProtocol string `toml:"otlp_protocol"`
	// OTLPHeadersEnv names an environment variable holding "k=v,k2=v2" request
	// headers (typically an auth token). The secret never lives in the config file.
	OTLPHeadersEnv string `toml:"otlp_headers_env"`
	// TracesSampleRatio is the fraction of new traces kept (default 1.0). Traces
	// that continue a sampled parent are always kept.
	TracesSampleRatio *float64 `toml:"traces_sample_ratio"`
	// Prometheus serves the same metrics at GET /metrics.
	Prometheus bool `toml:"prometheus"`
	// MetricsIntervalMS is the OTLP metric push interval (default 10000).
	MetricsIntervalMS int `toml:"metrics_interval_ms"`
}

// Enabled reports whether anything would be exported.
func (c Config) Enabled() bool { return c.OTLPEndpoint != "" || c.Prometheus }

// Validate checks the config without touching the network.
func (c Config) Validate() error {
	if c.OTLPProtocol != "" && c.OTLPProtocol != "http/protobuf" {
		return fmt.Errorf("telemetry: otlp_protocol %q is not supported (only http/protobuf)", c.OTLPProtocol)
	}
	if c.OTLPEndpoint != "" {
		u, err := url.Parse(c.OTLPEndpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("telemetry: otlp_endpoint %q must be an http(s) URL", c.OTLPEndpoint)
		}
	}
	if r := c.TracesSampleRatio; r != nil && (*r < 0 || *r > 1) {
		return fmt.Errorf("telemetry: traces_sample_ratio must be between 0 and 1")
	}
	if c.MetricsIntervalMS < 0 {
		return fmt.Errorf("telemetry: metrics_interval_ms cannot be negative")
	}
	return nil
}

// Provider is what Setup returns.
type Provider struct {
	// TracerProvider and MeterProvider are the installed SDK providers (nil when
	// nothing is configured). They are also set as the OpenTelemetry globals, but
	// the globals bind once per process, so tests and embedders should use these.
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	// MetricsHandler serves Prometheus text; nil unless Config.Prometheus.
	MetricsHandler http.Handler
	// Shutdown flushes and stops the exporters. Always safe to call.
	Shutdown func(context.Context) error
}

// Setup installs global tracer and meter providers when something is configured.
// With nothing configured it installs nothing and the instrumentation is a no-op.
func Setup(ctx context.Context, cfg Config, getenv func(string) string, version string) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	noop := &Provider{Shutdown: func(context.Context) error { return nil }}
	if !cfg.Enabled() {
		return noop, nil
	}
	name := cfg.ServiceName
	if name == "" {
		name = "ignatius"
	}
	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithAttributes(
		attribute.String("service.name", name), attribute.String("service.version", version)))
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}
	headers := parseHeaders(getenv(cfg.OTLPHeadersEnv))
	base := strings.TrimRight(cfg.OTLPEndpoint, "/")

	var shutdowns []func(context.Context) error
	out := &Provider{}
	out.Shutdown = func(ctx context.Context) error {
		var errs []error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			errs = append(errs, shutdowns[i](ctx))
		}
		return errors.Join(errs...)
	}

	if base != "" {
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(base + "/v1/traces")}
		if len(headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(headers))
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: trace exporter: %w", err)
		}
		ratio := 1.0
		if cfg.TracesSampleRatio != nil {
			ratio = *cfg.TracesSampleRatio
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		)
		otel.SetTracerProvider(tp)
		out.TracerProvider = tp
		shutdowns = append(shutdowns, tp.Shutdown)
	}

	var readers []sdkmetric.Option
	if base != "" {
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(base + "/v1/metrics")}
		if len(headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(headers))
		}
		exp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: metric exporter: %w", err)
		}
		interval := 10 * time.Second
		if cfg.MetricsIntervalMS > 0 {
			interval = time.Duration(cfg.MetricsIntervalMS) * time.Millisecond
		}
		readers = append(readers, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))))
	}
	if cfg.Prometheus {
		reg := prometheus.NewRegistry()
		pe, err := promexporter.New(promexporter.WithRegisterer(reg), promexporter.WithoutScopeInfo())
		if err != nil {
			return nil, fmt.Errorf("telemetry: prometheus exporter: %w", err)
		}
		readers = append(readers, sdkmetric.WithReader(pe))
		out.MetricsHandler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	}
	mp := sdkmetric.NewMeterProvider(append(readers, sdkmetric.WithResource(res))...)
	otel.SetMeterProvider(mp)
	out.MeterProvider = mp
	shutdowns = append(shutdowns, mp.Shutdown)
	return out, nil
}

// parseHeaders reads the OTLP "k=v,k2=v2" header format.
func parseHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok && k != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
