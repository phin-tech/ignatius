package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/phin-tech/ignatius/go/events"
	"github.com/phin-tech/ignatius/go/ignatius"
	"github.com/phin-tech/ignatius/go/store"
)

//go:embed dashboard.html
var dashboardHTML []byte

type Server struct {
	cfg       Config
	router    *Router
	stats     *Stats
	clients   []*client
	users     map[string]*client // [[users]] principals (11.6)
	userHash  map[string]string
	keys      *keyStore
	dummyHash []byte
	throttle  *throttle
	now       func() time.Time // a test seam for rate limiting
	log       *slog.Logger
	mux       *http.ServeMux
	store     store.Store // nil without a [store] table (SPEC 13)
	writer    *storeWriter
	auditor   *auditor        // nil unless [audit] sample_rate > 0 (SPEC 13.7)
	events    *events.Emitter // nil without [[events.sinks]] (SPEC 14); a nil Emitter is a no-op
	evals     *evalManager    // nil unless [eval] enabled (SPEC 15)
}

// New builds the HTTP server. getenv resolves the client-key env var.
func New(cfg Config, reg ignatius.Registry, getenv func(string) string) (*Server, error) {
	cfg.applyDefaults()
	rt, err := NewRouter(cfg, reg)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, router: rt, stats: NewStats(), now: time.Now, log: slog.Default()}
	if s.clients, err = buildClients(cfg, getenv, s.now()); err != nil {
		return nil, err
	}
	if err := validateAllowlists(s.clients, rt); err != nil {
		return nil, err
	}
	if err := s.initKeys(); err != nil {
		return nil, err
	}
	if s.store, err = openStore(cfg, getenv); err != nil {
		return nil, err
	}
	if s.store != nil {
		s.writer = newStoreWriter(s.store, cfg.Store.QueueSize, s.log)
	}
	s.auditor = newAuditor(s)
	if cfg.Eval.Enabled {
		s.evals = &evalManager{keep: cfg.Eval.Keep}
	}
	if len(cfg.Events.Sinks) > 0 {
		if s.events, err = events.Build(cfg.Events, getenv, s.log); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST /v1/systemone", s.traced("L1", s.handleSystemOne))
	s.mux.HandleFunc("POST /v1/route", s.traced("L2", s.handleRoute))
	s.mux.HandleFunc("POST /v1/ignatius/feedback", s.handleFeedback)
	s.mux.HandleFunc("POST /v1/evals", s.handleEvalStart)
	s.mux.HandleFunc("GET /v1/evals", s.handleEvalList)
	s.mux.HandleFunc("GET /v1/evals/{id}", s.handleEvalGet)
	s.mux.HandleFunc("DELETE /v1/evals/{id}", s.handleEvalDelete)
	s.mux.HandleFunc("GET /v1/models", s.handleModels)
	s.mux.HandleFunc("GET /v1/routes", s.handleRoutes)
	s.mux.HandleFunc("GET /v1/stats", s.handleStats)
	s.mux.HandleFunc("GET /v1/profiles", s.handleProfilesGet)
	s.mux.HandleFunc("PUT /v1/profiles/{name}", s.handleProfilePut)
	s.mux.HandleFunc("DELETE /v1/profiles/{name}", s.handleProfileDelete)
	s.mux.HandleFunc("PUT /v1/routes/{name}", s.handleRoutePut)
	s.mux.HandleFunc("DELETE /v1/routes/{name}", s.handleRouteDelete)
	s.mux.HandleFunc("GET /v1/login", s.handleLoginInfo)
	s.mux.HandleFunc("POST /v1/login", s.handleLogin)
	s.mux.HandleFunc("POST /v1/logout", s.handleLogout)
	s.mux.HandleFunc("GET /v1/keys", s.handleKeyList)
	s.mux.HandleFunc("POST /v1/keys", s.handleKeyCreate)
	s.mux.HandleFunc("DELETE /v1/keys/{id}", s.handleKeyRevoke)
	s.mux.HandleFunc("GET /{$}", s.handleDashboard)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	return s, nil
}

// HasClientKeys reports whether callers must authenticate: any configured key or user.
func (s *Server) HasClientKeys() bool                              { return len(s.clients) > 0 || len(s.users) > 0 }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }
func (s *Server) Router() *Router                                  { return s.router }

// lookup identifies the caller from the Authorization header without writing a
// response. With no keys configured everything is the anonymous admin client (main
// refuses that on a non-loopback address). The returned reason is "" on success,
// else "missing" (no Bearer credential, HTTP 403) or "invalid" (wrong token, 401),
// matching Decis and Jev. The client's key is never kept, only compared.
func (s *Server) lookup(r *http.Request) (c *client, reason string) {
	if !s.HasClientKeys() {
		return anonymous, ""
	}
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil, "missing"
	}
	var match *client
	for _, c := range s.clients { // no early exit: compare against every key
		if subtle.ConstantTimeCompare([]byte(token), []byte(c.key)) == 1 {
			match = c
		}
	}
	if match == nil { // a runtime key or a login session
		match = s.keys.find(token, s.now())
	}
	if match == nil {
		return nil, "invalid"
	}
	return match, ""
}

// authorize writes the 403/401 and returns false when the caller is not recognised.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (*client, bool) {
	c, reason := s.lookup(r)
	switch reason {
	case "missing":
		writeErr(w, http.StatusForbidden, "missing_credentials", "a Bearer token is required", nil)
	case "invalid":
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "invalid API key", nil)
	}
	return c, reason == ""
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit", nil)
		} else {
			writeErr(w, http.StatusBadRequest, "bad_request", "could not read request body", nil)
		}
		return nil, false
	}
	return body, true
}

func (s *Server) run(r *http.Request, req ignatius.Request, plan ignatius.Plan) (ignatius.Routed, error) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.RequestTimeoutMS)*time.Millisecond)
	defer cancel()
	ctx = ignatius.WithRouteLabel(ctx, infoFrom(r.Context()).route)
	return ignatius.Run(ctx, s.router.Reg, req, plan)
}

func (s *Server) planError(w http.ResponseWriter, err error) {
	var pe *ignatius.PlanError
	var re *ResolveError
	switch {
	case errors.As(err, &pe):
		writeErr(w, http.StatusUnprocessableEntity, "invalid_plan", pe.Error(), map[string]any{"problems": pe.Problems})
	case errors.As(err, &re):
		writeErr(w, http.StatusUnprocessableEntity, "unknown_model", re.Message,
			map[string]any{"routes": re.Routes, "profiles": re.Profiles, "models": re.Models, "inline_grammar": re.Grammar})
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error(), nil)
	}
}

// ---- L1: Jev-compatible ----------------------------------------------------

type wireAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

type source struct {
	Model        string   `json:"model"`
	Contributors []string `json:"contributors,omitempty"`
}

func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	info := infoFrom(r.Context())
	info.client = c.name
	if !s.admit(w, c) {
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		State     any                          `json:"state"`
		Images    []string                     `json:"images"`
		Model     string                       `json:"model"`
		Questions map[string]ignatius.Question `json:"questions"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "request body must be a JSON object with state, model and questions", nil)
		return
	}
	req := ignatius.Request{State: in.State, Images: in.Images, Questions: in.Questions}
	if err := req.Validate(); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", err.Error(), nil)
		return
	}
	info.route = s.routeLabel(in.Model)
	if !s.routeAllowed(w, c, in.Model) {
		return
	}
	plan, err := s.router.Resolve(in.Model)
	if err != nil {
		s.planError(w, err)
		return
	}
	if plan.Mode == ignatius.ModeFanOut && (plan.Reduce == "" || plan.Reduce == "none") {
		writeErr(w, http.StatusUnprocessableEntity, "reducer_required",
			"fan_out through /v1/systemone must reduce to one answer; set reduce, or use /v1/route", nil)
		return
	}
	start := time.Now()
	routed, err := s.run(r, req, plan)
	if err != nil {
		s.planError(w, err)
		return
	}
	info.noteRouted(routed)
	id := info.requestID
	s.stats.Record("L1", in.Model, id, c.name, routed, time.Since(start), s.cascadeSavings(plan, routed))
	s.record(r.Context(), c, "L1", info.route, id, req, plan, routed, time.Since(start))
	s.auditor.maybe(c, info.route, id, req, plan, routed)
	if !routed.OK && len(req.Images) > 0 && allUnsupported(routed.Failures) {
		// Nothing was wrong upstream: no model on this route takes this many images. That is the
		// caller's request, not a gateway failure, so it is not a 5xx a client would retry.
		writeErr(w, http.StatusUnprocessableEntity, "images_not_supported",
			"no model on this route takes this request's images (see the failures, and the model's [limits])",
			map[string]any{"failures": routed.Failures, "trace": routed.Trace})
		return
	}
	if !routed.OK { // the Jev contract needs an answer for every question
		writeErr(w, http.StatusBadGateway, "routing_failed", "could not produce an answer for every question",
			map[string]any{"failures": routed.Failures, "trace": routed.Trace, "answered": len(routed.Answers)})
		return
	}
	answers := map[string]wireAnswer{}
	sources := map[string]source{}
	var usage ignatius.Usage
	for id, a := range routed.Answers {
		wa := wireAnswer{Type: a.Type, Noul: a.Noul, Choice: a.Choice, Score: a.Score, Probabilities: a.Probabilities, Legend: a.Legend}
		if a.Type != "noul" { // the contract gives noul no confidence field
			wa.Confidence = a.Confidence
		}
		answers[id] = wa
		sources[id] = source{Model: a.Model, Contributors: a.Contributors}
	}
	for _, res := range routed.Results {
		usage.InputTokens += res.Usage.InputTokens
		usage.OutputTokens += res.Usage.OutputTokens
	}
	w.Header().Set("X-Request-Id", id)
	writeJSON(w, http.StatusOK, map[string]any{
		"model": in.Model, "answers": answers, "usage": usage,
		"ignatius": map[string]any{
			"request_id": id, "mode": routed.Mode, "sources": sources,
			"trace": routed.Trace, "failures": routed.Failures,
		},
	})
}

// ---- L2: native ------------------------------------------------------------

func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	info := infoFrom(r.Context())
	info.client = c.name
	if !s.admit(w, c) {
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		Request ignatius.Request `json:"request"`
		Plan    *ignatius.Plan   `json:"plan"`
		Route   string           `json:"route"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "body must be {request, plan|route}: "+err.Error(), nil)
		return
	}
	if (in.Plan == nil) == (in.Route == "") {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", `exactly one of "plan" and "route" is required`, nil)
		return
	}
	if err := in.Request.Validate(); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_request", err.Error(), nil)
		return
	}
	var plan ignatius.Plan
	if in.Plan != nil {
		info.route = "plan"
		if c.routes != nil { // an explicit plan could name anything
			writeErr(w, http.StatusForbidden, "route_not_allowed",
				fmt.Sprintf("client %q is limited to named routes and may not send a plan", c.name), nil)
			return
		}
		plan = *in.Plan
	} else {
		info.route = s.routeLabel(in.Route)
		if !s.routeAllowed(w, c, in.Route) {
			return
		}
		var err error
		if plan, err = s.router.Resolve(in.Route); err != nil {
			s.planError(w, err)
			return
		}
	}
	start := time.Now()
	routed, err := s.run(r, in.Request, plan)
	if err != nil {
		s.planError(w, err)
		return
	}
	info.noteRouted(routed)
	id := info.requestID
	label := in.Route
	if label == "" {
		label = "plan"
	}
	s.stats.Record("L2", label, id, c.name, routed, time.Since(start), s.cascadeSavings(plan, routed))
	s.record(r.Context(), c, "L2", info.route, id, in.Request, plan, routed, time.Since(start))
	s.auditor.maybe(c, info.route, id, in.Request, plan, routed)
	w.Header().Set("X-Request-Id", id)
	writeJSON(w, http.StatusOK, struct {
		ignatius.Routed
		RequestID string `json:"request_id"`
	}{routed, id})
}

// ---- discovery and health --------------------------------------------------

type modelStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ready | not_ready | unknown | unsupported
}

func (s *Server) statuses(ctx context.Context) []modelStatus {
	names := s.router.ModelNames()
	out := make([]modelStatus, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = modelStatus{ID: n, Status: s.router.Reg.Status(ctx, n)}
		}()
	}
	wg.Wait()
	return out
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	all := s.statuses(r.Context())
	shown := make([]modelStatus, 0, len(all))
	for _, m := range all {
		if c.visible(m.ID) { // a restricted client sees only the models it was given
			shown = append(shown, m)
		}
	}
	def := ""
	if c.visible(s.cfg.DefaultRoute) {
		def = s.cfg.DefaultRoute
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "default": def, "data": shown})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if !c.admin { // stats show other clients' traffic metadata
		writeErr(w, http.StatusForbidden, "admin_required", "this key is valid but not an admin key", nil)
		return
	}
	snap := s.stats.Snapshot(s.statuses(r.Context()))
	snap.EvalEnabled = s.evals != nil
	s.describeModels(snap.Models)
	writeJSON(w, http.StatusOK, snap)
}

// describeModels says what each row is, so an alias like "cheap" is not a mystery: the
// upstream model it asks for, the host it talks to, and the profiles that point at it.
// The host is host:port only: the path, query and any credentials in the URL stay out.
func (s *Server) describeModels(models []ModelSnapshot) {
	byTarget := map[string][]string{}
	ps := s.router.rc.snapshot()
	for _, name := range ps.names() {
		t := ps.target[name]
		byTarget[t] = append(byTarget[t], name)
	}
	for i := range models {
		mc := s.cfg.Models[models[i].ID]
		models[i].Provider, models[i].Upstream = mc.Provider, mc.Model
		if u, err := url.Parse(mc.BaseURL); err == nil {
			models[i].Host = u.Host
		}
		if p := byTarget[models[i].ID]; p != nil {
			models[i].Profiles = p
		}
	}
}

// cascadeSavings estimates what a cascade saved against sending the first call's
// token counts straight to the plan's final tier at that tier's prices. It is an
// estimate (models tokenize differently) and can be negative. Nil if not a
// cascade or the final tier has no price.
func (s *Server) cascadeSavings(plan ignatius.Plan, routed ignatius.Routed) *float64 {
	if plan.Mode != ignatius.ModeCascade || len(plan.Tiers) == 0 || len(routed.Results) == 0 {
		return nil
	}
	// The baseline needs the first tier's real token counts. Skip the request when
	// tier 0 did not actually run (it failed or its breaker was open, so Results[0]
	// is a later tier) or was served from the cache (usage 0).
	if first := routed.Results[0]; first.Model != plan.Tiers[0].Model || first.Cached > 0 {
		return nil
	}
	last, ok := s.cfg.Models[plan.Tiers[len(plan.Tiers)-1].Model]
	if !ok || (last.PriceInputPerMTok == nil && last.PriceOutputPerMTok == nil) {
		return nil
	}
	var in, out float64
	if last.PriceInputPerMTok != nil {
		in = *last.PriceInputPerMTok
	}
	if last.PriceOutputPerMTok != nil {
		out = *last.PriceOutputPerMTok
	}
	u := routed.Results[0].Usage
	baseline := float64(u.InputTokens)*in/1e6 + float64(u.OutputTokens)*out/1e6
	actual := 0.0
	if routed.CostUSD != nil {
		actual = *routed.CostUSD
	}
	saved := baseline - actual
	return &saved
}

// handleDashboard serves the static status page. The page holds no data: it
// fetches /v1/stats and /v1/routes with the viewer's own key.
func (s *Server) handleDashboard(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(dashboardHTML)
}

// handleReady is 200 unless every model is known not to be available. It is
// unauthenticated (orchestrator probes), so it reports counts only, never names.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	st := s.statuses(r.Context())
	ready := 0
	for _, m := range st {
		if m.Status == "ready" || m.Status == "unknown" {
			ready++
		}
	}
	code := http.StatusServiceUnavailable
	if ready > 0 {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]int{"ready": ready, "total": len(st)})
}

func requestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, typ, msg string, extra map[string]any) {
	e := map[string]any{"type": typ, "message": msg}
	for k, v := range extra {
		e[k] = v
	}
	writeJSON(w, code, map[string]any{"error": e})
}

// allUnsupported reports whether there are failures and every one is of kind "unsupported".
func allUnsupported(fs []ignatius.Failure) bool {
	if len(fs) == 0 {
		return false
	}
	for _, f := range fs {
		if f.Error.Kind != ignatius.KindUnsupported {
			return false
		}
	}
	return true
}
