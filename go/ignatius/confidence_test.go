package ignatius

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fp(v float64) *float64 { return &v }

func TestDerivedConfidenceFormulas(t *testing.T) {
	cases := []struct {
		name string
		a    Answer
		want *float64 // nil: the provider's number (or the 1.1 fallback) stands
	}{
		// choice: (p_max - 1/K) / (1 - 1/K)
		{"two options, a coin flip", Answer{Type: "choice", Probabilities: map[string]float64{"a": 0.5, "b": 0.5}}, fp(0)},
		{"two options, certain", Answer{Type: "choice", Probabilities: map[string]float64{"a": 1, "b": 0}}, fp(1)},
		{"two options, 0.6", Answer{Type: "choice", Probabilities: map[string]float64{"a": 0.6, "b": 0.4}}, fp(0.2)},
		{"three options", Answer{Type: "choice", Probabilities: map[string]float64{"a": 0.7, "b": 0.2, "c": 0.1}}, fp(0.55)},
		{"the same 0.7 over more options is more confident", Answer{Type: "choice", Probabilities: map[string]float64{"a": 0.7, "b": 0.1, "c": 0.1, "d": 0.1}}, fp((0.7 - 0.25) / 0.75)},
		{"below a uniform guess clamps to zero", Answer{Type: "choice", Probabilities: map[string]float64{"a": 0.2, "b": 0.2, "c": 0.2}}, fp(0)},
		{"a single option", Answer{Type: "choice", Probabilities: map[string]float64{"a": 1}}, fp(1)},
		// score: max(0, 1 - sum p_i|i-m| / MAD_uniform)
		{"score, a spread distribution", Answer{Type: "score", Probabilities: map[string]float64{"0": 0.1, "1": 0.2, "2": 0.6, "3": 0.1}}, fp(0.5)},
		{"score, certain", Answer{Type: "score", Probabilities: map[string]float64{"0": 0, "1": 0, "2": 1, "3": 0}}, fp(1)},
		{"score, uniform", Answer{Type: "score", Probabilities: map[string]float64{"0": 0.25, "1": 0.25, "2": 0.25, "3": 0.25}}, nil},
		{"score, a single level", Answer{Type: "score", Probabilities: map[string]float64{"0": 1}}, fp(1)},
		{"score levels that are not indices", Answer{Type: "score", Probabilities: map[string]float64{"low": 0.5, "high": 0.5}}, nil},
		// nothing to derive from
		{"no probabilities", Answer{Type: "choice", Confidence: fp(0.9)}, nil},
		{"noul is derived elsewhere", Answer{Type: "noul", Noul: fp(0.9)}, nil},
	}
	for _, c := range cases {
		got := derivedConfidence(c.a)
		switch {
		case c.name == "score, uniform": // the uniform score is worth pinning exactly: it is 0
			if got == nil || math.Abs(*got) > 1e-12 {
				t.Errorf("%s: got %v, want 0", c.name, got)
			}
		case c.want == nil:
			if got != nil {
				t.Errorf("%s: got %v, want nil", c.name, *got)
			}
		case got == nil || math.Abs(*got-*c.want) > 1e-12:
			t.Errorf("%s: got %v, want %v", c.name, got, *c.want)
		}
	}
}

// The score result must not depend on Go's random map order.
func TestDerivedScoreIsDeterministic(t *testing.T) {
	a := Answer{Type: "score", Probabilities: map[string]float64{"0": 0.4, "1": 0.4, "2": 0.2}} // a tie for the mode
	first := *derivedConfidence(a)
	for range 50 {
		if got := *derivedConfidence(a); got != first {
			t.Fatalf("got %v then %v", first, got)
		}
	}
}

func TestPolicyKeepsTheProvidersNumberAsNative(t *testing.T) {
	inner := mockBackend{Answers: map[string]Answer{
		"c": {Type: "choice", Choice: "a", Probabilities: map[string]float64{"a": 0.6, "b": 0.4}, Confidence: fp(0.99)},
		"n": {Type: "noul", Noul: fp(0.9)},
		"x": {Type: "choice", Choice: "a", Confidence: fp(0.7)}, // no probabilities: the provider's number stands
	}}
	w, err := withConfidence{Inner: inner, Policy: ConfidenceDerived}.Call(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	c := w.Answers["c"]
	if math.Abs(*c.Confidence-0.2) > 1e-12 || c.NativeConfidence == nil || *c.NativeConfidence != 0.99 {
		t.Errorf("derived: %+v", c)
	}
	if n := w.Answers["n"]; n.NativeConfidence != nil || n.Confidence != nil {
		t.Errorf("noul is left to normalization: %+v", n)
	}
	if x := w.Answers["x"]; *x.Confidence != 0.7 || x.NativeConfidence != nil {
		t.Errorf("no probabilities: %+v", x)
	}
	if *inner.Answers["c"].Confidence != 0.99 {
		t.Error("the backend's own answers must not be mutated")
	}
	// The default policy changes nothing.
	w, _ = withConfidence{Inner: inner, Policy: ConfidenceProvider}.Call(context.Background(), Request{})
	if *w.Answers["c"].Confidence != 0.99 || w.Answers["c"].NativeConfidence != nil {
		t.Errorf("provider policy: %+v", w.Answers["c"])
	}
}

// Through the real registry and an HTTP provider: the same wire answer is judged differently by
// the two policies, and an unknown policy is a startup error.
func TestRegistryAppliesTheConfiguredPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"m","answers":{"q":{"type":"choice","choice":"a","probabilities":{"a":0.6,"b":0.4},"confidence":0.99}},"usage":{}}`))
	}))
	defer srv.Close()
	reg, err := BuildRegistry(map[string]ModelConfig{
		"trust":   {Provider: "systemone", BaseURL: srv.URL, Model: "m"},
		"derive":  {Provider: "systemone", BaseURL: srv.URL, Model: "m", Confidence: "derived"},
		"explict": {Provider: "systemone", BaseURL: srv.URL, Model: "m", Confidence: "provider"},
	}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	req := Request{State: "x", Questions: map[string]Question{"q": {Type: "choice", Instructions: "?", Criteria: []string{"a", "b"}}}}
	conf := func(alias string) (float64, *float64) {
		routed, err := Run(context.Background(), reg, req, Plan{Mode: ModeSingle, Model: alias})
		if err != nil || !routed.OK {
			t.Fatalf("%s: %v %+v", alias, err, routed)
		}
		a := routed.Answers["q"]
		return *a.Confidence, a.NativeConfidence
	}
	if c, n := conf("trust"); c != 0.99 || n != nil {
		t.Errorf("provider policy: %v %v", c, n)
	}
	if c, n := conf("explict"); c != 0.99 || n != nil {
		t.Errorf("explicit provider policy: %v %v", c, n)
	}
	c, n := conf("derive")
	if math.Abs(c-0.2) > 1e-12 || n == nil || *n != 0.99 {
		t.Errorf("derived policy: %v %v", c, n)
	}
	if _, err := BuildRegistry(map[string]ModelConfig{"x": {Provider: "systemone", BaseURL: srv.URL, Confidence: "vibes"}},
		func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), `"x"`) || !strings.Contains(err.Error(), "derived") {
		t.Errorf("an unknown policy must be a startup error naming the model: %v", err)
	}
	// native_confidence is on the L2 wire only when the policy replaced a number.
	routed, _ := Run(context.Background(), reg, req, Plan{Mode: ModeSingle, Model: "derive"})
	b, _ := json.Marshal(routed.Answers["q"])
	if !strings.Contains(string(b), `"native_confidence":0.99`) {
		t.Errorf("json: %s", b)
	}
	routed, _ = Run(context.Background(), reg, req, Plan{Mode: ModeSingle, Model: "trust"})
	if b, _ = json.Marshal(routed.Answers["q"]); strings.Contains(string(b), "native_confidence") {
		t.Errorf("json: %s", b)
	}
}
