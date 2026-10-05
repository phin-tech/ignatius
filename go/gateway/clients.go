package gateway

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ClientConfig is one [[clients]] entry (SPEC 11.4). The key itself is never in
// the config; KeyEnv names the environment variable that holds it.
type ClientConfig struct {
	Name               string   `toml:"name"`
	KeyEnv             string   `toml:"key_env"`
	RateLimitPerMinute int      `toml:"rate_limit_per_minute"`
	Burst              int      `toml:"burst"`
	Admin              bool     `toml:"admin"`
	Routes             []string `toml:"routes"`
	// StoreContent keeps this client's state, questions and every model's answers in the
	// store (SPEC 13.3). Off by default. RetentionDays deletes that content after this many
	// days; 0 falls back to [store] retention_days, which deletes everything.
	StoreContent  bool `toml:"store_content"`
	RetentionDays int  `toml:"retention_days"`
}

// client is an authenticated caller. The key is held only to compare tokens.
//
// name is the metrics and counter label, always a configured client or user, so a
// runtime key's free-form name cannot grow label cardinality. label is the full
// "<principal>/<name>" used in the status page and the audit log.
type client struct {
	name   string
	label  string
	key    string
	admin  bool
	routes map[string]bool // nil = every route, alias and inline route is allowed
	bucket *bucket         // nil = unlimited

	perMinute, burst int // the configured limit, 0 = unlimited; bounds what a child may be given

	storeContent bool // SPEC 13.3; read through root(), so a runtime key follows its principal

	// Runtime keys and login sessions (SPEC 11.5, 11.6).
	id      string
	hash    [32]byte
	created time.Time
	expires time.Time // zero = never
	parent  *client   // runtime key: the principal that minted it
	owner   *client   // login session: the user it is a credential of
	session bool      // held in memory only
}

// full is the audit name.
func (c *client) full() string {
	if c.label != "" {
		return c.label
	}
	return c.name
}

// principal is who a request acts as: a session acts as its user, everything else as itself.
func (c *client) principal() *client {
	if c.owner != nil {
		return c.owner
	}
	return c
}

// chain is every bucket owner a request is charged to, leaf first: the key's own,
// then each ancestor principal's. Minting more keys therefore never raises a rate.
func (c *client) chain() []*client {
	var out []*client
	for p := c.principal(); p != nil; p = p.parent {
		out = append(out, p)
	}
	return out
}

// root is the configured client or user a credential ultimately acts as.
func (c *client) root() *client {
	for {
		switch {
		case c.owner != nil:
			c = c.owner
		case c.parent != nil:
			c = c.parent
		default:
			return c
		}
	}
}

func (c *client) expired(now time.Time) bool { return !c.expires.IsZero() && !now.Before(c.expires) }

// anonymous is used when no keys are configured at all (loopback development).
var anonymous = &client{name: "anonymous", admin: true}

// bucket is a token bucket refilled continuously.
type bucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(perMinute, burst int, now time.Time) *bucket {
	if burst <= 0 {
		burst = perMinute / 10
		if burst < 1 {
			burst = 1
		}
	}
	return &bucket{rate: float64(perMinute) / 60, burst: float64(burst), tokens: float64(burst), last: now}
}

func (b *bucket) refill(now time.Time) {
	if dt := now.Sub(b.last).Seconds(); dt > 0 {
		b.tokens = math.Min(b.burst, b.tokens+dt*b.rate)
		b.last = now
	}
}

// take spends one token, or reports how long until one is available.
func (b *bucket) take(now time.Time) (bool, time.Duration) {
	return takeAll([]*bucket{b}, now)
}

// takeAll spends one token from every bucket, or from none. The buckets are locked in
// the order given (callers pass root first) so concurrent chains cannot deadlock, and
// all are checked before any is spent: a refusal at one level never burns tokens at
// another. The wait returned is the longest any of them needs.
func takeAll(bs []*bucket, now time.Time) (bool, time.Duration) {
	var wait time.Duration
	for _, b := range bs {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.refill(now)
		if b.tokens < 1 {
			if w := time.Duration((1 - b.tokens) / b.rate * float64(time.Second)); w > wait {
				wait = w
			}
		}
	}
	if wait > 0 {
		return false, wait
	}
	for _, b := range bs {
		b.tokens--
	}
	return true, 0
}

// buildClients validates [[clients]] and the legacy api_key_env keys. Legacy keys
// all become the admin, unlimited client "default".
func buildClients(cfg Config, getenv func(string) string, now time.Time) ([]*client, error) {
	var out []*client
	seenKey := map[string]string{}
	add := func(c *client) error {
		if prev, dup := seenKey[c.key]; dup {
			return fmt.Errorf("clients %q and %q share a key; every client needs its own", prev, c.name)
		}
		seenKey[c.key] = c.name
		out = append(out, c)
		return nil
	}
	for _, k := range strings.Split(getenv(cfg.APIKeyEnv), ",") {
		if k = strings.TrimSpace(k); k != "" {
			if err := add(&client{name: "default", key: k, admin: true}); err != nil {
				return nil, err
			}
		}
	}
	names := map[string]bool{"default": true}
	for i, cc := range cfg.Clients {
		switch {
		case cc.Name == "":
			return nil, fmt.Errorf("clients[%d]: name is required", i)
		case names[cc.Name]:
			return nil, fmt.Errorf("client name %q is duplicated or reserved", cc.Name)
		case cc.KeyEnv == "":
			return nil, fmt.Errorf("client %q: key_env is required", cc.Name)
		case cc.RateLimitPerMinute < 0 || cc.Burst < 0:
			return nil, fmt.Errorf("client %q: rate_limit_per_minute and burst cannot be negative", cc.Name)
		}
		names[cc.Name] = true
		key := strings.TrimSpace(getenv(cc.KeyEnv))
		if key == "" {
			return nil, fmt.Errorf("client %q: environment variable %s is not set", cc.Name, cc.KeyEnv)
		}
		c := &client{name: cc.Name, key: key, admin: cc.Admin, storeContent: cc.StoreContent}
		if cc.RateLimitPerMinute > 0 {
			c.bucket = newBucket(cc.RateLimitPerMinute, cc.Burst, now)
			c.perMinute, c.burst = cc.RateLimitPerMinute, int(c.bucket.burst)
		}
		if len(cc.Routes) > 0 {
			c.routes = map[string]bool{}
			for _, r := range cc.Routes {
				c.routes[r] = true
			}
		}
		if err := add(c); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// validateAllowlists checks every allowlist entry names a real route or alias.
func validateAllowlists(cs []*client, rt *Router) error {
	for _, c := range cs {
		for name := range c.routes {
			if name == "inline" {
				continue
			}
			if !rt.knownName(name) {
				return fmt.Errorf("client %q: routes entry %q is not a route, profile or model", c.name, name)
			}
		}
	}
	return nil
}

// admit counts the request and applies the client's rate limit.
func (s *Server) admit(w http.ResponseWriter, c *client) bool {
	s.stats.NoteRequest(c.name)
	var bs []*bucket
	for _, p := range c.chain() { // leaf first; lock root first
		if p.bucket != nil {
			bs = append([]*bucket{p.bucket}, bs...)
		}
	}
	if len(bs) == 0 {
		return true
	}
	ok, retry := takeAll(bs, s.now())
	if ok {
		return true
	}
	s.stats.NoteLimited(c.name)
	gt.rateLimited.Add(context.Background(), 1, metric.WithAttributes(attribute.String("client", c.name)))
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeErr(w, http.StatusTooManyRequests, "rate_limited",
		fmt.Sprintf("rate limit exceeded for client %q", c.name), map[string]any{"retry_after_s": secs})
	return false
}

// routeAllowed enforces a client's route allowlist. An entry is a named route
// (usable as that route only) or an alias (usable alone, and inside an inline
// route). "inline" permits inline routes, but every alias inside one must itself
// be on the list, so "inline" can never reach a model the client was not given.
// A restricted client gets the same 403 for a forbidden and an unknown name, so
// it cannot enumerate routes or models from errors.
func (s *Server) routeAllowed(w http.ResponseWriter, c *client, model string) bool {
	if c.routes == nil {
		return true
	}
	name, kind := s.router.Classify(model)
	ok := false
	switch kind {
	case "route", "profile", "alias": // an entry is matched by the name the request used
		ok = c.routes[name]
	case "inline":
		ok = c.routes["inline"] && s.inlineAliasesAllowed(c, name)
	}
	if !ok {
		writeErr(w, http.StatusForbidden, "route_not_allowed",
			fmt.Sprintf("client %q may not use that route", c.name), nil)
	}
	return ok
}

// inlineAliasesAllowed reports whether every model named inside an inline route
// is on the client's allowlist.
func (s *Server) inlineAliasesAllowed(c *client, model string) bool {
	plan, err := ignatius.ParseInline(model, s.router.MaxInline)
	if err != nil {
		return false
	}
	for _, m := range plan.Models {
		if !c.routes[m] {
			return false
		}
	}
	for _, t := range plan.Tiers {
		if !c.routes[t.Model] {
			return false
		}
	}
	return true
}

// visible reports whether discovery endpoints may show a route or alias name to c.
func (c *client) visible(name string) bool { return c.routes == nil || c.routes[name] }

// knownName reports whether name is a route, profile or model alias.
func (r *Router) knownName(name string) bool {
	ps := r.rc.snapshot()
	_, isRoute := ps.route[name]
	_, isModel := r.Reg[name]
	_, isProfile := ps.target[name]
	return isRoute || isModel || isProfile
}
