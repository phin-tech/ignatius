package gateway

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/phin-tech/ignatius/go/ignatius"
)

func editRoutes(c *Config) { c.Admin.EditRoutes = true }
func editBoth(c *Config)   { c.Admin.EditRoutes, c.Admin.EditProfiles = true, true }

const cascadeJevFirst = `{"plan":{"mode":"cascade","tiers":[{"model":"best","threshold":0.9},"fast"]}}`

func TestRouteEditingIsOffByDefaultAndSeparateFromProfiles(t *testing.T) {
	rig := newRig(t, func(c *Config) {
		c.Admin.EditProfiles = true // profiles on, routes off
		c.Clients = []ClientConfig{{Name: "plain", KeyEnv: "KP"}, {Name: "ops", KeyEnv: "KO", Admin: true}}
	}, map[string]string{"KP": "kp", "KO": "ko"})
	put := func(auth string) *httptest.ResponseRecorder {
		return do(rig.s, "PUT", "/v1/routes/tiered", cascadeJevFirst, auth)
	}
	if rec := put(""); rec.Code != 403 {
		t.Errorf("unauthenticated: %d", rec.Code)
	}
	if rec := put("Bearer kp"); rec.Code != 403 || !strings.Contains(rec.Body.String(), "admin_required") {
		t.Errorf("a non-admin must not edit routes: %d %s", rec.Code, rec.Body)
	}
	if rec := put("Bearer ko"); rec.Code != 403 || !strings.Contains(rec.Body.String(), "editing_disabled") {
		t.Errorf("edit_profiles does not switch route editing on: %d %s", rec.Code, rec.Body)
	}
	if rec := do(rig.s, "DELETE", "/v1/routes/tiered", "", "Bearer ko"); rec.Code != 403 {
		t.Errorf("delete is gated the same way: %d", rec.Code)
	}
	if rec := do(rig.s, "PUT", "/v1/profiles/fast", `{"target":"jev"}`, "Bearer ko"); rec.Code != 200 {
		t.Errorf("and profile editing still works on its own: %d %s", rec.Code, rec.Body)
	}
	// and the reverse
	rig2 := newRig(t, editRoutes, nil)
	if rec := do(rig2.s, "PUT", "/v1/profiles/fast", `{"target":"jev"}`, ""); rec.Code != 403 || !strings.Contains(rec.Body.String(), "editing_disabled") {
		t.Errorf("edit_routes does not switch profile editing on: %d %s", rec.Code, rec.Body)
	}
}

func TestEditingARouteTakesEffectAtOnceAndCanBeReverted(t *testing.T) {
	rig := newRig(t, editRoutes, nil)
	s := rig.s
	logs := logTo(s)

	// the config route is fast > best; the cheap model (jeff) is unsure, the big one (jev) is sure
	before, _ := s.router.Resolve("tiered")
	if before.Tiers[0].Model != "jeff" {
		t.Fatalf("setup: %+v", before.Tiers)
	}
	rec := do(s, "PUT", "/v1/routes/tiered", cascadeJevFirst, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"changed":true`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"tiered":"override"`) || !strings.Contains(rec.Body.String(), `"cascade fast@0.80 > best"`) && !strings.Contains(rec.Body.String(), `cascade fast`) {
		t.Errorf("the view marks it as an override and remembers the config plan: %s", rec.Body)
	}
	after, _ := s.router.Resolve("tiered")
	if after.Tiers[0].Model != "jev" || after.Tiers[1].Model != "jeff" || after.Tiers[0].Threshold == nil || *after.Tiers[0].Threshold.Default != 0.9 {
		t.Errorf("the very next request uses the new plan, with profiles resolved: %+v", after.Tiers)
	}
	jevBefore := rig.jev.posts
	if rec := do(s, "POST", "/v1/systemone", l1("tiered"), ""); rec.Code != 200 || rig.jev.posts != jevBefore+1 {
		t.Errorf("the request must now start at jev: %d jev %d->%d", rec.Code, jevBefore, rig.jev.posts)
	}
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "config_changed" {
			line = m
		}
	}
	if line == nil || line["kind"] != "route" || line["name"] != "tiered" || line["op"] != "set" ||
		!strings.Contains(fmt.Sprint(line["from"]), "cascade fast") || !strings.Contains(fmt.Sprint(line["to"]), "cascade best@0.90") {
		t.Errorf("the audit line says what changed, in words: %v", line)
	}
	// revert restores the config plan, and a config route with no override cannot be deleted
	if rec := do(s, "DELETE", "/v1/routes/tiered", "", ""); rec.Code != 200 {
		t.Fatalf("revert: %d %s", rec.Code, rec.Body)
	}
	if got, _ := s.router.Resolve("tiered"); got.Tiers[0].Model != "jeff" {
		t.Errorf("revert restores the config plan, got %+v", got.Tiers)
	}
	if rec := do(s, "DELETE", "/v1/routes/tiered", "", ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "not_removable") {
		t.Errorf("a config route that is not overridden stays: %d %s", rec.Code, rec.Body)
	}
}

func TestCreatingAndDeletingRoutesAndTheVersionIsShared(t *testing.T) {
	rig := newRig(t, editBoth, nil)
	s := rig.s
	version := func() uint64 { return s.router.rc.snapshot().version }
	do(s, "PUT", "/v1/profiles/turbo", `{"target":"jev"}`, "")
	v1 := version()
	rec := do(s, "PUT", "/v1/routes/ensemble", `{"plan":{"mode":"fan_out","models":["fast","turbo"],"reduce":"mean","min_success":1}}`, "")
	if rec.Code != 200 || version() != v1+1 {
		t.Fatalf("create: %d %s (version %d -> %d)", rec.Code, rec.Body, v1, version())
	}
	if rec := do(s, "POST", "/v1/systemone", l1("ensemble"), ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"mode":"fan_out"`) {
		t.Errorf("a runtime-created route is usable at once: %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "POST", "/v1/route", `{"request":{"state":"x","questions":{"q":{"type":"noul","instructions":"?"}}},"route":"ensemble"}`, ""); rec.Code != 200 {
		t.Errorf("and through the native API: %d %s", rec.Code, rec.Body)
	}
	// profiles and routes share one version, so a stale tab loses to either kind of edit
	stale := v1
	if rec := do(s, "PUT", "/v1/routes/other", fmt.Sprintf(`{"plan":{"mode":"single","model":"jeff"},"expected_version":%d}`, stale), ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "version_conflict") {
		t.Errorf("a route edit with a stale version must conflict: %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "DELETE", "/v1/routes/ensemble", "", ""); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "POST", "/v1/systemone", l1("ensemble"), ""); rec.Code != 422 {
		t.Errorf("a deleted route no longer resolves: %d", rec.Code)
	}
}

func TestRouteEditsAreValidated(t *testing.T) {
	rig := newRig(t, func(c *Config) {
		editBoth(c)
		c.Routes["named-alias"] = map[string]any{"mode": "single", "model": "jeff"}
	}, nil)
	many := make([]string, maxPlanModels+1)
	for i := range many {
		many[i] = "jeff"
	}
	manyJSON, _ := json.Marshal(many)
	for name, tc := range map[string]struct {
		method, path, body, want string
		code                     int
	}{
		"no plan":               {"PUT", "/v1/routes/x", `{}`, "needs a \\\"plan\\\"", 422},
		"misspelt field":        {"PUT", "/v1/routes/x", `{"plan":{"mode":"cascade","tires":[]}}`, "tires", 400},
		"unknown mode":          {"PUT", "/v1/routes/x", `{"plan":{"mode":"race","model":"jeff"}}`, "unknown mode", 422},
		"single without model":  {"PUT", "/v1/routes/x", `{"plan":{"mode":"single"}}`, "requires model", 422},
		"unknown model":         {"PUT", "/v1/routes/x", `{"plan":{"mode":"single","model":"nope"}}`, "unknown model", 422},
		"cascade without tiers": {"PUT", "/v1/routes/x", `{"plan":{"mode":"cascade"}}`, "requires tiers", 422},
		"duplicate fan-out":     {"PUT", "/v1/routes/x", `{"plan":{"mode":"fan_out","models":["fast","jeff"]}}`, "duplicate model", 422},
		"bad reducer":           {"PUT", "/v1/routes/x", `{"plan":{"mode":"fan_out","models":["jeff","jev"],"reduce":"median"}}`, "unknown reduce", 422},
		"threshold above 1":     {"PUT", "/v1/routes/x", `{"plan":{"mode":"cascade","tiers":[{"model":"jeff","threshold":1.5},"jev"]}}`, "between 0 and 1", 422},
		"negative threshold":    {"PUT", "/v1/routes/x", `{"plan":{"mode":"cascade","threshold":-0.1,"tiers":["jeff","jev"]}}`, "between 0 and 1", 422},
		"bad threshold key":     {"PUT", "/v1/routes/x", `{"plan":{"mode":"cascade","tiers":[{"model":"jeff","threshold":{"image":0.5}},"jev"]}}`, "noul, choice and score", 422},
		"min_success too big":   {"PUT", "/v1/routes/x", `{"plan":{"mode":"fan_out","models":["jeff","jev"],"min_success":3}}`, "min_success", 422},
		"timeout too long":      {"PUT", "/v1/routes/x", `{"plan":{"mode":"single","model":"jeff","timeout_ms":99999999}}`, "timeout_ms", 422},
		"too many members":      {"PUT", "/v1/routes/x", `{"plan":{"mode":"cascade","tiers":` + tiersOf(manyJSON) + `}}`, "at most", 422},
		"bad name":              {"PUT", "/v1/routes/Has.Caps", `{"plan":{"mode":"single","model":"jeff"}}`, "must be lowercase", 422},
		"separator in name":     {"PUT", "/v1/routes/a:b", `{"plan":{"mode":"single","model":"jeff"}}`, "must be lowercase", 422},
		"reserved name":         {"PUT", "/v1/routes/inline", `{"plan":{"mode":"single","model":"jeff"}}`, "reserved", 422},
		"clashes with alias":    {"PUT", "/v1/routes/jeff", `{"plan":{"mode":"single","model":"jev"}}`, "clashes with a model alias", 422},
		"clashes with profile":  {"PUT", "/v1/routes/fast", `{"plan":{"mode":"single","model":"jev"}}`, "of the same name", 422},
		"delete unknown":        {"DELETE", "/v1/routes/ghost", "", "no such route", 422},
	} {
		rec := do(rig.s, tc.method, tc.path, tc.body, "")
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: got %d %s, want %d containing %q", name, rec.Code, rec.Body, tc.code, tc.want)
		}
	}
	if v := rig.s.router.rc.snapshot().version; v != 0 {
		t.Errorf("not one refused edit may have changed anything, version is %d", v)
	}
	for i := 0; i < maxRoutes; i++ { // fill to the cap, then one more
		do(rig.s, "PUT", fmt.Sprintf("/v1/routes/r%d", i), `{"plan":{"mode":"single","model":"jeff"}}`, "")
	}
	if rec := do(rig.s, "PUT", "/v1/routes/onetoomany", `{"plan":{"mode":"single","model":"jeff"}}`, ""); rec.Code != 422 || !strings.Contains(rec.Body.String(), "at most") {
		t.Errorf("the cap on added routes must hold: %d %s", rec.Code, rec.Body)
	}
}

func tiersOf(namesJSON []byte) string {
	var names []string
	json.Unmarshal(namesJSON, &names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%q", n)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// Profiles and routes constrain each other, so an edit to one must be judged against the other.
func TestProfileAndRouteEditsAreCheckedAgainstEachOther(t *testing.T) {
	rig := newRig(t, editBoth, nil)
	s := rig.s
	do(s, "PUT", "/v1/profiles/turbo", `{"target":"jev"}`, "")
	if rec := do(s, "PUT", "/v1/routes/uses-turbo", `{"plan":{"mode":"cascade","tiers":["fast","turbo"]}}`, ""); rec.Code != 200 {
		t.Fatalf("a route may use a runtime-added profile: %d %s", rec.Code, rec.Body)
	}
	// deleting that profile would leave the route naming something that no longer exists
	rec := do(s, "DELETE", "/v1/profiles/turbo", "", "")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "uses-turbo") || !strings.Contains(rec.Body.String(), "unknown model") {
		t.Errorf("deleting a profile a route uses must be refused, naming the route: %d %s", rec.Code, rec.Body)
	}
	// retargeting a profile so that a route's members collapse is refused as before
	do(s, "PUT", "/v1/routes/pair", `{"plan":{"mode":"fan_out","models":["fast","best"]}}`, "")
	if rec := do(s, "PUT", "/v1/profiles/fast", `{"target":"jev"}`, ""); rec.Code != 422 || !strings.Contains(rec.Body.String(), `route \"pair\" is invalid`) {
		t.Errorf("a retarget that collapses an edited route is refused: %d %s", rec.Code, rec.Body)
	}
	// and a profile may not take the name of a route someone created
	if rec := do(s, "PUT", "/v1/profiles/pair", `{"target":"jev"}`, ""); rec.Code != 422 || !strings.Contains(rec.Body.String(), "clashes with a route") {
		t.Errorf("a profile cannot shadow a route: %d %s", rec.Code, rec.Body)
	}
	if got, _ := s.router.Resolve("uses-turbo"); got.Tiers[1].Model != "jev" {
		t.Errorf("the route still resolves through its profile: %+v", got.Tiers)
	}
}

func TestDeletesThatWouldBreakNextStartupAreRefused(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	// a saved profile and a saved route, both named in a client's allowlist: it boots
	os.WriteFile(state, []byte(`{"version":3,"profiles":{"turbo":"jev"},"routes":{"team":{"mode":"single","model":"jeff"}}}`), 0o600)
	rig := newRig(t, func(c *Config) {
		editBoth(c)
		c.Admin.StateFile = state
		c.Clients = []ClientConfig{{Name: "app", KeyEnv: "KA", Admin: true, Routes: []string{"team", "turbo"}}}
	}, map[string]string{"KA": "ka"})
	if _, err := rig.s.router.Resolve("team"); err != nil {
		t.Fatalf("setup: the saved route should be loaded: %v", err)
	}
	// deleting either would make the NEXT startup fail validating the allowlist, so refuse it now
	for _, path := range []string{"/v1/routes/team", "/v1/profiles/turbo"} {
		rec := do(rig.s, "DELETE", path, "", "Bearer ka")
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), "allowlist") {
			t.Errorf("DELETE %s: %d %s", path, rec.Code, rec.Body)
		}
	}
	// but changing them is fine
	if rec := do(rig.s, "PUT", "/v1/routes/team", `{"plan":{"mode":"single","model":"jev"}}`, "Bearer ka"); rec.Code != 200 {
		t.Errorf("editing an allowlisted route is allowed: %d %s", rec.Code, rec.Body)
	}
}

func TestRouteEditsPersistReloadAndStaleOnesAreSkipped(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	mut := func(c *Config) { editBoth(c); c.Admin.StateFile = state }
	rig := newRig(t, mut, nil)
	do(rig.s, "PUT", "/v1/routes/tiered", cascadeJevFirst, "")                                       // overrides a config route
	do(rig.s, "PUT", "/v1/routes/solo", `{"plan":{"mode":"single","model":"best"}}`, "")             // a new one
	do(rig.s, "PUT", "/v1/profiles/turbo", `{"target":"jeff"}`, "")                                  // and a profile
	do(rig.s, "PUT", "/v1/routes/tiered", `{"plan":{"mode":"cascade","tiers":["fast","best"]}}`, "") // back to the config shape, but written differently...

	raw, _ := os.ReadFile(state)
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatal(err)
	}
	if _, saved := sf.Routes["solo"]; !saved || sf.Profiles["turbo"] != "jeff" {
		t.Errorf("added routes and profiles are saved: %s", raw)
	}
	if info, _ := os.Stat(state); info.Mode().Perm() != 0o600 {
		t.Errorf("the state file is private, mode %v", info.Mode().Perm())
	}

	rig2 := newRig(t, mut, nil) // a restart
	if got, err := rig2.s.router.Resolve("solo"); err != nil || got.Model != "jev" {
		t.Errorf("a saved route must survive a restart: %+v %v", got, err)
	}
	if v := do(rig2.s, "GET", "/v1/routes", "", "").Body.String(); !strings.Contains(v, `"persisted":true`) || !strings.Contains(v, `"version":4`) {
		t.Errorf("the version continues across restarts: %s", v)
	}

	// a saved route that names a model the config no longer has is skipped, not fatal
	os.WriteFile(state, []byte(`{"version":9,"routes":{"ok":{"mode":"single","model":"jeff"},"gone":{"mode":"single","model":"retired-model"}}}`), 0o600)
	rig3 := newRig(t, mut, nil)
	if _, err := rig3.s.router.Resolve("ok"); err != nil {
		t.Errorf("the valid saved route still loads: %v", err)
	}
	if _, err := rig3.s.router.Resolve("gone"); err == nil {
		t.Error("the stale route must not load")
	}
	if v := do(rig3.s, "GET", "/v1/routes", "", "").Body.String(); !strings.Contains(v, `"stale":["route:gone"]`) {
		t.Errorf("and the admin is told which one was skipped: %s", v)
	}
}

func TestRouteViewShowsWhatTheEditorNeeds(t *testing.T) {
	rig := newRig(t, func(c *Config) {
		editRoutes(c)
		c.Clients = []ClientConfig{{Name: "plain", KeyEnv: "KP"}, {Name: "ops", KeyEnv: "KO", Admin: true}}
	}, map[string]string{"KP": "kp", "KO": "ko"})
	admin := do(rig.s, "GET", "/v1/routes", "", "Bearer ko").Body.String()
	for _, want := range []string{`"editable":true`, `"version":0`, `"sources":{"tiered":"config"}`, `"choices":{"aliases":["jeff","jev"],"profiles":["best","fast"]}`} {
		if !strings.Contains(admin, want) {
			t.Errorf("the admin view is missing %s: %s", want, admin)
		}
	}
	plain := do(rig.s, "GET", "/v1/routes", "", "Bearer kp").Body.String()
	for _, secret := range []string{"sources", "choices", "version", "changes"} {
		if strings.Contains(plain, secret) {
			t.Errorf("a non-admin must not see editing context (%q): %s", secret, plain)
		}
	}
}

func TestConcurrentRouteEditsAndRequestsAreConsistent(t *testing.T) {
	rig := newRig(t, editBoth, nil)
	var wg, rw sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				plans := []string{`{"plan":{"mode":"cascade","tiers":["fast","best"]}}`, `{"plan":{"mode":"cascade","tiers":["best","fast"]}}`, `{"plan":{"mode":"single","model":"best"}}`}
				do(rig.s, "PUT", "/v1/routes/live", plans[(n+i)%3], "")
				do(rig.s, "PUT", "/v1/profiles/fast", fmt.Sprintf(`{"target":%q}`, []string{"jeff", "jev"}[n%2]), "")
			}
		}(i)
	}
	do(rig.s, "PUT", "/v1/routes/live", `{"plan":{"mode":"single","model":"best"}}`, "")
	var bad int
	var mu sync.Mutex
	for i := 0; i < 6; i++ {
		rw.Add(1)
		go func() {
			defer rw.Done()
			for n := 0; n < 60; n++ {
				if rec := do(rig.s, "POST", "/v1/systemone", l1("live"), ""); rec.Code != 200 {
					mu.Lock()
					bad++
					mu.Unlock()
				}
			}
		}()
	}
	rw.Wait()
	close(stop)
	wg.Wait()
	if bad != 0 {
		t.Errorf("%d requests failed while routes were being edited", bad)
	}
}

var _ = ignatius.ModeSingle

// The limits are for edits. A config route is held to the plan rules alone, so a config
// that loaded before editing existed still loads.
func TestConfigRoutesAreNotHeldToTheEditLimits(t *testing.T) {
	members := make([]any, maxPlanModels+3) // over the cap on members
	for i := range members {
		members[i] = map[string]any{"model": "jeff", "threshold": 0.5}
	}
	rig := newRig(t, func(c *Config) {
		editRoutes(c)
		c.Routes["force"] = map[string]any{ // threshold above 1: a deliberate way to force escalation
			"mode": "cascade", "tiers": []any{map[string]any{"model": "jeff", "threshold": 1.01}, "jev"}, "timeout_ms": 99_999_999}
		c.Routes["wide"] = map[string]any{"mode": "cascade", "tiers": members}
		c.Routes["picky"] = map[string]any{"mode": "fan_out", "models": []any{"jeff", "jev"}, "min_success": 5}
	}, nil) // the gateway must start
	for _, name := range []string{"force", "wide", "picky"} {
		if _, err := rig.s.router.Resolve(name); err != nil {
			t.Errorf("config route %q must still load: %v", name, err)
		}
	}
	// re-saving a config route unchanged is a no-op, so it is not suddenly refused
	cur, _ := rig.s.router.Resolve("force")
	same, _ := json.Marshal(map[string]any{"plan": rig.s.router.rc.snapshot().route["force"]})
	_ = cur
	if rec := do(rig.s, "PUT", "/v1/routes/force", string(same), ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"changed":false`) {
		t.Errorf("saving a config route as it already is changes nothing: %d %s", rec.Code, rec.Body)
	}
	// but changing it makes it a runtime route, which must meet the limits
	rec := do(rig.s, "PUT", "/v1/routes/force", `{"plan":{"mode":"cascade","tiers":[{"model":"jeff","threshold":1.01},"jev"]}}`, "")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "between 0 and 1") {
		t.Errorf("an edit must meet the limits: %d %s", rec.Code, rec.Body)
	}
	if rec := do(rig.s, "PUT", "/v1/routes/force", `{"plan":{"mode":"cascade","tiers":[{"model":"jeff","threshold":0.9},"jev"]}}`, ""); rec.Code != 200 {
		t.Errorf("a valid edit of a config route works: %d %s", rec.Code, rec.Body)
	}
	if rec := do(rig.s, "DELETE", "/v1/routes/force", "", ""); rec.Code != 200 {
		t.Fatalf("revert: %d %s", rec.Code, rec.Body)
	}
	if got, _ := rig.s.router.Resolve("force"); *got.Tiers[0].Threshold.Default != 1.01 {
		t.Errorf("reverting restores the config plan, limits and all: %+v", got.Tiers[0].Threshold)
	}
}

// A DELETE's version rides in the URL because proxies commonly strip a DELETE's body, which
// would silently disable the conflict check.
func TestDeleteVersionTravelsInTheURL(t *testing.T) {
	rig := newRig(t, editBoth, nil)
	s := rig.s
	do(s, "PUT", "/v1/routes/scratch", `{"plan":{"mode":"single","model":"jeff"}}`, "")
	do(s, "PUT", "/v1/profiles/turbo", `{"target":"jev"}`, "")
	cur := s.router.rc.snapshot().version
	for _, path := range []string{"/v1/routes/scratch", "/v1/profiles/turbo"} {
		rec := do(s, "DELETE", fmt.Sprintf("%s?expected_version=%d", path, cur-1), "", "") // a stale version, no body at all
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "version_conflict") {
			t.Errorf("DELETE %s with a stale version in the URL must conflict: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := do(s, "DELETE", "/v1/routes/scratch?expected_version=abc", "", ""); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_query") {
		t.Errorf("a malformed version is a 400: %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "DELETE", fmt.Sprintf("/v1/routes/scratch?expected_version=%d", cur), "", ""); rec.Code != 200 {
		t.Errorf("the current version in the URL is accepted: %d %s", rec.Code, rec.Body)
	}
	// the body form still works, and the body wins if both are given
	if rec := do(s, "DELETE", "/v1/profiles/turbo?expected_version=0", fmt.Sprintf(`{"expected_version":%d}`, s.router.rc.snapshot().version), ""); rec.Code != 200 {
		t.Errorf("a body version is still honoured: %d %s", rec.Code, rec.Body)
	}
}
