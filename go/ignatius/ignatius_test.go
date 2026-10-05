package ignatius

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

func noulReq() Request {
	return Request{State: "x", Questions: map[string]Question{"q": {Type: "noul", Instructions: "?"}}}
}

func TestSystemOneRequestShapeAndNormalization(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != "POST" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{"model":"decis/x","answers":{"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":7,"output_tokens":3},"decis":{"ignored":true}}`)
	}))
	defer srv.Close()

	reg := Registry{"jeff": &SystemOne{BaseURL: srv.URL + "/", Model: "jev-latest", APIKey: "k"}}
	routed, err := Run(context.Background(), reg, noulReq(), Plan{Mode: ModeSingle, Model: "jeff"})
	if err != nil || !routed.OK {
		t.Fatalf("err=%v routed=%+v", err, routed)
	}
	if gotAuth != "Bearer k" || gotBody["model"] != "jev-latest" || gotBody["state"] != "x" {
		t.Errorf("auth=%q body=%v", gotAuth, gotBody)
	}
	a := routed.Answers["q"]
	if a.Model != "jeff" || a.Confidence == nil || *a.Confidence < 0.7999 || *a.Confidence > 0.8001 {
		t.Errorf("answer = %+v", a)
	}
	if routed.Results[0].Usage.InputTokens != 7 {
		t.Errorf("usage = %+v", routed.Results[0].Usage)
	}
}

func TestSystemOneFailureKinds(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", 503)
	}))
	defer bad.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "not json") }))
	defer garbage.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	reg := Registry{
		"http":      &SystemOne{BaseURL: bad.URL},
		"decode":    &SystemOne{BaseURL: garbage.URL},
		"timeout":   &SystemOne{BaseURL: slow.URL, Timeout: 50 * time.Millisecond},
		"transport": &SystemOne{BaseURL: dead.URL},
		"openai":    unsupported{"openai"},
	}
	for alias, want := range map[string]string{
		"http": KindHTTP, "decode": KindDecode, "timeout": KindTimeout, "transport": KindTransport, "openai": KindUnsupported,
	} {
		r, err := Run(context.Background(), reg, noulReq(), Plan{Mode: ModeSingle, Model: alias})
		if err != nil || r.OK || len(r.Failures) != 1 || r.Failures[0].Error.Kind != want {
			t.Errorf("%s: want kind %s, got %+v (err %v)", alias, want, r.Failures, err)
		}
	}
}

func TestPlanValidation(t *testing.T) {
	reg := Registry{"a": unsupported{}, "b": unsupported{}}
	bad := []Plan{
		{Mode: "nope"},
		{Mode: ModeSingle},
		{Mode: ModeSingle, Model: "zzz"},
		{Mode: ModeFanOut},
		{Mode: ModeFanOut, Models: []string{"a", "a"}},
		{Mode: ModeFanOut, Models: []string{"a"}, Reduce: "median"},
		{Mode: ModeCascade},
		{Mode: ModeCascade, Tiers: []Tier{{Model: "a"}, {Model: "zzz"}}},
	}
	for i, p := range bad {
		if _, err := Run(context.Background(), reg, noulReq(), p); err == nil {
			t.Errorf("case %d (%+v): expected PlanError", i, p)
		} else if _, ok := err.(*PlanError); !ok {
			t.Errorf("case %d: error type %T", i, err)
		}
	}
}

func TestPlanJSONShapes(t *testing.T) {
	var p Plan
	in := `{"mode":"cascade","threshold":0.7,"tiers":["a",{"model":"b","threshold":{"noul":0.5}},{"model":"c","threshold":0.9}]}`
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatal(err)
	}
	if p.Tiers[0].Model != "a" || p.Tiers[1].Threshold.ByType["noul"] != 0.5 || *p.Tiers[2].Threshold.Default != 0.9 || *p.Threshold.Default != 0.7 {
		t.Errorf("parsed plan = %+v", p)
	}
	// per-type map falls back to the cascade-level threshold for missing types
	if got := p.Tiers[1].bar("choice", p); got != 0.7 {
		t.Errorf("fallback bar = %v want 0.7", got)
	}
	if got := (Tier{Model: "x"}).bar("noul", Plan{}); got != defaultThreshold {
		t.Errorf("default bar = %v", got)
	}
}

func TestEscalateIfOverridesConfidence(t *testing.T) {
	sure := mockBackend{Answers: map[string]Answer{"q": {Type: "noul", Noul: f(0.99)}}}
	other := mockBackend{Answers: map[string]Answer{"q": {Type: "noul", Noul: f(0.4)}}}
	reg := Registry{"a": sure, "b": other}
	plan := Plan{Mode: ModeCascade, Tiers: []Tier{{Model: "a"}, {Model: "b"}}}
	r, _ := Run(context.Background(), reg, noulReq(), plan, WithEscalateIf(func(string, Answer) bool { return true }))
	if len(r.Trace) != 2 || r.Answers["q"].Model != "a" { // a is still more confident, so best-seen keeps it
		t.Errorf("trace=%+v answers=%+v", r.Trace, r.Answers)
	}
}

func TestCascadeStopsWhenEverythingSettled(t *testing.T) {
	reg := Registry{
		"a": mockBackend{Answers: map[string]Answer{"q": {Type: "noul", Noul: f(0.99)}}},
		"b": mockBackend{Error: &struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		}{"http", "must not be called"}},
	}
	r, _ := Run(context.Background(), reg, noulReq(), Plan{Mode: ModeCascade, Tiers: []Tier{{Model: "a"}, {Model: "b"}}})
	if len(r.Trace) != 1 || len(r.Failures) != 0 || !r.OK {
		t.Errorf("expected one hop, no failures: %+v", r)
	}
}

func TestVoteTieBreaksOnProbability(t *testing.T) {
	mk := func(choice string, pa, pb float64) Result {
		return Result{Answers: map[string]Answer{"c": {Type: "choice", Choice: choice, Model: "m", Probabilities: map[string]float64{"a": pa, "b": pb}}}}
	}
	qs := map[string]Question{"c": {Type: "choice"}}
	got := reduce("vote", qs, []Result{mk("a", 0.55, 0.45), mk("b", 0.1, 0.9)})
	if got["c"].Choice != "b" { // 1 vote each; b's summed probability is higher
		t.Errorf("choice = %q", got["c"].Choice)
	}
	got = reduce("mean", qs, []Result{mk("a", 0.9, 0.1), mk("b", 0.4, 0.6), mk("b", 0.4, 0.6)})
	if got["c"].Choice != "a" { // mean a = 0.567 beats mean b = 0.433 despite 2 votes for b
		t.Errorf("mean choice = %q", got["c"].Choice)
	}
}

func TestPlanTimeoutBoundsTheWholeRun(t *testing.T) {
	reg := Registry{"a": mockBackend{LatencyMS: 500}}
	start := time.Now()
	r, _ := Run(context.Background(), reg, noulReq(), Plan{Mode: ModeSingle, Model: "a", TimeoutMS: 40})
	if time.Since(start) > 300*time.Millisecond || len(r.Failures) != 1 || r.Failures[0].Error.Kind != KindTimeout {
		t.Errorf("took %v, failures %+v", time.Since(start), r.Failures)
	}
}

func TestParseInline(t *testing.T) {
	p, err := ParseInline("fan-out:jeff,jev|mean", 5)
	if err != nil || p.Mode != ModeFanOut || len(p.Models) != 2 || p.Reduce != "mean" {
		t.Errorf("fan-out: %+v %v", p, err)
	}
	if p, _ := ParseInline("fan_out:a,b", 5); p.Reduce != "vote" {
		t.Errorf("default reducer = %q", p.Reduce)
	}
	p, err = ParseInline("cascade:jeff@0.5>clef-flash>jev", 5)
	if err != nil || len(p.Tiers) != 3 || *p.Tiers[0].Threshold.Default != 0.5 || p.Tiers[1].Model != "clef-flash" || p.Tiers[1].Threshold != nil {
		t.Errorf("cascade: %+v %v", p, err)
	}
	if _, err := ParseInline("jeff-gemma", 5); err != ErrNotInline {
		t.Errorf("plain alias should be ErrNotInline, got %v", err)
	}
	for _, bad := range []string{"fan-out:", "fan-out:a,,b", "cascade:a@x>b", "cascade:a@2>b", "vote:a,b", "fan-out:a,b,c,d,e,f"} {
		if _, err := ParseInline(bad, 5); err == nil || err == ErrNotInline {
			t.Errorf("%q: expected a grammar error, got %v", bad, err)
		}
	}
}

func TestRequestValidate(t *testing.T) {
	if (Request{}).Validate() == nil {
		t.Error("empty request should be invalid")
	}
	if (Request{Questions: map[string]Question{"q": {Type: "rating"}}}).Validate() == nil {
		t.Error("bad type should be invalid")
	}
	if err := noulReq().Validate(); err != nil {
		t.Error(err)
	}
}
