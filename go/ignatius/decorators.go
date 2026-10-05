package ignatius

import "context"

// probeOf forwards a readiness probe through a decorator.
func probeOf(ctx context.Context, b Backend) string {
	if p, ok := b.(Prober); ok {
		return p.Probe(ctx)
	}
	return "unknown"
}

// Priced prices a backend's calls from the usage it reports (SPEC 11.3).
type Priced struct {
	Inner                 Backend
	InPerMTok, OutPerMTok float64 // USD per million tokens
}

func (p *Priced) Call(ctx context.Context, req Request) (WireResponse, error) {
	w, err := p.Inner.Call(ctx, req)
	if err != nil {
		return w, err
	}
	c := float64(w.Usage.InputTokens)*p.InPerMTok/1e6 + float64(w.Usage.OutputTokens)*p.OutPerMTok/1e6
	w.CostUSD = &c
	return w, nil
}

func (p *Priced) Probe(ctx context.Context) string { return probeOf(ctx, p.Inner) }
