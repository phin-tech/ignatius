package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/phin-tech/ignatius/go/ignatius"
)

func f64(v float64) *float64 { return &v }

// setupWith builds the registry the way main does, so decorators are in play.
func setupWith(t *testing.T, models map[string]ignatius.ModelConfig, opts ignatius.Options, mut func(*Config)) *Server {
	t.Helper()
	cfg := Config{Models: models}
	if mut != nil {
		mut(&cfg)
	}
	cfg.applyDefaults()
	reg, err := ignatius.BuildRegistryWith(cfg.Models, func(string) string { return "" }, opts)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, reg, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mc(f *fake) ignatius.ModelConfig {
	return ignatius.ModelConfig{Provider: "systemone", BaseURL: f.URL, Model: "jev-latest"}
}

func snapshotOf(t *testing.T, s *Server) StatsSnapshot {
	t.Helper()
	var snap StatsSnapshot
	if err := json.Unmarshal(do(s, "GET", "/v1/stats", "", "").Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestSpendAndCascadeSavingsEstimate(t *testing.T) {
	cheap, smart := newFake(t, unsure), newFake(t, sure) // each call reports 5 input, 2 output tokens
	cm, sm := mc(cheap), mc(smart)
	cm.PriceInputPerMTok = f64(100_000)   // $0.10 per token
	sm.PriceInputPerMTok = f64(1_000_000) // $1 per token
	s := setupWith(t, map[string]ignatius.ModelConfig{"cheap": cm, "smart": sm}, ignatius.Options{}, nil)

	do(s, "POST", "/v1/systemone", l1("cascade:cheap@0.5>smart"), "") // cheap is unsure: escalates
	snap := snapshotOf(t, s)
	if diff(snap.TotalCost, 5.5) > 1e-9 { // 5 tokens*$0.1 + 5 tokens*$1
		t.Errorf("total cost = %v, want 5.5", snap.TotalCost)
	}
	cost := map[string]float64{}
	for _, m := range snap.Models {
		cost[m.ID] = m.CostUSD
	}
	if diff(cost["cheap"], 0.5) > 1e-9 || diff(cost["smart"], 5) > 1e-9 {
		t.Errorf("per-model spend wrong: %v", cost)
	}
	// Baseline = the first call's 5 tokens at the final tier's $1 = $5; actual $5.5.
	// Escalating everything cost MORE than going straight to the strong model.
	if snap.CascadeSavingsEstimate == nil || diff(*snap.CascadeSavingsEstimate, -0.5) > 1e-9 {
		t.Errorf("savings estimate = %v, want -0.5 (a cascade that escalates everything loses money)", snap.CascadeSavingsEstimate)
	}
	if snap.Recent[0].CostUSD == nil || diff(*snap.Recent[0].CostUSD, 5.5) > 1e-9 {
		t.Errorf("recent request cost = %v", snap.Recent[0].CostUSD)
	}

	// A cascade that stays on the cheap tier saves money against the strong tier.
	cheap.answers = sure
	do(s, "POST", "/v1/systemone", l1("cascade:cheap@0.5>smart"), "")
	snap = snapshotOf(t, s)
	if want := -0.5 + (5 - 0.5); diff(*snap.CascadeSavingsEstimate, want) > 1e-9 {
		t.Errorf("cumulative savings = %v, want %v", *snap.CascadeSavingsEstimate, want)
	}

	// No price on the final tier: no estimate, and no cost on unpriced results.
	s2 := setupWith(t, map[string]ignatius.ModelConfig{"cheap": cm, "smart": mc(smart)}, ignatius.Options{}, nil)
	do(s2, "POST", "/v1/systemone", l1("cascade:cheap@0.5>smart"), "")
	if got := snapshotOf(t, s2).CascadeSavingsEstimate; got != nil {
		t.Errorf("an unpriced final tier must give no estimate, got %v", *got)
	}
}

func TestCacheThroughTheGateway(t *testing.T) {
	up := newFake(t, sure)
	m := mc(up)
	m.PriceInputPerMTok = f64(1_000_000)
	s := setupWith(t, map[string]ignatius.ModelConfig{"a": m}, ignatius.Options{Cache: ignatius.CacheConfig{Enabled: true}}, nil)

	first := do(s, "POST", "/v1/systemone", l1("a"), "")
	second := do(s, "POST", "/v1/systemone", l1("a"), "")
	if up.posts != 1 {
		t.Fatalf("the repeat should be served from cache: upstream got %d posts", up.posts)
	}
	var r1, r2 struct {
		Answers map[string]any `json:"answers"`
		Usage   ignatius.Usage `json:"usage"`
	}
	json.Unmarshal(first.Body.Bytes(), &r1)
	json.Unmarshal(second.Body.Bytes(), &r2)
	if len(r2.Answers) != 1 || r2.Usage.InputTokens != 0 {
		t.Errorf("cached response: answers=%v usage=%+v", r2.Answers, r2.Usage)
	}
	snap := snapshotOf(t, s)
	if diff(snap.TotalCost, 5) > 1e-9 { // only the first call cost anything
		t.Errorf("a cached response must cost nothing: total %v", snap.TotalCost)
	}
	if snap.Models[0].Cached != 1 || snap.Recent[0].Cached != 1 {
		t.Errorf("cache hits not reported: model=%d recent=%d", snap.Models[0].Cached, snap.Recent[0].Cached)
	}
	// the cached latency is not a call: p50 must reflect only real calls
	do(s, "POST", "/v1/systemone", strings.Replace(l1("a"), `"?"`, `"a different question"`, 1), "")
	if up.posts != 2 {
		t.Errorf("a different question must miss: posts=%d", up.posts)
	}
}

func TestBreakerThroughTheGateway(t *testing.T) {
	sick, ok := newFake(t, sure), newFake(t, sure)
	sick.status = 503
	bad := mc(sick)
	bad.Breaker = &ignatius.BreakerConfig{FailureThreshold: 2}
	s := setupWith(t, map[string]ignatius.ModelConfig{"sick": bad, "ok": mc(ok)}, ignatius.Options{}, func(c *Config) {
		c.Routes = map[string]map[string]any{"safe": {"mode": "cascade", "tiers": []any{"sick", "ok"}}}
	})

	for i := 0; i < 2; i++ { // two real failures open the breaker; the cascade still answers from `ok`
		if rec := do(s, "POST", "/v1/systemone", l1("safe"), ""); rec.Code != 200 {
			t.Fatalf("cascade should fall through to ok: %d %s", rec.Code, rec.Body)
		}
	}
	if sick.posts != 2 {
		t.Fatalf("expected 2 real calls, got %d", sick.posts)
	}
	rec := do(s, "POST", "/v1/systemone", l1("safe"), "")
	if rec.Code != 200 || sick.posts != 2 {
		t.Errorf("an open breaker must not call the sick model: code=%d posts=%d", rec.Code, sick.posts)
	}
	recent := snapshotOf(t, s).Recent[0].Detail.Calls
	if len(recent) != 2 || recent[0].OK || recent[0].Error.Kind != ignatius.KindCircuitOpen || recent[1].Model != "ok" {
		t.Errorf("trace should show the skipped tier as circuit_open: %+v", recent)
	}
	if body := do(s, "GET", "/v1/models", "", "").Body.String(); !strings.Contains(body, `"id":"sick","status":"circuit_open"`) {
		t.Errorf("/v1/models should report circuit_open: %s", body)
	}
	// readiness: still ready (ok is up); down only when every model is unavailable
	if do(s, "GET", "/readyz", "", "").Code != 200 {
		t.Error("readyz should stay 200 while one model is up")
	}
	ok.ready = 503
	if do(s, "GET", "/readyz", "", "").Code != 503 {
		t.Error("readyz should be 503 when no model is available")
	}
}

func diff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

func TestSavingsEstimateSkipsRequestsWhereTierZeroDidNotRun(t *testing.T) {
	sick, smart := newFake(t, sure), newFake(t, sure)
	sick.status = 503
	cheap, sm := mc(sick), mc(smart)
	sm.PriceInputPerMTok = f64(1_000_000)
	cheap.Breaker = &ignatius.BreakerConfig{FailureThreshold: 1}
	s := setupWith(t, map[string]ignatius.ModelConfig{"sick": cheap, "smart": sm}, ignatius.Options{}, nil)

	// tier 0 fails (then its breaker is open): Results[0] is the strong tier, so there is no honest baseline
	for i := 0; i < 3; i++ {
		do(s, "POST", "/v1/systemone", l1("cascade:sick>smart"), "")
	}
	if got := snapshotOf(t, s).CascadeSavingsEstimate; got != nil {
		t.Errorf("no estimate when tier 0 never ran, got %v", *got)
	}

	// Tier 0 served entirely from its cache but still unsure, so it escalates to a strong
	// tier that is NOT cached. Tier 0's cached usage is 0, so a baseline built from it
	// would be a fake $0 and the request would register as a bogus loss.
	cheap2, smart2 := newFake(t, unsure), newFake(t, sure)
	c2, s2m := mc(cheap2), mc(smart2)
	c2.PriceInputPerMTok, s2m.PriceInputPerMTok = f64(100_000), f64(1_000_000)
	yes, no := true, false
	c2.Cache, s2m.Cache = &yes, &no
	s3 := setupWith(t, map[string]ignatius.ModelConfig{"cheap": c2, "smart": s2m}, ignatius.Options{}, nil)
	do(s3, "POST", "/v1/systemone", l1("cascade:cheap@0.5>smart"), "") // cold: counts (-0.5)
	first := *snapshotOf(t, s3).CascadeSavingsEstimate
	rec := do(s3, "POST", "/v1/systemone", l1("cascade:cheap@0.5>smart"), "") // tier 0 cached, tier 1 pays again
	if cheap2.posts != 1 || smart2.posts != 2 || rec.Code != 200 {
		t.Fatalf("setup: tier 0 should be cached and tier 1 not: cheap=%d smart=%d code=%d", cheap2.posts, smart2.posts, rec.Code)
	}
	if got := *snapshotOf(t, s3).CascadeSavingsEstimate; diff(got, first) > 1e-12 {
		t.Errorf("a request whose tier 0 was cached must not move the estimate: %v -> %v", first, got)
	}
}
