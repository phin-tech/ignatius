package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
	"github.com/phin-tech/ignatius/go/store"
)

const (
	choiceSureB = `{"q":{"type":"choice","choice":"b","probabilities":{"a":0.05,"b":0.95},"confidence":0.9}}`
)

// auditServer is storeServer with auditing on at the given rate.
func auditServer(t *testing.T, models map[string]*fake, rate float64, mut func(*Config)) (*Server, store.Store) {
	t.Helper()
	s, st, _ := storeServer(t, models, func(c *Config) {
		c.Audit = &AuditConfig{SampleRate: rate, MaxInflight: 4}
		if mut != nil {
			mut(c)
		}
	})
	return s, st
}

func audits(t *testing.T, s *Server, st store.Store, client string) []store.Audit {
	t.Helper()
	s.Flush(context.Background())
	as, err := st.Audits(context.Background(), client, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return as
}

func TestAnAuditComparesTheSettledTierWithTheNext(t *testing.T) {
	cheap, smart := newFake(t, choiceSure), newFake(t, choiceSure) // both say "a": they agree
	s, st := auditServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, 1, nil)
	id := reqID(t, s, choiceReq("cascade:cheap@0.5>smart", "x"), "kb")

	as := audits(t, s, st, "b")
	if len(as) != 1 {
		t.Fatalf("audits = %+v", as)
	}
	a := as[0]
	if a.RequestID != id || a.QuestionID != "q" || a.Model != "cheap" || a.AuditModel != "smart" || !a.Agreed || a.Type != "choice" ||
		a.Threshold == nil || *a.Threshold != 0.5 || a.Confidence == nil || *a.Confidence != 0.9 || a.AuditConfidence == nil || a.Route != "inline" {
		t.Errorf("audit = %+v", a)
	}
	if smart.posts != 1 {
		t.Errorf("the next tier should have been asked once, in the background: %d posts", smart.posts)
	}

	// Disagreement is recorded as such.
	smart.mu.Lock()
	smart.answers = choiceSureB
	smart.mu.Unlock()
	reqID(t, s, choiceReq("cascade:cheap@0.5>smart", "x"), "kb")
	var disagreed int
	for _, a := range audits(t, s, st, "b") {
		if !a.Agreed {
			disagreed++
		}
	}
	if disagreed != 1 {
		t.Errorf("want one disagreement, got %d", disagreed)
	}
}

func TestAnAuditNeverChangesTheResponse(t *testing.T) {
	cheap, smart := newFake(t, choiceSure), newFake(t, choiceSureB) // the auditor disagrees
	s, _ := auditServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, 1, nil)
	rec := do(s, "POST", "/v1/systemone", choiceReq("cascade:cheap@0.5>smart", "x"), "Bearer kb")
	m := decode(t, rec)
	answers := m["answers"].(map[string]any)["q"].(map[string]any)
	sources := m["ignatius"].(map[string]any)["sources"].(map[string]any)["q"].(map[string]any)
	if rec.Code != 200 || answers["choice"] != "a" || sources["model"] != "cheap" {
		t.Fatalf("the cheap tier's answer must stand: %d %s", rec.Code, rec.Body)
	}
	if trace := m["ignatius"].(map[string]any)["trace"].([]any); len(trace) != 1 {
		t.Errorf("the trace must show only the tiers that served the request, not the audit: %v", trace)
	}
	s.Flush(context.Background())
}

func TestWhatIsNotAuditedAndWhy(t *testing.T) {
	cheap, smart := newFake(t, choiceUnsure), newFake(t, choiceSure)
	s, st := auditServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, 1, func(c *Config) {
		c.Routes = map[string]map[string]any{"single-route": {"mode": "single", "model": "cheap"}}
	})
	// Escalated: the cheap tier was unsure, so the next tier already answered it.
	reqID(t, s, choiceReq("cascade:cheap@0.5>smart", "x"), "kb")
	// A single model has no next tier, and a fan-out is not a cascade.
	reqID(t, s, choiceReq("single-route", "x"), "kb")
	reqID(t, s, choiceReq("fan-out:cheap,smart|vote", "x"), "kb")
	if as := audits(t, s, st, "b"); len(as) != 0 {
		t.Errorf("nothing here should be audited: %+v", as)
	}
	// smart was called by the escalation and by the fan-out, and by no audit.
	if smart.posts != 2 {
		t.Errorf("smart was called %d times, want 2 (the escalation and the fan-out)", smart.posts)
	}
}

func TestTheTierMathWithThreeTiers(t *testing.T) {
	cheap, mid, smart := newFake(t, choiceSure), newFake(t, choiceSure), newFake(t, choiceSure)
	// cheap is 0.9 confident but its bar is 0.95: it escalates. mid settles at 0.5 and is audited against smart.
	s, st := auditServer(t, map[string]*fake{"cheap": cheap, "mid": mid, "smart": smart}, 1, nil)
	reqID(t, s, choiceReq("cascade:cheap@0.95>mid@0.5>smart", "x"), "kb")
	as := audits(t, s, st, "b")
	if len(as) != 1 || as[0].Model != "mid" || as[0].AuditModel != "smart" || as[0].Threshold == nil || *as[0].Threshold != 0.5 {
		t.Fatalf("audits = %+v", as)
	}
}

func TestSampling(t *testing.T) {
	cheap, smart := newFake(t, choiceSure), newFake(t, choiceSure)
	s, st := auditServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, 0.5, nil)
	var roll float64
	s.auditor.rand = func() float64 { return roll }
	roll = 0.7 // above the rate: not sampled
	reqID(t, s, choiceReq("cascade:cheap@0.5>smart", "x"), "kb")
	roll = 0.2 // below: sampled
	reqID(t, s, choiceReq("cascade:cheap@0.5>smart", "x"), "kb")
	if as := audits(t, s, st, "b"); len(as) != 1 {
		t.Fatalf("want exactly the sampled request audited, got %d", len(as))
	}
}

func TestOffByDefaultAndWithRateZero(t *testing.T) {
	cheap, smart := newFake(t, choiceSure), newFake(t, choiceSure)
	s, st, _ := storeServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, nil) // no [audit] at all
	if s.auditor != nil {
		t.Fatal("no [audit] table must mean no auditor")
	}
	reqID(t, s, choiceReq("cascade:cheap@0.5>smart", "x"), "kb")
	s2, _ := auditServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, 0, nil)
	if s2.auditor != nil {
		t.Fatal("sample_rate 0 must mean no auditor")
	}
	if as := audits(t, s, st, "b"); len(as) != 0 || smart.posts != 0 {
		t.Errorf("audits %+v, smart posts %d", as, smart.posts)
	}
}

func TestAFailedAuditCostsNothingAndSkipsTheRow(t *testing.T) {
	cheap, smart := newFake(t, choiceSure), newFake(t, choiceSure)
	smart.status = 503
	s, st := auditServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, 1, nil)
	rec := do(s, "POST", "/v1/systemone", choiceReq("cascade:cheap@0.5>smart", "x"), "Bearer kb")
	if rec.Code != 200 {
		t.Fatalf("a failing auditor must not fail the request: %d %s", rec.Code, rec.Body)
	}
	if as := audits(t, s, st, "b"); len(as) != 0 {
		t.Errorf("a failed audit has nothing to record: %+v", as)
	}
}

// slow is an upstream that holds every call until released.
func slow(t *testing.T, answers string) (*httptest.Server, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			return
		}
		<-release
		fmt.Fprintf(w, `{"model":"fake","answers":%s,"usage":{"input_tokens":1,"output_tokens":1}}`, answers)
	}))
	t.Cleanup(srv.Close)
	return srv, release
}

func TestAuditsAreBoundedAndNeverQueued(t *testing.T) {
	cheap := newFake(t, choiceSure)
	srv, release := slow(t, choiceSure)
	s, st := auditServer(t, map[string]*fake{"cheap": cheap}, 1, func(c *Config) {
		c.Audit.MaxInflight = 1
		c.Models["smart"] = ignatius.ModelConfig{Provider: "systemone", BaseURL: srv.URL, Model: "jev-latest", APIKeyEnv: "K"}
	})
	for range 4 { // each response returns at once; the first audit holds the only slot
		if rec := do(s, "POST", "/v1/systemone", choiceReq("cascade:cheap@0.5>smart", "x"), "Bearer kb"); rec.Code != 200 {
			t.Fatalf("%d", rec.Code)
		}
	}
	close(release)
	if as := audits(t, s, st, "b"); len(as) != 1 {
		t.Fatalf("with one slot, four requests make one audit: got %d", len(as))
	}
}

// spyStore records the audits written through it, so a test can read them after Close has
// closed the real store.
type spyStore struct {
	store.Store
	mu   sync.Mutex
	rows []store.Audit
}

func (s *spyStore) RecordAudits(ctx context.Context, as []store.Audit) error {
	s.mu.Lock()
	s.rows = append(s.rows, as...)
	s.mu.Unlock()
	return s.Store.RecordAudits(ctx, as)
}

func TestCloseWaitsForARunningAudit(t *testing.T) {
	cheap := newFake(t, choiceSure)
	srv, release := slow(t, choiceSure)
	s, _, _ := storeServer(t, map[string]*fake{"cheap": cheap}, func(c *Config) {
		c.Audit = &AuditConfig{SampleRate: 1, MaxInflight: 2}
		c.Models["smart"] = ignatius.ModelConfig{Provider: "systemone", BaseURL: srv.URL, Model: "jev-latest", APIKeyEnv: "K"}
	})
	spy := &spyStore{Store: s.store}
	s.auditor.st = spy
	if rec := do(s, "POST", "/v1/systemone", choiceReq("cascade:cheap@0.5>smart", "x"), "Bearer kb"); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("Close returned while an audit was still running")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned")
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.rows) != 1 || !spy.rows[0].Agreed {
		t.Fatalf("the audit that was running at Close must finish and be written: %+v", spy.rows)
	}
}

func TestAnAuditHoldsNoContent(t *testing.T) {
	o := observe(t)
	cheap, smart := newFake(t, choiceSure), newFake(t, choiceSureB)
	s, st, logs := storeServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, func(c *Config) {
		c.Audit = &AuditConfig{SampleRate: 1, MaxInflight: 4}
	})
	rec := do(s, "POST", "/v1/systemone", choiceReq("cascade:cheap@0.5>smart", sentinel+"-state"), "Bearer ka") // a opted in to content
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	as := audits(t, s, st, "a")
	if len(as) != 1 {
		t.Fatalf("audits = %+v", as)
	}
	if fmt.Sprintf("%+v", as[0]) != strings.ReplaceAll(fmt.Sprintf("%+v", as[0]), sentinel, "") {
		t.Error("an audit row carries content")
	}
	for name, haystack := range map[string]string{"spans and metrics": o.allTelemetryText(), "log lines": logs.String()} {
		if strings.Contains(haystack, sentinel) {
			t.Errorf("%s leaked content from an audit:\n%.400s", name, haystack)
		}
	}
	if !strings.Contains(logs.String(), `"msg":"audit"`) {
		t.Errorf("an audit should log one line: %s", logs)
	}
}

func TestAgrees(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name       string
		a, b       ignatius.Answer
		agreed, ok bool
	}{
		{"same choice", ignatius.Answer{Type: "choice", Choice: "a"}, ignatius.Answer{Type: "choice", Choice: "a"}, true, true},
		{"different choice", ignatius.Answer{Type: "choice", Choice: "a"}, ignatius.Answer{Type: "choice", Choice: "b"}, false, true},
		{"choice without a value", ignatius.Answer{Type: "choice"}, ignatius.Answer{Type: "choice", Choice: "b"}, false, false},
		{"noul same side", ignatius.Answer{Type: "noul", Noul: f(0.9)}, ignatius.Answer{Type: "noul", Noul: f(0.6)}, true, true},
		{"noul other side", ignatius.Answer{Type: "noul", Noul: f(0.9)}, ignatius.Answer{Type: "noul", Noul: f(0.2)}, false, true},
		{"noul at one half counts as yes", ignatius.Answer{Type: "noul", Noul: f(0.5)}, ignatius.Answer{Type: "noul", Noul: f(0.7)}, true, true},
		{"score same level", ignatius.Answer{Type: "score", Score: f(2.2)}, ignatius.Answer{Type: "score", Score: f(1.8)}, true, true},
		{"score other level", ignatius.Answer{Type: "score", Score: f(2.2)}, ignatius.Answer{Type: "score", Score: f(0.4)}, false, true},
		{"different types", ignatius.Answer{Type: "choice", Choice: "a"}, ignatius.Answer{Type: "noul", Noul: f(1)}, false, false},
		{"unknown type", ignatius.Answer{Type: "x"}, ignatius.Answer{Type: "x"}, false, false},
	}
	for _, c := range cases {
		if agreed, ok := agrees(c.a, c.b); agreed != c.agreed || ok != c.ok {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, agreed, ok, c.agreed, c.ok)
		}
	}
}

func TestAuditsFeedCalibrationAndRankBelowPeople(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	var as []store.Audit
	for i := 0; i < 30; i++ { // 30 confident answers all agreed with the next tier
		as = append(as, store.Audit{RequestID: fmt.Sprintf("r%d", i), QuestionID: "q", Client: "a", Type: "choice", Model: "jeff", Confidence: f(0.92), Agreed: true})
	}
	rows := Calibrate(AuditFeedback(as), 0.95, 20)
	if len(rows) != 1 || rows[0].Labeled != 30 || rows[0].Accuracy != 1 || rows[0].Recommended == nil {
		t.Fatalf("rows = %+v", rows)
	}
	// A person's verdict outranks an audit of the same decision.
	audit := AuditFeedback([]store.Audit{{RequestID: "r", QuestionID: "q", Client: "a", Type: "choice", Model: "jeff", Confidence: f(0.9), Agreed: true}})
	human := store.Feedback{RequestID: "r", QuestionID: "q", Client: "a", Owner: "a", Verdict: "bad", Source: "human", Type: "choice", Model: "jeff", Confidence: f(0.9)}
	if got := store.Dedupe(append(audit, human)); len(got) != 1 || got[0].Verdict != "bad" {
		t.Fatalf("the human verdict must win: %+v", got)
	}
	automated := store.Feedback{RequestID: "r", QuestionID: "q", Client: "a", Owner: "a", Verdict: "good", Source: "automated"}
	if got := store.Dedupe(append(audit, automated)); got[0].Source != "automated" {
		t.Fatalf("an automated signal outranks an audit: %+v", got)
	}
}

func TestAuditConfigValidation(t *testing.T) {
	st := &StoreConfig{}
	for name, c := range map[string]Config{
		"rate above one":    {Store: st, Audit: &AuditConfig{SampleRate: 1.5}},
		"negative rate":     {Store: st, Audit: &AuditConfig{SampleRate: -0.1}},
		"negative inflight": {Store: st, Audit: &AuditConfig{SampleRate: 0.1, MaxInflight: -1}},
		"needs a store":     {Audit: &AuditConfig{SampleRate: 0.1}},
	} {
		if err := c.validateAudit(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	for name, c := range map[string]Config{
		"off, no store needed": {Audit: &AuditConfig{SampleRate: 0}},
		"on":                   {Store: st, Audit: &AuditConfig{SampleRate: 0.05, MaxInflight: 2}},
		"absent":               {},
	} {
		if err := c.validateAudit(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	c := Config{Audit: &AuditConfig{SampleRate: 0.1}}
	c.applyDefaults()
	if c.Audit.MaxInflight != 4 {
		t.Errorf("default max_inflight = %d", c.Audit.MaxInflight)
	}
}

// An audit is an ordinary call to the next tier and is counted by its breaker. It must never be
// the call that uses a half-open probe, nor poke a tier already known to be failing.
func TestNoAuditIsSentToATierWhoseBreakerIsNotClosed(t *testing.T) {
	cheap, smart := newFake(t, choiceSure), newFake(t, choiceSure)
	smart.status = 503
	s, st := auditServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, 1, func(c *Config) {
		smartCfg := c.Models["smart"]
		one := 1
		smartCfg.Breaker = &ignatius.BreakerConfig{FailureThreshold: one, CooldownMS: 60000}
		c.Models["smart"] = smartCfg
	})
	reqID(t, s, choiceReq("cascade:cheap@0.5>smart", "x"), "kb") // the audit fails once and opens the breaker
	s.Flush(context.Background())
	if got := s.router.Reg.BreakerState("smart"); got != "open" {
		t.Fatalf("setup: smart's breaker is %q, want open", got)
	}
	before := smart.posts
	for range 3 {
		reqID(t, s, choiceReq("cascade:cheap@0.5>smart", "x"), "kb")
	}
	if smart.posts != before {
		t.Errorf("audits reached a tier with an open breaker: %d calls, want %d", smart.posts, before)
	}
	if as := audits(t, s, st, "b"); len(as) != 0 {
		t.Errorf("nothing could have been recorded: %+v", as)
	}
	if got := (ignatius.Registry{}).BreakerState("none"); got != "" {
		t.Errorf("an unknown alias has no breaker state: %q", got)
	}
}
