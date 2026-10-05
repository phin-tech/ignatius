package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
	"golang.org/x/crypto/bcrypt"
)

// newErr builds a server from cfg and returns New's error.
func startErr(t *testing.T, cfg Config) error {
	t.Helper()
	cfg.applyDefaults()
	reg, err := ignatius.BuildRegistry(cfg.Models, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(cfg, reg, func(k string) string { return testEnv[k] })
	return err
}

var testEnv = map[string]string{"KO": "ops-key", "KA": "app-key"}

// keyServer builds a gateway with self-service keys on, two configured clients ("ops", an
// admin; "app", restricted to c2 and limited), and its state under dir.
func keyServer(t *testing.T, dir string, mut func(*Config)) *Server {
	t.Helper()
	cheap, smart := newFake(t, unsure), newFake(t, sure)
	return setup(t, map[string]*fake{"cheap": cheap, "smart": smart}, func(c *Config) {
		c.Admin.SelfServiceKeys = true
		c.Admin.StateFile = filepath.Join(dir, "state.json")
		c.Clients = []ClientConfig{
			{Name: "ops", KeyEnv: "KO", Admin: true},
			{Name: "app", KeyEnv: "KA", Routes: []string{"c2", "cheap"}, RateLimitPerMinute: 60, Burst: 6},
		}
		c.Routes = map[string]map[string]any{"c2": {"mode": "cascade", "tiers": []any{"cheap", "smart"}}}
		if mut != nil {
			mut(c)
		}
	}, testEnv)
}

func mintKey(t *testing.T, s *Server, auth, body string) (int, map[string]any) {
	t.Helper()
	rec := do(s, "POST", "/v1/keys", body, "Bearer "+auth)
	return rec.Code, decode(t, rec)
}

func TestSelfServiceKeysAreOffByDefault(t *testing.T) {
	s := keyServer(t, t.TempDir(), func(c *Config) { c.Admin.SelfServiceKeys = false })
	if code, _ := mintKey(t, s, "ops-key", `{"name":"x"}`); code != 403 {
		t.Errorf("minting with the feature off: %d, want 403", code)
	}
}

func TestSelfServiceKeysNeedAStateFileAndAStartingKey(t *testing.T) {
	cases := map[string]struct {
		mut  func(*Config)
		want string
	}{
		"no state file": {func(c *Config) { c.Admin.StateFile = "" }, "needs state_file"},
		"no root key":   {func(c *Config) { c.Clients = nil }, "at least one configured key or user"},
	}
	for name, tc := range cases {
		mut, want := tc.mut, tc.want
		t.Run(name, func(t *testing.T) {
			fk := newFake(t, sure)
			cfg := Config{Models: map[string]ignatius.ModelConfig{"m": {Provider: "systemone", BaseURL: fk.URL}},
				Clients: []ClientConfig{{Name: "ops", KeyEnv: "KO"}}}
			cfg.Admin.SelfServiceKeys, cfg.Admin.StateFile = true, filepath.Join(t.TempDir(), "s.json")
			mut(&cfg)
			if err := startErr(t, cfg); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v, want one containing %q", err, want)
			}
		})
	}
}

func TestAnyKeyHolderMintsAWorkingKey(t *testing.T) {
	s := keyServer(t, t.TempDir(), nil)
	code, out := mintKey(t, s, "app-key", `{"name":"ci"}`)
	if code != 201 {
		t.Fatalf("mint: %d %v", code, out)
	}
	key, _ := out["key"].(string)
	if !strings.HasPrefix(key, "ig_") || out["name"] != "app/ci" {
		t.Fatalf("response: %v", out)
	}
	if got := do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer "+key).Code; got != 200 {
		t.Errorf("minted key could not route: %d", got)
	}
	// Inherited the creator's allowlist: it cannot reach what "app" cannot.
	if got := do(s, "POST", "/v1/systemone", l1("smart"), "Bearer "+key).Code; got != 403 {
		t.Errorf("a restricted creator minted an unrestricted key: %d", got)
	}
	list := do(s, "GET", "/v1/keys", "", "Bearer app-key")
	if list.Code != 200 || strings.Contains(list.Body.String(), key) || !strings.Contains(list.Body.String(), `"app/ci"`) {
		t.Errorf("listing: %d %s", list.Code, list.Body)
	}
	if !strings.Contains(list.Body.String(), `"id"`) {
		t.Errorf("listing should carry ids: %s", list.Body)
	}
}

func TestAKeyNeverExceedsItsCreator(t *testing.T) {
	s := keyServer(t, t.TempDir(), nil)
	cases := map[string]struct{ auth, body, field string }{
		"admin from non-admin": {"app-key", `{"name":"a","admin":true}`, "admin"},
		"route not held":       {"app-key", `{"name":"b","routes":["smart"]}`, "routes"},
		"faster than creator":  {"app-key", `{"name":"c","rate_limit_per_minute":600}`, "rate_limit_per_minute"},
		"unlimited from limit": {"app-key", `{"name":"d","rate_limit_per_minute":0}`, "rate_limit_per_minute"},
		"burst above creator":  {"app-key", `{"name":"e","rate_limit_per_minute":30,"burst":50}`, "burst"},
	}
	for name, c := range cases {
		code, out := mintKey(t, s, c.auth, c.body)
		if code != 422 {
			t.Errorf("%s: %d %v", name, code, out)
			continue
		}
		if f := out["error"].(map[string]any)["field"]; f != c.field {
			t.Errorf("%s: field %v, want %s", name, f, c.field)
		}
	}
	// A key with an expiry cannot mint one that outlives it.
	_, short := mintKey(t, s, "ops-key", `{"name":"short","expires_in_s":60}`)
	if code, out := mintKey(t, s, short["key"].(string), `{"name":"long","expires_in_s":3600}`); code != 422 {
		t.Errorf("outliving the creator: %d %v", code, out)
	}
	// An admin may mint an admin, and a child of it that is not.
	if code, out := mintKey(t, s, "ops-key", `{"name":"adm","admin":true}`); code != 201 {
		t.Errorf("admin mint: %d %v", code, out)
	}
}

func TestKeyNamesAreValidatedAndUnique(t *testing.T) {
	s := keyServer(t, t.TempDir(), nil)
	for _, bad := range []string{`{"name":""}`, `{"name":"Has Space"}`, `{"name":"a/b"}`, `{"name":"x","extra":1}`} {
		if code, _ := mintKey(t, s, "ops-key", bad); code != 422 && code != 400 {
			t.Errorf("%s: %d, want 4xx", bad, code)
		}
	}
	mintKey(t, s, "ops-key", `{"name":"dup"}`)
	if code, _ := mintKey(t, s, "ops-key", `{"name":"dup"}`); code != 409 {
		t.Errorf("duplicate name: %d, want 409", code)
	}
	if code, _ := mintKey(t, s, "ops-key", `{"name":"x","routes":["ghost"]}`); code != 422 {
		t.Errorf("unknown route: %d, want 422", code)
	}
}

func TestKeyCaps(t *testing.T) {
	s := keyServer(t, t.TempDir(), func(c *Config) { c.Admin.MaxKeysPerPrincipal = 2; c.Admin.MaxKeys = 3 })
	for i, n := range []string{"a", "b"} {
		if code, out := mintKey(t, s, "ops-key", `{"name":"`+n+`"}`); code != 201 {
			t.Fatalf("key %d: %d %v", i, code, out)
		}
	}
	if code, _ := mintKey(t, s, "ops-key", `{"name":"c"}`); code != 409 {
		t.Errorf("per-principal cap: %d, want 409", code)
	}
	mintKey(t, s, "app-key", `{"name":"d"}`)
	if code, _ := mintKey(t, s, "app-key", `{"name":"e"}`); code != 409 {
		t.Errorf("total cap: %d, want 409", code)
	}
}

func TestRevokeCascadesAndIsScoped(t *testing.T) {
	s := keyServer(t, t.TempDir(), nil)
	_, mid := mintKey(t, s, "ops-key", `{"name":"mid"}`)
	_, leaf := mintKey(t, s, mid["key"].(string), `{"name":"leaf"}`)
	_, other := mintKey(t, s, "app-key", `{"name":"other"}`)

	// A descendant cannot revoke its ancestor, nor a stranger's key; both look absent.
	if got := do(s, "DELETE", "/v1/keys/"+mid["id"].(string), "", "Bearer "+leaf["key"].(string)).Code; got != 404 {
		t.Errorf("revoking an ancestor: %d, want 404", got)
	}
	if got := do(s, "DELETE", "/v1/keys/"+other["id"].(string), "", "Bearer "+mid["key"].(string)).Code; got != 404 {
		t.Errorf("revoking a stranger's key: %d, want 404", got)
	}
	// Revoking the middle key takes the leaf with it.
	rec := do(s, "DELETE", "/v1/keys/"+mid["id"].(string), "", "Bearer ops-key")
	if rec.Code != 200 || len(decode(t, rec)["revoked"].([]any)) != 2 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	for _, k := range []string{mid["key"].(string), leaf["key"].(string)} {
		if got := do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer "+k).Code; got != 401 {
			t.Errorf("a revoked key still works: %d", got)
		}
	}
	if got := do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer "+other["key"].(string)).Code; got != 200 {
		t.Errorf("an unrelated key was revoked: %d", got)
	}
}

func TestLogoutRevokesTheCallingKey(t *testing.T) {
	s := keyServer(t, t.TempDir(), nil)
	_, k := mintKey(t, s, "ops-key", `{"name":"me"}`)
	if got := do(s, "POST", "/v1/logout", "", "Bearer "+k["key"].(string)).Code; got != 200 {
		t.Fatalf("logout: %d", got)
	}
	if got := do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer "+k["key"].(string)).Code; got != 401 {
		t.Errorf("key still works after logout: %d", got)
	}
	if got := do(s, "POST", "/v1/logout", "", "Bearer ops-key").Code; got != 409 {
		t.Errorf("a configured key is not revocable at runtime: %d, want 409", got)
	}
}

func TestMintingNeverRaisesAHoldersRate(t *testing.T) {
	s := keyServer(t, t.TempDir(), nil)
	now := time.Now()
	s.now = func() time.Time { return now } // no refill
	var keys []string
	for _, n := range []string{"a", "b", "c", "d"} {
		_, k := mintKey(t, s, "app-key", `{"name":"`+n+`"}`)
		keys = append(keys, k["key"].(string))
	}
	// "app" has a burst of 6 and four mints already spent 4 tokens: only 2 requests remain
	// across the whole tree, however many keys it has made.
	ok := 0
	for i := 0; i < 8; i++ {
		if do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer "+keys[i%len(keys)]).Code == 200 {
			ok++
		}
	}
	if ok != 2 {
		t.Errorf("%d requests got through the tree's shared bucket, want 2", ok)
	}
}

func TestRefusedChargeBurnsNoTokensAtOtherLevels(t *testing.T) {
	s := keyServer(t, t.TempDir(), nil)
	now := time.Now()
	s.now = func() time.Time { return now }
	// A child limited to 1 burst under "app" (burst 6).
	_, k := mintKey(t, s, "app-key", `{"name":"slow","rate_limit_per_minute":6,"burst":1}`)
	child := k["key"].(string)
	var app *client
	for _, c := range s.clients {
		if c.name == "app" {
			app = c
		}
	}
	if got := do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer "+child).Code; got != 200 {
		t.Fatalf("first request: %d", got)
	}
	before := app.bucket.tokens
	if got := do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer "+child).Code; got != 429 {
		t.Fatalf("second request should be limited by the child: %d", got)
	}
	if app.bucket.tokens != before {
		t.Errorf("a refused request spent %.1f tokens at the parent", before-app.bucket.tokens)
	}
}

func TestKeysSurviveARestartAndStoreOnlyHashes(t *testing.T) {
	dir := t.TempDir()
	s := keyServer(t, dir, nil)
	_, k := mintKey(t, s, "ops-key", `{"name":"keep","admin":true}`)
	token := k["key"].(string)

	raw, err := os.ReadFile(filepath.Join(dir, "state.json.keys"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("the key itself was written to disk")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "state.json.keys")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}

	s2 := keyServer(t, dir, nil)
	if got := do(s2, "GET", "/v1/keys", "", "Bearer "+token).Code; got != 200 {
		t.Errorf("minted key after restart: %d", got)
	}
}

func TestReplayDropsKeysWhoseCreatorChanged(t *testing.T) {
	dir := t.TempDir()
	s := keyServer(t, dir, nil)
	_, fast := mintKey(t, s, "app-key", `{"name":"fast","rate_limit_per_minute":60}`)
	_, gone := mintKey(t, s, "app-key", `{"name":"gone"}`)
	_, under := mintKey(t, s, gone["key"].(string), `{"name":"under"}`)
	_, safe := mintKey(t, s, "ops-key", `{"name":"safe"}`)

	// Restart with "app" tightened and renamed away: its keys are stale, not fatal.
	s2 := keyServer(t, dir, func(c *Config) {
		c.Clients = []ClientConfig{{Name: "ops", KeyEnv: "KO", Admin: true}, {Name: "app2", KeyEnv: "KA"}}
	})
	for name, k := range map[string]map[string]any{"fast": fast, "gone": gone, "under": under} {
		if got := do(s2, "GET", "/v1/keys", "", "Bearer "+k["key"].(string)).Code; got != 401 {
			t.Errorf("%s: still valid after its creator went away: %d", name, got)
		}
	}
	if got := do(s2, "GET", "/v1/keys", "", "Bearer "+safe["key"].(string)).Code; got != 200 {
		t.Errorf("an unaffected key was dropped: %d", got)
	}
	if len(s2.keys.staleKeys()) != 3 {
		t.Errorf("stale: %v", s2.keys.staleKeys())
	}
}

func TestExpiredKeysStopWorkingAndArePruned(t *testing.T) {
	s := keyServer(t, t.TempDir(), nil)
	_, k := mintKey(t, s, "ops-key", `{"name":"brief","expires_in_s":60}`)
	token := k["key"].(string)
	if got := do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer "+token).Code; got != 200 {
		t.Fatalf("before expiry: %d", got)
	}
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if got := do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer "+token).Code; got != 401 {
		t.Errorf("after expiry: %d, want 401", got)
	}
	do(s, "GET", "/v1/keys", "", "Bearer ops-key") // listing prunes
	if len(s.keys.runtime) != 0 {
		t.Errorf("expired key not pruned: %d left", len(s.keys.runtime))
	}
}

// --- users and login ---

func hashFor(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func userServer(t *testing.T, dir string, mut func(*Config)) *Server {
	t.Helper()
	return keyServer(t, dir, func(c *Config) {
		c.Users = []UserConfig{
			{Name: "sam", PasswordHash: hashFor(t, "correct horse"), Admin: true},
			{Name: "guest", PasswordHash: hashFor(t, "guest-pass"), Routes: []string{"cheap"}},
		}
		if mut != nil {
			mut(c)
		}
	})
}

func login(s *Server, user, pw string) (int, map[string]any) {
	b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	rec := do(s, "POST", "/v1/login", string(b), "")
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestUsersMustCarryABcryptHash(t *testing.T) {
	for name, tc := range map[string]struct {
		u    UserConfig
		want string
	}{
		"plaintext": {UserConfig{Name: "x", PasswordHash: "hunter2"}, "bcrypt hash"},
		"empty":     {UserConfig{Name: "x"}, "bcrypt hash"},
		"no name":   {UserConfig{PasswordHash: hashFor(t, "p")}, "name is required"},
		"clash":     {UserConfig{Name: "ops", PasswordHash: hashFor(t, "p")}, "duplicated"},
		"bad name":  {UserConfig{Name: "Not Ok", PasswordHash: hashFor(t, "p")}, "lowercase"},
		"reserved":  {UserConfig{Name: "default", PasswordHash: hashFor(t, "p")}, "duplicated"},
		"neg limit": {UserConfig{Name: "y", PasswordHash: hashFor(t, "p"), RateLimitPerMinute: -1}, "negative"},
	} {
		u, want := tc.u, tc.want
		t.Run(name, func(t *testing.T) {
			fk := newFake(t, sure)
			cfg := Config{Models: map[string]ignatius.ModelConfig{"m": {Provider: "systemone", BaseURL: fk.URL}},
				Clients: []ClientConfig{{Name: "ops", KeyEnv: "KO"}}, Users: []UserConfig{u}}
			if err := startErr(t, cfg); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v, want one containing %q", err, want)
			}
		})
	}
}

func TestLoginGivesASessionThatWorks(t *testing.T) {
	s := userServer(t, t.TempDir(), nil)
	code, out := login(s, "sam", "correct horse")
	if code != 200 || out["admin"] != true || out["user"] != "sam" {
		t.Fatalf("login: %d %v", code, out)
	}
	key := out["key"].(string)
	if got := do(s, "POST", "/v1/systemone", l1("smart"), "Bearer "+key).Code; got != 200 {
		t.Errorf("session could not route: %d", got)
	}
	if got := do(s, "GET", "/v1/stats", "", "Bearer "+key).Code; got != 200 {
		t.Errorf("an admin user could not read stats: %d", got)
	}
	// A restricted user stays restricted.
	_, g := login(s, "guest", "guest-pass")
	if got := do(s, "POST", "/v1/systemone", l1("smart"), "Bearer "+g["key"].(string)).Code; got != 403 {
		t.Errorf("guest escaped its allowlist: %d", got)
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	s := userServer(t, t.TempDir(), nil)
	c1, o1 := login(s, "sam", "wrong")
	c2, o2 := login(s, "nobody", "wrong")
	c3, o3 := login(s, "sam", strings.Repeat("x", 100))
	b1, _ := json.Marshal(o1)
	b2, _ := json.Marshal(o2)
	b3, _ := json.Marshal(o3)
	if c1 != 401 || c2 != 401 || c3 != 401 || string(b1) != string(b2) || string(b1) != string(b3) {
		t.Errorf("wrong password / unknown user / overlong differ: %d %s | %d %s | %d %s", c1, b1, c2, b2, c3, b3)
	}
}

func TestLoginIsThrottledBeforeAnyPasswordCheck(t *testing.T) {
	s := userServer(t, t.TempDir(), nil)
	now := time.Now()
	s.now = func() time.Time { return now }
	for i := 0; i < freeFailures; i++ {
		if code, _ := login(s, "sam", "wrong"); code != 401 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	// Locked out, even with the right password.
	rec := do(s, "POST", "/v1/login", `{"username":"sam","password":"correct horse"}`, "")
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("locked-out login: %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	now = now.Add(time.Minute)
	if code, _ := login(s, "sam", "correct horse"); code != 200 {
		t.Errorf("after the lockout: %d", code)
	}
}

func TestSessionsAreInMemoryAndOutlivedByTheKeysTheyMint(t *testing.T) {
	dir := t.TempDir()
	s := userServer(t, dir, nil)
	_, out := login(s, "sam", "correct horse")
	session := out["key"].(string)
	code, minted := mintKey(t, s, session, `{"name":"script"}`)
	if code != 201 || minted["name"] != "sam/script" {
		t.Fatalf("mint from session: %d %v", code, minted)
	}
	script := minted["key"].(string)

	// Nine hours on: the session has expired, the key it minted has not.
	s.now = func() time.Time { return time.Now().Add(9 * time.Hour) }
	if got := do(s, "GET", "/v1/keys", "", "Bearer "+session).Code; got != 401 {
		t.Errorf("expired session: %d, want 401", got)
	}
	if got := do(s, "POST", "/v1/systemone", l1("cheap"), "Bearer "+script).Code; got != 200 {
		t.Errorf("the key outlived by its session stopped working: %d", got)
	}

	// A restart signs everyone out but keeps the minted key.
	s2 := userServer(t, dir, nil)
	_, out2 := login(s2, "sam", "correct horse")
	if got := do(s2, "POST", "/v1/systemone", l1("cheap"), "Bearer "+script).Code; got != 200 {
		t.Errorf("minted key after restart: %d", got)
	}
	if got := do(s2, "GET", "/v1/keys", "", "Bearer "+session).Code; got != 401 {
		t.Errorf("old session after restart: %d, want 401", got)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json.keys"))
	if strings.Contains(string(raw), out2["key"].(string)) || strings.Contains(string(raw), session) {
		t.Error("a session reached the disk")
	}
}

func TestSessionLimitDropsTheOldest(t *testing.T) {
	s := userServer(t, t.TempDir(), func(c *Config) { c.Admin.MaxSessionsPerUser = 2 })
	var keys []string
	for i := 0; i < 3; i++ {
		_, o := login(s, "sam", "correct horse")
		keys = append(keys, o["key"].(string))
	}
	want := []int{401, 200, 200}
	for i, k := range keys {
		if got := do(s, "GET", "/v1/keys", "", "Bearer "+k).Code; got != want[i] {
			t.Errorf("session %d: %d, want %d", i, got, want[i])
		}
	}
}

func TestLogoutEndsASession(t *testing.T) {
	s := userServer(t, t.TempDir(), nil)
	_, o := login(s, "sam", "correct horse")
	k := o["key"].(string)
	if got := do(s, "POST", "/v1/logout", "", "Bearer "+k).Code; got != 200 {
		t.Fatalf("logout: %d", got)
	}
	if got := do(s, "GET", "/v1/keys", "", "Bearer "+k).Code; got != 401 {
		t.Errorf("session after logout: %d, want 401", got)
	}
}

func TestUsersAloneDoNotMakeTheGatewayAnonymous(t *testing.T) {
	s := keyServer(t, t.TempDir(), func(c *Config) {
		c.Clients = nil
		c.Users = []UserConfig{{Name: "sam", PasswordHash: hashFor(t, "pw"), Admin: true}}
	})
	if got := do(s, "POST", "/v1/systemone", l1("cheap"), "").Code; got != 403 {
		t.Errorf("an unauthenticated request with only users configured: %d, want 403", got)
	}
}

func TestSourceAddress(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/login", nil)
	r.RemoteAddr = "192.0.2.7:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.4")
	plain := userServer(t, t.TempDir(), nil)
	if got := plain.sourceAddr(r); got != "192.0.2.7" {
		t.Errorf("untrusted proxy: %q, want the socket address", got)
	}
	proxied := userServer(t, t.TempDir(), func(c *Config) { c.Admin.TrustProxy = true })
	if got := proxied.sourceAddr(r); got != "198.51.100.4" {
		t.Errorf("trusted proxy: %q, want the right-most entry", got)
	}
}

func TestLoginInfoOnlyWhenUsersAreConfigured(t *testing.T) {
	if got := do(userServer(t, t.TempDir(), nil), "GET", "/v1/login", "", "").Code; got != 200 {
		t.Errorf("with users: %d, want 200", got)
	}
	if got := do(keyServer(t, t.TempDir(), nil), "GET", "/v1/login", "", "").Code; got != 404 {
		t.Errorf("without users: %d, want 404", got)
	}
}

func TestUnknownUserCostsAsMuchAsAWrongPassword(t *testing.T) {
	// The hash an unknown name is compared against must match the cost of the real ones, or
	// the faster failure tells an attacker which usernames exist.
	s := userServer(t, t.TempDir(), func(c *Config) {
		h, _ := bcrypt.GenerateFromPassword([]byte("pw"), 12)
		c.Users = []UserConfig{{Name: "sam", PasswordHash: string(h)}}
	})
	if cost, err := bcrypt.Cost(s.dummyHash); err != nil || cost != 12 {
		t.Errorf("dummy hash cost %d (%v), want 12", cost, err)
	}
}

func TestAttackerChosenUsernamesAreNotStored(t *testing.T) {
	s := userServer(t, t.TempDir(), nil)
	huge := strings.Repeat("A", 100000)
	for i := 0; i < 3; i++ {
		if code, _ := login(s, huge, "x"); code != 401 {
			t.Fatalf("huge name: %d, want the normal 401", code)
		}
	}
	for k := range s.throttle.m {
		if len(k) > 100 || strings.HasPrefix(k, "user:A") {
			t.Errorf("throttle holds an attacker-chosen key (%d bytes)", len(k))
		}
	}
}

func TestOneGoodLoginDoesNotClearTheAddressThrottle(t *testing.T) {
	s := userServer(t, t.TempDir(), nil)
	now := time.Now()
	s.now = func() time.Time { return now }
	for i := 0; i < freeFailures-1; i++ {
		login(s, "guest", "wrong")
	}
	login(s, "sam", "correct horse") // a valid account logs in from the same address
	login(s, "nobody", "wrong")      // one more failure from the address
	if code, _ := login(s, "nobody2", "wrong"); code != 429 {
		t.Errorf("address throttle was cleared by an unrelated good login: %d, want 429", code)
	}
}
