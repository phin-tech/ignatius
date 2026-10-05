package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
)

// The runtime config is the part of the gateway an operator may change while it runs:
// PROFILES (an intent name pointing at a model alias) and ROUTES (a named plan). The
// config file stays the source of truth. Edits are overrides on top of it, held in one
// immutable snapshot that is swapped atomically, so a request never sees half an edit
// and reads take no lock (SPEC 12).
//
// Profile names are rewritten to the real model alias BEFORE a plan runs, so the circuit
// breaker, the cache, cost accounting, traces and stats all see the real model, never a
// duplicate under the profile's name.
//
// Models (URLs, keys) and clients (keys, access) are NOT editable at runtime: they carry
// secrets and access control and belong in the config.

const (
	maxProfiles   = 64
	maxRoutes     = 64 // runtime-added routes; config-defined ones are not counted
	maxPlanModels = 10
	maxChangeLog  = 50
	maxTimeoutMS  = 10 * 60 * 1000
)

// A runtime-created name is a safe, bounded token: it never contains the inline-route
// separators (: , > @ |), so inline routes stay unambiguous, and it is a metric label.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// These are metric label values or protocol markers, so no profile or new route may take them.
var reservedNames = map[string]bool{"inline": true, "plan": true, "unknown": true, "none": true, "jev-latest": true}

var (
	ErrEditingDisabled = errors.New("editing is disabled (set [admin] edit_profiles / edit_routes = true)")
	ErrVersionConflict = errors.New("the configuration changed since you loaded it")
	ErrNotRemovable    = errors.New("this is defined in the config file; change it, or edit the config to remove it")
)

// EditError is a caller mistake (HTTP 422).
type EditError struct{ Msg string }

func (e *EditError) Error() string { return e.Msg }

func bad(format string, a ...any) error { return &EditError{fmt.Sprintf(format, a...)} }

// cfgSnapshot is one immutable view of the editable config.
type cfgSnapshot struct {
	version     uint64
	target      map[string]string        // profile -> model alias
	source      map[string]string        // profile -> "config" | "override" | "added"
	route       map[string]ignatius.Plan // route name -> plan, as authored (profile names unresolved)
	routeSource map[string]string        // route -> "config" | "override" | "added"
}

func sortedKeys[V any](m map[string]V) []string {
	n := make([]string, 0, len(m))
	for k := range m {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func (s *cfgSnapshot) names() []string      { return sortedKeys(s.target) }
func (s *cfgSnapshot) routeNames() []string { return sortedKeys(s.route) }

// ConfigChange is one audited edit.
type ConfigChange struct {
	Time    time.Time `json:"time"`
	Actor   string    `json:"actor"`
	Kind    string    `json:"kind"` // profile | route
	Op      string    `json:"op"`   // set | remove
	Name    string    `json:"name"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to,omitempty"`
	Version uint64    `json:"version"`
}

type runtimeCfg struct {
	cur          atomic.Pointer[cfgSnapshot]
	mu           sync.Mutex // serializes writers
	baseProfiles map[string]string
	baseRoutes   map[string]ignatius.Plan
	editProfiles bool
	editRoutes   bool
	statePath    string
	defaultRoute string
	allowlisted  map[string]bool // names that a client's route allowlist mentions
	changes      []ConfigChange  // newest last, bounded; guarded by mu
	stale        []string        // "kind:name" overrides that no longer apply, set at load
	reg          ignatius.Registry
	now          func() time.Time
}

func planJSON(p ignatius.Plan) string {
	b, _ := json.Marshal(p)
	return string(b)
}

func cloneThreshold(t *ignatius.Threshold) *ignatius.Threshold {
	if t == nil {
		return nil
	}
	out := &ignatius.Threshold{}
	if t.Default != nil {
		v := *t.Default
		out.Default = &v
	}
	if t.ByType != nil {
		out.ByType = make(map[string]float64, len(t.ByType))
		for k, v := range t.ByType {
			out.ByType[k] = v
		}
	}
	return out
}

// clonePlan copies a plan deeply: snapshots must never share mutable state with a caller.
func clonePlan(p ignatius.Plan) ignatius.Plan {
	out := p
	out.Models = append([]string(nil), p.Models...)
	out.Threshold = cloneThreshold(p.Threshold)
	if p.Tiers != nil {
		out.Tiers = make([]ignatius.Tier, len(p.Tiers))
		for i, t := range p.Tiers {
			t.Threshold = cloneThreshold(t.Threshold)
			out.Tiers[i] = t
		}
	}
	return out
}

func copySnapshot(s *cfgSnapshot) *cfgSnapshot {
	n := &cfgSnapshot{version: s.version, target: map[string]string{}, source: map[string]string{},
		route: map[string]ignatius.Plan{}, routeSource: map[string]string{}}
	for k, v := range s.target {
		n.target[k] = v
	}
	for k, v := range s.source {
		n.source[k] = v
	}
	for k, v := range s.route {
		n.route[k] = clonePlan(v)
	}
	for k, v := range s.routeSource {
		n.routeSource[k] = v
	}
	return n
}

// validateProfile checks one profile's name and target against what exists.
func validateProfile(name, target string, reg ignatius.Registry, routes map[string]ignatius.Plan) error {
	switch {
	case !nameRE.MatchString(name):
		return bad("profile name %q must be lowercase letters, digits, '_', '.' or '-' (at most 64 characters, starting with a letter or digit)", name)
	case reservedNames[name]:
		return bad("profile name %q is reserved", name)
	}
	if _, isAlias := reg[name]; isAlias {
		return bad("profile %q clashes with a model alias of the same name", name)
	}
	if _, isRoute := routes[name]; isRoute {
		return bad("profile %q clashes with a route of the same name", name)
	}
	if _, ok := reg[target]; !ok {
		return bad("profile %q targets %q, which is not a model alias (profiles point at models, not at other profiles or routes)", name, target)
	}
	return nil
}

// checkThreshold is a range check the plan validator does not do.
func checkThreshold(t *ignatius.Threshold, where string) error {
	if t == nil {
		return nil
	}
	in := func(v float64) bool { return v >= 0 && v <= 1 }
	if t.Default != nil && !in(*t.Default) {
		return bad("%s: threshold must be between 0 and 1", where)
	}
	for k, v := range t.ByType {
		if k != "noul" && k != "choice" && k != "score" {
			return bad("%s: threshold keys are noul, choice and score, not %q", where, k)
		}
		if !in(v) {
			return bad("%s: the %s threshold must be between 0 and 1", where, k)
		}
	}
	return nil
}

// checkPlan are the limits an edited plan must meet on top of ignatius.Plan.Validate.
func checkPlan(p ignatius.Plan) error {
	if n := len(p.Models) + len(p.Tiers); n > maxPlanModels {
		return bad("a route may name at most %d models, got %d", maxPlanModels, n)
	}
	if err := checkThreshold(p.Threshold, "route"); err != nil {
		return err
	}
	for i, t := range p.Tiers {
		if err := checkThreshold(t.Threshold, fmt.Sprintf("tier %d", i+1)); err != nil {
			return err
		}
	}
	if p.MinSuccess < 0 || (p.Mode == ignatius.ModeFanOut && p.MinSuccess > len(p.Models)) {
		return bad("min_success must be between 0 and the number of models")
	}
	if p.TimeoutMS < 0 || p.TimeoutMS > maxTimeoutMS {
		return bad("timeout_ms must be between 0 and %d", maxTimeoutMS)
	}
	return nil
}

// validate is the single gate every snapshot passes: at startup, on every edit, and when
// saved overrides are replayed. It checks the WHOLE snapshot, because a profile edit can
// break a route, a route edit can clash with a profile, and a delete can orphan the
// default route.
func (r *runtimeCfg) validate(ps *cfgSnapshot) error {
	if len(ps.target) > maxProfiles {
		return bad("at most %d profiles are allowed, got %d", maxProfiles, len(ps.target))
	}
	for _, name := range ps.names() {
		if err := validateProfile(name, ps.target[name], r.reg, ps.route); err != nil {
			return err
		}
	}
	added := 0
	for _, name := range ps.routeNames() {
		plan := ps.route[name]
		if ps.routeSource[name] == "added" {
			added++
			switch {
			case !nameRE.MatchString(name):
				return bad("route name %q must be lowercase letters, digits, '_', '.' or '-' (at most 64 characters, starting with a letter or digit)", name)
			case reservedNames[name]:
				return bad("route name %q is reserved", name)
			}
			if _, isAlias := r.reg[name]; isAlias {
				return bad("route %q clashes with a model alias of the same name", name)
			}
		}
		// a route/profile name clash is caught above, from the profile side
		if err := substitute(plan, ps).Validate(r.reg); err != nil {
			return bad("route %q is invalid: %v", name, err)
		}
		// The extra limits apply to routes created or changed at runtime ONLY. A config-defined
		// route is held to the plan rules alone, exactly as before editing existed, so a config
		// that loads today still loads (a threshold above 1, for instance, is a valid way to
		// force escalation). Reverting an override restores the config plan, which is
		// config-sourced again and so exempt.
		if ps.routeSource[name] != "config" {
			if err := checkPlan(plan); err != nil {
				return bad("route %q: %v", name, err)
			}
		}
	}
	if added > maxRoutes {
		return bad("at most %d routes can be added at runtime", maxRoutes)
	}
	if d := r.defaultRoute; d != "" {
		_, isRoute := ps.route[d]
		_, isProfile := ps.target[d]
		_, isModel := r.reg[d]
		if !isRoute && !isProfile && !isModel {
			return bad("default_route %q is not a route, profile or model", d)
		}
	}
	return nil
}

func newRuntime(cfg Config, reg ignatius.Registry, routes map[string]ignatius.Plan, allowlisted map[string]bool) (*runtimeCfg, error) {
	r := &runtimeCfg{baseProfiles: map[string]string{}, baseRoutes: map[string]ignatius.Plan{},
		editProfiles: cfg.Admin.EditProfiles, editRoutes: cfg.Admin.EditRoutes, statePath: cfg.Admin.StateFile,
		defaultRoute: cfg.DefaultRoute, allowlisted: allowlisted, reg: reg, now: time.Now}
	ps := &cfgSnapshot{target: map[string]string{}, source: map[string]string{}, route: map[string]ignatius.Plan{}, routeSource: map[string]string{}}
	for name, target := range cfg.Profiles {
		r.baseProfiles[name], ps.target[name], ps.source[name] = target, target, "config"
	}
	for name, plan := range routes {
		r.baseRoutes[name], ps.route[name], ps.routeSource[name] = clonePlan(plan), clonePlan(plan), "config"
	}
	if err := r.validate(ps); err != nil { // the config itself must be valid: this is strict
		return nil, err
	}
	if r.statePath != "" {
		if err := r.loadOverrides(ps); err != nil {
			return nil, err
		}
	}
	r.cur.Store(ps)
	return r, nil
}

type stateFile struct {
	Version  uint64                   `json:"version"`
	Profiles map[string]string        `json:"profiles"`
	Routes   map[string]ignatius.Plan `json:"routes"`
}

// loadOverrides replays the state file on top of the config. Unlike the config, it is
// LENIENT: an override that no longer validates (its model was removed, or it would break
// a route) is skipped and reported as stale, never fatal, so a stale edit can never lock
// the operator out. A corrupt file is an error: silently ignoring it could hide a problem.
func (r *runtimeCfg) loadOverrides(ps *cfgSnapshot) error {
	raw, err := os.ReadFile(r.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("saved config %s: %w", r.statePath, err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return fmt.Errorf("saved config %s: %w", r.statePath, err)
	}
	ps.version = sf.Version
	try := func(kind, name string, apply func(*cfgSnapshot)) {
		trial := copySnapshot(ps)
		apply(trial)
		if r.validate(trial) != nil { // each override is judged against the ones applied before it
			r.stale = append(r.stale, kind+":"+name)
			return
		}
		apply(ps)
	}
	for _, name := range sortedKeys(sf.Profiles) { // profiles first: a route may use a runtime-added one
		target := sf.Profiles[name]
		try("profile", name, func(s *cfgSnapshot) {
			s.target[name] = target
			if _, inConfig := r.baseProfiles[name]; inConfig {
				s.source[name] = "override"
			} else {
				s.source[name] = "added"
			}
		})
	}
	for _, name := range sortedKeys(sf.Routes) {
		plan := sf.Routes[name]
		try("route", name, func(s *cfgSnapshot) {
			s.route[name] = clonePlan(plan)
			if _, inConfig := r.baseRoutes[name]; inConfig {
				s.routeSource[name] = "override"
			} else {
				s.routeSource[name] = "added"
			}
		})
	}
	return nil
}

// persist writes only what differs from the config, atomically (temp file + rename, 0600).
// No state path means edits live in memory only.
func (r *runtimeCfg) persist(ps *cfgSnapshot) error {
	if r.statePath == "" {
		return nil
	}
	sf := stateFile{Version: ps.version, Profiles: map[string]string{}, Routes: map[string]ignatius.Plan{}}
	for name, t := range ps.target {
		if b, inConfig := r.baseProfiles[name]; !inConfig || b != t {
			sf.Profiles[name] = t
		}
	}
	for name, p := range ps.route {
		if b, inConfig := r.baseRoutes[name]; !inConfig || planJSON(b) != planJSON(p) {
			sf.Routes[name] = p
		}
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.statePath), ".ignatius-*.tmp")
	if err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	defer os.Remove(tmp.Name()) // a no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.statePath)
}

func (r *runtimeCfg) snapshot() *cfgSnapshot { return r.cur.Load() }

func (r *runtimeCfg) recentChanges() []ConfigChange {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ConfigChange, len(r.changes))
	for i, c := range r.changes { // newest first
		out[len(r.changes)-1-i] = c
	}
	return out
}

func (r *runtimeCfg) staleOverrides() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stale...)
}

// edit runs one change under the writer lock: check the version, build the next snapshot,
// validate the WHOLE of it, persist, and only then swap it in. A failed save therefore
// leaves memory and disk agreeing, and a refused edit changes nothing.
func (r *runtimeCfg) edit(actor, kind, op, name string, expected *uint64,
	mutate func(cur, next *cfgSnapshot) (from, to string, changed bool, err error)) (ConfigChange, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.cur.Load()
	if expected != nil && *expected != cur.version {
		return ConfigChange{}, false, ErrVersionConflict
	}
	next := copySnapshot(cur)
	from, to, changed, err := mutate(cur, next)
	if err != nil {
		return ConfigChange{}, false, err
	}
	c := ConfigChange{Time: r.now(), Actor: actor, Kind: kind, Op: op, Name: name, From: from, To: to, Version: cur.version}
	if !changed {
		return c, false, nil // nothing to do
	}
	next.version++
	c.Version = next.version
	if err := r.validate(next); err != nil {
		var ee *EditError
		if kind == "profile" && errors.As(err, &ee) { // say the EDIT is what breaks it
			return ConfigChange{}, false, bad("this change would break the configuration: %s", ee.Msg)
		}
		return ConfigChange{}, false, err
	}
	if err := r.persist(next); err != nil {
		return ConfigChange{}, false, err
	}
	r.cur.Store(next)
	r.changes = append(r.changes, c)
	if len(r.changes) > maxChangeLog {
		r.changes = r.changes[len(r.changes)-maxChangeLog:]
	}
	return c, true, nil
}

// refuseIfAllowlisted stops a delete that would make the next startup fail: client route
// allowlists are validated at startup, so an entry naming something that no longer exists
// would stop the gateway from booting.
func (r *runtimeCfg) refuseIfAllowlisted(name string) error {
	if r.allowlisted[name] {
		return bad("%q is named in a client's route allowlist; remove it from that client in the config first", name)
	}
	return nil
}

// ---- profiles ----------------------------------------------------------------------------

// setProfile points a profile at a model, creating it if new.
func (r *runtimeCfg) setProfile(actor, name, target string, expected *uint64) (ConfigChange, bool, error) {
	if !r.editProfiles {
		return ConfigChange{}, false, ErrEditingDisabled
	}
	return r.edit(actor, "profile", "set", name, expected, func(cur, next *cfgSnapshot) (string, string, bool, error) {
		from, existed := cur.target[name]
		if existed && from == target {
			return from, target, false, nil
		}
		next.target[name] = target
		switch b, inConfig := r.baseProfiles[name]; {
		case inConfig && b == target:
			next.source[name] = "config" // retargeted back to the config value
		case inConfig:
			next.source[name] = "override"
		default:
			next.source[name] = "added"
		}
		return from, target, true, nil
	})
}

// removeProfile drops a runtime-added profile, or reverts an overridden one to its config
// value. A profile that comes from the config and has no override cannot be removed.
func (r *runtimeCfg) removeProfile(actor, name string, expected *uint64) (ConfigChange, bool, error) {
	if !r.editProfiles {
		return ConfigChange{}, false, ErrEditingDisabled
	}
	return r.edit(actor, "profile", "remove", name, expected, func(cur, next *cfgSnapshot) (string, string, bool, error) {
		from, ok := cur.target[name]
		if !ok {
			return "", "", false, bad("no such profile %q", name)
		}
		if cur.source[name] == "config" {
			return "", "", false, ErrNotRemovable
		}
		if b, inConfig := r.baseProfiles[name]; inConfig { // revert
			next.target[name], next.source[name] = b, "config"
			return from, b, true, nil
		}
		if err := r.refuseIfAllowlisted(name); err != nil {
			return "", "", false, err
		}
		delete(next.target, name)
		delete(next.source, name)
		return from, "", true, nil
	})
}

// ---- routes ------------------------------------------------------------------------------

// planSummary is a one-line human description of a plan, for the audit trail.
func planSummary(p ignatius.Plan) string {
	thr := func(t *ignatius.Threshold) string {
		switch {
		case t == nil:
			return ""
		case t.Default != nil:
			return fmt.Sprintf("@%.2f", *t.Default)
		}
		return "@per-type"
	}
	switch p.Mode {
	case ignatius.ModeSingle:
		return "single " + p.Model
	case ignatius.ModeFanOut:
		red := p.Reduce
		if red == "" {
			red = "none"
		}
		return "fan_out " + strings.Join(p.Models, ",") + " | " + red
	default:
		parts := make([]string, len(p.Tiers))
		for i, t := range p.Tiers {
			parts[i] = t.Model + thr(t.Threshold)
		}
		return "cascade " + strings.Join(parts, " > ")
	}
}

// setRoute creates a route or replaces one with a new plan.
func (r *runtimeCfg) setRoute(actor, name string, plan ignatius.Plan, expected *uint64) (ConfigChange, bool, error) {
	if !r.editRoutes {
		return ConfigChange{}, false, ErrEditingDisabled
	}
	return r.edit(actor, "route", "set", name, expected, func(cur, next *cfgSnapshot) (string, string, bool, error) {
		plan = clonePlan(plan)
		to := planSummary(plan)
		from, existed := "", false
		if old, ok := cur.route[name]; ok {
			from, existed = planSummary(old), true
			if planJSON(old) == planJSON(plan) {
				return from, to, false, nil
			}
		}
		next.route[name] = plan
		switch b, inConfig := r.baseRoutes[name]; {
		case inConfig && planJSON(b) == planJSON(plan):
			next.routeSource[name] = "config"
		case inConfig:
			next.routeSource[name] = "override"
		case existed:
			next.routeSource[name] = cur.routeSource[name]
		default:
			next.routeSource[name] = "added"
		}
		return from, to, true, nil
	})
}

// removeRoute drops a runtime-added route, or reverts an overridden one to its config plan.
func (r *runtimeCfg) removeRoute(actor, name string, expected *uint64) (ConfigChange, bool, error) {
	if !r.editRoutes {
		return ConfigChange{}, false, ErrEditingDisabled
	}
	return r.edit(actor, "route", "remove", name, expected, func(cur, next *cfgSnapshot) (string, string, bool, error) {
		old, ok := cur.route[name]
		if !ok {
			return "", "", false, bad("no such route %q", name)
		}
		if cur.routeSource[name] == "config" {
			return "", "", false, ErrNotRemovable
		}
		if b, inConfig := r.baseRoutes[name]; inConfig { // revert
			next.route[name], next.routeSource[name] = clonePlan(b), "config"
			return planSummary(old), planSummary(b), true, nil
		}
		if name == r.defaultRoute {
			return "", "", false, bad("%q is the default route, so it cannot be removed", name)
		}
		if err := r.refuseIfAllowlisted(name); err != nil {
			return "", "", false, err
		}
		delete(next.route, name)
		delete(next.routeSource, name)
		return planSummary(old), "", true, nil
	})
}

// substitute rewrites profile names in a plan to the real aliases they point at. It
// returns a copy: stored plans are shared and must not be mutated.
func substitute(plan ignatius.Plan, ps *cfgSnapshot) ignatius.Plan {
	m := func(name string) string {
		if t, ok := ps.target[name]; ok {
			return t
		}
		return name
	}
	out := plan
	out.Model = m(plan.Model)
	if plan.Models != nil {
		out.Models = make([]string, len(plan.Models))
		for i, n := range plan.Models {
			out.Models[i] = m(n)
		}
	}
	if plan.Tiers != nil {
		out.Tiers = make([]ignatius.Tier, len(plan.Tiers))
		for i, t := range plan.Tiers {
			t.Model = m(t.Model)
			out.Tiers[i] = t
		}
	}
	return out
}
