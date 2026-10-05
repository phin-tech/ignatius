// Package gateway is the Ignatius HTTP server: a Jev-compatible /v1/systemone
// (L1, mode chosen by the model name) and the native /v1/route (L2, explicit Plan).
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/phin-tech/ignatius/go/events"
	"github.com/phin-tech/ignatius/go/ignatius"
	"github.com/phin-tech/ignatius/go/telemetry"
)

type Config struct {
	Listen            string                          `toml:"listen"`
	APIKeyEnv         string                          `toml:"api_key_env"`
	MaxRequestBytes   int64                           `toml:"max_request_bytes"`
	RequestTimeoutMS  int                             `toml:"request_timeout_ms"`
	DefaultRoute      string                          `toml:"default_route"`
	AllowInlineRoutes *bool                           `toml:"allow_inline_routes"`
	MaxInlineModels   int                             `toml:"max_inline_models"`
	Cache             ignatius.CacheConfig            `toml:"cache"`
	Telemetry         telemetry.Config                `toml:"telemetry"`
	Store             *StoreConfig                    `toml:"store"`  // nil: no database (SPEC 13)
	Events            events.Config                   `toml:"events"` // SPEC 14
	Audit             *AuditConfig                    `toml:"audit"`  // SPEC 13.7
	Eval              EvalConfig                      `toml:"eval"`   // SPEC 15
	Profiles          map[string]string               `toml:"profiles"`
	Admin             AdminConfig                     `toml:"admin"`
	Clients           []ClientConfig                  `toml:"clients"`
	Users             []UserConfig                    `toml:"users"`
	Models            map[string]ignatius.ModelConfig `toml:"models"`
	Routes            map[string]map[string]any       `toml:"routes"`
}

// AdminConfig is the [admin] table. Editing is opt-in: the config file is the source of
// truth, and a deployment that wants it read-only simply leaves this off. Models and
// clients are never editable at runtime: they carry URLs, keys and access control.
type AdminConfig struct {
	// EditProfiles lets an admin client change profiles at runtime, via the API and the
	// status page.
	EditProfiles bool `toml:"edit_profiles"`
	// EditRoutes lets an admin client create, change and delete routes the same way.
	EditRoutes bool `toml:"edit_routes"`
	// StateFile is a JSON file where edits are saved as overrides on top of the config,
	// and reloaded at startup. Empty: edits live in memory and are lost on restart. The
	// directory must exist and be writable. With several replicas each keeps its own
	// edits, so edit one replica or leave editing off.
	StateFile string `toml:"state_file"`

	// SelfServiceKeys lets any valid key mint more keys (SPEC 11.5). It needs StateFile.
	SelfServiceKeys     bool `toml:"self_service_keys"`
	MaxKeys             int  `toml:"max_keys"`               // runtime keys in total; default 1000
	MaxKeysPerPrincipal int  `toml:"max_keys_per_principal"` // keys one principal may have created; default 20

	// Login sessions for [[users]] (SPEC 11.6).
	LoginTTLS          int  `toml:"login_ttl_s"`           // default 8 hours
	MaxSessionsPerUser int  `toml:"max_sessions_per_user"` // default 10
	TrustProxy         bool `toml:"trust_proxy"`           // use the right-most X-Forwarded-For
}

// StoreConfig is the [store] table (SPEC 13.1). Without it nothing is stored and the
// feedback endpoint is off.
type StoreConfig struct {
	Driver string `toml:"driver"` // "sqlite" (default) | "postgres"
	// DSNEnv names the environment variable holding the file path (sqlite, default
	// "ignatius.db") or the connection string (postgres). Default IGNATIUS_STORE_DSN.
	DSNEnv string `toml:"dsn_env"`
	// RetentionDays deletes request metadata, stored content and feedback older than this.
	// 0 (the default) keeps everything until an operator purges it.
	RetentionDays int `toml:"retention_days"`
	// QueueSize bounds the in-memory queue of requests waiting to be written (default 1000).
	// Writes are asynchronous, so the store is off the request path; a full queue drops the
	// request's record (counted in ignatius.store.dropped) and a crash loses what is queued.
	QueueSize int `toml:"queue_size"`
}

func (c Config) validateAudit() error {
	if c.Audit == nil {
		return nil
	}
	switch {
	case c.Audit.SampleRate < 0 || c.Audit.SampleRate > 1:
		return fmt.Errorf("audit: sample_rate must be between 0 and 1")
	case c.Audit.MaxInflight < 0:
		return fmt.Errorf("audit: max_inflight cannot be negative")
	case c.Audit.SampleRate > 0 && c.Store == nil:
		return fmt.Errorf("audit: sample_rate above 0 needs a [store] to write the results to")
	}
	return nil
}

func (c Config) validateStore() error {
	if c.Store == nil {
		for _, cc := range c.Clients {
			if cc.StoreContent || cc.RetentionDays != 0 {
				return fmt.Errorf("client %q sets store_content or retention_days, which need a [store] table", cc.Name)
			}
		}
		for _, u := range c.Users {
			if u.StoreContent || u.RetentionDays != 0 {
				return fmt.Errorf("user %q sets store_content or retention_days, which need a [store] table", u.Name)
			}
		}
		return nil
	}
	switch c.Store.Driver {
	case "sqlite", "postgres":
	default:
		return fmt.Errorf("store: driver must be \"sqlite\" or \"postgres\", got %q", c.Store.Driver)
	}
	if c.Store.RetentionDays < 0 || c.Store.QueueSize < 0 {
		return fmt.Errorf("store: retention_days and queue_size cannot be negative")
	}
	for _, cc := range c.Clients {
		if cc.RetentionDays < 0 {
			return fmt.Errorf("client %q: retention_days cannot be negative", cc.Name)
		}
	}
	for _, u := range c.Users {
		if u.RetentionDays < 0 {
			return fmt.Errorf("user %q: retention_days cannot be negative", u.Name)
		}
	}
	return nil
}

// strictEvents rejects a key under [events] that this build does not know. The rest of the file
// is decoded leniently, as it always was, but a misplaced event setting (a webhook's secret_env
// left at the top of the sink instead of under its options) would otherwise be dropped silently
// and the events sent unsigned.
func strictEvents(raw []byte) error {
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil // the real parse reported it
	}
	ev, ok := doc["events"]
	if !ok {
		return nil
	}
	b, err := toml.Marshal(map[string]any{"events": ev})
	if err != nil {
		return nil
	}
	var only struct {
		Events events.Config `toml:"events"`
	}
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&only); err != nil {
		var se *toml.StrictMissingError
		if errors.As(err, &se) {
			var keys []string
			for _, d := range se.Errors {
				keys = append(keys, strings.Join(d.Key(), "."))
			}
			return fmt.Errorf("unknown setting under [events]: %s (a plugin's settings go in [events.sinks.options])", strings.Join(keys, ", "))
		}
		return nil
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := toml.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if len(c.Models) == 0 {
		return Config{}, fmt.Errorf("%s defines no models", path)
	}
	if err := c.Telemetry.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validateStore(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validateAudit(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Eval.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := strictEvents(raw); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Events.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) applyDefaults() {
	c.Eval.applyDefaults()
	if c.Audit != nil && c.Audit.MaxInflight == 0 {
		c.Audit.MaxInflight = 4
	}
	if c.Store != nil {
		if c.Store.Driver == "" {
			c.Store.Driver = "sqlite"
		}
		if c.Store.DSNEnv == "" {
			c.Store.DSNEnv = "IGNATIUS_STORE_DSN"
		}
		if c.Store.QueueSize == 0 {
			c.Store.QueueSize = 1000
		}
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8081"
	}
	if c.APIKeyEnv == "" {
		c.APIKeyEnv = "IGNATIUS_API_KEY"
	}
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = 2 << 20
	}
	if c.RequestTimeoutMS == 0 {
		c.RequestTimeoutMS = 30000
	}
	if c.MaxInlineModels == 0 {
		c.MaxInlineModels = 5
	}
	if c.Admin.MaxKeys == 0 {
		c.Admin.MaxKeys = 1000
	}
	if c.Admin.MaxKeysPerPrincipal == 0 {
		c.Admin.MaxKeysPerPrincipal = 20
	}
	if c.Admin.LoginTTLS == 0 {
		c.Admin.LoginTTLS = 8 * 3600
	}
	if c.Admin.MaxSessionsPerUser == 0 {
		c.Admin.MaxSessionsPerUser = 10
	}
}

func (c Config) allowInline() bool { return c.AllowInlineRoutes == nil || *c.AllowInlineRoutes }

// plans converts the TOML routes into Plans (via JSON, so Plan has one parser).
func (c Config) plans() (map[string]ignatius.Plan, error) {
	out := map[string]ignatius.Plan{}
	for name, raw := range c.Routes {
		b, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", name, err)
		}
		var p ignatius.Plan
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("route %q: %w", name, err)
		}
		out[name] = p
	}
	return out, nil
}
