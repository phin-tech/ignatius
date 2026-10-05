package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/phin-tech/ignatius/go/ignatius"
)

// fake is a Jev-compatible upstream returning a fixed answers JSON.
type fake struct {
	*httptest.Server
	answers string
	status  int
	ready   int
	gotAuth string
	gotHdr  string
	mu      sync.Mutex // guards the fields below against concurrent handlers
	posts   int        // upstream POSTs received (not readiness probes)
	trace   string     // the traceparent header of the last POST
	body    string     // if set, written instead of the normal answers JSON (e.g. an error body)
	seen    []string   // the request bodies received, newest last
}

func newFake(t testing.TB, answers string) *fake {
	t.Helper()
	f := &fake{answers: answers, status: 200, ready: 200}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(f.ready)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, string(raw))
		f.posts++
		f.trace = r.Header.Get("traceparent")
		f.gotAuth = r.Header.Get("Authorization")
		f.gotHdr = r.Header.Get("X-Api-Key")
		status, body, answers := f.status, f.body, f.answers
		f.mu.Unlock()
		w.WriteHeader(status)
		if body != "" {
			io.WriteString(w, body)
			return
		}
		io.WriteString(w, `{"model":"fake","answers":`+answers+`,"usage":{"input_tokens":5,"output_tokens":2}}`)
	}))
	t.Cleanup(f.Close)
	return f
}

const (
	sure   = `{"q":{"type":"noul","noul":0.95}}`
	unsure = `{"q":{"type":"noul","noul":0.55}}`
)

func setup(t *testing.T, models map[string]*fake, mut func(*Config), env map[string]string) *Server {
	t.Helper()
	cfg := Config{Models: map[string]ignatius.ModelConfig{}}
	for name, f := range models {
		cfg.Models[name] = ignatius.ModelConfig{Provider: "systemone", BaseURL: f.URL, Model: "jev-latest", APIKeyEnv: "K"}
	}
	if mut != nil {
		mut(&cfg)
	}
	cfg.applyDefaults()
	getenv := func(k string) string { return env[k] }
	reg, err := ignatius.BuildRegistry(cfg.Models, getenv)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, reg, getenv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func do(s *Server, method, path, body, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func l1(model string) string {
	return `{"state":"x","model":"` + model + `","questions":{"q":{"type":"noul","instructions":"?"}}}`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json %q: %v", rec.Body, err)
	}
	return m
}

func TestL1AliasRouteAndInline(t *testing.T) {
	cheap, smart := newFake(t, unsure), newFake(t, sure)
	s := setup(t, map[string]*fake{"cheap": cheap, "smart": smart}, func(c *Config) {
		c.Routes = map[string]map[string]any{"c2": {"mode": "cascade", "tiers": []any{"cheap", "smart"}}}
		c.DefaultRoute = "c2"
	}, map[string]string{"K": "up"})

	// plain alias = single
	rec := do(s, "POST", "/v1/systemone", l1("cheap"), "")
	m := decode(t, rec)
	if rec.Code != 200 || m["ignatius"].(map[string]any)["mode"] != "single" {
		t.Fatalf("alias: %d %s", rec.Code, rec.Body)
	}
	if cheap.gotAuth != "Bearer up" {
		t.Errorf("upstream auth = %q", cheap.gotAuth)
	}
	// noul answers carry no confidence field on the wire
	if _, has := m["answers"].(map[string]any)["q"].(map[string]any)["confidence"]; has {
		t.Errorf("noul answer must not carry confidence: %s", rec.Body)
	}
	// named cascade: cheap is unsure (0.55 -> 0.1), escalates to smart
	for _, model := range []string{"c2", "jev-latest", "", "cascade:cheap>smart"} {
		rec = do(s, "POST", "/v1/systemone", l1(model), "")
		m = decode(t, rec)
		src := m["ignatius"].(map[string]any)["sources"].(map[string]any)["q"].(map[string]any)
		if rec.Code != 200 || src["model"] != "smart" || len(m["ignatius"].(map[string]any)["trace"].([]any)) != 2 {
			t.Errorf("model %q: %d %s", model, rec.Code, rec.Body)
		}
	}
	// inline fan-out with a reducer
	rec = do(s, "POST", "/v1/systemone", l1("fan-out:cheap,smart|mean"), "")
	if m = decode(t, rec); rec.Code != 200 || m["ignatius"].(map[string]any)["mode"] != "fan_out" {
		t.Errorf("fan-out: %d %s", rec.Code, rec.Body)
	}
	if u := m["usage"].(map[string]any); u["input_tokens"].(float64) != 10 {
		t.Errorf("usage should sum across calls: %v", u)
	}
}

func TestL1Errors(t *testing.T) {
	a := newFake(t, sure)
	s := setup(t, map[string]*fake{"a": a}, func(c *Config) {
		c.Routes = map[string]map[string]any{"raw": {"mode": "fan_out", "models": []any{"a"}}}
	}, nil)
	cases := []struct {
		name, body string
		code       int
		contains   string
	}{
		{"unknown model", l1("nope"), 422, "unknown_model"},
		{"no default route", l1(""), 422, "default_route"},
		{"bad inline grammar", l1("cascade:a@x>a"), 422, "threshold"},
		{"fan-out without reducer", l1("raw"), 422, "reducer_required"},
		{"no questions", `{"state":"x","model":"a","questions":{}}`, 422, "invalid_request"},
		{"bad json", `nope`, 400, "invalid_json"},
	}
	for _, c := range cases {
		rec := do(s, "POST", "/v1/systemone", c.body, "")
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	// disabling inline routes
	off := false
	s2 := setup(t, map[string]*fake{"a": a}, func(c *Config) { c.AllowInlineRoutes = &off }, nil)
	if rec := do(s2, "POST", "/v1/systemone", l1("fan-out:a"), ""); rec.Code != 422 || strings.Contains(rec.Body.String(), "inline_grammar\":\"fan") {
		t.Errorf("inline off: %d %s", rec.Code, rec.Body)
	}
}

func TestL1UpstreamFailureIs502WithDetail(t *testing.T) {
	a := newFake(t, sure)
	a.status = 503
	s := setup(t, map[string]*fake{"a": a}, nil, nil)
	rec := do(s, "POST", "/v1/systemone", l1("a"), "")
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), `"kind":"http"`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestScoreLegendPassesThrough(t *testing.T) {
	a := newFake(t, `{"q":{"type":"score","score":1.2,"confidence":0.8,"legend":{"0":"low","1":"high"},"probabilities":{"0":0.4,"1":0.6}}}`)
	s := setup(t, map[string]*fake{"a": a}, nil, nil)
	body := `{"state":"x","model":"a","questions":{"q":{"type":"score","instructions":"?"}}}`
	rec := do(s, "POST", "/v1/systemone", body, "")
	if !strings.Contains(rec.Body.String(), `"legend":{"0":"low","1":"high"}`) {
		t.Errorf("legend lost: %s", rec.Body)
	}
}

func TestL2Route(t *testing.T) {
	a, b := newFake(t, unsure), newFake(t, sure)
	s := setup(t, map[string]*fake{"a": a, "b": b}, nil, nil)
	req := `"request":{"state":"x","questions":{"q":{"type":"noul","instructions":"?"}}}`

	rec := do(s, "POST", "/v1/route", `{`+req+`,"plan":{"mode":"fan_out","models":["a","b"]}}`, "")
	m := decode(t, rec)
	if rec.Code != 200 || m["ok"] != true || len(m["results"].([]any)) != 2 || len(m["answers"].(map[string]any)) != 0 || m["request_id"] == "" {
		t.Errorf("raw fan-out: %d %s", rec.Code, rec.Body)
	}
	rec = do(s, "POST", "/v1/route", `{`+req+`,"route":"cascade:a>b"}`, "")
	if m = decode(t, rec); rec.Code != 200 || len(m["trace"].([]any)) != 2 {
		t.Errorf("route string: %d %s", rec.Code, rec.Body)
	}
	for name, body := range map[string]string{
		"both":          `{` + req + `,"plan":{"mode":"single","model":"a"},"route":"a"}`,
		"neither":       `{` + req + `}`,
		"bad plan":      `{` + req + `,"plan":{"mode":"cascade"}}`,
		"unknown alias": `{` + req + `,"plan":{"mode":"single","model":"zz"}}`,
	} {
		if rec := do(s, "POST", "/v1/route", body, ""); rec.Code != 422 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// provider failures are data, not HTTP errors
	a.status = 500
	rec = do(s, "POST", "/v1/route", `{`+req+`,"plan":{"mode":"single","model":"a"}}`, "")
	if m = decode(t, rec); rec.Code != 200 || m["ok"] != false || len(m["failures"].([]any)) != 1 {
		t.Errorf("failure as data: %d %s", rec.Code, rec.Body)
	}
}

func TestAuthAndDiscovery(t *testing.T) {
	a := newFake(t, sure)
	s := setup(t, map[string]*fake{"a": a}, nil, map[string]string{"IGNATIUS_API_KEY": "k1,k2"})
	for auth, want := range map[string]int{"": 403, "Basic x": 403, "Bearer bad": 401, "Bearer k1": 200, "Bearer k2": 200} {
		if got := do(s, "POST", "/v1/systemone", l1("a"), auth).Code; got != want {
			t.Errorf("auth %q: %d want %d", auth, got, want)
		}
	}
	if do(s, "POST", "/v1/systemone", "garbage", "").Code != 403 {
		t.Error("auth must run before body validation")
	}
	if do(s, "GET", "/v1/models", "", "").Code != 403 || do(s, "GET", "/healthz", "", "").Code != 200 {
		t.Error("models needs auth, healthz does not")
	}
	rec := do(s, "GET", "/v1/models", "", "Bearer k1")
	if !strings.Contains(rec.Body.String(), `"id":"a","status":"ready"`) {
		t.Errorf("models: %s", rec.Body)
	}
	a.ready = 503
	if do(s, "GET", "/readyz", "", "").Code != 503 {
		t.Error("readyz should be 503 when every model is not ready")
	}
	a.ready = 404 // hosted APIs have no /readyz: unknown counts as available
	if do(s, "GET", "/readyz", "", "").Code != 200 {
		t.Error("unknown status should not fail readiness")
	}
}

func TestBodyLimit(t *testing.T) {
	a := newFake(t, sure)
	s := setup(t, map[string]*fake{"a": a}, func(c *Config) { c.MaxRequestBytes = 20 }, nil)
	if got := do(s, "POST", "/v1/systemone", l1("a"), "").Code; got != 413 {
		t.Errorf("got %d want 413", got)
	}
}

func TestRouteValidationAtStartup(t *testing.T) {
	a := newFake(t, sure)
	cfg := Config{Models: map[string]ignatius.ModelConfig{"a": {Provider: "systemone", BaseURL: a.URL}},
		Routes: map[string]map[string]any{"bad": {"mode": "cascade", "tiers": []any{"a", "ghost"}}}}
	cfg.applyDefaults()
	reg, _ := ignatius.BuildRegistry(cfg.Models, func(string) string { return "" })
	if _, err := New(cfg, reg, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("expected startup error naming ghost, got %v", err)
	}
	cfg.Routes = nil
	cfg.DefaultRoute = "ghost"
	if _, err := New(cfg, reg, func(string) string { return "" }); err == nil {
		t.Error("bad default_route should fail startup")
	}
}

func TestCustomAuthHeader(t *testing.T) {
	a := newFake(t, sure)
	cfg := Config{Models: map[string]ignatius.ModelConfig{"a": {Provider: "systemone", BaseURL: a.URL, APIKeyEnv: "K", AuthHeader: "X-Api-Key"}}}
	cfg.applyDefaults()
	env := func(string) string { return "secret" }
	reg, _ := ignatius.BuildRegistry(cfg.Models, env)
	s, _ := New(cfg, reg, func(string) string { return "" })
	do(s, "POST", "/v1/systemone", l1("a"), "")
	if a.gotHdr != "secret" || a.gotAuth != "" {
		t.Errorf("X-Api-Key=%q Authorization=%q", a.gotHdr, a.gotAuth)
	}
}

func TestLoadConfigTOML(t *testing.T) {
	path := t.TempDir() + "/ignatius.toml"
	doc := `
listen = "127.0.0.1:9999"
api_key_env = "MY_KEY"
default_route = "smart"
allow_inline_routes = false

[models.cheap]
provider = "systemone"
base_url = "http://cheap:8000"
model = "jev-latest"
api_key_env = "CHEAP_KEY"
auth_header = "X-Api-Key"
timeout_ms = 3000

[models.big]
provider = "systemone"
base_url = "http://big:8000"
price_input_per_mtok = 0.042
price_output_per_mtok = 0.0
cache = false

[models.big.breaker]
enabled = false

[models.cheap.breaker]
failure_threshold = 9
cooldown_ms = 1234

[cache]
enabled = true
ttl_ms = 60000
max_entries = 500

[[clients]]
name = "billing"
key_env = "BILLING_KEY"
rate_limit_per_minute = 600
burst = 25
admin = true
routes = ["smart", "inline"]

[routes.smart]
mode = "cascade"
threshold = 1
tiers = [ { model = "cheap", threshold = { noul = 0.5, choice = 0.9 } }, "big" ]

[routes.vote]
mode = "fan_out"
models = ["cheap", "big"]
reduce = "vote"
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9999" || cfg.APIKeyEnv != "MY_KEY" || cfg.allowInline() || cfg.MaxInlineModels != 5 {
		t.Errorf("scalars/defaults wrong: %+v", cfg)
	}
	if m := cfg.Models["cheap"]; m.BaseURL != "http://cheap:8000" || m.AuthHeader != "X-Api-Key" || m.TimeoutMS != 3000 || m.APIKeyEnv != "CHEAP_KEY" {
		t.Errorf("cheap = %+v", m)
	}
	plans, err := cfg.plans()
	if err != nil {
		t.Fatal(err)
	}
	smart := plans["smart"]
	if smart.Mode != "cascade" || len(smart.Tiers) != 2 || smart.Tiers[1].Model != "big" ||
		smart.Tiers[0].Threshold.ByType["choice"] != 0.9 || *smart.Threshold.Default != 1 {
		t.Errorf("mixed-type route parsed wrong: %+v", smart)
	}
	if v := plans["vote"]; v.Reduce != "vote" || len(v.Models) != 2 {
		t.Errorf("vote = %+v", v)
	}
	// the resilience, cost and access-control keys decode into the right types
	big, cheapM := cfg.Models["big"], cfg.Models["cheap"]
	if big.PriceInputPerMTok == nil || *big.PriceInputPerMTok != 0.042 || big.PriceOutputPerMTok == nil || *big.PriceOutputPerMTok != 0 {
		t.Errorf("prices not decoded (a zero output price must stay set, not unset): %+v", big)
	}
	if big.Cache == nil || *big.Cache || big.Breaker == nil || big.Breaker.Enabled == nil || *big.Breaker.Enabled {
		t.Errorf("cache=false and breaker enabled=false not decoded: cache=%v breaker=%+v", big.Cache, big.Breaker)
	}
	if cheapM.Cache != nil || cheapM.Breaker == nil || cheapM.Breaker.FailureThreshold != 9 || cheapM.Breaker.CooldownMS != 1234 || cheapM.Breaker.Enabled != nil {
		t.Errorf("breaker tuning not decoded: %+v", cheapM)
	}
	if !cfg.Cache.Enabled || cfg.Cache.TTLMS != 60000 || cfg.Cache.MaxEntries != 500 {
		t.Errorf("[cache] not decoded: %+v", cfg.Cache)
	}
	if len(cfg.Clients) != 1 || cfg.Clients[0].Name != "billing" || cfg.Clients[0].RateLimitPerMinute != 600 ||
		cfg.Clients[0].Burst != 25 || !cfg.Clients[0].Admin || len(cfg.Clients[0].Routes) != 2 {
		t.Errorf("[[clients]] not decoded: %+v", cfg.Clients)
	}
	env := func(k string) string { return map[string]string{"BILLING_KEY": "bk"}[k] }
	reg, err := ignatius.BuildRegistryWith(cfg.Models, env, ignatius.Options{Cache: cfg.Cache})
	if err != nil {
		t.Fatal(err)
	}
	peel := func(b ignatius.Backend) ignatius.Backend { // the outermost wrapper is the limits one
		if l, ok := b.(ignatius.Limited); ok {
			return l.Inner
		}
		return b
	}
	if _, ok := peel(reg["cheap"]).(*ignatius.Cached); !ok { // global cache on, no per-model override
		t.Errorf("cheap should be cached, got %T", reg["cheap"])
	}
	if _, ok := peel(reg["big"]).(*ignatius.Priced); !ok { // cache=false and breaker disabled leave pricing outermost
		t.Errorf("big should be the bare Priced backend, got %T", reg["big"])
	}
	if _, err := New(cfg, reg, env); err != nil {
		t.Errorf("config should validate and start: %v", err)
	}
	if _, err := LoadConfig(t.TempDir() + "/missing.toml"); err == nil {
		t.Error("missing file should error")
	}
}

func TestDashboardServedAtRootOnly(t *testing.T) {
	a := newFake(t, sure)
	s := setup(t, map[string]*fake{"a": a}, nil, map[string]string{"IGNATIUS_API_KEY": "k"})
	rec := do(s, "GET", "/", "", "") // the page itself needs no key; its data does
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") ||
		!strings.Contains(rec.Body.String(), "Ignatius") {
		t.Fatalf("dashboard: %d %v", rec.Code, rec.Header())
	}
	if strings.Contains(rec.Body.String(), "<script src") || strings.Contains(rec.Body.String(), "href=") {
		t.Error("dashboard must not load external resources")
	}
	if do(s, "GET", "/nope", "", "").Code != 404 {
		t.Error("only exactly / serves the dashboard")
	}
	if do(s, "GET", "/v1/stats", "", "").Code != 403 {
		t.Error("stats must require auth")
	}
}

func TestStatsRecordMetadataOnly(t *testing.T) {
	good, bad := newFake(t, sure), newFake(t, sure)
	bad.status = 500
	s := setup(t, map[string]*fake{"good": good, "bad": bad}, nil, nil)

	body := `{"state":"SECRET-STATE-TEXT","model":"%s","questions":{"q":{"type":"noul","instructions":"SECRET-QUESTION"}}}`
	for _, model := range []string{"good", "good", "bad", "cascade:bad>good"} {
		do(s, "POST", "/v1/systemone", strings.Replace(body, "%s", model, 1), "")
	}
	raw := do(s, "GET", "/v1/stats", "", "").Body.String()
	if strings.Contains(raw, "SECRET") {
		t.Errorf("stats leaked request content: %s", raw)
	}
	var st struct {
		Models []ModelSnapshot `json:"models"`
		Recent []RecentRequest `json:"recent"`
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	by := map[string]ModelSnapshot{}
	for _, m := range st.Models {
		by[m.ID] = m
	}
	if by["good"].Calls != 3 || by["bad"].Calls != 2 || by["bad"].Failures["http"] != 2 || by["good"].Status != "ready" {
		t.Errorf("model counters wrong: %+v", by)
	}
	if len(st.Recent) != 4 || st.Recent[0].Route != "cascade:bad>good" || st.Recent[0].Mode != "cascade" ||
		strings.Join(st.Recent[0].Models, ",") != "bad,good" || !st.Recent[0].OK || st.Recent[0].Failures != 1 {
		t.Errorf("recent[0] (newest first) wrong: %+v", st.Recent)
	}
	if st.Recent[1].OK || st.Recent[1].Route != "bad" { // the failed single call
		t.Errorf("recent[1] = %+v", st.Recent[1])
	}
}

func TestStatsBounded(t *testing.T) {
	st := NewStats()
	long := strings.Repeat("x", 500)
	for i := 0; i < recentWindow+25; i++ {
		st.Record("L1", long, "id", "c", ignatius.Routed{Mode: "single", OK: true,
			Results: []ignatius.Result{{Model: "m", LatencyMS: int64(i)}}}, 0, nil)
	}
	snap := st.Snapshot([]modelStatus{{ID: "m", Status: "ready"}})
	models, recent := snap.Models, snap.Recent
	if len(recent) != recentWindow || len(recent[0].Route) > maxRouteLen+4 {
		t.Errorf("recent len %d, route len %d", len(recent), len(recent[0].Route))
	}
	if models[0].Calls != recentWindow+25 {
		t.Errorf("calls = %d", models[0].Calls)
	}
	if len(st.models["m"].latencies) != latencyWindow && len(st.models["m"].latencies) != recentWindow+25 {
		t.Errorf("latency window = %d", len(st.models["m"].latencies))
	}
}

func TestPercentile(t *testing.T) {
	xs := []int64{50, 10, 40, 20, 30, 60, 70, 80, 90, 100}
	if got := percentile(xs, 50); got != 50 {
		t.Errorf("p50 = %d", got)
	}
	if got := percentile(xs, 95); got != 100 {
		t.Errorf("p95 = %d", got)
	}
	if percentile(nil, 50) != 0 || percentile([]int64{7}, 95) != 7 {
		t.Error("edge cases")
	}
}

func statsOf(t *testing.T, s *Server) (recent []RecentRequest) {
	t.Helper()
	var st struct {
		Recent []RecentRequest `json:"recent"`
	}
	if err := json.Unmarshal(do(s, "GET", "/v1/stats", "", "").Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st.Recent
}

func TestStatsDetailCascadeExplainsEscalation(t *testing.T) {
	cheap, smart := newFake(t, unsure), newFake(t, sure) // unsure: noul 0.55 -> confidence 0.1
	s := setup(t, map[string]*fake{"cheap": cheap, "smart": smart}, nil, nil)
	do(s, "POST", "/v1/systemone", l1("cascade:cheap@0.5>smart"), "")

	d := statsOf(t, s)[0].Detail
	if len(d.Calls) != 2 || d.Calls[0].Tier == nil || *d.Calls[0].Tier != 0 || d.Calls[1].Model != "smart" {
		t.Fatalf("calls = %+v", d.Calls)
	}
	q := d.Calls[0].Questions[0]
	if q.ID != "q" || q.Confidence == nil || *q.Confidence > 0.11 || q.Threshold == nil || *q.Threshold != 0.5 || !q.Escalated {
		t.Errorf("tier 0 should record confidence 0.1 < threshold 0.5 -> escalated: %+v", q)
	}
	last := d.Calls[1].Questions[0]
	if last.Threshold != nil || last.Escalated || last.Confidence == nil {
		t.Errorf("last tier has no threshold and never escalates: %+v", last)
	}
}

func TestStatsDetailFanOutAgreementAndFailure(t *testing.T) {
	a := newFake(t, `{"q":{"type":"choice","choice":"SECRETLABEL-A","probabilities":{"SECRETLABEL-A":0.9,"SECRETLABEL-B":0.1}}}`)
	b := newFake(t, `{"q":{"type":"choice","choice":"SECRETLABEL-B","probabilities":{"SECRETLABEL-A":0.2,"SECRETLABEL-B":0.8}}}`)
	broken := newFake(t, sure)
	broken.status = 500
	s := setup(t, map[string]*fake{"a": a, "b": b, "broken": broken}, nil, nil)
	body := `{"state":"x","model":"fan-out:a,b,broken|vote","questions":{"q":{"type":"choice","instructions":"?","criteria":["SECRETLABEL-A","SECRETLABEL-B"]}}}`
	do(s, "POST", "/v1/systemone", body, "")

	raw := do(s, "GET", "/v1/stats", "", "").Body.String()
	if strings.Contains(raw, "SECRETLABEL") {
		t.Errorf("answer values must never be stored: %s", raw)
	}
	d := statsOf(t, s)[0].Detail
	if len(d.Calls) != 3 || len(d.Agreement) != 1 || d.Agreement[0].Models != 2 || d.Agreement[0].Distinct != 2 {
		t.Fatalf("detail = %+v", d)
	}
	var failed *CallDetail
	for i := range d.Calls {
		if !d.Calls[i].OK {
			failed = &d.Calls[i]
		}
	}
	if failed == nil || failed.Model != "broken" || failed.Error == nil || failed.Error.Kind != "http" {
		t.Errorf("failed call detail = %+v", failed)
	}
}

// The example configs in deploy/ are documentation people copy: they must keep loading and must
// build into a working registry, or they rot unnoticed.
func TestExampleConfigsLoad(t *testing.T) {
	files, err := filepath.Glob("../../deploy/*/ignatius.toml")
	if err != nil || len(files) < 2 {
		t.Fatalf("expected the dunce-union and clef examples: %v %v", files, err)
	}
	env := map[string]string{"CLOUDFLARE_ACCOUNT_ID": "acc", "CLOUDFLARE_API_TOKEN": "tok", "TYPESAFE_API_KEY": "k", "DUNCE_API_KEY": "d", "IGNATIUS_API_KEY": "i"}
	getenv := func(k string) string { return env[k] }
	for _, f := range files {
		cfg, err := LoadConfig(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		reg, err := ignatius.BuildRegistry(cfg.Models, getenv)
		if err != nil {
			t.Errorf("%s: models: %v", f, err)
			continue
		}
		if _, err := New(cfg, reg, getenv); err != nil {
			t.Errorf("%s: server: %v", f, err)
		}
	}
}
