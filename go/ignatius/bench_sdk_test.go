package ignatius

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// discard is a span processor that keeps nothing: it measures the cost of creating
// and finishing recorded spans without any export.
type discard struct{}

func (discard) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (discard) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (discard) Shutdown(context.Context) error                  { return nil }
func (discard) ForceFlush(context.Context) error                { return nil }

// With IGNATIUS_BENCH_SDK=1 the benchmark runs with a real SDK that samples every
// span and records every metric, the worst case an application would see.
func init() {
	if os.Getenv("IGNATIUS_BENCH_SDK") != "1" {
		return
	}
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(discard{})))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader())))
}
