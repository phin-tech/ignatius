package ignatius

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// BreakerConfig configures a model's circuit breaker (SPEC 11.1). The breaker
// is on unless Enabled is explicitly false.
type BreakerConfig struct {
	Enabled          *bool `toml:"enabled"`
	FailureThreshold int   `toml:"failure_threshold"`
	CooldownMS       int   `toml:"cooldown_ms"`
}

const (
	defaultBreakerThreshold = 5
	defaultBreakerCooldown  = 30 * time.Second
)

type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// Breaker stops calling a failing backend. After FailureThreshold consecutive
// countable failures it opens and rejects calls instantly (kind circuit_open);
// after the cooldown it lets exactly one probe call through.
type Breaker struct {
	inner     Backend
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu       sync.Mutex
	state    breakerState
	failures int
	openedAt time.Time
	probing  bool
}

// NewBreaker wraps inner. A nil now uses time.Now.
func NewBreaker(inner Backend, cfg BreakerConfig, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	th, cd := cfg.FailureThreshold, time.Duration(cfg.CooldownMS)*time.Millisecond
	if th <= 0 {
		th = defaultBreakerThreshold
	}
	if cd <= 0 {
		cd = defaultBreakerCooldown
	}
	return &Breaker{inner: inner, threshold: th, cooldown: cd, now: now}
}

// outcome of a call as the breaker sees it.
type outcome int

const (
	outSuccess outcome = iota // the server answered (including a 4xx: it is alive)
	outFailure                // a sick-server signal: counts toward opening
	outIgnore                 // says nothing about the server's health
)

func classify(err error) outcome {
	if err == nil {
		return outSuccess
	}
	var ce *CallError
	if errors.As(err, &ce) {
		switch ce.Kind {
		case KindTimeout, KindTransport, KindDecode:
			return outFailure
		case KindHTTP:
			if ce.Status >= 500 || ce.Status == 429 {
				return outFailure
			}
			return outSuccess
		}
		return outIgnore // unsupported, circuit_open
	}
	switch {
	case errors.Is(err, context.Canceled):
		return outIgnore // the caller gave up; not the model's fault
	case errors.Is(err, context.DeadlineExceeded):
		return outFailure
	}
	return outFailure // anything else is a transport problem
}

func (b *Breaker) admit() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		return nil
	case stateOpen:
		if wait := b.cooldown - b.now().Sub(b.openedAt); wait > 0 {
			return &CallError{Kind: KindCircuitOpen, Message: fmt.Sprintf("breaker open; next probe in %s", wait.Round(time.Second))}
		}
		b.state, b.probing = stateHalfOpen, true
		return nil
	default: // half-open: one probe at a time
		if b.probing {
			return &CallError{Kind: KindCircuitOpen, Message: "breaker half-open; probe in flight"}
		}
		b.probing = true
		return nil
	}
}

func (b *Breaker) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	probe := b.state == stateHalfOpen
	switch classify(err) {
	case outSuccess:
		b.failures, b.state, b.probing = 0, stateClosed, false
	case outFailure:
		b.failures++
		if probe || b.failures >= b.threshold {
			b.state, b.openedAt, b.probing = stateOpen, b.now(), false
		}
	case outIgnore:
		if probe {
			b.probing = false // let another call probe
		}
	}
}

func (b *Breaker) Call(ctx context.Context, req Request) (WireResponse, error) {
	if err := b.admit(); err != nil {
		return WireResponse{}, err
	}
	w, err := b.inner.Call(ctx, req)
	b.record(err)
	return w, err
}

// State is "closed", "open" or "half_open".
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return [...]string{"closed", "open", "half_open"}[b.state]
}

// BreakerState is the circuit breaker state ("closed", "open" or "half_open") of an alias, or
// "" when it has none. A cache in front of the breaker is looked through.
func (r Registry) BreakerState(alias string) string {
	b := r[alias]
	for {
		switch v := b.(type) {
		case Limited:
			b = v.Inner
		case *Cached:
			b = v.Inner
		case *Breaker:
			return v.State()
		default:
			return ""
		}
	}
}

func (b *Breaker) Probe(ctx context.Context) string {
	if b.State() != "closed" {
		return KindCircuitOpen
	}
	return probeOf(ctx, b.inner)
}
