package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/phin-tech/ignatius/go/events"
	"github.com/phin-tech/ignatius/go/ignatius"
	"github.com/phin-tech/ignatius/go/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// This file is SPEC 13: the optional store, the feedback endpoint, and retention.
// Privacy rule (SPEC 13.3): stored content is for the store and the admin export only.
// Nothing here puts state, questions or answers in a span, metric, log line or stats.

var feedbackCounter, _ = gt.meter.Int64Counter("ignatius.feedback",
	metric.WithDescription("Feedback verdicts received."))

// Close drains the store's write queue and the event queues (each up to 10 seconds), then
// closes the store.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if s.evals != nil {
		s.evals.cancelAll() // a run in progress is cancelled, not left calling models
	}
	s.auditor.close(ctx) // before the writer and the store: audits write to the store
	if s.writer != nil {
		s.writer.Close(ctx)
	}
	err := s.events.Close(ctx)
	if s.store != nil {
		if e := s.store.Close(); err == nil {
			err = e
		}
	}
	return err
}

// Flush waits until every request recorded so far has been written to the store. It is for
// tests and for a caller about to read the store directly.
func (s *Server) Flush(ctx context.Context) {
	s.auditor.flush(ctx)
	if s.writer != nil {
		s.writer.Flush(ctx)
	}
}

// Store is the open store, or nil when [store] is not configured. Writes are asynchronous:
// call Flush before reading what a request just wrote.
func (s *Server) Store() store.Store { return s.store }

func openStore(cfg Config, getenv func(string) string) (store.Store, error) {
	if cfg.Store == nil {
		return nil, nil
	}
	return store.Open(cfg.Store.Driver, getenv(cfg.Store.DSNEnv))
}

// contentOptIn reports whether the configured client or user stores (and so may emit) content.
func (s *Server) contentOptIn(name string) bool {
	for _, c := range s.cfg.Clients {
		if c.Name == name {
			return c.StoreContent
		}
	}
	for _, u := range s.cfg.Users {
		if u.Name == name {
			return u.StoreContent
		}
	}
	return false
}

// outcomes says, per answered question, which model supplied the final answer and the bar
// a cascade applied to it.
func outcomes(routed ignatius.Routed) []store.Outcome {
	out := make([]store.Outcome, 0, len(routed.Answers))
	for id, a := range routed.Answers {
		o := store.Outcome{QuestionID: id, Type: a.Type, Model: a.Model, Confidence: a.Confidence}
		for _, h := range routed.Trace {
			if h.Model != a.Model {
				continue
			}
			for _, d := range h.Detail {
				if d.ID == id && d.Threshold != nil {
					o.Threshold = d.Threshold
				}
			}
		}
		out = append(out, o)
	}
	return out
}

// requestEvent is the data of an ignatius.request.completed event. Content is present only
// for a client that opted in (SPEC 14.3).
type requestEvent struct {
	RequestID  string          `json:"request_id"`
	Client     string          `json:"client"`
	Layer      string          `json:"layer"`
	Route      string          `json:"route"`
	Mode       string          `json:"mode"`
	OK         bool            `json:"ok"`
	DurationMS int64           `json:"duration_ms"`
	Images     int             `json:"images"` // how many images the request carried; never the images
	CostUSD    *float64        `json:"cost_usd,omitempty"`
	Failures   []string        `json:"failure_kinds,omitempty"`
	Questions  []store.Outcome `json:"questions"`
	Content    *eventContent   `json:"content,omitempty"`
}

type eventContent struct {
	State     any                          `json:"state"`
	Questions map[string]ignatius.Question `json:"questions"`
	Answers   map[string]ignatius.Answer   `json:"answers"`
	Results   []ignatius.Result            `json:"results"`
}

// record hands a finished request to the store's writer and to the event sinks. Neither
// blocks the request, and neither can fail it.
func (s *Server) record(ctx context.Context, c *client, layer, route, id string, req ignatius.Request, plan ignatius.Plan, routed ignatius.Routed, elapsed time.Duration) {
	if s.store == nil && !s.events.Enabled(events.TypeRequestCompleted) {
		return
	}
	optIn := c.root().storeContent
	outs := outcomes(routed)
	if s.store != nil {
		r := store.Request{ID: id, Client: c.name, Layer: layer, Route: route, Mode: routed.Mode, OK: routed.OK,
			At: s.now(), Outcomes: outs, ImageCount: len(req.Images)}
		if optIn {
			var err error
			ct := &store.Content{}
			for _, f := range []struct {
				dst *[]byte
				v   any
			}{{&ct.State, req.State}, {&ct.Questions, req.Questions}, {&ct.Answers, routed.Answers},
				{&ct.Results, routed.Results}, {&ct.Plan, plan}} {
				if *f.dst, err = json.Marshal(f.v); err != nil {
					s.log.WarnContext(ctx, "store: encode", "request_id", id, "error_kind", "encode")
					ct = nil
					break
				}
			}
			r.Content = ct
		}
		if r.Content != nil || !optIn { // an opted-in request that could not be encoded is not stored half-way
			s.writer.Enqueue(r)
		}
	}
	if s.events.Enabled(events.TypeRequestCompleted) {
		d := requestEvent{RequestID: id, Client: c.name, Layer: layer, Route: route, Mode: routed.Mode, OK: routed.OK,
			DurationMS: elapsed.Milliseconds(), Images: len(req.Images), CostUSD: routed.CostUSD, Questions: outs}
		for _, f := range routed.Failures {
			d.Failures = append(d.Failures, f.Error.Kind) // kinds only, as in the log line
		}
		if optIn {
			d.Content = &eventContent{State: req.State, Questions: req.Questions, Answers: routed.Answers, Results: routed.Results}
		}
		s.events.Emit(events.New(events.TypeRequestCompleted, s.now(), d))
	}
}

// ---- POST /v1/ignatius/feedback --------------------------------------------

type feedbackIn struct {
	RequestID  string          `json:"request_id"`
	QuestionID string          `json:"question_id"`
	Verdict    string          `json:"verdict"`
	Correct    json.RawMessage `json:"correct"`
	Source     string          `json:"source"`
}

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if !s.admit(w, c) {
		return
	}
	if s.store == nil {
		writeErr(w, http.StatusNotFound, "feedback_disabled", "this gateway has no [store], so it cannot take feedback", nil)
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in feedbackIn
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "body must be {request_id, question_id, verdict, correct?, source?}", nil)
		return
	}
	switch {
	case in.RequestID == "" || in.QuestionID == "":
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", "request_id and question_id are required", nil)
		return
	case in.Verdict != "good" && in.Verdict != "bad":
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", `verdict must be "good" or "bad"`, nil)
		return
	}
	if in.Source == "" {
		in.Source = "human"
	}
	if in.Source != "human" && in.Source != "automated" {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", `source must be "human" or "automated"`, nil)
		return
	}
	correct := bytes.TrimSpace(in.Correct)
	if len(correct) == 0 || bytes.Equal(correct, []byte("null")) {
		correct = nil
	}

	ctx := r.Context()
	o, owner, hasContent, err := s.store.Outcome(ctx, in.RequestID, in.QuestionID)
	if errors.Is(err, store.ErrNotFound) && s.writer != nil {
		// The request may still be queued for writing: wait for it, once.
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		s.writer.Wait(wctx, in.RequestID)
		cancel()
		o, owner, hasContent, err = s.store.Outcome(ctx, in.RequestID, in.QuestionID)
	}
	if errors.Is(err, store.ErrNotFound) {
		// One that never existed, one past retention and an unknown question look the same.
		writeErr(w, http.StatusNotFound, "unknown_request", "no such request or question", nil)
		return
	}
	if err != nil {
		s.log.WarnContext(ctx, "store: feedback lookup failed", "error_kind", "store")
		writeErr(w, http.StatusInternalServerError, "store_error", "could not read the store", nil)
		return
	}
	if correct != nil {
		// Another client's questions are not shown to this one, so the corrected answer is
		// checked against them only for the client that made the request.
		var ct *store.Content
		if hasContent && owner == c.name {
			ct, _ = s.store.Content(ctx, c.name, in.RequestID)
		}
		if msg := checkCorrect(correct, o.Type, ct, in.QuestionID); msg != "" {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_request", msg, nil)
			return
		}
	}
	err = s.store.AddFeedback(ctx, store.Feedback{RequestID: in.RequestID, QuestionID: in.QuestionID, Client: c.name, Owner: owner,
		Verdict: in.Verdict, Correct: correct, Source: in.Source, At: s.now(),
		Type: o.Type, Model: o.Model, Confidence: o.Confidence, Threshold: o.Threshold})
	if err != nil {
		s.log.WarnContext(ctx, "store: feedback write failed", "error_kind", "store")
		writeErr(w, http.StatusInternalServerError, "store_error", "could not write the store", nil)
		return
	}
	feedbackCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("client", c.name),
		attribute.String("verdict", in.Verdict), attribute.String("source", in.Source)))
	if s.events.Enabled(events.TypeFeedbackReceived) {
		d := feedbackEvent{RequestID: in.RequestID, QuestionID: in.QuestionID, Client: c.name, Owner: owner,
			Verdict: in.Verdict, Source: in.Source, Model: o.Model, Type: o.Type, Confidence: o.Confidence,
			Threshold: o.Threshold, HasCorrect: correct != nil}
		if correct != nil && s.contentOptIn(owner) { // the corrected answer is content about the owner's data
			d.Correct = json.RawMessage(correct)
		}
		s.events.Emit(events.New(events.TypeFeedbackReceived, s.now(), d))
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "feedback", slog.String("request_id", in.RequestID), slog.String("client", c.name),
		slog.String("owner", owner), slog.String("verdict", in.Verdict), slog.String("source", in.Source), slog.Bool("has_correct", correct != nil))
	writeJSON(w, http.StatusOK, map[string]any{"status": "recorded", "request_id": in.RequestID, "question_id": in.QuestionID})
}

// feedbackEvent is the data of an ignatius.feedback.received event.
type feedbackEvent struct {
	RequestID  string          `json:"request_id"`
	QuestionID string          `json:"question_id"`
	Client     string          `json:"client"` // who gave the feedback
	Owner      string          `json:"owner"`  // who made the request
	Verdict    string          `json:"verdict"`
	Source     string          `json:"source"`
	Type       string          `json:"type"`
	Model      string          `json:"model"` // the model that supplied the answer
	Confidence *float64        `json:"confidence"`
	Threshold  *float64        `json:"threshold,omitempty"`
	HasCorrect bool            `json:"has_correct"`
	Correct    json.RawMessage `json:"correct,omitempty"` // only when the owner opted in to content
}

// checkCorrect validates a corrected answer. Without stored content the question's
// criteria are unknown, so only the value's shape (by question type) is checked.
func checkCorrect(raw []byte, qtype string, ct *store.Content, qid string) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "correct must be a JSON value"
	}
	var q ignatius.Question
	haveQ := false
	if ct != nil {
		var qs map[string]ignatius.Question
		if json.Unmarshal(ct.Questions, &qs) == nil {
			q, haveQ = qs[qid]
		}
	}
	switch qtype {
	case "choice":
		label, isStr := v.(string)
		if !isStr {
			return "correct must be the option's label, a string, for a choice question"
		}
		if haveQ {
			found, known := false, false
			switch opts := q.Criteria.(type) {
			case map[string]any: // the wire form: option -> description
				_, found = opts[label]
				known = true
			case []any: // the Python SDK's shorthand: a list of option names
				known = true
				for _, o := range opts {
					found = found || o == label
				}
			}
			if known && !found {
				return "correct is not one of this question's options"
			}
		}
	case "score":
		n, isNum := v.(float64)
		if !isNum {
			return "correct must be a level number for a score question"
		}
		if levels, ok := q.Criteria.([]any); haveQ && ok && (n < 0 || n > float64(len(levels)-1)) {
			return "correct is outside this question's levels"
		}
	case "noul":
		switch x := v.(type) {
		case bool:
		case float64:
			if x < 0 || x > 1 {
				return "correct must be true, false or a probability between 0 and 1 for a noul question"
			}
		default:
			return "correct must be true, false or a probability between 0 and 1 for a noul question"
		}
	}
	return ""
}

// ---- retention -------------------------------------------------------------

// Sweep applies the retention settings once. [store] retention_days deletes everything
// older than that for every client; a client's own retention_days deletes only its stored
// content. It returns what was deleted.
func (s *Server) Sweep(ctx context.Context) (store.PurgeResult, error) {
	var total store.PurgeResult
	if s.store == nil {
		return total, nil
	}
	now := s.now()
	add := func(r store.PurgeResult) {
		total.Requests += r.Requests
		total.Contents += r.Contents
		total.Feedback += r.Feedback
	}
	if d := s.cfg.Store.RetentionDays; d > 0 {
		r, err := s.store.Purge(ctx, store.PurgeFilter{Before: now.AddDate(0, 0, -d)})
		if err != nil {
			return total, err
		}
		add(r)
	}
	days := map[string]int{}
	for _, cc := range s.cfg.Clients {
		days[cc.Name] = cc.RetentionDays
	}
	for _, u := range s.cfg.Users {
		days[u.Name] = u.RetentionDays
	}
	for name, d := range days {
		if d <= 0 {
			continue
		}
		r, err := s.store.Purge(ctx, store.PurgeFilter{Client: name, Before: now.AddDate(0, 0, -d), ContentOnly: true})
		if err != nil {
			return total, err
		}
		add(r)
	}
	return total, nil
}

// StartRetention sweeps now and then every interval until ctx is done. It does nothing
// when there is no store or no retention is configured.
func (s *Server) StartRetention(ctx context.Context, every time.Duration) {
	if s.store == nil || !s.hasRetention() {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			if r, err := s.Sweep(ctx); err != nil {
				s.log.Warn("store: retention sweep failed", "error_kind", "store")
			} else if r.Requests+r.Contents+r.Feedback > 0 {
				s.log.Info("store: retention sweep", "requests", r.Requests, "contents", r.Contents, "feedback", r.Feedback)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (s *Server) hasRetention() bool {
	if s.cfg.Store.RetentionDays > 0 {
		return true
	}
	for _, c := range s.cfg.Clients {
		if c.RetentionDays > 0 {
			return true
		}
	}
	for _, u := range s.cfg.Users {
		if u.RetentionDays > 0 {
			return true
		}
	}
	return false
}
