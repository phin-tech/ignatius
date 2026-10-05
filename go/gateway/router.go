package gateway

import (
	"errors"
	"sort"

	"github.com/phin-tech/ignatius/go/ignatius"
)

// Router resolves a model/route string to a Plan: named route, profile, alias, then
// inline grammar (SPEC 6.1, 12). Profile names are rewritten to their real model
// aliases before the plan is returned, so everything downstream sees real models.
type Router struct {
	Reg          ignatius.Registry
	DefaultRoute string
	AllowInline  bool
	MaxInline    int
	rc           *runtimeCfg // the editable part: profiles and routes
}

// ResolveError is a caller mistake (HTTP 422).
type ResolveError struct {
	Message  string
	Routes   []string
	Profiles []string
	Models   []string
	Grammar  string
}

func (e *ResolveError) Error() string { return e.Message }

func NewRouter(cfg Config, reg ignatius.Registry) (*Router, error) {
	plans, err := cfg.plans()
	if err != nil {
		return nil, err
	}
	allow := map[string]bool{}
	for _, c := range cfg.Clients {
		for _, name := range c.Routes {
			allow[name] = true
		}
	}
	rc, err := newRuntime(cfg, reg, plans, allow)
	if err != nil {
		return nil, err
	}
	return &Router{Reg: reg, DefaultRoute: cfg.DefaultRoute, AllowInline: cfg.allowInline(),
		MaxInline: cfg.MaxInlineModels, rc: rc}, nil
}

func (r *Router) RouteNames() []string { return r.rc.snapshot().routeNames() }

func (r *Router) ModelNames() []string {
	n := make([]string, 0, len(r.Reg))
	for k := range r.Reg {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func (r *Router) unknown(msg string) *ResolveError {
	e := &ResolveError{Message: msg, Routes: r.RouteNames(), Profiles: r.rc.snapshot().names(), Models: r.ModelNames()}
	if r.AllowInline {
		e.Grammar = ignatius.InlineGrammar
	}
	return e
}

// Resolve maps a model string to the Plan it names. An empty string or
// "jev-latest" (the official SDK's default) means DefaultRoute. The order is named
// route, profile, alias, then inline grammar; profile names are rewritten to their
// current real aliases in the returned plan.
func (r *Router) Resolve(model string) (ignatius.Plan, error) {
	if model == "" || model == "jev-latest" {
		if r.DefaultRoute == "" {
			return ignatius.Plan{}, r.unknown("no model given and no default_route configured")
		}
		model = r.DefaultRoute
	}
	ps := r.rc.snapshot() // one snapshot per request: an edit mid-request cannot split it
	if p, ok := ps.route[model]; ok {
		return substitute(p, ps), nil
	}
	if t, ok := ps.target[model]; ok {
		return ignatius.Plan{Mode: ignatius.ModeSingle, Model: t}, nil
	}
	if _, ok := r.Reg[model]; ok {
		return ignatius.Plan{Mode: ignatius.ModeSingle, Model: model}, nil
	}
	if r.AllowInline {
		p, err := ignatius.ParseInline(model, r.MaxInline)
		if err == nil {
			return substitute(p, ps), nil
		}
		if !errors.Is(err, ignatius.ErrNotInline) {
			return ignatius.Plan{}, r.unknown(err.Error())
		}
	}
	return ignatius.Plan{}, r.unknown("no such route, profile or model: " + model)
}

// Classify says what a model string names, after default substitution, without
// resolving it: kind is "route", "profile", "alias", "inline" or "unknown".
func (r *Router) Classify(model string) (name, kind string) {
	if model == "" || model == "jev-latest" {
		model = r.DefaultRoute
	}
	if model == "" {
		return "", "unknown"
	}
	ps := r.rc.snapshot()
	if _, ok := ps.route[model]; ok {
		return model, "route"
	}
	if _, ok := ps.target[model]; ok {
		return model, "profile"
	}
	if _, ok := r.Reg[model]; ok {
		return model, "alias"
	}
	if r.AllowInline {
		if _, err := ignatius.ParseInline(model, r.MaxInline); err == nil {
			return model, "inline"
		}
	}
	return model, "unknown"
}
