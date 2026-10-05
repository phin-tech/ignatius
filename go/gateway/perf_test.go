package gateway

// Performance: the same question through Jev directly vs through Ignatius.
//
// Real Jev (opt-in; costs a few dozen API calls, key stays in the environment):
//
//	IGNATIUS_PERF=1 TYPESAFE_API_KEY=... go test ./gateway -run TestPerfJev -v
//	  IGNATIUS_PERF_N=50                      iterations per path (default 30)
//	  IGNATIUS_PERF_BASE_URL=https://...      override the Jev base URL
//
// Overhead with the network removed (always runnable, no key):
//
//	go test ./gateway -run xxx -bench Overhead -benchmem
//
// Remote latency jitter is usually larger than Ignatius's own cost, so the real
// test reports two things: the median difference between paths (what a caller
// sees, noisy) and Ignatius's internal overhead (client wall time minus Jev's
// own measured latency, which cancels the network).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
)

const perfQuestions = `{
  "department": {"type": "choice", "instructions": "Which team should handle this?",
                 "criteria": {"billing": "invoices, payments, refunds", "technical": "bugs, outages, system errors", "sales": "pricing, new contracts"}},
  "churn_risk": {"type": "noul", "instructions": "Does the user threaten to cancel or leave?"},
  "urgency":    {"type": "score", "instructions": "How urgent is this?", "criteria": ["no deadline", "this week", "today", "already late"]}
}`

const perfState = `{"subject": "Duplicate charge on invoice #4411", "body": "We were billed twice for March. Refund it today or we cancel our plan."}`

type sample struct {
	wall       time.Duration
	upstreamMS int64 // Jev's own latency as seen by Ignatius; -1 when not observable
	choice     string
}

func perfClient() *http.Client {
	return &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}}
}

func timedPost(c *http.Client, url, key string, body []byte) ([]byte, time.Duration, error) {
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	d := time.Since(start)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, raw)
	}
	return raw, d, err
}

func TestPerfJevDirectVsIgnatius(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if os.Getenv("IGNATIUS_PERF") != "1" || key == "" {
		t.Skip("set IGNATIUS_PERF=1 and TYPESAFE_API_KEY to run against real Jev")
	}
	base := os.Getenv("IGNATIUS_PERF_BASE_URL")
	if base == "" {
		base = "https://api.typesafe.ai"
	}
	n := 30
	if v, err := strconv.Atoi(os.Getenv("IGNATIUS_PERF_N")); err == nil && v > 0 {
		n = v
	}

	cfg := Config{DefaultRoute: "jev", Models: map[string]ignatius.ModelConfig{
		"jev": {Provider: "systemone", BaseURL: base, Model: "jev-latest", APIKeyEnv: "TYPESAFE_API_KEY", TimeoutMS: 20000}}}
	cfg.applyDefaults()
	reg, err := ignatius.BuildRegistry(cfg.Models, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, reg, func(string) string { return "" }) // no client key: isolates routing cost from auth
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv)
	defer gw.Close()

	directBody := []byte(`{"state":` + perfState + `,"model":"jev-latest","questions":` + perfQuestions + `}`)
	l1Body := directBody
	l2Body := []byte(`{"request":{"state":` + perfState + `,"questions":` + perfQuestions + `},"plan":{"mode":"single","model":"jev"}}`)

	type path struct {
		name string
		run  func(c *http.Client) (sample, error)
	}
	choiceOf := func(raw []byte) string {
		var r struct {
			Answers map[string]struct {
				Choice string `json:"choice"`
			} `json:"answers"`
			Results []struct {
				LatencyMS int64 `json:"latency_ms"`
			} `json:"results"`
		}
		_ = json.Unmarshal(raw, &r)
		return r.Answers["department"].Choice
	}
	paths := []path{
		{"direct to Jev", func(c *http.Client) (sample, error) {
			raw, d, err := timedPost(c, base+"/v1/systemone", key, directBody)
			return sample{d, -1, choiceOf(raw)}, err
		}},
		{"via Ignatius L1 (/v1/systemone)", func(c *http.Client) (sample, error) {
			raw, d, err := timedPost(c, gw.URL+"/v1/systemone", "", l1Body)
			return sample{d, -1, choiceOf(raw)}, err
		}},
		{"via Ignatius L2 (/v1/route)", func(c *http.Client) (sample, error) {
			raw, d, err := timedPost(c, gw.URL+"/v1/route", "", l2Body)
			var r struct {
				Results []struct {
					LatencyMS int64 `json:"latency_ms"`
				} `json:"results"`
			}
			_ = json.Unmarshal(raw, &r)
			up := int64(-1)
			if len(r.Results) == 1 {
				up = r.Results[0].LatencyMS
			}
			return sample{d, up, choiceOf(raw)}, err
		}},
	}

	// One client per path (own connection pool), warmed up so TLS setup is not measured.
	clients := make([]*http.Client, len(paths))
	for i := range paths {
		clients[i] = perfClient()
		for w := 0; w < 3; w++ {
			if _, err := paths[i].run(clients[i]); err != nil {
				t.Fatalf("warmup %q: %v", paths[i].name, err)
			}
		}
	}

	// Interleave the paths each round so network drift hits all three equally.
	samples := make([][]sample, len(paths))
	for round := 0; round < n; round++ {
		for i, p := range paths {
			s, err := p.run(clients[i])
			if err != nil {
				t.Fatalf("%q round %d: %v", p.name, round, err)
			}
			samples[i] = append(samples[i], s)
		}
	}

	t.Logf("question: 3 questions (choice + noul + score), %d interleaved rounds per path, base %s", n, base)
	t.Logf("%-34s %8s %8s %8s %8s", "path (client wall time, ms)", "min", "p50", "p95", "mean")
	med := make([]float64, len(paths))
	for i, p := range paths {
		ds := walls(samples[i])
		med[i] = pct(ds, 50)
		t.Logf("%-34s %8.1f %8.1f %8.1f %8.1f", p.name, ds[0], med[i], pct(ds, 95), mean(ds))
	}
	t.Logf("median delta vs direct: L1 %+.1f ms, L2 %+.1f ms (includes network jitter)", med[1]-med[0], med[2]-med[0])

	var internal []float64
	for _, s := range samples[2] {
		if s.upstreamMS >= 0 {
			internal = append(internal, float64(s.wall.Microseconds())/1000-float64(s.upstreamMS))
		}
	}
	sort.Float64s(internal)
	t.Logf("Ignatius internal overhead (L2 wall - Jev's own latency): p50 %.1f ms, p95 %.1f ms, max %.1f ms",
		pct(internal, 50), pct(internal, 95), internal[len(internal)-1])
	t.Logf("note: Jev's latency is recorded in whole ms, so that line has about +/-1 ms resolution; " +
		"BenchmarkOverhead gives the precise figure")

	// Correctness and a loose regression guard. Network deltas are only logged:
	// asserting on them would make the test flaky.
	want := samples[0][0].choice
	for i, ss := range samples {
		for _, s := range ss {
			if s.choice != want {
				t.Errorf("%q returned %q, direct returned %q", paths[i].name, s.choice, want)
				break
			}
		}
	}
	if p := pct(internal, 50); p > 25 {
		t.Errorf("Ignatius internal overhead p50 = %.1f ms, expected well under 25 ms", p)
	}
}

func walls(ss []sample) []float64 {
	out := make([]float64, len(ss))
	for i, s := range ss {
		out[i] = float64(s.wall.Microseconds()) / 1000
	}
	sort.Float64s(out)
	return out
}

func pct(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted)*p + 99) / 100
	if idx < 1 {
		idx = 1
	}
	return sorted[idx-1]
}

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// ---- Overhead with the network removed --------------------------------------

func overheadFixture(b *testing.B) (direct, l1, l2 func()) {
	b.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"fake","answers":{"department":{"type":"choice","choice":"billing","confidence":0.97,"probabilities":{"billing":0.97,"technical":0.02,"sales":0.01}},"churn_risk":{"type":"noul","noul":0.98},"urgency":{"type":"score","score":2,"confidence":0.9,"legend":{"0":"a","1":"b","2":"c","3":"d"},"probabilities":{"0":0,"1":0.05,"2":0.9,"3":0.05}}},"usage":{"input_tokens":433,"output_tokens":72}}`)
	}))
	b.Cleanup(up.Close)
	cfg := Config{DefaultRoute: "m", Models: map[string]ignatius.ModelConfig{"m": {Provider: "systemone", BaseURL: up.URL, Model: "jev-latest"}}}
	cfg.applyDefaults()
	reg, _ := ignatius.BuildRegistry(cfg.Models, func(string) string { return "" })
	srv, err := New(cfg, reg, func(string) string { return "" })
	if err != nil {
		b.Fatal(err)
	}
	gw := httptest.NewServer(srv)
	b.Cleanup(gw.Close)

	c := perfClient()
	post := func(url string, body []byte) func() {
		return func() {
			resp, err := c.Post(url, "application/json", bytes.NewReader(body))
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				b.Fatalf("HTTP %d", resp.StatusCode)
			}
		}
	}
	direct = post(up.URL+"/v1/systemone", []byte(`{"state":`+perfState+`,"model":"jev-latest","questions":`+perfQuestions+`}`))
	l1 = post(gw.URL+"/v1/systemone", []byte(`{"state":`+perfState+`,"model":"m","questions":`+perfQuestions+`}`))
	l2 = post(gw.URL+"/v1/route", []byte(`{"request":{"state":`+perfState+`,"questions":`+perfQuestions+`},"plan":{"mode":"single","model":"m"}}`))
	return
}

func BenchmarkOverhead(b *testing.B) {
	direct, l1, l2 := overheadFixture(b)
	for _, bc := range []struct {
		name string
		fn   func()
	}{{"direct", direct}, {"via_L1", l1}, {"via_L2", l2}} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < 20; i++ {
				bc.fn()
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bc.fn()
			}
		})
	}
}
