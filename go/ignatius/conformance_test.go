package ignatius

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// mockBackend replays a fixture's raw wire answers so normalization is covered.
type mockBackend struct {
	LatencyMS int   `json:"latency_ms"`
	Usage     Usage `json:"usage"`
	Price     *struct {
		In  float64 `json:"input_per_mtok"`
		Out float64 `json:"output_per_mtok"`
	} `json:"price"`
	Error *struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"error"`
	// Limits is the model's [limits] table (SPEC 3.1); absent means the default: no images.
	Limits *LimitsConfig `json:"limits"`
	// Confidence is the model's confidence policy (SPEC 1.2); empty means "provider".
	Confidence string            `json:"confidence"`
	Answers    map[string]Answer `json:"answers"`
}

func (m mockBackend) Call(ctx context.Context, _ Request) (WireResponse, error) {
	select {
	case <-time.After(time.Duration(m.LatencyMS) * time.Millisecond):
	case <-ctx.Done():
		return WireResponse{}, ctx.Err()
	}
	if m.Error != nil {
		return WireResponse{}, &CallError{Kind: m.Error.Kind, Message: m.Error.Message}
	}
	return WireResponse{Model: "mock", Answers: m.Answers, Usage: m.Usage}, nil
}

type fixture struct {
	Name    string                 `json:"name"`
	Mode    string                 `json:"mode"`
	Args    json.RawMessage        `json:"args"`
	Request Request                `json:"request"`
	Mock    map[string]mockBackend `json:"mock"`
	Expect  any                    `json:"expect"`
}

func TestConformance(t *testing.T) {
	files, err := filepath.Glob("../../spec/fixtures/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		var fx fixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Run(filepath.Base(f), func(t *testing.T) {
			reg := Registry{}
			for alias, m := range fx.Mock {
				var b Backend = m
				if m.Confidence != "" { // a policy goes through the real decorator
					b = withConfidence{Inner: b, Policy: m.Confidence}
				}
				if m.Price != nil { // priced mocks go through the real Priced decorator
					b = &Priced{Inner: b, InPerMTok: m.Price.In, OutPerMTok: m.Price.Out}
				}
				var limits LimitsConfig
				if m.Limits != nil {
					limits = *m.Limits
				}
				reg[alias] = Limited{Inner: b, Limits: limits} // every configured model is wrapped, as in BuildRegistry
			}
			plan := Plan{Mode: fx.Mode}
			if err := json.Unmarshal(fx.Args, &plan); err != nil {
				t.Fatalf("args: %v", err)
			}
			routed, err := Run(context.Background(), reg, fx.Request, plan)
			if err != nil {
				t.Fatalf("%s: run: %v", fx.Name, err)
			}
			b, _ := json.Marshal(routed)
			var got any
			_ = json.Unmarshal(b, &got)
			subset(t, "$", fx.Expect, got)
		})
	}
}

// subset asserts want is contained in got: objects by key, arrays by length and
// element, numbers to 1e-9.
func subset(t *testing.T, path string, want, got any) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: want object, got %T (%v)", path, got, got)
			return
		}
		for k, wv := range w {
			gv, present := g[k]
			if !present {
				t.Errorf("%s.%s: missing", path, k)
				continue
			}
			subset(t, path+"."+k, wv, gv)
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			t.Errorf("%s: want array len %d, got %v", path, len(w), got)
			return
		}
		for i := range w {
			subset(t, path+"["+string(rune('0'+i))+"]", w[i], g[i])
		}
	case float64:
		g, ok := got.(float64)
		if !ok || math.Abs(g-w) > 1e-9 {
			t.Errorf("%s: want %v, got %v", path, w, got)
		}
	default:
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s: want %v, got %v", path, want, got)
		}
	}
}
