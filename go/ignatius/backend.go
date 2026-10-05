package ignatius

import (
	"context"
	"errors"
	"sort"
	"time"
)

// WireResponse is a provider's raw answer, before normalization.
type WireResponse struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	CostUSD *float64          `json:"-"` // set by the Priced decorator
	Cached  int               `json:"-"` // set by the Cached decorator
}

// CallError is a classified provider failure. Status is the HTTP status for
// kind "http" (0 otherwise); the circuit breaker uses it to tell a sick server
// (5xx, 429) from a caller mistake (other 4xx).
type CallError struct {
	Kind, Message string
	Status        int
}

func (e *CallError) Error() string { return e.Kind + ": " + e.Message }

// Backend performs one raw call. Implementations should honor ctx.
type Backend interface {
	Call(ctx context.Context, req Request) (WireResponse, error)
}

// Registry maps an alias to its Backend.
type Registry map[string]Backend

func (r Registry) names() []string {
	n := make([]string, 0, len(r))
	for k := range r {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// call runs one backend call, normalizing the result or classifying the failure.
// It is traced and measured; see telemetry.go.
func (r Registry) call(ctx context.Context, alias string, req Request) (Result, *Failure) {
	return r.instrumentedCall(ctx, alias, req, func(ctx context.Context) (Result, *Failure) {
		return r.callUninstrumented(ctx, alias, req)
	})
}

func (r Registry) callUninstrumented(ctx context.Context, alias string, req Request) (Result, *Failure) {
	start := time.Now()
	ms := func() int64 { return time.Since(start).Milliseconds() }
	b, ok := r[alias]
	if !ok {
		return Result{}, &Failure{Model: alias, Error: ErrorBody{KindUnsupported, "unknown model " + alias}}
	}
	wire, err := b.Call(ctx, req)
	if err != nil {
		var ce *CallError
		switch {
		case errors.As(err, &ce):
			return Result{}, &Failure{Model: alias, Error: ErrorBody{ce.Kind, ce.Message}, LatencyMS: ms()}
		case errors.Is(err, context.DeadlineExceeded):
			return Result{}, &Failure{Model: alias, Error: ErrorBody{KindTimeout, err.Error()}, LatencyMS: ms()}
		default:
			return Result{}, &Failure{Model: alias, Error: ErrorBody{KindTransport, err.Error()}, LatencyMS: ms()}
		}
	}
	return Result{Model: alias, Answers: normalize(alias, wire.Answers), Usage: wire.Usage, LatencyMS: ms(),
		CostUSD: wire.CostUSD, Cached: wire.Cached}, nil
}

type Prober interface {
	// Probe returns "ready", "not_ready" or "unknown" (no readiness endpoint).
	Probe(ctx context.Context) string
}

// Status probes a registered alias; backends without a Prober are "unknown".
func (r Registry) Status(ctx context.Context, alias string) string {
	if p, ok := r[alias].(Prober); ok {
		return p.Probe(ctx)
	}
	if _, ok := r[alias].(unsupported); ok {
		return "unsupported"
	}
	return "unknown"
}
