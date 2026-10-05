package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/phin-tech/ignatius/go/eval"
	"github.com/phin-tech/ignatius/go/ignatius"
)

// Evaluations (SPEC 15): an admin runs a labeled test set through routes and reads the verdict, from the
// status page or the API. It is opt-in and bounded, because a run spends real model calls: one at a time, a
// capped dataset, a capped number of routes. The dataset lives in memory for the run only; a report holds
// numbers and names, never content, and nothing about a run is stored, logged or put in telemetry beyond its
// id, client and counts.

// EvalConfig is the [eval] table.
type EvalConfig struct {
	// Enabled turns the endpoints and the status page's Evaluate panel on. Default off.
	Enabled bool `toml:"enabled"`
	// MaxItems bounds a dataset (default 500). MaxConcurrency bounds items in flight (default 8).
	MaxItems       int `toml:"max_items"`
	MaxConcurrency int `toml:"max_concurrency"`
	// MaxBodyBytes bounds the upload (default 8 MiB), which is separate from max_request_bytes.
	MaxBodyBytes int64 `toml:"max_body_bytes"`
	// Keep is how many finished runs are held in memory (default 10).
	Keep int `toml:"keep"`
}

func (c *EvalConfig) applyDefaults() {
	if c.MaxItems == 0 {
		c.MaxItems = 500
	}
	if c.MaxConcurrency == 0 {
		c.MaxConcurrency = 8
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 8 << 20
	}
	if c.Keep == 0 {
		c.Keep = 10
	}
}

func (c EvalConfig) validate() error {
	if c.MaxItems < 0 || c.MaxConcurrency < 0 || c.MaxBodyBytes < 0 || c.Keep < 0 {
		return errors.New("eval: max_items, max_concurrency, max_body_bytes and keep cannot be negative")
	}
	return nil
}

const maxEvalRoutes = 5

type evalJob struct {
	ID       string       `json:"id"`
	Status   string       `json:"status"` // running | done | failed | canceled
	Client   string       `json:"client"`
	Routes   []string     `json:"routes"`
	Items    int          `json:"items"`
	Sweep    bool         `json:"sweep"`
	Done     int          `json:"done"`
	Total    int          `json:"total"`
	Started  time.Time    `json:"started"`
	Finished *time.Time   `json:"finished,omitempty"`
	Error    string       `json:"error,omitempty"`
	Report   *eval.Report `json:"report,omitempty"`

	cancel context.CancelFunc
}

type evalManager struct {
	mu   sync.Mutex
	jobs []*evalJob // oldest first
	keep int
	wg   sync.WaitGroup
}

// snapshot copies a job for a response (the report, when asked for, is shared and never mutated again).
func (m *evalManager) snapshot(j *evalJob, withReport bool) evalJob {
	c := *j
	c.cancel = nil
	if !withReport {
		c.Report = nil
	}
	return c
}

func (m *evalManager) find(id string) *evalJob {
	for _, j := range m.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (m *evalManager) running() *evalJob {
	for _, j := range m.jobs {
		if j.Status == "running" {
			return j
		}
	}
	return nil
}

// add registers a job, and drops the oldest finished ones beyond keep. It fails if one is already running.
func (m *evalManager) add(j *evalJob) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running() != nil {
		return false
	}
	m.jobs = append(m.jobs, j)
	for finished := 0; ; {
		finished = 0
		for _, x := range m.jobs {
			if x.Status != "running" {
				finished++
			}
		}
		if finished <= m.keep {
			break
		}
		for i, x := range m.jobs {
			if x.Status != "running" {
				m.jobs = append(m.jobs[:i], m.jobs[i+1:]...)
				break
			}
		}
	}
	return true
}

func (m *evalManager) cancelAll() {
	m.mu.Lock()
	for _, j := range m.jobs {
		if j.Status == "running" && j.cancel != nil {
			j.cancel()
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// evalAdmin authorizes an admin caller and checks the feature is on. It writes the refusal itself.
func (s *Server) evalAdmin(w http.ResponseWriter, r *http.Request) (*client, bool) {
	c, ok := s.authorize(w, r)
	if !ok {
		return nil, false
	}
	if !c.admin {
		writeErr(w, http.StatusForbidden, "admin_required", "this key is valid but not an admin key", nil)
		return nil, false
	}
	if s.evals == nil {
		writeErr(w, http.StatusNotFound, "eval_disabled", "evaluations are off: set [eval] enabled = true (a run spends model calls)", nil)
		return nil, false
	}
	return c, true
}

type evalStart struct {
	Routes      []string    `json:"routes"`
	Data        string      `json:"data"`  // JSONL, one item per line
	Items       []eval.Item `json:"items"` // or the items as a JSON array
	Sweep       bool        `json:"sweep"`
	Concurrency int         `json:"concurrency"`
	Misses      *int        `json:"misses"`
}

func (s *Server) handleEvalStart(w http.ResponseWriter, r *http.Request) {
	c, ok := s.evalAdmin(w, r)
	if !ok {
		return
	}
	var in evalStart
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.Eval.MaxBodyBytes))
	if err := dec.Decode(&in); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "the dataset exceeds [eval] max_body_bytes", nil)
		} else {
			writeErr(w, http.StatusBadRequest, "invalid_json", "body must be {routes, data|items, sweep?, concurrency?, misses?}", nil)
		}
		return
	}
	if n := len(in.Routes); n == 0 || n > maxEvalRoutes {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "routes must list between 1 and 5 routes, profiles, aliases or inline routes", nil)
		return
	}
	if (in.Data == "") == (len(in.Items) == 0) {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", `exactly one of "data" (JSONL) and "items" is required`, nil)
		return
	}
	var cands []eval.Candidate
	for _, name := range in.Routes {
		name = strings.TrimSpace(name)
		if !s.routeAllowed(w, c, name) {
			return
		}
		plan, err := s.router.Resolve(name)
		if err != nil {
			s.planError(w, err)
			return
		}
		cands = append(cands, eval.Candidate{Name: name, Plan: plan})
	}
	var items []eval.Item
	if in.Data != "" {
		var err error
		if items, err = eval.ParseJSONL(strings.NewReader(in.Data), s.cfg.Eval.MaxItems); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_dataset", err.Error(), nil)
			return
		}
	} else {
		if len(in.Items) > s.cfg.Eval.MaxItems {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_dataset", "more items than [eval] max_items allows", nil)
			return
		}
		items = in.Items
		for i := range items {
			if err := items[i].Validate(); err != nil {
				writeErr(w, http.StatusUnprocessableEntity, "invalid_dataset", "item "+itoa(i+1)+": "+err.Error(), nil)
				return
			}
		}
	}
	opts := eval.Options{Concurrency: in.Concurrency, Sweep: in.Sweep, ItemTimeout: time.Duration(s.cfg.RequestTimeoutMS) * time.Millisecond}
	if opts.Concurrency <= 0 || opts.Concurrency > s.cfg.Eval.MaxConcurrency {
		opts.Concurrency = min(4, s.cfg.Eval.MaxConcurrency)
		if in.Concurrency > 0 {
			opts.Concurrency = s.cfg.Eval.MaxConcurrency
		}
	}
	if in.Misses != nil {
		opts.Misses = *in.Misses
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := &evalJob{ID: requestID(), Status: "running", Client: c.name, Items: len(items), Sweep: in.Sweep, Started: s.now(), cancel: cancel}
	for _, c := range cands {
		job.Routes = append(job.Routes, c.Name)
	}
	if !s.evals.add(job) {
		cancel()
		writeErr(w, http.StatusConflict, "eval_busy", "an evaluation is already running: wait for it or cancel it", nil)
		return
	}
	opts.Progress = func(done, total int) {
		s.evals.mu.Lock()
		job.Done, job.Total = done, total
		s.evals.mu.Unlock()
	}
	s.log.Info("eval_started", "id", job.ID, "client", c.name, "routes", len(cands), "items", len(items), "sweep", in.Sweep)
	s.evals.wg.Add(1)
	go func() {
		defer s.evals.wg.Done()
		defer cancel()
		rep, err := eval.Run(ctx, s.router.Reg, items, cands, opts)
		status, msg := "done", ""
		switch {
		case errors.Is(err, context.Canceled):
			status = "canceled"
		case err != nil:
			status, msg = "failed", err.Error() // a plan error names a route and a model, never content
		}
		// Log before the status is published, so whoever sees "done" can rely on the line being written.
		s.log.Info("eval_finished", "id", job.ID, "status", status)
		s.evals.mu.Lock()
		now := s.now()
		job.Finished, job.Status, job.Error = &now, status, msg
		if status == "done" {
			job.Report = rep
		}
		s.evals.mu.Unlock()
	}()
	s.evals.mu.Lock()
	snap := s.evals.snapshot(job, false)
	s.evals.mu.Unlock()
	writeJSON(w, http.StatusAccepted, snap)
}

func (s *Server) handleEvalGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.evalAdmin(w, r); !ok {
		return
	}
	s.evals.mu.Lock()
	defer s.evals.mu.Unlock()
	j := s.evals.find(r.PathValue("id"))
	if j == nil {
		writeErr(w, http.StatusNotFound, "unknown_eval", "no such evaluation (finished runs are kept in memory only, and only the most recent)", nil)
		return
	}
	writeJSON(w, http.StatusOK, s.evals.snapshot(j, true))
}

func (s *Server) handleEvalList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.evalAdmin(w, r); !ok {
		return
	}
	s.evals.mu.Lock()
	defer s.evals.mu.Unlock()
	out := make([]evalJob, 0, len(s.evals.jobs))
	for i := len(s.evals.jobs) - 1; i >= 0; i-- { // newest first
		out = append(out, s.evals.snapshot(s.evals.jobs[i], false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"evals": out})
}

// handleEvalDelete cancels a running evaluation, or forgets a finished one.
func (s *Server) handleEvalDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.evalAdmin(w, r); !ok {
		return
	}
	s.evals.mu.Lock()
	defer s.evals.mu.Unlock()
	j := s.evals.find(r.PathValue("id"))
	if j == nil {
		writeErr(w, http.StatusNotFound, "unknown_eval", "no such evaluation", nil)
		return
	}
	if j.Status == "running" {
		j.cancel() // the goroutine marks it canceled
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "canceling"})
		return
	}
	for i, x := range s.evals.jobs {
		if x == j {
			s.evals.jobs = append(s.evals.jobs[:i], s.evals.jobs[i+1:]...)
			break
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

var _ = ignatius.ModeSingle // the plans come from the router; this keeps the import honest if it is trimmed
