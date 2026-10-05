package gateway

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
	"github.com/phin-tech/ignatius/go/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Shadow auditing (SPEC 13.7). A cascade lets a cheap tier answer on its own when it is
// confident. To learn whether that confidence was earned without anyone labeling anything, a
// sampled fraction of cascade requests also put the questions a tier settled to the next tier,
// in the background after the response has gone, and record whether the two agreed. The audit
// never changes or delays a response, and it stores no content: no state, no question, no answer
// value, only the models, their confidences and the verdict of the comparison.

// AuditConfig is the [audit] table. Auditing is off unless sample_rate is above zero, and it
// needs a [store] to write to.
type AuditConfig struct {
	// SampleRate is the fraction of cascade requests audited, 0 to 1. Default 0 (off).
	SampleRate float64 `toml:"sample_rate"`
	// MaxInflight bounds the audits running at once; one that finds them all busy is skipped
	// and counted, never queued. Default 4.
	MaxInflight int `toml:"max_inflight"`
}

var (
	auditCounter, _ = gt.meter.Int64Counter("ignatius.audits",
		metric.WithDescription("Shadow-audit comparisons recorded, by the tier that settled the question."))
	auditSkipped, _ = gt.meter.Int64Counter("ignatius.audit.skipped",
		metric.WithDescription("Sampled audits not run, by reason (busy, unhealthy, failed)."))
)

type auditor struct {
	st      store.Store
	reg     ignatius.Registry
	log     *slog.Logger
	rate    float64
	timeout time.Duration
	now     func() time.Time
	rand    func() float64 // a test seam

	sem chan struct{}
	wg  sync.WaitGroup
	mu  sync.Mutex
	off bool // set by close: no new audits
}

func newAuditor(s *Server) *auditor {
	if s.cfg.Audit == nil || s.cfg.Audit.SampleRate <= 0 || s.store == nil {
		return nil
	}
	return &auditor{st: s.store, reg: s.router.Reg, log: s.log, rate: s.cfg.Audit.SampleRate,
		timeout: time.Duration(s.cfg.RequestTimeoutMS) * time.Millisecond, now: func() time.Time { return s.now() }, rand: rand.Float64,
		sem: make(chan struct{}, s.cfg.Audit.MaxInflight)}
}

// settled is one question a non-final tier answered by itself.
type settled struct {
	id     string
	model  string
	conf   *float64
	thresh float64
	answer ignatius.Answer
}

// settledByTier collects, per next-tier model, the questions an earlier tier settled and the
// answers it gave. A question settled at the last tier has no next tier, and one that was
// escalated was already asked of the next tier, so neither is audited.
func settledByTier(plan ignatius.Plan, routed ignatius.Routed) map[string][]settled {
	out := map[string][]settled{}
	for _, hop := range routed.Trace {
		if hop.Error != nil || hop.Tier+1 >= len(plan.Tiers) {
			continue
		}
		next := plan.Tiers[hop.Tier+1].Model
		for _, d := range hop.Detail {
			ans, ok := routed.Answers[d.ID]
			if d.Threshold == nil || d.Escalated || !ok || ans.Model != hop.Model { // not settled here
				continue
			}
			out[next] = append(out[next], settled{id: d.ID, model: hop.Model, conf: d.Confidence, thresh: *d.Threshold, answer: ans})
		}
	}
	return out
}

// maybe samples the request and, if chosen, audits it in the background. It returns at once.
func (a *auditor) maybe(c *client, route, requestID string, req ignatius.Request, plan ignatius.Plan, routed ignatius.Routed) {
	if a == nil || plan.Mode != ignatius.ModeCascade || !routed.OK {
		return
	}
	groups := settledByTier(plan, routed)
	if len(groups) == 0 || a.rand() >= a.rate {
		return
	}
	a.mu.Lock()
	if a.off {
		a.mu.Unlock()
		return
	}
	select {
	case a.sem <- struct{}{}:
		a.wg.Add(1)
	default:
		a.mu.Unlock()
		auditSkipped.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", "busy")))
		return
	}
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		defer func() { <-a.sem }()
		a.run(c.name, route, requestID, req, groups)
	}()
}

func (a *auditor) run(client, route, requestID string, req ignatius.Request, groups map[string][]settled) {
	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()
	ctx = ignatius.WithRouteLabel(ctx, "audit")
	var rows []store.Audit
	for next, qs := range groups {
		// An audit is an ordinary call to the next tier, counted by its breaker. Never spend a
		// half-open probe on one, or poke a tier that is already known to be failing.
		if st := a.reg.BreakerState(next); st != "" && st != "closed" {
			auditSkipped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "unhealthy")))
			continue
		}
		sub := ignatius.Request{State: req.State, Images: req.Images, Questions: map[string]ignatius.Question{}}
		for _, q := range qs {
			sub.Questions[q.id] = req.Questions[q.id]
		}
		res, err := ignatius.Run(ctx, a.reg, sub, ignatius.Plan{Mode: ignatius.ModeSingle, Model: next})
		if err != nil || !res.OK {
			auditSkipped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "failed")))
			continue
		}
		for _, q := range qs {
			other, ok := res.Answers[q.id]
			if !ok {
				continue
			}
			agreed, comparable := agrees(q.answer, other)
			if !comparable {
				continue
			}
			thr := q.thresh
			rows = append(rows, store.Audit{RequestID: requestID, QuestionID: q.id, Client: client, Route: route,
				Type: q.answer.Type, Model: q.model, Confidence: q.conf, Threshold: &thr, AuditModel: next,
				AuditConfidence: other.Confidence, Agreed: agreed, At: a.now()})
			auditCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("model", q.model),
				attribute.String("type", q.answer.Type), attribute.Bool("agreed", agreed)))
		}
	}
	if len(rows) == 0 {
		return
	}
	// Write even if the audit's own deadline is nearly spent: the comparison is the point.
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer wcancel()
	if err := a.st.RecordAudits(wctx, rows); err != nil {
		a.log.Warn("audit: record failed", "request_id", requestID, "error_kind", "store") // err can echo database values
		return
	}
	agreed := 0
	for _, r := range rows {
		if r.Agreed {
			agreed++
		}
	}
	a.log.LogAttrs(ctx, slog.LevelInfo, "audit", slog.String("request_id", requestID), slog.String("client", client),
		slog.Int("questions", len(rows)), slog.Int("agreed", agreed))
}

// close stops new audits and waits for the running ones, until ctx is done.
func (a *auditor) close(ctx context.Context) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.off = true
	a.mu.Unlock()
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		a.log.Warn("audit: shut down with audits still running")
	}
}

// flush waits for the audits running now, for tests and for a caller about to read the store.
func (a *auditor) flush(ctx context.Context) {
	if a == nil {
		return
	}
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// agrees compares two answers to the same question. Both must be of the same type and carry
// a value; otherwise the pair is not comparable and is not recorded. A choice agrees on the
// same option; a noul on the same side of one half; a score on the same nearest level.
func agrees(a, b ignatius.Answer) (agreed, comparable bool) {
	if a.Type != b.Type {
		return false, false
	}
	switch a.Type {
	case "choice":
		if a.Choice == "" || b.Choice == "" {
			return false, false
		}
		return a.Choice == b.Choice, true
	case "noul":
		if a.Noul == nil || b.Noul == nil {
			return false, false
		}
		return (*a.Noul >= 0.5) == (*b.Noul >= 0.5), true
	case "score":
		if a.Score == nil || b.Score == nil {
			return false, false
		}
		return math.Round(*a.Score) == math.Round(*b.Score), true
	}
	return false, false
}
