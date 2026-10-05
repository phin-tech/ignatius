package ignatius

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for breaker and cache tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// scripted is a backend whose next result is controlled by the test.
type scripted struct {
	mu    sync.Mutex
	calls int
	got   []Request
	err   error
	ans   map[string]Answer
	usage Usage
	block chan struct{} // if set, Call waits for it (to hold a probe in flight)
}

func (s *scripted) Call(ctx context.Context, req Request) (WireResponse, error) {
	s.mu.Lock()
	s.calls++
	s.got = append(s.got, req)
	err, ans, usage, block := s.err, s.ans, s.usage, s.block
	s.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		return WireResponse{}, err
	}
	out := map[string]Answer{}
	for id := range req.Questions {
		if a, ok := ans[id]; ok {
			out[id] = a
		} else {
			out[id] = Answer{Type: "noul", Noul: f(0.9)}
		}
	}
	return WireResponse{Answers: out, Usage: usage}, nil
}

func (s *scripted) set(err error) { s.mu.Lock(); s.err = err; s.mu.Unlock() }
func (s *scripted) n() int        { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

func httpErr(status int) error { return &CallError{Kind: KindHTTP, Message: "x", Status: status} }
func callOnce(b Backend) error { _, err := b.Call(context.Background(), noulReq()); return err }

func isOpen(err error) bool {
	var ce *CallError
	return errors.As(err, &ce) && ce.Kind == KindCircuitOpen
}

func TestBreakerOpensAfterConsecutiveFailuresAndSkipsTheNetwork(t *testing.T) {
	clk, inner := newClock(), &scripted{err: httpErr(503)}
	b := NewBreaker(inner, BreakerConfig{FailureThreshold: 3, CooldownMS: 10000}, clk.Now)
	for i := 0; i < 3; i++ {
		if err := callOnce(b); err == nil || isOpen(err) {
			t.Fatalf("call %d should reach the backend and fail, got %v", i, err)
		}
	}
	if b.State() != "open" {
		t.Fatalf("state = %s, want open", b.State())
	}
	before := inner.n()
	if err := callOnce(b); !isOpen(err) || inner.n() != before {
		t.Errorf("open breaker must reject instantly without calling: err=%v calls %d->%d", err, before, inner.n())
	}
}

func TestBreakerCountsOnlySickServerSignals(t *testing.T) {
	clk, inner := newClock(), &scripted{}
	b := NewBreaker(inner, BreakerConfig{FailureThreshold: 3}, clk.Now)
	run := func(err error, n int) {
		inner.set(err)
		for i := 0; i < n; i++ {
			callOnce(b)
		}
	}
	run(httpErr(400), 10) // caller mistakes: the server is alive
	run(httpErr(404), 10)
	run(&CallError{Kind: KindUnsupported, Message: "x"}, 10)
	run(context.Canceled, 10) // the caller gave up
	if b.State() != "closed" {
		t.Fatalf("non-countable errors opened the breaker")
	}
	run(httpErr(503), 2)
	run(nil, 1) // a success resets the run of failures
	run(httpErr(500), 2)
	if b.State() != "closed" {
		t.Fatalf("failures were not consecutive; breaker should be closed")
	}
	run(httpErr(400), 1) // a 4xx also proves liveness and resets the count
	run(httpErr(500), 2)
	if b.State() != "closed" {
		t.Fatalf("4xx should reset the failure count")
	}
	for name, e := range map[string]error{
		"429": httpErr(429), "timeout": &CallError{Kind: KindTimeout, Message: "x"},
		"transport": &CallError{Kind: KindTransport, Message: "x"}, "decode": &CallError{Kind: KindDecode, Message: "x"},
		"deadline": context.DeadlineExceeded, "other": errors.New("boom"),
	} {
		b2 := NewBreaker(&scripted{err: e}, BreakerConfig{FailureThreshold: 2}, clk.Now)
		callOnce(b2)
		callOnce(b2)
		if b2.State() != "open" {
			t.Errorf("%s should count as a failure", name)
		}
	}
}

func TestBreakerHalfOpenAdmitsExactlyOneProbe(t *testing.T) {
	clk := newClock()
	inner := &scripted{err: httpErr(500)}
	b := NewBreaker(inner, BreakerConfig{FailureThreshold: 2, CooldownMS: 5000}, clk.Now)
	callOnce(b)
	callOnce(b)
	clk.Advance(4 * time.Second)
	if !isOpen(callOnce(b)) {
		t.Fatal("still cooling down: must reject")
	}
	clk.Advance(2 * time.Second) // cooldown elapsed

	inner.mu.Lock()
	inner.err, inner.block = nil, make(chan struct{})
	release := inner.block
	inner.mu.Unlock()
	probeDone := make(chan error, 1)
	go func() { probeDone <- callOnce(b) }()
	for deadline := time.Now().Add(2 * time.Second); inner.n() < 3 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	second := make(chan error, 1)
	go func() { second <- callOnce(b) }() // a second caller while the probe is in flight
	select {
	case err := <-second:
		if !isOpen(err) {
			t.Errorf("only one probe at a time, got %v", err)
		}
	case <-time.After(time.Second): // a broken breaker lets it through, where it blocks on the held probe
		t.Error("second caller was admitted while the probe was in flight")
	}
	close(release)
	if err := <-probeDone; err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if b.State() != "closed" {
		t.Errorf("successful probe should close the breaker, state=%s", b.State())
	}
	if err := callOnce(b); err != nil {
		t.Errorf("closed breaker should pass calls: %v", err)
	}
}

func TestBreakerFailedProbeReopensAndRestartsCooldown(t *testing.T) {
	clk, inner := newClock(), &scripted{err: httpErr(503)}
	b := NewBreaker(inner, BreakerConfig{FailureThreshold: 1, CooldownMS: 5000}, clk.Now)
	callOnce(b)
	clk.Advance(6 * time.Second)
	if err := callOnce(b); err == nil || isOpen(err) { // the probe reaches the backend and fails
		t.Fatalf("probe should reach the backend: %v", err)
	}
	if b.State() != "open" {
		t.Fatalf("failed probe should re-open, state=%s", b.State())
	}
	clk.Advance(4 * time.Second) // 4s into the NEW cooldown
	if !isOpen(callOnce(b)) {
		t.Error("cooldown must restart after a failed probe")
	}
}

func TestBreakerCancelledProbeDoesNotWedgeIt(t *testing.T) {
	clk, inner := newClock(), &scripted{err: httpErr(500)}
	b := NewBreaker(inner, BreakerConfig{FailureThreshold: 1, CooldownMS: 1000}, clk.Now)
	callOnce(b)
	clk.Advance(2 * time.Second)
	inner.set(context.Canceled)
	callOnce(b) // the probe is cancelled by its caller: no verdict
	inner.set(nil)
	if err := callOnce(b); err != nil {
		t.Errorf("another call should be allowed to probe after a cancelled probe: %v", err)
	}
	if b.State() != "closed" {
		t.Errorf("state = %s", b.State())
	}
}

func TestOpenBreakerMakesCascadeSkipTheTierInstantly(t *testing.T) {
	clk := newClock()
	sick := &scripted{err: httpErr(503)}
	reg := Registry{
		"cheap": NewBreaker(sick, BreakerConfig{FailureThreshold: 2}, clk.Now),
		"smart": &scripted{ans: map[string]Answer{"q": {Type: "noul", Noul: f(0.99)}}},
	}
	plan := Plan{Mode: ModeCascade, Tiers: []Tier{{Model: "cheap"}, {Model: "smart"}}}
	for i := 0; i < 2; i++ {
		Run(context.Background(), reg, noulReq(), plan)
	}
	before := sick.n()
	r, _ := Run(context.Background(), reg, noulReq(), plan)
	if sick.n() != before {
		t.Error("the open tier must not be called")
	}
	if !r.OK || r.Answers["q"].Model != "smart" {
		t.Errorf("cascade should still answer from the next tier: %+v", r)
	}
	if r.Trace[0].Error == nil || r.Trace[0].Error.Kind != KindCircuitOpen || r.Trace[0].LatencyMS > 5 {
		t.Errorf("trace should show a skipped tier, kind circuit_open, ~0ms: %+v", r.Trace[0])
	}
}

// ---- cache ------------------------------------------------------------------

func twoQ() Request {
	return Request{State: "ticket", Questions: map[string]Question{
		"a": {Type: "noul", Instructions: "A?"}, "b": {Type: "noul", Instructions: "B?"}}}
}

func TestCacheServesPerQuestionAndMergesMisses(t *testing.T) {
	inner := &scripted{usage: Usage{InputTokens: 100, OutputTokens: 10}}
	c := &Cached{Inner: inner, C: NewCache(CacheConfig{}, nil), Alias: "m", UpstreamModel: "jev-latest", Priced: true}

	w, err := c.Call(context.Background(), twoQ())
	if err != nil || inner.n() != 1 || w.Cached != 0 || len(w.Answers) != 2 {
		t.Fatalf("first call should miss everything: %v calls=%d %+v", err, inner.n(), w)
	}
	w, _ = c.Call(context.Background(), twoQ())
	if inner.n() != 1 || w.Cached != 2 || len(w.Answers) != 2 || w.Usage.InputTokens != 0 {
		t.Errorf("identical repeat should be fully cached: calls=%d %+v", inner.n(), w)
	}
	if w.CostUSD == nil || *w.CostUSD != 0 {
		t.Errorf("a fully cached call on a priced model costs 0, got %v", w.CostUSD)
	}

	partial := Request{State: "ticket", Questions: map[string]Question{
		"b": {Type: "noul", Instructions: "B?"}, "c": {Type: "noul", Instructions: "C?"}}}
	w, _ = c.Call(context.Background(), partial)
	last := inner.got[len(inner.got)-1]
	if inner.n() != 2 || len(last.Questions) != 1 || last.Questions["c"].Instructions != "C?" {
		t.Errorf("only the uncached question should go upstream: %+v", last.Questions)
	}
	if w.Cached != 1 || len(w.Answers) != 2 {
		t.Errorf("merged result should hold the hit and the miss: %+v", w)
	}
}

func TestCacheKeyIsContentNotQuestionIDOrState(t *testing.T) {
	inner := &scripted{}
	c := &Cached{Inner: inner, C: NewCache(CacheConfig{}, nil), Alias: "m", UpstreamModel: "u"}
	ask := func(id string, q Question, state any) WireResponse {
		w, _ := c.Call(context.Background(), Request{State: state, Questions: map[string]Question{id: q}})
		return w
	}
	q := Question{Type: "choice", Instructions: "Which?", Criteria: map[string]any{"x": "1", "y": "2"}}
	ask("first", q, map[string]any{"k": "v"})
	if w := ask("renamed", q, map[string]any{"k": "v"}); w.Cached != 1 {
		t.Error("same content under a different question id should hit")
	}
	if w := ask("first", q, map[string]any{"k": "other"}); w.Cached != 0 {
		t.Error("different state must miss")
	}
	q2 := q
	q2.Instructions = "Which one?"
	if w := ask("first", q2, map[string]any{"k": "v"}); w.Cached != 0 {
		t.Error("different instructions must miss")
	}
	other := &Cached{Inner: inner, C: c.C, Alias: "other-model", UpstreamModel: "u"}
	if w, _ := other.Call(context.Background(), Request{State: map[string]any{"k": "v"}, Questions: map[string]Question{"first": q}}); w.Cached != 0 {
		t.Error("a different model alias must not share entries")
	}
}

func TestCacheTTLAndLRU(t *testing.T) {
	clk, inner := newClock(), &scripted{}
	c := &Cached{Inner: inner, C: NewCache(CacheConfig{TTLMS: 1000, MaxEntries: 2}, clk.Now), Alias: "m"}
	q := func(s string) Request {
		return Request{State: s, Questions: map[string]Question{"q": {Type: "noul", Instructions: "?"}}}
	}

	c.Call(context.Background(), q("one"))
	clk.Advance(1500 * time.Millisecond)
	if w, _ := c.Call(context.Background(), q("one")); w.Cached != 0 {
		t.Error("entry should have expired after its TTL")
	}

	c.Call(context.Background(), q("a"))
	c.Call(context.Background(), q("b"))
	c.Call(context.Background(), q("a")) // touch a: b is now least recently used
	c.Call(context.Background(), q("c")) // evicts b
	if c.C.Len() != 2 {
		t.Errorf("cache should hold at most 2 entries, has %d", c.C.Len())
	}
	if w, _ := c.Call(context.Background(), q("a")); w.Cached != 1 {
		t.Error("recently used entry should have survived eviction")
	}
	if w, _ := c.Call(context.Background(), q("b")); w.Cached != 0 {
		t.Error("least recently used entry should have been evicted")
	}
}

func TestCacheNeverStoresFailuresOrSharesMutableAnswers(t *testing.T) {
	inner := &scripted{err: httpErr(500)}
	c := &Cached{Inner: inner, C: NewCache(CacheConfig{}, nil), Alias: "m"}
	req := Request{State: "s", Questions: map[string]Question{"q": {Type: "choice", Instructions: "?"}}}
	if _, err := c.Call(context.Background(), req); err == nil {
		t.Fatal("expected the backend error to propagate")
	}
	inner.set(nil)
	inner.mu.Lock()
	inner.ans = map[string]Answer{"q": {Type: "choice", Choice: "a", Probabilities: map[string]float64{"a": 0.9, "b": 0.1}}}
	inner.mu.Unlock()
	w, _ := c.Call(context.Background(), req)
	if w.Cached != 0 || inner.n() != 2 {
		t.Fatal("a failed call must not have populated the cache")
	}
	hit, _ := c.Call(context.Background(), req)
	hit.Answers["q"].Probabilities["a"] = 0 // a caller mutating its copy...
	again, _ := c.Call(context.Background(), req)
	if again.Answers["q"].Probabilities["a"] != 0.9 {
		t.Error("...must not corrupt the cached entry")
	}
}

// ---- pricing and wiring -----------------------------------------------------

func TestPricingAndRunTotals(t *testing.T) {
	use := Usage{InputTokens: 1_000_000, OutputTokens: 500_000}
	reg := Registry{
		"jev":   &Priced{Inner: &scripted{usage: use}, InPerMTok: 0.042, OutPerMTok: 0},
		"clef":  &Priced{Inner: &scripted{usage: Usage{InputTokens: 2_000_000}}, InPerMTok: 0.5},
		"free":  &scripted{usage: use}, // unpriced
		"broke": &Priced{Inner: &scripted{err: httpErr(500)}, InPerMTok: 9},
	}
	r, _ := Run(context.Background(), reg, noulReq(), Plan{Mode: ModeFanOut, Models: []string{"jev", "clef", "free", "broke"}})
	byModel := map[string]*float64{}
	for _, res := range r.Results {
		byModel[res.Model] = res.CostUSD
	}
	if byModel["jev"] == nil || diff(*byModel["jev"], 0.042) > 1e-12 || diff(*byModel["clef"], 1.0) > 1e-12 {
		t.Errorf("per-result cost wrong: %v %v", byModel["jev"], byModel["clef"])
	}
	if byModel["free"] != nil {
		t.Error("an unpriced model must have no cost_usd")
	}
	if r.CostUSD == nil || diff(*r.CostUSD, 1.042) > 1e-12 {
		t.Errorf("total = %v, want 1.042 (failures and unpriced add nothing)", r.CostUSD)
	}
	r, _ = Run(context.Background(), reg, noulReq(), Plan{Mode: ModeSingle, Model: "free"})
	if r.CostUSD != nil {
		t.Error("a run with no priced results has no total")
	}
}

func diff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// peel removes the outermost Limited wrapper, to look at the stack under it.
func peel(b Backend) Backend {
	if l, ok := b.(Limited); ok {
		return l.Inner
	}
	return b
}

func TestBuildRegistryWithDecoratorStack(t *testing.T) {
	off := false
	on := true
	price := 0.042
	models := map[string]ModelConfig{
		"full":    {Provider: "systemone", BaseURL: "http://x", Model: "jev-latest", PriceInputPerMTok: &price},
		"nobreak": {Provider: "systemone", BaseURL: "http://x", Breaker: &BreakerConfig{Enabled: &off}},
		"optout":  {Provider: "systemone", BaseURL: "http://x", Cache: &off},
		"gpt":     {Provider: "openai"},
		"optin":   {Provider: "systemone", BaseURL: "http://x", Cache: &on},
		"tuned":   {Provider: "systemone", BaseURL: "http://x", Breaker: &BreakerConfig{FailureThreshold: 9, CooldownMS: 1234}},
	}
	env := func(string) string { return "" }

	reg, err := BuildRegistryWith(models, env, Options{Cache: CacheConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	// Outermost is the limits wrapper; under it the cache, then the breaker, then pricing, then the backend.
	if _, ok := reg["full"].(Limited); !ok {
		t.Fatalf("every systemone model is wrapped in Limited, got %T", reg["full"])
	}
	c, ok := peel(reg["full"]).(*Cached)
	if !ok || !c.Priced {
		t.Fatalf("full = %T", reg["full"])
	}
	br, ok := c.Inner.(*Breaker)
	if !ok {
		t.Fatalf("cache should wrap the breaker, got %T", c.Inner)
	}
	if _, ok := br.inner.(*Priced); !ok {
		t.Fatalf("breaker should wrap pricing, got %T", br.inner)
	}
	if _, ok := peel(reg["nobreak"]).(*Cached).Inner.(*SystemOne); !ok {
		t.Error("breaker disabled: the cache should wrap the raw backend")
	}
	if _, ok := peel(reg["optout"]).(*Breaker); !ok {
		t.Errorf("cache=false should override the global setting, got %T", reg["optout"])
	}
	if _, ok := reg["gpt"].(unsupported); !ok {
		t.Errorf("unsupported providers are not decorated, got %T", reg["gpt"])
	}
	if tb := peel(reg["tuned"]).(*Cached).Inner.(*Breaker); tb.threshold != 9 || tb.cooldown != 1234*time.Millisecond {
		t.Errorf("breaker settings not applied: %d %v", tb.threshold, tb.cooldown)
	}
	if peel(reg["full"]).(*Cached).C != peel(reg["tuned"]).(*Cached).C {
		t.Error("cached models should share one LRU")
	}

	reg, _ = BuildRegistryWith(models, env, Options{}) // global cache off
	if _, ok := peel(reg["full"]).(*Breaker); !ok {
		t.Errorf("cache off globally: full should be breaker-wrapped, got %T", reg["full"])
	}
	if _, ok := peel(reg["optin"]).(*Cached); !ok {
		t.Errorf("cache=true should opt a model in even when the global cache is off, got %T", reg["optin"])
	}
}

func TestProbeForwardsThroughDecorators(t *testing.T) {
	clk := newClock()
	inner := &probing{status: "ready"}
	var b Backend = &Cached{Inner: NewBreaker(&Priced{Inner: inner}, BreakerConfig{FailureThreshold: 1}, clk.Now), C: NewCache(CacheConfig{}, nil)}
	if got := probeOf(context.Background(), b); got != "ready" {
		t.Errorf("probe = %q", got)
	}
	inner.err = httpErr(500)
	callOnce(b.(*Cached).Inner)
	if got := probeOf(context.Background(), b); got != KindCircuitOpen {
		t.Errorf("an open breaker should report circuit_open, got %q", got)
	}
}

type probing struct {
	scripted
	status string
	err    error
}

func (p *probing) Probe(context.Context) string { return p.status }
func (p *probing) Call(ctx context.Context, r Request) (WireResponse, error) {
	if p.err != nil {
		return WireResponse{}, p.err
	}
	return p.scripted.Call(ctx, r)
}

// newHeaderSpy is a Jev-compatible upstream that records each request's traceparent.
func newHeaderSpy(got *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = append(*got, r.Header.Get("traceparent"))
		io.WriteString(w, `{"answers":{"q":{"type":"noul","noul":0.9}}}`)
	}))
}
