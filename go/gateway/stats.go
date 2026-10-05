package gateway

import (
	"sort"
	"sync"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
)

const (
	latencyWindow = 200 // latencies kept per model for percentiles
	recentWindow  = 50  // recent requests kept for the dashboard
	maxRouteLen   = 80  // client-controlled strings are truncated before storage
)

// Stats is an in-memory, process-lifetime view of traffic for the status page.
// It stores metadata only (model names, kinds, timings); never state, questions
// or answers. It resets on restart; use real metrics for history.
type Stats struct {
	mu          sync.Mutex
	started     time.Time
	models      map[string]*modelStats
	clients     map[string]*clientStats
	recent      []RecentRequest // newest last, bounded
	totalCost   float64
	savings     float64 // estimated cascade savings, can be negative
	savingsSeen bool
}

type modelStats struct {
	calls     int
	failures  map[string]int
	latencies []int64 // ring of the last latencyWindow
	next      int
	cost      float64
	cached    int
}

type clientStats struct {
	requests, limited int
	cost              float64
}

// RecentRequest is one finished request, as shown on the dashboard.
type RecentRequest struct {
	Time       time.Time     `json:"time"`
	RequestID  string        `json:"request_id"`
	Layer      string        `json:"layer"` // L1 | L2
	Route      string        `json:"route"`
	Mode       string        `json:"mode"`
	OK         bool          `json:"ok"`
	DurationMS int64         `json:"duration_ms"`
	Models     []string      `json:"models"` // cascade: call order; otherwise successes then failures
	Failures   int           `json:"failures"`
	Client     string        `json:"client"`
	CostUSD    *float64      `json:"cost_usd,omitempty"`
	Cached     int           `json:"cached,omitempty"` // questions served from the cache
	Detail     RequestDetail `json:"detail"`
}

// RequestDetail is the drill-down for one request. It holds structure, timings,
// confidences and thresholds only; answer values are never stored.
type RequestDetail struct {
	Calls     []CallDetail `json:"calls"`
	Agreement []Agreement  `json:"agreement,omitempty"` // fan_out with 2+ responders
}

// CallDetail is one model call.
type CallDetail struct {
	Tier      *int                `json:"tier,omitempty"` // cascade only
	Model     string              `json:"model"`
	OK        bool                `json:"ok"`
	LatencyMS int64               `json:"latency_ms"`
	Error     *ignatius.ErrorBody `json:"error,omitempty"`
	Questions []CallQuestion      `json:"questions"`
}

type CallQuestion struct {
	ID         string   `json:"id"`
	Type       string   `json:"type,omitempty"`
	Confidence *float64 `json:"confidence"`
	Threshold  *float64 `json:"threshold,omitempty"` // cascade: the bar applied at this tier
	Escalated  bool     `json:"escalated"`           // cascade only
}

// Agreement says how much the fan-out responders agreed, without their answers.
type Agreement struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Models   int      `json:"models"`
	Distinct int      `json:"distinct,omitempty"` // choice: number of different choices
	Spread   *float64 `json:"spread,omitempty"`   // noul/score: max - min
}

func NewStats() *Stats {
	return &Stats{started: time.Now(), models: map[string]*modelStats{}, clients: map[string]*clientStats{}}
}

func clip(s string) string {
	if len(s) > maxRouteLen {
		return s[:maxRouteLen] + "…"
	}
	return s
}

func (st *Stats) client(name string) *clientStats {
	c := st.clients[name]
	if c == nil {
		c = &clientStats{}
		st.clients[name] = c
	}
	return c
}

// NoteRequest counts an attempt by a client (including ones rate limiting rejects).
func (st *Stats) NoteRequest(client string) {
	st.mu.Lock()
	st.client(client).requests++
	st.mu.Unlock()
}

// NoteLimited counts a request rejected by the client's rate limit.
func (st *Stats) NoteLimited(client string) {
	st.mu.Lock()
	st.client(client).limited++
	st.mu.Unlock()
}

// Record notes a finished request. savings is the cascade savings estimate, if any.
func (st *Stats) Record(layer, route, id, clientName string, routed ignatius.Routed, d time.Duration, savings *float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	rr := RecentRequest{Time: time.Now(), RequestID: id, Layer: layer, Route: clip(route), Mode: routed.Mode,
		OK: routed.OK, DurationMS: d.Milliseconds(), Models: []string{}, Failures: len(routed.Failures),
		Client: clientName, CostUSD: routed.CostUSD, Detail: buildDetail(routed)}
	get := func(m string) *modelStats {
		ms := st.models[m]
		if ms == nil {
			ms = &modelStats{failures: map[string]int{}}
			st.models[m] = ms
		}
		return ms
	}
	for _, r := range routed.Results {
		ms := get(r.Model)
		ms.calls++
		ms.cached += r.Cached
		rr.Cached += r.Cached
		if r.CostUSD != nil {
			ms.cost += *r.CostUSD
		}
		if r.Cached > 0 && r.LatencyMS == 0 && len(r.Answers) == r.Cached {
			continue // served entirely from cache: no call happened, so no latency sample
		}
		if len(ms.latencies) < latencyWindow {
			ms.latencies = append(ms.latencies, r.LatencyMS)
		} else {
			ms.latencies[ms.next] = r.LatencyMS
			ms.next = (ms.next + 1) % latencyWindow
		}
	}
	for _, f := range routed.Failures {
		ms := get(f.Model)
		ms.calls++
		ms.failures[f.Error.Kind]++
	}
	if routed.CostUSD != nil {
		st.totalCost += *routed.CostUSD
		st.client(clientName).cost += *routed.CostUSD
	}
	if savings != nil {
		st.savings += *savings
		st.savingsSeen = true
	}
	if len(routed.Trace) > 0 { // a cascade's trace is the true call order
		for _, h := range routed.Trace {
			rr.Models = append(rr.Models, h.Model)
		}
	} else { // single / fan_out: successes then failures (fan-out calls are concurrent)
		for _, r := range routed.Results {
			rr.Models = append(rr.Models, r.Model)
		}
		for _, f := range routed.Failures {
			rr.Models = append(rr.Models, f.Model)
		}
	}
	st.recent = append(st.recent, rr)
	if len(st.recent) > recentWindow {
		st.recent = st.recent[len(st.recent)-recentWindow:]
	}
}

// ModelSnapshot is the per-model view in /v1/stats.
type ModelSnapshot struct {
	ID       string         `json:"id"`
	Provider string         `json:"provider,omitempty"`       // e.g. systemone
	Upstream string         `json:"upstream_model,omitempty"` // the model name sent upstream, e.g. jev-latest
	Host     string         `json:"host,omitempty"`           // host:port of the backend, never the path or credentials
	Profiles []string       `json:"profiles"`                 // profiles currently pointing at this model
	Status   string         `json:"status"`
	Calls    int            `json:"calls"`
	Failures map[string]int `json:"failures"`
	P50MS    int64          `json:"p50_ms"`
	P95MS    int64          `json:"p95_ms"`
	CostUSD  float64        `json:"cost_usd"`
	Cached   int            `json:"cached"` // questions served from the cache
}

// ClientSnapshot is the per-client view: counts and spend, never the key.
type ClientSnapshot struct {
	Name        string  `json:"name"`
	Requests    int     `json:"requests"`
	RateLimited int     `json:"rate_limited"`
	CostUSD     float64 `json:"cost_usd"`
}

// StatsSnapshot is the body of GET /v1/stats.
type StatsSnapshot struct {
	EvalEnabled bool             `json:"eval_enabled"` // the status page shows its Evaluate panel only when this is true
	UptimeS     int64            `json:"uptime_s"`
	Models      []ModelSnapshot  `json:"models"`
	Clients     []ClientSnapshot `json:"clients"`
	Recent      []RecentRequest  `json:"recent"`
	TotalCost   float64          `json:"total_cost_usd"`
	// CascadeSavingsEstimate is nil until a priced cascade has run. It is an
	// estimate and can be negative (see Server.cascadeSavings).
	CascadeSavingsEstimate *float64 `json:"cascade_savings_estimate_usd"`
}

// Snapshot returns the current numbers for the given configured models.
func (st *Stats) Snapshot(statuses []modelStatus) StatsSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := StatsSnapshot{UptimeS: int64(time.Since(st.started).Seconds()), TotalCost: st.totalCost,
		Models: []ModelSnapshot{}, Clients: []ClientSnapshot{}}
	if st.savingsSeen {
		v := st.savings
		out.CascadeSavingsEstimate = &v
	}
	for _, s := range statuses {
		snap := ModelSnapshot{ID: s.ID, Status: s.Status, Failures: map[string]int{}, Profiles: []string{}}
		if ms := st.models[s.ID]; ms != nil {
			snap.Calls, snap.CostUSD, snap.Cached = ms.calls, ms.cost, ms.cached
			for k, v := range ms.failures {
				snap.Failures[k] = v
			}
			snap.P50MS, snap.P95MS = percentile(ms.latencies, 50), percentile(ms.latencies, 95)
		}
		out.Models = append(out.Models, snap)
	}
	names := make([]string, 0, len(st.clients))
	for n := range st.clients {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := st.clients[n]
		out.Clients = append(out.Clients, ClientSnapshot{Name: n, Requests: c.requests, RateLimited: c.limited, CostUSD: c.cost})
	}
	out.Recent = make([]RecentRequest, len(st.recent))
	for i, r := range st.recent { // newest first
		out.Recent[len(st.recent)-1-i] = r
	}
	return out
}

func percentile(xs []int64, p int) int64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]int64(nil), xs...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	idx := (len(c)*p + 99) / 100 // nearest-rank
	if idx < 1 {
		idx = 1
	}
	return c[idx-1]
}

func buildDetail(r ignatius.Routed) RequestDetail {
	d := RequestDetail{Calls: []CallDetail{}}
	if len(r.Trace) > 0 { // cascade: the trace is the authoritative per-tier record
		for _, h := range r.Trace {
			tier := h.Tier
			c := CallDetail{Tier: &tier, Model: h.Model, OK: h.Error == nil, LatencyMS: h.LatencyMS, Error: safeError(h.Error), Questions: []CallQuestion{}}
			for _, q := range h.Detail {
				c.Questions = append(c.Questions, CallQuestion{ID: q.ID, Confidence: q.Confidence, Threshold: q.Threshold, Escalated: q.Escalated})
			}
			d.Calls = append(d.Calls, c)
		}
		return d
	}
	for _, res := range r.Results {
		c := CallDetail{Model: res.Model, OK: true, LatencyMS: res.LatencyMS, Questions: []CallQuestion{}}
		ids := make([]string, 0, len(res.Answers))
		for id := range res.Answers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			a := res.Answers[id]
			c.Questions = append(c.Questions, CallQuestion{ID: id, Type: a.Type, Confidence: a.Confidence})
		}
		d.Calls = append(d.Calls, c)
	}
	for _, f := range r.Failures {
		d.Calls = append(d.Calls, CallDetail{Model: f.Model, OK: false, LatencyMS: f.LatencyMS, Error: safeError(&f.Error), Questions: []CallQuestion{}})
	}
	if r.Mode == ignatius.ModeFanOut && len(r.Results) >= 2 {
		d.Agreement = agreement(r.Results)
	}
	return d
}

// agreement compares responders per question without keeping what they said.
func agreement(results []ignatius.Result) []Agreement {
	type acc struct {
		typ      string
		n        int
		choices  map[string]bool
		min, max float64
	}
	by := map[string]*acc{}
	for _, res := range results {
		for id, a := range res.Answers {
			x := by[id]
			if x == nil {
				x = &acc{typ: a.Type, choices: map[string]bool{}}
				by[id] = x
			}
			x.n++
			switch a.Type {
			case "choice":
				x.choices[a.Choice] = true
			case "noul", "score":
				v := 0.0
				if a.Type == "noul" && a.Noul != nil {
					v = *a.Noul
				} else if a.Score != nil {
					v = *a.Score
				}
				if x.n == 1 || v < x.min {
					x.min = v
				}
				if x.n == 1 || v > x.max {
					x.max = v
				}
			}
		}
	}
	ids := make([]string, 0, len(by))
	for id := range by {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Agreement, 0, len(ids))
	for _, id := range ids {
		x := by[id]
		ag := Agreement{ID: id, Type: x.typ, Models: x.n}
		if x.typ == "choice" {
			ag.Distinct = len(x.choices)
		} else {
			sp := x.max - x.min
			ag.Spread = &sp
		}
		out = append(out, ag)
	}
	return out
}
