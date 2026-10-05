package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// UserConfig is one [[users]] entry (SPEC 11.6). Only a bcrypt hash is ever in the config.
type UserConfig struct {
	Name               string   `toml:"name"`
	PasswordHash       string   `toml:"password_hash"`
	Admin              bool     `toml:"admin"`
	Routes             []string `toml:"routes"`
	RateLimitPerMinute int      `toml:"rate_limit_per_minute"`
	Burst              int      `toml:"burst"`
	StoreContent       bool     `toml:"store_content"`  // SPEC 13.3
	RetentionDays      int      `toml:"retention_days"` // content retention; see ClientConfig
}

// maxPasswordBytes is bcrypt's input limit; it ignores anything past it.
const maxPasswordBytes = 72

// buildUsers validates [[users]] and returns one principal per user. The principal has
// no key of its own: a user signs in and holds sessions.
func buildUsers(cfg Config, taken map[string]bool, now time.Time) (map[string]*client, error) {
	out := map[string]*client{}
	for i, u := range cfg.Users {
		switch {
		case u.Name == "":
			return nil, fmt.Errorf("users[%d]: name is required", i)
		case !nameRule.MatchString(u.Name):
			return nil, fmt.Errorf("user %q: name must be lowercase letters, digits, '.', '_' or '-'", u.Name)
		case taken[u.Name] || out[u.Name] != nil:
			return nil, fmt.Errorf("user name %q is duplicated, or equals a client name", u.Name)
		case u.RateLimitPerMinute < 0 || u.Burst < 0:
			return nil, fmt.Errorf("user %q: rate_limit_per_minute and burst cannot be negative", u.Name)
		}
		if _, err := bcrypt.Cost([]byte(u.PasswordHash)); err != nil {
			return nil, fmt.Errorf("user %q: password_hash must be a bcrypt hash (use `ignatius hash-password`), never a plaintext password", u.Name)
		}
		c := &client{name: u.Name, admin: u.Admin, storeContent: u.StoreContent}
		if u.RateLimitPerMinute > 0 {
			c.bucket = newBucket(u.RateLimitPerMinute, u.Burst, now)
			c.perMinute, c.burst = u.RateLimitPerMinute, int(c.bucket.burst)
		}
		if len(u.Routes) > 0 {
			c.routes = map[string]bool{}
			for _, r := range u.Routes {
				c.routes[r] = true
			}
		}
		out[u.Name] = c
	}
	return out, nil
}

// newDummyHash makes the hash an unknown user is compared against, at the highest cost any
// configured user has, so a wrong user and a wrong password take the same time.
func newDummyHash(cfg Config) []byte {
	cost := bcrypt.MinCost
	for _, u := range cfg.Users {
		if c, err := bcrypt.Cost([]byte(u.PasswordHash)); err == nil && c > cost {
			cost = c
		}
	}
	h, _ := bcrypt.GenerateFromPassword([]byte("ignatius-dummy"), cost)
	return h
}

func (s *Server) dummyCompare(pw []byte) { _ = bcrypt.CompareHashAndPassword(s.dummyHash, pw) }

// throttle slows guessing: after a few failures a key (a source address or a username)
// is locked out for a doubling interval. It is consulted before any bcrypt work.
type throttle struct {
	mu sync.Mutex
	m  map[string]*throttleEntry
}

type throttleEntry struct {
	fails int
	until time.Time
	seen  time.Time
}

const (
	freeFailures = 5
	maxLockout   = 5 * time.Minute
	maxEntries   = 10000
)

func newThrottle() *throttle { return &throttle{m: map[string]*throttleEntry{}} }

// wait reports how long key must still wait; zero means it may try.
func (t *throttle) wait(key string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.m[key]; e != nil && now.Before(e.until) {
		return e.until.Sub(now)
	}
	return 0
}

func (t *throttle) fail(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.m) >= maxEntries {
		for k, e := range t.m {
			if now.Sub(e.seen) > maxLockout {
				delete(t.m, k)
			}
		}
	}
	e := t.m[key]
	if e == nil {
		if len(t.m) >= maxEntries { // full of live entries: do not grow without bound
			return
		}
		e = &throttleEntry{}
		t.m[key] = e
	}
	e.fails++
	e.seen = now
	if e.fails >= freeFailures {
		d := time.Second << min(e.fails-freeFailures, 9)
		e.until = now.Add(min(d, maxLockout))
	}
}

func (t *throttle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, key)
}

// sourceAddr is the caller's address: the socket's, or with trust_proxy the right-most
// X-Forwarded-For entry (the one the proxy appended).
func (s *Server) sourceAddr(r *http.Request) string {
	if s.cfg.Admin.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if a := strings.TrimSpace(parts[len(parts)-1]); a != "" {
				return a
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleLoginInfo tells the status page whether to offer a sign-in form. It reveals no
// user names.
func (s *Server) handleLoginInfo(w http.ResponseWriter, _ *http.Request) {
	if len(s.users) == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "login is not enabled", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"password_login": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if len(s.users) == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "login is not enabled", nil)
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be {\"username\", \"password\"}", nil)
		return
	}
	now := s.now()
	// A name that could not be a user's is treated as an unknown user: it is not used as a
	// throttle key or logged, so an attacker cannot make us store what they choose.
	valid := nameRule.MatchString(in.Username)
	who := "(invalid)"
	addr, userKey := "addr:"+s.sourceAddr(r), ""
	if valid {
		who, userKey = in.Username, "user:"+in.Username
	}
	// Throttle first: bcrypt is CPU an attacker could otherwise make us spend.
	if wait := max(s.throttle.wait(addr, now), s.throttle.wait(userKey, now)); wait > 0 {
		secs := int(wait.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many failed sign-ins; try again later", map[string]any{"retry_after_s": secs})
		return
	}
	u, known := s.users[in.Username]
	pw := []byte(in.Password)
	good := false
	switch {
	case !known:
		s.dummyCompare(pw)
	case len(pw) > maxPasswordBytes:
		s.dummyCompare(pw[:maxPasswordBytes])
	default:
		good = bcrypt.CompareHashAndPassword([]byte(s.userHash[in.Username]), pw) == nil
	}
	if !good {
		s.throttle.fail(addr, now)
		if valid {
			s.throttle.fail(userKey, now)
		}
		s.log.Warn("login_failed", "user", who, "addr", s.sourceAddr(r))
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password", nil)
		return
	}
	s.throttle.reset(userKey) // not the address: one good account must not clear guesses at others

	token, err := newToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not create a session", nil)
		return
	}
	ttl := time.Duration(s.cfg.Admin.LoginTTLS) * time.Second
	sess := &client{name: u.name, label: u.name, admin: u.admin, routes: u.routes, owner: u, session: true,
		hash: hashKey(token), created: now, expires: now.Add(ttl)}
	s.keys.addSession(sess, s.cfg.Admin.MaxSessionsPerUser, now)
	s.log.Info("login_ok", "user", u.name, "addr", s.sourceAddr(r))
	writeJSON(w, http.StatusOK, map[string]any{"key": token, "expires_in_s": int(ttl.Seconds()), "user": u.name, "admin": u.admin})
}

// keyView is a runtime key as listed: never the key itself.
func keyView(c *client) map[string]any {
	v := map[string]any{"id": c.id, "name": c.full(), "creator": c.parent.full(), "created": c.created, "admin": c.admin}
	if !c.expires.IsZero() {
		v["expires"] = c.expires
	}
	if c.perMinute > 0 {
		v["rate_limit_per_minute"], v["burst"] = c.perMinute, c.burst
	}
	if c.routes != nil {
		rs := []string{}
		for n := range c.routes {
			rs = append(rs, n)
		}
		v["routes"] = sortedStrings(rs)
	}
	return v
}

func (s *Server) keysEnabled(w http.ResponseWriter) bool {
	if !s.cfg.Admin.SelfServiceKeys {
		writeErr(w, http.StatusForbidden, "editing_disabled", "self-service keys are off (set [admin] self_service_keys = true)", nil)
		return false
	}
	return true
}

func writeKeyErr(w http.ResponseWriter, err error) {
	var ke *KeyError
	if errors.As(err, &ke) {
		var details map[string]any
		if ke.Field != "" {
			details = map[string]any{"field": ke.Field}
		}
		writeErr(w, ke.Status, ke.Code, ke.Msg, details)
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal", err.Error(), nil)
}

func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok || !s.keysEnabled(w) || !s.admit(w, c) {
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var m mint
	if err := strictJSON(body, &m); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	k, token, err := s.keys.mintKey(c.principal(), m, s.now(), s.router.knownName)
	if err != nil {
		writeKeyErr(w, err)
		return
	}
	s.log.Info("key_created", "actor", c.principal().full(), "id", k.id, "name", k.full(), "admin", k.admin,
		"rate_limit_per_minute", k.perMinute, "routes", sortedStrings(mapKeys(k.routes)))
	writeJSON(w, http.StatusCreated, map[string]any{"id": k.id, "name": k.full(), "key": token})
}

func (s *Server) handleKeyList(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok || !s.keysEnabled(w) {
		return
	}
	out := []map[string]any{}
	for _, k := range s.keys.visibleTo(c.principal(), c.admin, s.now()) {
		out = append(out, keyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok || !s.keysEnabled(w) {
		return
	}
	target := s.keys.byID(r.PathValue("id"))
	// The same 404 for a missing key and one the caller may not touch.
	if target == nil || (!c.admin && !(target != c.principal() && within(target, c.principal()))) {
		writeErr(w, http.StatusNotFound, "not_found", "no such key", nil)
		return
	}
	s.revokeAndReport(w, c, target)
}

func (s *Server) revokeAndReport(w http.ResponseWriter, actor, target *client) {
	gone, err := s.keys.revoke(target)
	if err != nil {
		writeKeyErr(w, err)
		return
	}
	ids := []string{}
	for _, g := range gone {
		ids = append(ids, g.id)
		s.log.Info("key_revoked", "actor", actor.principal().full(), "id", g.id, "name", g.full())
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": ids})
}

// handleLogout ends the calling session, or revokes the calling runtime key and the keys
// under it. A configured client's key is not revocable at runtime.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	switch {
	case c.session:
		s.keys.dropSession(c)
		writeJSON(w, http.StatusOK, map[string]any{"revoked": []string{}})
	case c.parent != nil:
		s.revokeAndReport(w, c, c)
	default:
		writeErr(w, http.StatusConflict, "not_removable", "this key comes from the config; remove it there", nil)
	}
}
