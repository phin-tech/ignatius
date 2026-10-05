package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// keyStore holds the runtime keys (SPEC 11.5) and the login sessions (11.6). Runtime keys
// are persisted, hashed, in a file beside the state file; sessions live in memory only.
// The tree is made of principals (a configured client, a user, a runtime key) linked by
// name, so rotating a configured key never orphans the keys under it.
type keyStore struct {
	mu         sync.RWMutex
	principals map[string]*client // full name -> configured client, user or runtime key
	runtime    []*client          // creation order, parents before children
	sessions   []*client
	path       string
	max        int
	maxPer     int
	stale      []string
}

var nameRule = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func newKeyStore(cfg Config) *keyStore {
	k := &keyStore{principals: map[string]*client{}, max: cfg.Admin.MaxKeys, maxPer: cfg.Admin.MaxKeysPerPrincipal}
	if cfg.Admin.SelfServiceKeys && cfg.Admin.StateFile != "" {
		k.path = cfg.Admin.StateFile + ".keys"
	}
	return k
}

func hashKey(token string) [32]byte { return sha256.Sum256([]byte(token)) }

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ig_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// find matches a presented token against every stored hash without an early exit. It
// returns nil for no match and for an expired key.
func (k *keyStore) find(token string, now time.Time) *client {
	h := hashKey(token)
	k.mu.RLock()
	defer k.mu.RUnlock()
	var match *client
	for _, list := range [][]*client{k.runtime, k.sessions} {
		for _, c := range list {
			if subtle.ConstantTimeCompare(h[:], c.hash[:]) == 1 {
				match = c
			}
		}
	}
	if match == nil || match.expired(now) {
		return nil
	}
	return match
}

// pruneLocked drops expired keys, and the keys under any key dropped.
func (k *keyStore) pruneLocked(now time.Time) {
	dead := map[*client]bool{}
	for _, c := range k.runtime {
		if c.expired(now) || (c.parent != nil && dead[c.parent]) {
			dead[c] = true
		}
	}
	k.runtime = keep(k.runtime, func(c *client) bool { return !dead[c] })
	k.sessions = keep(k.sessions, func(c *client) bool { return !c.expired(now) })
	for n, p := range k.principals {
		if dead[p] {
			delete(k.principals, n)
		}
	}
}

func keep(in []*client, ok func(*client) bool) []*client {
	out := in[:0:0]
	for _, c := range in {
		if ok(c) {
			out = append(out, c)
		}
	}
	return out
}

// within reports whether anc is p or one of p's ancestors.
func within(c, anc *client) bool {
	for p := c; p != nil; p = p.parent {
		if p == anc {
			return true
		}
	}
	return false
}

// mint is a request for a new key.
type mint struct {
	Name      string   `json:"name"`
	Rate      *int     `json:"rate_limit_per_minute"`
	Burst     *int     `json:"burst"`
	Routes    []string `json:"routes"`
	Admin     bool     `json:"admin"`
	ExpiresIn *int     `json:"expires_in_s"`
}

// KeyError is a refused mint: Field names the limit the creator would be exceeded on.
type KeyError struct {
	Code, Field, Msg string
	Status           int
}

func (e *KeyError) Error() string { return e.Msg }

func exceeds(field, format string, a ...any) *KeyError {
	return &KeyError{Code: "exceeds_creator", Field: field, Status: http.StatusUnprocessableEntity, Msg: fmt.Sprintf(format, a...)}
}

// childOf builds the key a creator may mint, or says which field exceeds the creator.
func childOf(parent *client, m mint, now time.Time, routeOK func(string) bool) (*client, *KeyError) {
	bad := func(msg string) *KeyError {
		return &KeyError{Code: "invalid_key", Status: http.StatusUnprocessableEntity, Msg: msg}
	}
	if !nameRule.MatchString(m.Name) {
		return nil, bad("name must be lowercase letters, digits, '.', '_' or '-' (at most 63 characters)")
	}
	c := &client{name: parent.name, label: parent.full() + "/" + m.Name, parent: parent, created: now, admin: m.Admin}
	if m.Admin && !parent.admin {
		return nil, exceeds("admin", "this key is not an admin and cannot mint one")
	}
	if len(m.Routes) > 0 {
		c.routes = map[string]bool{}
		for _, r := range m.Routes {
			if r != "inline" && !routeOK(r) {
				return nil, bad(fmt.Sprintf("routes entry %q is not a route, profile or model", r))
			}
			if parent.routes != nil && !parent.routes[r] {
				return nil, exceeds("routes", "route %q is not one this key may use", r)
			}
			c.routes[r] = true
		}
	} else if parent.routes != nil {
		c.routes = map[string]bool{}
		for r := range parent.routes {
			c.routes[r] = true
		}
	}
	if m.Rate != nil {
		if *m.Rate < 0 {
			return nil, bad("rate_limit_per_minute cannot be negative")
		}
		if parent.perMinute > 0 && (*m.Rate == 0 || *m.Rate > parent.perMinute) {
			return nil, exceeds("rate_limit_per_minute", "the limit cannot exceed this key's %d per minute", parent.perMinute)
		}
		if *m.Rate > 0 {
			b := 0
			if m.Burst != nil {
				if *m.Burst < 0 {
					return nil, bad("burst cannot be negative")
				}
				b = *m.Burst
			}
			c.bucket = newBucket(*m.Rate, b, now)
			if m.Burst == nil && parent.perMinute > 0 && int(c.bucket.burst) > parent.burst {
				c.bucket = newBucket(*m.Rate, parent.burst, now) // the default burst never exceeds the creator's
			}
			c.perMinute, c.burst = *m.Rate, int(c.bucket.burst)
			if parent.perMinute > 0 && c.burst > parent.burst {
				return nil, exceeds("burst", "burst cannot exceed this key's %d", parent.burst)
			}
		}
	}
	c.expires = parent.expires
	if m.ExpiresIn != nil {
		if *m.ExpiresIn <= 0 {
			return nil, bad("expires_in_s must be positive")
		}
		e := now.Add(time.Duration(*m.ExpiresIn) * time.Second)
		if !parent.expires.IsZero() && e.After(parent.expires) {
			return nil, exceeds("expires_in_s", "the key cannot outlive its creator, which expires at %s", parent.expires.UTC().Format(time.RFC3339))
		}
		c.expires = e
	}
	return c, nil
}

// keyRecord and keyFile are the persisted form. Only hashes are stored.
type keyRecord struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Parent  string    `json:"parent"`
	Hash    string    `json:"hash"`
	Admin   bool      `json:"admin,omitempty"`
	Routes  []string  `json:"routes,omitempty"`
	Rate    int       `json:"rate_limit_per_minute,omitempty"`
	Burst   int       `json:"burst,omitempty"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires,omitempty"`
}

type keyFile struct {
	Keys []keyRecord `json:"keys"`
}

func recordOf(c *client) keyRecord {
	r := keyRecord{ID: c.id, Name: c.label[len(c.parent.full())+1:], Parent: c.parent.full(),
		Hash: hex.EncodeToString(c.hash[:]), Admin: c.admin, Rate: c.perMinute, Burst: c.burst,
		Created: c.created, Expires: c.expires}
	for n := range c.routes {
		r.Routes = append(r.Routes, n)
	}
	sort.Strings(r.Routes)
	return r
}

// save writes the runtime keys by atomic rename at mode 0600.
func (k *keyStore) save(list []*client) error {
	if k.path == "" {
		return nil
	}
	f := keyFile{Keys: []keyRecord{}}
	for _, c := range list {
		f.Keys = append(f.Keys, recordOf(c))
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(k.path), ".ignatius-keys-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), k.path)
}

// load replays the saved keys on top of the config. A key whose creator is gone, or
// would now exceed it, is skipped and reported as stale, never fatal, and so is
// everything under it. Expired keys are dropped.
func (k *keyStore) load(now time.Time, routeOK func(string) bool) error {
	if k.path == "" {
		return nil
	}
	raw, err := os.ReadFile(k.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f keyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("saved keys %s: %w", k.path, err)
	}
	for _, r := range f.Keys {
		parent := k.principals[r.Parent]
		if parent == nil {
			k.stale = append(k.stale, r.Parent+"/"+r.Name)
			continue
		}
		if !r.Expires.IsZero() && !now.Before(r.Expires) {
			continue
		}
		m := mint{Name: r.Name, Admin: r.Admin, Routes: r.Routes}
		if r.Rate > 0 {
			rate, burst := r.Rate, r.Burst
			m.Rate, m.Burst = &rate, &burst
		}
		c, kerr := childOf(parent, m, r.Created, routeOK)
		h, herr := hex.DecodeString(r.Hash)
		if kerr != nil || herr != nil || len(h) != 32 {
			k.stale = append(k.stale, r.Parent+"/"+r.Name)
			continue
		}
		c.expires = r.Expires // childOf inherited the parent's; restore what was granted
		copy(c.hash[:], h)
		c.id = r.ID
		if k.principals[c.label] != nil {
			continue
		}
		k.principals[c.label] = c
		k.runtime = append(k.runtime, c)
	}
	return nil
}

// mintKey creates a key for parent, persisting it before it is returned.
func (k *keyStore) mintKey(parent *client, m mint, now time.Time, routeOK func(string) bool) (*client, string, error) {
	c, kerr := childOf(parent, m, now, routeOK)
	if kerr != nil {
		return nil, "", kerr
	}
	token, err := newToken()
	if err != nil {
		return nil, "", err
	}
	c.hash = hashKey(token)
	idb := make([]byte, 6)
	if _, err := rand.Read(idb); err != nil {
		return nil, "", err
	}
	c.id = hex.EncodeToString(idb)

	k.mu.Lock()
	defer k.mu.Unlock()
	k.pruneLocked(now)
	own := 0
	for _, r := range k.runtime {
		if r.parent == parent {
			own++
		}
	}
	switch {
	case len(k.runtime) >= k.max:
		return nil, "", &KeyError{Code: "too_many_keys", Status: http.StatusConflict, Msg: fmt.Sprintf("the gateway already has %d runtime keys", k.max)}
	case own >= k.maxPer:
		return nil, "", &KeyError{Code: "too_many_keys", Status: http.StatusConflict, Msg: fmt.Sprintf("this key has already created %d keys", k.maxPer)}
	case k.principals[c.label] != nil:
		return nil, "", &KeyError{Code: "name_taken", Status: http.StatusConflict, Msg: fmt.Sprintf("%q already exists", c.label)}
	}
	next := append(append([]*client{}, k.runtime...), c)
	if err := k.save(next); err != nil {
		return nil, "", &KeyError{Code: "save_failed", Status: http.StatusInternalServerError, Msg: "could not save the key: " + err.Error()}
	}
	k.runtime = next
	k.principals[c.label] = c
	return c, token, nil
}

// revoke removes target and every key under it, returning the keys removed.
func (k *keyStore) revoke(target *client) ([]*client, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var gone, left []*client
	for _, c := range k.runtime {
		if within(c, target) {
			gone = append(gone, c)
		} else {
			left = append(left, c)
		}
	}
	if len(gone) == 0 {
		return nil, nil
	}
	if err := k.save(left); err != nil {
		return nil, &KeyError{Code: "save_failed", Status: http.StatusInternalServerError, Msg: "could not save the change: " + err.Error()}
	}
	k.runtime = left
	for _, c := range gone {
		delete(k.principals, c.label)
	}
	return gone, nil
}

// byID finds a runtime key.
func (k *keyStore) byID(id string) *client {
	k.mu.RLock()
	defer k.mu.RUnlock()
	for _, c := range k.runtime {
		if c.id == id {
			return c
		}
	}
	return nil
}

// visibleTo lists the runtime keys under p; an admin sees them all.
func (k *keyStore) visibleTo(p *client, admin bool, now time.Time) []*client {
	k.mu.Lock()
	k.pruneLocked(now)
	defer k.mu.Unlock()
	var out []*client
	for _, c := range k.runtime {
		if admin || (c != p && within(c, p)) {
			out = append(out, c)
		}
	}
	return out
}

// addSession registers a login session for user, dropping the user's oldest past max.
func (k *keyStore) addSession(c *client, perUser int, now time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pruneLocked(now)
	n := 0
	for _, s := range k.sessions {
		if s.owner == c.owner {
			n++
		}
	}
	var out []*client
	for _, s := range k.sessions {
		if s.owner == c.owner && n >= perUser {
			n--
			continue
		}
		out = append(out, s)
	}
	k.sessions = append(out, c)
}

func (k *keyStore) dropSession(c *client) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.sessions = keep(k.sessions, func(s *client) bool { return s != c })
}

func (k *keyStore) staleKeys() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return append([]string(nil), k.stale...)
}
