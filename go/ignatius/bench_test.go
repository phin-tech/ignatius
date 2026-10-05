package ignatius

import (
	"context"
	"testing"
)

// BenchmarkRun measures the orchestration overhead of Run over an instant backend,
// so any instrumentation cost shows up directly. See bench_sdk_test.go for running
// it with a real OpenTelemetry SDK installed.
//
//	go test ./ignatius -run xxx -bench Run -benchmem
//	IGNATIUS_BENCH_SDK=1 go test ./ignatius -run xxx -bench Run -benchmem
func BenchmarkRun(b *testing.B) {
	three := Request{State: "x", Questions: map[string]Question{
		"a": {Type: "noul", Instructions: "?"}, "b": {Type: "noul", Instructions: "?"}, "c": {Type: "noul", Instructions: "?"}}}
	reg := Registry{
		"cheap": &scripted{ans: map[string]Answer{"a": {Type: "noul", Noul: f(0.55)}, "b": {Type: "noul", Noul: f(0.99)}, "c": {Type: "noul", Noul: f(0.55)}}},
		"smart": &scripted{ans: map[string]Answer{"a": {Type: "noul", Noul: f(0.99)}, "b": {Type: "noul", Noul: f(0.99)}, "c": {Type: "noul", Noul: f(0.99)}}},
	}
	for name, plan := range map[string]Plan{
		"single":  {Mode: ModeSingle, Model: "smart"},
		"fan_out": {Mode: ModeFanOut, Models: []string{"cheap", "smart"}, Reduce: "vote"},
		"cascade": {Mode: ModeCascade, Tiers: []Tier{{Model: "cheap", Threshold: &Threshold{Default: f(0.5)}}, {Model: "smart"}}},
	} {
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := Run(ctx, reg, three, plan); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
