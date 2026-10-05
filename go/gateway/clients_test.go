package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
)

// setupClients builds a server with named clients over two fake upstreams.
func setupClients(t *testing.T, clients []ClientConfig, env map[string]string, mut func(*Config)) (*Server, *fake, *fake) {
	t.Helper()
	cheap, smart := newFake(t, unsure), newFake(t, sure)
	s := setup(t, map[string]*fake{"cheap": cheap, "smart": smart}, func(c *Config) {
		c.Clients = clients
		c.Routes = map[string]map[string]any{"c2": {"mode": "cascade", "tiers": []any{"cheap", "smart"}}}
		if mut != nil {
			mut(c)
		}
	}, env)
	return s, cheap, smart
}

func TestNamedClientsAuthenticateAndAreCounted(t *testing.T) {
	s, _, _ := setupClients(t, []ClientConfig{
		{Name: "billing", KeyEnv: "KB"}, {Name: "search", KeyEnv: "KS"},
	}, map[string]string{"KB": "key-b", "KS": "key-s", "IGNATIUS_API_KEY": "legacy"}, nil)
	for _, c := range []struct { // an ordered slice: the "oldest request" assertion below depends on it
		auth string
		want int
	}{{"Bearer key-b", 200}, {"Bearer key-s", 200}, {"Bearer legacy", 200}, {"Bearer nope", 401}, {"", 403}} {
		if got := do(s, "POST", "/v1/systemone", l1("cheap"), c.auth).Code; got != c.want {
			t.Errorf("auth %q: %d want %d", c.auth, got, c.want)
		}
	}
	snap := s.stats.Snapshot(s.statuses(httptest.NewRequest("GET", "/", nil).Context()))
	by := map[string]ClientSnapshot{}
	for _, c := range snap.Clients {
		by[c.Name] = c
	}
	if by["billing"].Requests != 1 || by["search"].Requests != 1 || by["default"].Requests != 1 {
		t.Errorf("per-client counts wrong: %+v", snap.Clients)
	}
	raw, _ := json.Marshal(snap)
	for _, secret := range []string{"key-b", "key-s", "legacy"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("stats leaked a key (%q): %s", secret, raw)
		}
	}
	if got := snap.Recent[len(snap.Recent)-1].Client; got != "billing" {
		t.Errorf("oldest recent request should be attributed to billing, got %q", got)
	}
}

func TestClientConfigValidation(t *testing.T) {
	bad := map[string]struct {
		clients []ClientConfig
		env     map[string]string
		want    string
	}{
		"no name":           {[]ClientConfig{{KeyEnv: "K"}}, map[string]string{"K": "x"}, "name is required"},
		"reserved name":     {[]ClientConfig{{Name: "default", KeyEnv: "K"}}, map[string]string{"K": "x"}, "reserved"},
		"duplicate name":    {[]ClientConfig{{Name: "a", KeyEnv: "K"}, {Name: "a", KeyEnv: "K2"}}, map[string]string{"K": "x", "K2": "y"}, "duplicated"},
		"no key_env":        {[]ClientConfig{{Name: "a"}}, nil, "key_env is required"},
		"unset env var":     {[]ClientConfig{{Name: "a", KeyEnv: "MISSING"}}, nil, "MISSING is not set"},
		"shared key":        {[]ClientConfig{{Name: "a", KeyEnv: "K"}, {Name: "b", KeyEnv: "K2"}}, map[string]string{"K": "same", "K2": "same"}, "share a key"},
		"clashes w/ legacy": {[]ClientConfig{{Name: "a", KeyEnv: "K"}}, map[string]string{"K": "same", "IGNATIUS_API_KEY": "same"}, "share a key"},
		"negative rate":     {[]ClientConfig{{Name: "a", KeyEnv: "K", RateLimitPerMinute: -1}}, map[string]string{"K": "x"}, "negative"},
		"allowlist typo":    {[]ClientConfig{{Name: "a", KeyEnv: "K", Routes: []string{"ghost"}}}, map[string]string{"K": "x"}, "ghost"},
	}
	for name, c := range bad {
		fk := newFake(t, sure)
		cfg := Config{Models: map[string]ignatius.ModelConfig{"m": {Provider: "systemone", BaseURL: fk.URL}}, Clients: c.clients}
		cfg.applyDefaults()
		reg, _ := ignatius.BuildRegistry(cfg.Models, func(string) string { return "" })
		_, err := New(cfg, reg, func(k string) string { return c.env[k] })
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}

func TestRateLimitTokenBucket(t *testing.T) {
	s, _, _ := setupClients(t, []ClientConfig{
		{Name: "slow", KeyEnv: "KS", RateLimitPerMinute: 60, Burst: 3}, // 1 token/s, burst 3
		{Name: "free", KeyEnv: "KF"},
	}, map[string]string{"KS": "ks", "KF": "kf"}, nil)
	clock := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	s.clients[0].bucket.last = clock // buckets were created against the real clock
	s.clients[0].bucket.tokens = 3
	post := func(auth string) *httptest.ResponseRecorder { return do(s, "POST", "/v1/systemone", l1("cheap"), auth) }

	for i := 0; i < 3; i++ {
		if got := post("Bearer ks").Code; got != 200 {
			t.Fatalf("burst request %d: %d", i, got)
		}
	}
	rec := post("Bearer ks")
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "rate_limited") || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("4th request should be limited with Retry-After 1: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	for i := 0; i < 5; i++ { // another client is unaffected
		if got := post("Bearer kf").Code; got != 200 {
			t.Fatalf("unlimited client was limited: %d", got)
		}
	}
	clock = clock.Add(1100 * time.Millisecond) // one token refilled
	if got := post("Bearer ks").Code; got != 200 {
		t.Errorf("after 1.1s a token should be available: %d", got)
	}
	if got := post("Bearer ks").Code; got != 429 {
		t.Errorf("and only one: %d", got)
	}
	clock = clock.Add(time.Hour) // refill is capped at the burst
	ok := 0
	for i := 0; i < 10 && post("Bearer ks").Code == 200; i++ {
		ok++
	}
	if ok != 3 {
		t.Errorf("tokens must cap at burst 3, got %d", ok)
	}
	var limited int
	for _, c := range s.stats.Snapshot(nil).Clients {
		if c.Name == "slow" {
			limited = c.RateLimited
		}
	}
	if limited < 3 {
		t.Errorf("rate_limited counter = %d, want at least 3", limited)
	}
	// dashboard polling is not rate limited
	for i := 0; i < 20; i++ {
		if do(s, "GET", "/v1/routes", "", "Bearer ks").Code != 200 {
			t.Fatal("discovery endpoints must not consume tokens")
		}
	}
}

func TestBucketMath(t *testing.T) {
	now := time.Now()
	b := newBucket(120, 0, now) // 2/s; default burst = rate/10 = 12
	if b.burst != 12 {
		t.Errorf("default burst = %v, want 12", b.burst)
	}
	if newBucket(5, 0, now).burst != 1 {
		t.Error("burst floors at 1")
	}
	for i := 0; i < 12; i++ {
		if ok, _ := b.take(now); !ok {
			t.Fatalf("token %d", i)
		}
	}
	ok, wait := b.take(now)
	if ok || wait < 490*time.Millisecond || wait > 510*time.Millisecond {
		t.Errorf("empty bucket at 2/s should say ~500ms, got ok=%v wait=%v", ok, wait)
	}
}

func TestRouteAllowlist(t *testing.T) {
	s, _, _ := setupClients(t, []ClientConfig{
		{Name: "locked", KeyEnv: "KL", Routes: []string{"c2"}},
		{Name: "inliner", KeyEnv: "KI", Routes: []string{"cheap", "inline"}},
		{Name: "open", KeyEnv: "KO"},
	}, map[string]string{"KL": "kl", "KI": "ki", "KO": "ko"}, func(c *Config) { c.DefaultRoute = "c2" })
	type tc struct {
		auth, model string
		want        int
	}
	for _, c := range []tc{
		{"kl", "c2", 200}, {"kl", "cheap", 403}, {"kl", "cascade:cheap>smart", 403}, {"kl", "nonexistent", 403},
		{"kl", "jev-latest", 200}, // default route is c2, which is allowed
		{"ki", "cheap", 200}, {"ki", "smart", 403}, {"ki", "c2", 403},
		// "inline" never reaches a model the client was not given: every alias inside must be on the list
		{"ki", "fan-out:cheap|vote", 200}, {"ki", "cascade:cheap>cheap", 200},
		{"ki", "fan-out:cheap,smart|mean", 403}, {"ki", "fan-out:smart|most_confident", 403},
		{"ki", "cascade:cheap>smart", 403}, {"ki", "fan-out:cheap,ghost", 403},
		{"ko", "c2", 200}, {"ko", "cascade:cheap>smart", 200}, {"ko", "nonexistent", 422},
	} {
		if got := do(s, "POST", "/v1/systemone", l1(c.model), "Bearer "+c.auth).Code; got != c.want {
			t.Errorf("%s -> %q: got %d want %d", c.auth, c.model, got, c.want)
		}
	}
	req := `"request":{"state":"x","questions":{"q":{"type":"noul","instructions":"?"}}}`
	for _, c := range []tc{
		{"kl", `{` + req + `,"route":"c2"}`, 200},
		{"kl", `{` + req + `,"route":"cheap"}`, 403},
		{"kl", `{` + req + `,"plan":{"mode":"single","model":"cheap"}}`, 403}, // a plan could name anything
		{"ko", `{` + req + `,"plan":{"mode":"single","model":"cheap"}}`, 200},
	} {
		if got := do(s, "POST", "/v1/route", c.model, "Bearer "+c.auth).Code; got != c.want {
			t.Errorf("L2 %s %s: got %d want %d", c.auth, c.model, got, c.want)
		}
	}
	for _, model := range []string{"nonexistent", "fan-out:cheap,ghost", "cascade:smart>ghost"} {
		for _, who := range []string{"kl", "ki"} {
			body := do(s, "POST", "/v1/systemone", l1(model), "Bearer "+who).Body.String()
			if strings.Contains(body, "smart") || strings.Contains(body, "c2") || strings.Contains(body, "ghost") {
				t.Errorf("%s asking for %q learned names from the error: %s", who, model, body)
			}
		}
	}
}

func TestRestrictedClientsOnlySeeWhatTheyMayUse(t *testing.T) {
	s, _, _ := setupClients(t, []ClientConfig{
		{Name: "locked", KeyEnv: "KL", Routes: []string{"c2"}},
		{Name: "cheap-only", KeyEnv: "KC", Routes: []string{"cheap", "inline"}},
		{Name: "open", KeyEnv: "KO"},
	}, map[string]string{"KL": "kl", "KC": "kc", "KO": "ko"}, func(c *Config) { c.DefaultRoute = "c2" })
	get := func(path, key string) string { return do(s, "GET", path, "", "Bearer "+key).Body.String() }

	if m := get("/v1/models", "kl"); strings.Contains(m, "cheap") || strings.Contains(m, "smart") {
		t.Errorf("locked client lists models it may not use: %s", m)
	}
	if m := get("/v1/models", "kc"); !strings.Contains(m, `"id":"cheap"`) || strings.Contains(m, "smart") || strings.Contains(m, `"default":"c2"`) {
		t.Errorf("cheap-only client should see exactly its alias and no default: %s", m)
	}
	if r := get("/v1/routes", "kl"); !strings.Contains(r, `"c2"`) || strings.Contains(r, "inline_grammar\":\"fan") {
		t.Errorf("locked client should see its route and no inline grammar: %s", r)
	}
	if r := get("/v1/routes", "kc"); strings.Contains(r, `"c2"`) || !strings.Contains(r, "fan-out") {
		t.Errorf("cheap-only client sees no named routes but may use inline: %s", r)
	}
	if m := get("/v1/models", "ko"); !strings.Contains(m, `"id":"cheap"`) || !strings.Contains(m, `"id":"smart"`) {
		t.Errorf("an unrestricted client sees everything: %s", m)
	}
}

func TestReadyzIsUnauthenticatedSoItNeverNamesModels(t *testing.T) {
	s, _, _ := setupClients(t, []ClientConfig{{Name: "x", KeyEnv: "KX"}}, map[string]string{"KX": "kx"}, nil)
	rec := do(s, "GET", "/readyz", "", "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "cheap") || strings.Contains(rec.Body.String(), "smart") {
		t.Errorf("readyz must not leak model names to unauthenticated callers: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"ready":2`) || !strings.Contains(rec.Body.String(), `"total":2`) {
		t.Errorf("readyz should report counts: %s", rec.Body)
	}
}

func TestStatsRequireAdmin(t *testing.T) {
	s, _, _ := setupClients(t, []ClientConfig{{Name: "plain", KeyEnv: "KP"}, {Name: "ops", KeyEnv: "KO", Admin: true}},
		map[string]string{"KP": "kp", "KO": "ko"}, nil)
	do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer kp")
	if rec := do(s, "GET", "/v1/stats", "", "Bearer kp"); rec.Code != 403 || !strings.Contains(rec.Body.String(), "admin_required") {
		t.Errorf("non-admin stats: %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "GET", "/v1/stats", "", "Bearer ko"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"client":"plain"`) {
		t.Errorf("admin stats: %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{"/v1/models", "/v1/routes"} { // discovery stays open to any valid client
		if do(s, "GET", p, "", "Bearer kp").Code != 200 {
			t.Errorf("%s should be open to any valid client", p)
		}
	}
}
