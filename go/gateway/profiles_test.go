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

// profileRig is two fake models that answer differently, so a test can tell which one
// a request really reached, plus profiles fast -> jeff and best -> jev.
type profileRig struct {
	s         *Server
	jeff, jev *fake
}

func newRig(t *testing.T, mut func(*Config), env map[string]string) *profileRig {
	t.Helper()
	jeff, jev := newFake(t, unsure), newFake(t, sure)
	cfg := Config{
		Models:   map[string]ignatius.ModelConfig{"jeff": {Provider: "systemone", BaseURL: jeff.URL}, "jev": {Provider: "systemone", BaseURL: jev.URL}},
		Profiles: map[string]string{"fast": "jeff", "best": "jev"},
		Routes:   map[string]map[string]any{"tiered": {"mode": "cascade", "tiers": []any{"fast", "best"}}},
	}
	if mut != nil {
		mut(&cfg)
	}
	cfg.applyDefaults()
	getenv := func(k string) string { return env[k] }
	reg, err := ignatius.BuildRegistry(cfg.Models, getenv)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, reg, getenv)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &profileRig{s: s, jeff: jeff, jev: jev}
}

func editable(c *Config) { c.Admin.EditProfiles = true }

func newErr(t *testing.T, mut func(*Config)) error {
	t.Helper()
	up := newFake(t, sure)
	cfg := Config{Models: map[string]ignatius.ModelConfig{"jeff": {Provider: "systemone", BaseURL: up.URL}}}
	mut(&cfg)
	cfg.applyDefaults()
	reg, _ := ignatius.BuildRegistry(cfg.Models, func(string) string { return "" })
	_, err := New(cfg, reg, func(string) string { return "" })
	return err
}

func sourcesOf(body string) []string {
	var r struct {
		Ignatius struct {
			Sources map[string]struct{ Model string } `json:"sources"`
		} `json:"ignatius"`
	}
	json.Unmarshal([]byte(body), &r)
	var out []string
	for _, s := range r.Ignatius.Sources {
		out = append(out, s.Model)
	}
	return out
}

func TestProfileResolvesToTheRealModelEverywhereANameCanAppear(t *testing.T) {
	rig := newRig(t, func(c *Config) { c.DefaultRoute = "best" }, nil)
	s := rig.s

	rec := do(s, "POST", "/v1/systemone", l1("fast"), "") // directly
	if rec.Code != 200 || rig.jeff.posts != 1 || rig.jev.posts != 0 {
		t.Fatalf("model=fast must reach jeff only: %d jeff=%d jev=%d", rec.Code, rig.jeff.posts, rig.jev.posts)
	}
	if got := sourcesOf(rec.Body.String()); len(got) != 1 || got[0] != "jeff" {
		t.Errorf("the answer must be attributed to the real model, not the profile: %v", got)
	}
	for model, wantHops := range map[string][]string{
		"tiered":               {"jeff", "jev"}, // a named cascade whose tiers are profile names
		"cascade:fast>best":    {"jeff", "jev"}, // an inline cascade
		"cascade:best>fast":    {"jev"},         // best is confident, so it never escalates
		"jev-latest":           {"jev"},         // default_route = a profile
		"cascade:fast@0.5>jev": {"jeff", "jev"}, // profiles and real aliases mix
	} {
		rec := do(s, "POST", "/v1/systemone", l1(model), "")
		var r struct {
			Ignatius struct{ Trace []ignatius.Hop }
		}
		json.Unmarshal(rec.Body.Bytes(), &r)
		var hops []string
		for _, h := range r.Ignatius.Trace {
			hops = append(hops, h.Model)
		}
		if rec.Code != 200 || (len(wantHops) > 1 && fmt.Sprint(hops) != fmt.Sprint(wantHops)) {
			t.Errorf("%q: code %d hops %v, want %v", model, rec.Code, hops, wantHops)
		}
	}
	rec = do(s, "POST", "/v1/systemone", l1("fan-out:fast,best|mean"), "") // inline fan-out
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"mode":"fan_out"`) {
		t.Errorf("inline fan-out over profiles: %d %s", rec.Code, rec.Body)
	}
	// stats attribute everything to the real models, never to "fast" or "best"
	for _, m := range snapshotOf(t, s).Models {
		if m.ID == "fast" || m.ID == "best" {
			t.Errorf("a profile appeared as a model in stats: %q", m.ID)
		}
	}
	// the route label for a profile is its (bounded, configured) name
	if recent := snapshotOf(t, s).Recent; recent[len(recent)-1].Route != "fast" {
		t.Errorf("the request keeps the name the caller used, got %q", recent[len(recent)-1].Route)
	}
}

func TestProfileConfigValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		profiles map[string]string
		want     string
	}{
		"unknown target":      {map[string]string{"fast": "nope"}, "not a model alias"},
		"target is a profile": {map[string]string{"fast": "jeff", "quick": "fast"}, "not a model alias"},
		"clashes with alias":  {map[string]string{"jeff": "jeff"}, "clashes with a model alias"},
		"clashes with route":  {map[string]string{"r": "jeff"}, "clashes with a route"},
		"uppercase":           {map[string]string{"Fast": "jeff"}, "must be lowercase"},
		"colon":               {map[string]string{"a:b": "jeff"}, "must be lowercase"},
		"comma":               {map[string]string{"a,b": "jeff"}, "must be lowercase"},
		"arrow":               {map[string]string{"a>b": "jeff"}, "must be lowercase"},
		"empty":               {map[string]string{"": "jeff"}, "must be lowercase"},
		"reserved inline":     {map[string]string{"inline": "jeff"}, "reserved"},
		"reserved unknown":    {map[string]string{"unknown": "jeff"}, "reserved"},
		"reserved jev-latest": {map[string]string{"jev-latest": "jeff"}, "reserved"},
		"too long":            {map[string]string{strings.Repeat("a", 65): "jeff"}, "must be lowercase"},
	} {
		err := newErr(t, func(c *Config) {
			c.Profiles = tc.profiles
			c.Routes = map[string]map[string]any{"r": {"mode": "single", "model": "jeff"}}
		})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, tc.want, err)
		}
	}
	many := map[string]string{}
	for i := 0; i <= maxProfiles; i++ {
		many[fmt.Sprintf("p%d", i)] = "jeff"
	}
	if err := newErr(t, func(c *Config) { c.Profiles = many }); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("more than %d profiles must be refused, got %v", maxProfiles, err)
	}
	// a route that names a profile that does not exist is still an error at startup
	if err := newErr(t, func(c *Config) {
		c.Routes = map[string]map[string]any{"r": {"mode": "cascade", "tiers": []any{"ghost", "jeff"}}}
	}); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("an unknown name in a route must still fail: %v", err)
	}
	if err := newErr(t, func(c *Config) { c.DefaultRoute = "ghost" }); err == nil || !strings.Contains(err.Error(), "default_route") {
		t.Errorf("an unknown default_route must fail: %v", err)
	}
}

func TestAllowlistsAndDiscoveryTreatProfilesAsNames(t *testing.T) {
	rig := newRig(t, func(c *Config) {
		c.Clients = []ClientConfig{
			{Name: "fastonly", KeyEnv: "KF", Routes: []string{"fast", "inline"}},
			{Name: "plain", KeyEnv: "KP"},
			{Name: "ops", KeyEnv: "KO", Admin: true},
		}
	}, map[string]string{"KF": "kf", "KP": "kp", "KO": "ko"})
	s := rig.s
	for model, want := range map[string]int{
		"fast": 200, "cascade:fast>fast": 200, "fan-out:fast|vote": 200, // the profile it was given, alone or inline
		"best": 403, "jeff": 403, "jev": 403, "tiered": 403, // the model behind it, other profiles, routes
		"cascade:fast>best": 403, "cascade:fast>jeff": 403, // inline cannot reach what it was not given
	} {
		if got := do(s, "POST", "/v1/systemone", l1(model), "Bearer kf").Code; got != want {
			t.Errorf("fastonly -> %q: got %d want %d", model, got, want)
		}
	}
	rec := do(s, "GET", "/v1/profiles", "", "Bearer kf")
	if !strings.Contains(rec.Body.String(), `"name":"fast"`) || !strings.Contains(rec.Body.String(), `"target":"jeff"`) {
		t.Errorf("a restricted client sees the profiles it may use, with the model behind each: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "best") || strings.Contains(rec.Body.String(), "jev") {
		t.Errorf("but nothing about profiles or models it was not given: %s", rec.Body)
	}
	if p := do(s, "GET", "/v1/profiles", "", "Bearer kp").Body.String(); !strings.Contains(p, `"target":"jeff"`) || !strings.Contains(p, `"target":"jev"`) || strings.Contains(p, `"source"`) {
		t.Errorf("an unrestricted non-admin sees every profile and its model, but not admin detail: %s", p)
	}
	a := do(s, "GET", "/v1/profiles", "", "Bearer ko").Body.String()
	for _, want := range []string{`"source":"config"`, `"version":0`, `"aliases":["jeff","jev"]`, `"editable":false`, `"persisted":false`} {
		if !strings.Contains(a, want) {
			t.Errorf("admin view is missing %s: %s", want, a)
		}
	}
	// an allowlist entry that names nothing is a startup error, a profile counts as something
	if err := newErr(t, func(c *Config) {
		c.Profiles = map[string]string{"fast": "jeff"}
		c.Clients = []ClientConfig{{Name: "x", KeyEnv: "K", Routes: []string{"ghost"}}}
	}); err == nil {
		t.Error("unknown allowlist entries must fail startup")
	}
}

func TestEditingIsOffByDefaultAndAdminOnly(t *testing.T) {
	rig := newRig(t, func(c *Config) {
		c.Clients = []ClientConfig{{Name: "plain", KeyEnv: "KP"}, {Name: "ops", KeyEnv: "KO", Admin: true}}
	}, map[string]string{"KP": "kp", "KO": "ko"})
	put := func(auth string) *httptest.ResponseRecorder {
		return do(rig.s, "PUT", "/v1/profiles/fast", `{"target":"jev"}`, auth)
	}
	if rec := put(""); rec.Code != 403 || !strings.Contains(rec.Body.String(), "missing_credentials") {
		t.Errorf("unauthenticated: %d %s", rec.Code, rec.Body)
	}
	if rec := put("Bearer nope"); rec.Code != 401 {
		t.Errorf("bad key: %d", rec.Code)
	}
	if rec := put("Bearer kp"); rec.Code != 403 || !strings.Contains(rec.Body.String(), "admin_required") {
		t.Errorf("a non-admin must not edit: %d %s", rec.Code, rec.Body)
	}
	if rec := put("Bearer ko"); rec.Code != 403 || !strings.Contains(rec.Body.String(), "editing_disabled") {
		t.Errorf("even an admin cannot edit unless [admin] edit_profiles is on: %d %s", rec.Code, rec.Body)
	}
	if rec := do(rig.s, "DELETE", "/v1/profiles/fast", "", "Bearer ko"); rec.Code != 403 {
		t.Errorf("delete is gated the same way: %d", rec.Code)
	}
	if got, _ := rig.s.router.Resolve("fast"); got.Model != "jeff" {
		t.Errorf("a refused edit must change nothing, fast resolves to %q", got.Model)
	}
}

func TestEditingProfilesEndToEnd(t *testing.T) {
	rig := newRig(t, editable, nil)
	s := rig.s
	logs := logTo(s)
	view := func(rec *httptest.ResponseRecorder) (v struct {
		Version  uint64
		Changed  bool
		Editable bool
		Profiles []struct{ Name, Target, Source, ConfigTarget string }
		Changes  []ConfigChange
	}) {
		raw := strings.NewReplacer(`"config_target"`, `"configtarget"`).Replace(rec.Body.String())
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("%v: %s", err, rec.Body)
		}
		return
	}
	// retarget fast: jeff -> jev, with the version we read
	cur := view(do(s, "GET", "/v1/profiles", "", ""))
	if !cur.Editable || cur.Version != 0 {
		t.Fatalf("starting state: %+v", cur)
	}
	rec := do(s, "PUT", "/v1/profiles/fast", fmt.Sprintf(`{"target":"jev","expected_version":%d}`, cur.Version), "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	v := view(rec)
	if !v.Changed || v.Version != 1 {
		t.Errorf("a real change bumps the version: %+v", v)
	}
	for _, p := range v.Profiles {
		if p.Name == "fast" && (p.Target != "jev" || p.Source != "override" || p.ConfigTarget != "jeff") {
			t.Errorf("fast should show as an override of its config value: %+v", p)
		}
	}
	// it takes effect for the very next request, including through a named cascade
	jeffBefore := rig.jeff.posts
	do(s, "POST", "/v1/systemone", l1("fast"), "")
	if rig.jeff.posts != jeffBefore || rig.jev.posts == 0 {
		t.Errorf("model=fast must now reach jev: jeff %d->%d jev %d", jeffBefore, rig.jeff.posts, rig.jev.posts)
	}
	if got, _ := s.router.Resolve("tiered"); got.Tiers[0].Model != "jev" || got.Tiers[1].Model != "jev" {
		t.Errorf("a named cascade follows the edit: %+v", got.Tiers)
	}
	// the audit line names who, what and the versions, and carries no model secrets
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "config_changed" {
			line = m
		}
	}
	if line == nil || line["actor"] != "anonymous" || line["name"] != "fast" || line["kind"] != "profile" || line["from"] != "jeff" || line["to"] != "jev" || line["version"].(float64) != 1 {
		t.Errorf("audit line: %v", line)
	}
	// a no-op edit changes nothing and is not audited
	logs.Reset()
	rec = do(s, "PUT", "/v1/profiles/fast", `{"target":"jev"}`, "")
	if v := view(rec); v.Changed || v.Version != 1 || strings.Contains(logs.String(), "config_changed") {
		t.Errorf("a no-op must not bump the version or be audited: %+v %s", v, logs)
	}
	// a stale version loses, and says what the current one is
	rec = do(s, "PUT", "/v1/profiles/fast", `{"target":"jeff","expected_version":0}`, "")
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"current_version":1`) || !strings.Contains(rec.Body.String(), "version_conflict") {
		t.Errorf("optimistic concurrency: %d %s", rec.Code, rec.Body)
	}
	// revert: deleting an override restores the config value
	rec = do(s, "DELETE", "/v1/profiles/fast", "", "")
	if v := view(rec); rec.Code != 200 || v.Version != 2 {
		t.Fatalf("revert: %d %s", rec.Code, rec.Body)
	}
	if got, _ := s.router.Resolve("fast"); got.Model != "jeff" {
		t.Errorf("revert should restore the config value, got %q", got.Model)
	}
	// a config-defined profile that has no override cannot be deleted
	if rec := do(s, "DELETE", "/v1/profiles/fast", "", ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "not_removable") {
		t.Errorf("config-defined profiles stay: %d %s", rec.Code, rec.Body)
	}
	// create a brand-new profile at runtime, use it, then delete it
	if rec := do(s, "PUT", "/v1/profiles/turbo", `{"target":"jev"}`, ""); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := do(s, "POST", "/v1/systemone", l1("cascade:fast>turbo"), ""); rec.Code != 200 {
		t.Errorf("a runtime-added profile is usable at once: %d %s", rec.Code, rec.Body)
	}
	if v := view(do(s, "DELETE", "/v1/profiles/turbo", "", "")); len(v.Profiles) != 2 {
		t.Errorf("deleting a runtime-added profile removes it: %+v", v.Profiles)
	}
	if rec := do(s, "POST", "/v1/systemone", l1("turbo"), ""); rec.Code != 422 {
		t.Errorf("and it no longer resolves: %d", rec.Code)
	}
	if h := view(do(s, "GET", "/v1/profiles", "", "")).Changes; len(h) != 4 || h[0].Op != "remove" || h[len(h)-1].To != "jev" {
		t.Errorf("the change history is newest first: %+v", h)
	}
}

func TestProfileEditsAreValidated(t *testing.T) {
	rig := newRig(t, editable, nil)
	for name, tc := range map[string]struct {
		method, path, body string
		code               int
		want               string
	}{
		"unknown model":        {"PUT", "/v1/profiles/fast", `{"target":"nope"}`, 422, "not a model alias"},
		"target is a profile":  {"PUT", "/v1/profiles/fast", `{"target":"best"}`, 422, "not a model alias"},
		"target is a route":    {"PUT", "/v1/profiles/fast", `{"target":"tiered"}`, 422, "not a model alias"},
		"empty target":         {"PUT", "/v1/profiles/fast", `{}`, 422, "not a model alias"},
		"name clashes (alias)": {"PUT", "/v1/profiles/jeff", `{"target":"jev"}`, 422, "clashes with a model alias"},
		"name clashes (route)": {"PUT", "/v1/profiles/tiered", `{"target":"jev"}`, 422, "clashes with a route"},
		"bad name":             {"PUT", "/v1/profiles/Has.Caps", `{"target":"jev"}`, 422, "must be lowercase"},
		"reserved name":        {"PUT", "/v1/profiles/inline", `{"target":"jev"}`, 422, "reserved"},
		"malformed json":       {"PUT", "/v1/profiles/fast", `{"target":`, 400, "invalid_json"},
		"delete unknown":       {"DELETE", "/v1/profiles/ghost", "", 422, "no such profile"},
	} {
		rec := do(rig.s, tc.method, tc.path, tc.body, "")
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: got %d %s, want %d containing %q", name, rec.Code, rec.Body, tc.code, tc.want)
		}
	}
	if got, _ := rig.s.router.Resolve("fast"); got.Model != "jeff" {
		t.Errorf("rejected edits must change nothing, fast resolves to %q", got.Model)
	}
	for i := 0; i < maxProfiles; i++ { // fill to the cap, then one more
		do(rig.s, "PUT", fmt.Sprintf("/v1/profiles/p%d", i), `{"target":"jev"}`, "")
	}
	if rec := do(rig.s, "PUT", "/v1/profiles/onetoomany", `{"target":"jev"}`, ""); rec.Code != 422 || !strings.Contains(rec.Body.String(), "at most") {
		t.Errorf("the profile cap must hold: %d %s", rec.Code, rec.Body)
	}
}

func TestEditsPersistAndReload(t *testing.T) {
	state := filepath.Join(t.TempDir(), "profiles.json")
	mut := func(c *Config) { editable(c); c.Admin.StateFile = state }
	rig := newRig(t, mut, nil)
	do(rig.s, "PUT", "/v1/profiles/fast", `{"target":"jev"}`, "")   // an override of a config profile
	do(rig.s, "PUT", "/v1/profiles/turbo", `{"target":"jeff"}`, "") // a new profile
	do(rig.s, "PUT", "/v1/profiles/best", `{"target":"jev"}`, "")   // set to its own config value: not an override

	info, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the state file must be private, mode %v", info.Mode().Perm())
	}
	var sf stateFile
	raw, _ := os.ReadFile(state)
	json.Unmarshal(raw, &sf)
	if sf.Version != 2 || len(sf.Profiles) != 2 || sf.Profiles["fast"] != "jev" || sf.Profiles["turbo"] != "jeff" {
		t.Errorf("only real differences from the config are saved: %s", raw)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(state), ".profiles-*")); len(left) != 0 {
		t.Errorf("no temp files may be left behind: %v", left)
	}

	// a fresh gateway on the same config + state file comes back with the edits
	rig2 := newRig(t, mut, nil)
	if got, _ := rig2.s.router.Resolve("fast"); got.Model != "jev" {
		t.Errorf("the override must survive a restart, fast resolves to %q", got.Model)
	}
	if got, _ := rig2.s.router.Resolve("turbo"); got.Model != "jeff" {
		t.Errorf("a runtime-added profile must survive a restart, got %q", got.Model)
	}
	view := do(rig2.s, "GET", "/v1/profiles", "", "").Body.String()
	if !strings.Contains(view, `"version":2`) || !strings.Contains(view, `"persisted":true`) {
		t.Errorf("version continues across restarts: %s", view)
	}
	// reverting the override rewrites the file without it
	do(rig2.s, "DELETE", "/v1/profiles/fast", "", "")
	raw, _ = os.ReadFile(state)
	sf = stateFile{} // json.Unmarshal merges into an existing map, so read into a fresh value
	json.Unmarshal(raw, &sf)
	if _, still := sf.Profiles["fast"]; still || sf.Version != 3 {
		t.Errorf("revert is persisted: %s", raw)
	}
}

func TestStaleAndUnwritableStateNeverBlockTheGateway(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "profiles.json")
	// an override whose model no longer exists, and one that clashes with a new alias
	os.WriteFile(state, []byte(`{"version":7,"profiles":{"fast":"retired-model","jeff":"jev","turbo":"jev"}}`), 0o600)
	rig := newRig(t, func(c *Config) { editable(c); c.Admin.StateFile = state }, nil)
	if got, _ := rig.s.router.Resolve("fast"); got.Model != "jeff" {
		t.Errorf("a stale override is ignored and the config value wins, got %q", got.Model)
	}
	if got, _ := rig.s.router.Resolve("turbo"); got.Model != "jev" {
		t.Errorf("valid overrides in the same file still apply: %q", got.Model)
	}
	view := do(rig.s, "GET", "/v1/profiles", "", "").Body.String()
	if !strings.Contains(view, `"stale":["profile:fast","profile:jeff"]`) || !strings.Contains(view, `"version":7`) {
		t.Errorf("stale overrides are reported to the admin: %s", view)
	}
	// a corrupt file IS an error: silently ignoring it could hide a real problem
	os.WriteFile(state, []byte(`{not json`), 0o600)
	if err := newErr(t, func(c *Config) { c.Admin.StateFile = state }); err == nil || !strings.Contains(err.Error(), "saved config") {
		t.Errorf("a corrupt state file must fail loudly: %v", err)
	}

	// if saving fails, the edit must not be applied in memory either
	bad := newRig(t, func(c *Config) { editable(c); c.Admin.StateFile = filepath.Join(dir, "no-such-dir", "p.json") }, nil)
	rec := do(bad.s, "PUT", "/v1/profiles/fast", `{"target":"jev"}`, "")
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "save_failed") {
		t.Errorf("an unwritable state path: %d %s", rec.Code, rec.Body)
	}
	if got, _ := bad.s.router.Resolve("fast"); got.Model != "jeff" {
		t.Errorf("a failed save must leave the live profile unchanged, got %q", got.Model)
	}
	if v := bad.s.router.rc.snapshot().version; v != 0 {
		t.Errorf("and the version must not move, got %d", v)
	}
}

// Requests and edits at the same time, under -race: every request sees one consistent
// snapshot, so it reaches exactly one of the two models and never errors or panics.
func TestConcurrentRequestsAndEditsAreConsistent(t *testing.T) {
	rig := newRig(t, editable, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				target := []string{"jeff", "jev"}[(n+i)%2]
				do(rig.s, "PUT", "/v1/profiles/fast", fmt.Sprintf(`{"target":%q}`, target), "")
			}
		}(i)
	}
	var bad int
	var mu sync.Mutex
	var rw sync.WaitGroup
	for i := 0; i < 6; i++ {
		rw.Add(1)
		go func() {
			defer rw.Done()
			for n := 0; n < 60; n++ {
				rec := do(rig.s, "POST", "/v1/systemone", l1("cascade:fast>best"), "")
				if m := sourcesOf(rec.Body.String()); rec.Code != 200 || len(m) == 0 {
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
		t.Errorf("%d requests failed while profiles were being edited", bad)
	}
	if v := rig.s.router.rc.snapshot().version; v == 0 {
		t.Error("the edits never happened, so this proved nothing")
	}
}

func TestAnEditThatWouldBreakARouteIsRefused(t *testing.T) {
	rig := newRig(t, func(c *Config) {
		editable(c)
		c.Routes["vote"] = map[string]any{"mode": "fan_out", "models": []any{"fast", "best"}, "reduce": "mean"}
	}, nil) // "vote" is a fan-out over fast and best
	rec := do(rig.s, "PUT", "/v1/profiles/fast", `{"target":"jev"}`, "")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `route \"vote\" is invalid`) || !strings.Contains(rec.Body.String(), "duplicate model") {
		t.Errorf("collapsing a fan-out onto one model must be refused, naming the route: %d %s", rec.Code, rec.Body)
	}
	if got, _ := rig.s.router.Resolve("fast"); got.Model != "jeff" || rig.s.router.rc.snapshot().version != 0 {
		t.Error("a refused edit changes nothing")
	}
	if rec := do(rig.s, "POST", "/v1/systemone", l1("vote"), ""); rec.Code != 200 {
		t.Errorf("and the route keeps working: %d %s", rec.Code, rec.Body)
	}
}

func TestRevertingAnOverrideCannotBreakARouteEither(t *testing.T) {
	jeff, jev, jed := newFake(t, unsure), newFake(t, sure), newFake(t, sure)
	mk := func(f *fake) ignatius.ModelConfig { return ignatius.ModelConfig{Provider: "systemone", BaseURL: f.URL} }
	cfg := Config{
		Models:   map[string]ignatius.ModelConfig{"jeff": mk(jeff), "jev": mk(jev), "jed": mk(jed)},
		Profiles: map[string]string{"a": "jeff", "b": "jev"},
		Routes:   map[string]map[string]any{"duo": {"mode": "fan_out", "models": []any{"a", "b"}}},
		Admin:    AdminConfig{EditProfiles: true},
	}
	cfg.applyDefaults()
	reg, _ := ignatius.BuildRegistry(cfg.Models, func(string) string { return "" })
	s, err := New(cfg, reg, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{`/v1/profiles/b {"target":"jed"}`, `/v1/profiles/a {"target":"jev"}`} { // jeff,jed then jev,jed: both fine
		path, body, _ := strings.Cut(step, " ")
		if rec := do(s, "PUT", path, body, ""); rec.Code != 200 {
			t.Fatalf("%s: %d %s", step, rec.Code, rec.Body)
		}
	}
	// reverting b now would put the config value (jev) next to a's override (jev): [jev, jev]
	rec := do(s, "DELETE", "/v1/profiles/b", "", "")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `route \"duo\" is invalid`) {
		t.Errorf("a revert that would break a route is refused: %d %s", rec.Code, rec.Body)
	}
}

func TestSavedOverridesThatWouldBreakARouteAreSkippedNotFatal(t *testing.T) {
	state := filepath.Join(t.TempDir(), "profiles.json")
	// "fast" -> jev would collapse the fan-out "vote" onto one model; "turbo" is fine
	os.WriteFile(state, []byte(`{"version":4,"profiles":{"fast":"jev","turbo":"jev"}}`), 0o600)
	rig := newRig(t, func(c *Config) {
		editable(c)
		c.Admin.StateFile = state
		c.Routes["vote"] = map[string]any{"mode": "fan_out", "models": []any{"fast", "best"}, "reduce": "mean"}
	}, nil) // must start
	if got, _ := rig.s.router.Resolve("fast"); got.Model != "jeff" {
		t.Errorf("the route-breaking override is skipped, got %q", got.Model)
	}
	if got, _ := rig.s.router.Resolve("turbo"); got.Model != "jev" {
		t.Errorf("the harmless one still applies, got %q", got.Model)
	}
	if v := do(rig.s, "GET", "/v1/profiles", "", "").Body.String(); !strings.Contains(v, `"stale":["profile:fast"]`) {
		t.Errorf("and the admin is told which one was skipped: %s", v)
	}
}

// The model name behind a profile is not treated as a secret, so a plan error may name
// it. What a restricted client still must not learn is anything it was not given access to.
func TestPlanErrorsKeepTheirDetailButStillRespectTheAllowlist(t *testing.T) {
	rig := newRig(t, func(c *Config) {
		c.Clients = []ClientConfig{{Name: "fastonly", KeyEnv: "KF", Routes: []string{"fast", "inline"}}}
	}, map[string]string{"KF": "kf"})
	// two uses of the same profile collapse onto one model: a duplicate in a fan-out
	rec := do(rig.s, "POST", "/v1/systemone", l1("fan-out:fast,fast"), "Bearer kf")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `duplicate model`) || !strings.Contains(rec.Body.String(), "jeff") {
		t.Errorf("the error keeps the detail a caller can act on: %d %s", rec.Code, rec.Body)
	}
	// reaching for something outside its allowlist is still a bare 403 that names nothing
	rec = do(rig.s, "POST", "/v1/systemone", l1("cascade:fast>best"), "Bearer kf")
	if rec.Code != 403 || strings.Contains(rec.Body.String(), "jev") || strings.Contains(rec.Body.String(), "best") {
		t.Errorf("a forbidden name must not be echoed or explained: %d %s", rec.Code, rec.Body)
	}
}

func TestStatsSayWhatEachModelIs(t *testing.T) {
	jeff, jev := newFake(t, unsure), newFake(t, sure)
	cfg := Config{
		Models: map[string]ignatius.ModelConfig{
			// a URL carrying credentials, a path and a query: none of that may reach the page
			"jeff": {Provider: "systemone", Model: "jeff-0.8b", BaseURL: strings.Replace(jeff.URL, "http://", "http://user:hunter2@", 1) + "/internal/path?token=SECRET-QUERY"},
			"jev":  {Provider: "systemone", Model: "jev-latest", BaseURL: jev.URL},
		},
		Profiles: map[string]string{"fast": "jeff", "best": "jev", "also-best": "jev"},
	}
	cfg.applyDefaults()
	reg, _ := ignatius.BuildRegistry(cfg.Models, func(string) string { return "" })
	s, err := New(cfg, reg, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshotOf(t, s)
	by := map[string]ModelSnapshot{}
	for _, m := range snap.Models {
		by[m.ID] = m
	}
	if by["jeff"].Upstream != "jeff-0.8b" || by["jeff"].Provider != "systemone" || fmt.Sprint(by["jeff"].Profiles) != "[fast]" {
		t.Errorf("jeff: %+v", by["jeff"])
	}
	if by["jev"].Upstream != "jev-latest" || fmt.Sprint(by["jev"].Profiles) != "[also-best best]" {
		t.Errorf("a model lists every profile that points at it: %+v", by["jev"])
	}
	raw, _ := json.Marshal(snap)
	for _, secret := range []string{"hunter2", "SECRET-QUERY", "/internal/path", "user:"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("stats leaked part of a backend URL (%q): %s", secret, raw)
		}
	}
	if !strings.HasPrefix(by["jeff"].Host, "127.0.0.1:") {
		t.Errorf("host should be host:port only, got %q", by["jeff"].Host)
	}
	// profiles move with an edit
	s.router.rc.editProfiles = true
	s.router.rc.setProfile("t", "fast", "jev", nil)
	if p := func() []string {
		for _, m := range snapshotOf(t, s).Models {
			if m.ID == "jev" {
				return m.Profiles
			}
		}
		return nil
	}(); fmt.Sprint(p) != "[also-best best fast]" {
		t.Errorf("after retargeting fast, jev lists it: %v", p)
	}
}
