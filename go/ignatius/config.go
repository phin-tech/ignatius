package ignatius

import (
	"fmt"
	"net/http"
	"time"
)

// ModelConfig is one entry of the [models] table.
type ModelConfig struct {
	Provider  string `toml:"provider"`
	BaseURL   string `toml:"base_url"`
	Model     string `toml:"model"`
	APIKeyEnv string `toml:"api_key_env"`
	// AccountIDEnv names the environment variable holding the Cloudflare account id, for
	// provider "workers-ai" (the id is part of the request URL, so it is kept out of the config).
	AccountIDEnv string `toml:"account_id_env"`
	// AuthHeader sends the key raw in this header (e.g. X-API-Key) instead of
	// "Authorization: Bearer <key>".
	AuthHeader string `toml:"auth_header"`
	TimeoutMS  int    `toml:"timeout_ms"`
	// Confidence chooses what thresholds are measured against: "provider" (the default) trusts
	// the provider's number, "derived" computes it from the probabilities (SPEC 1.2).
	Confidence string `toml:"confidence"`

	// Prices in USD per million tokens. Priced if either is set (SPEC 11.3).
	PriceInputPerMTok  *float64 `toml:"price_input_per_mtok"`
	PriceOutputPerMTok *float64 `toml:"price_output_per_mtok"`
	// Cache overrides the global [cache].enabled for this model (SPEC 11.2).
	Cache *bool `toml:"cache"`
	// PropagateTrace sends a W3C traceparent header to this model. Leave off for
	// third-party hosted APIs; turn on for models you run yourself (SPEC 10.1).
	PropagateTrace bool `toml:"propagate_trace"`
	// Breaker tunes the circuit breaker, which is on by default (SPEC 11.1).
	Breaker *BreakerConfig `toml:"breaker"`
	// Limits says what one call can take: questions and images (SPEC 3.1).
	Limits *LimitsConfig `toml:"limits"`
}

// Options carries the registry-wide settings and test seams.
type Options struct {
	Cache CacheConfig
	Now   func() time.Time // nil = time.Now (a test seam for the breaker and cache)
}

// BuildRegistry turns model configs into backends with default options (no cache).
func BuildRegistry(models map[string]ModelConfig, getenv func(string) string) (Registry, error) {
	return BuildRegistryWith(models, getenv, Options{})
}

// BuildRegistryWith builds the backends and wraps each in its decorators,
// outermost first: cache, circuit breaker, pricing, then the real backend.
func BuildRegistryWith(models map[string]ModelConfig, getenv func(string) string, opts Options) (Registry, error) {
	reg := Registry{}
	var cache *Cache // one LRU shared by every cached model; keys include the alias
	for alias, m := range models {
		if m.Provider != "systemone" && m.Provider != "workers-ai" {
			reg[alias] = unsupported{m.Provider}
			continue
		}
		if m.Provider == "systemone" && m.BaseURL == "" {
			return nil, fmt.Errorf("model %q: base_url is required", alias)
		}
		if m.Provider == "workers-ai" {
			if m.Model == "" || m.AccountIDEnv == "" || m.APIKeyEnv == "" {
				return nil, fmt.Errorf("model %q: workers-ai needs model (e.g. \"@cf/cloudflare/clef-flash\"), account_id_env and api_key_env", alias)
			}
			if getenv(m.AccountIDEnv) == "" {
				return nil, fmt.Errorf("model %q: environment variable %s is not set", alias, m.AccountIDEnv)
			}
		}
		t := m.TimeoutMS
		if t == 0 {
			t = 10000
		}
		if !validConfidencePolicy(m.Confidence) {
			return nil, confidencePolicyError(alias, m.Confidence)
		}
		var b Backend = &SystemOne{
			BaseURL: m.BaseURL, Model: m.Model, APIKey: getenv(m.APIKeyEnv), AuthHeader: m.AuthHeader,
			Client: http.DefaultClient, Timeout: time.Duration(t) * time.Millisecond,
			PropagateTrace: m.PropagateTrace,
		}
		if m.Provider == "workers-ai" {
			b = &WorkersAI{BaseURL: m.BaseURL, AccountID: getenv(m.AccountIDEnv), Model: m.Model, APIKey: getenv(m.APIKeyEnv),
				Client: http.DefaultClient, Timeout: time.Duration(t) * time.Millisecond}
		}
		if m.Confidence == ConfidenceDerived {
			b = withConfidence{Inner: b, Policy: m.Confidence}
		}
		priced := m.PriceInputPerMTok != nil || m.PriceOutputPerMTok != nil
		if priced {
			p := &Priced{Inner: b}
			if m.PriceInputPerMTok != nil {
				p.InPerMTok = *m.PriceInputPerMTok
			}
			if m.PriceOutputPerMTok != nil {
				p.OutPerMTok = *m.PriceOutputPerMTok
			}
			b = p
		}
		bc := BreakerConfig{}
		if m.Breaker != nil {
			bc = *m.Breaker
		}
		if bc.Enabled == nil || *bc.Enabled {
			b = NewBreaker(b, bc, opts.Now)
		}
		useCache := opts.Cache.Enabled
		if m.Cache != nil {
			useCache = *m.Cache
		}
		if useCache {
			if cache == nil {
				cache = NewCache(opts.Cache, opts.Now)
			}
			b = &Cached{Inner: b, C: cache, Alias: alias, UpstreamModel: m.Model, Priced: priced}
		}
		var limits LimitsConfig
		if m.Limits != nil {
			if m.Limits.MaxQuestions < 0 || m.Limits.MaxImages < 0 {
				return nil, fmt.Errorf("model %q: limits cannot be negative", alias)
			}
			limits = *m.Limits
		}
		reg[alias] = Limited{Inner: b, Limits: limits} // outermost: see Limited
	}
	return reg, nil
}
