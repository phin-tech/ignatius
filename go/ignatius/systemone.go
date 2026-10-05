package ignatius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/propagation"
)

// SystemOne is a Backend for any Jev-compatible server (Jev, Clef, Jeff via
// Decis, dunce-union, Ignatius itself): POST {BaseURL}/v1/systemone.
type SystemOne struct {
	BaseURL string
	Model   string
	APIKey  string
	// AuthHeader, if set, carries the raw key; otherwise Authorization: Bearer.
	AuthHeader string
	Client     *http.Client
	Timeout    time.Duration // per call; 0 = no extra bound beyond ctx
	// PropagateTrace sends a W3C traceparent header upstream. Off by default: it
	// would hand internal trace ids to a third-party hosted API (SPEC 10.1).
	PropagateTrace bool
}

func (s *SystemOne) Call(ctx context.Context, req Request) (WireResponse, error) {
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	payload := map[string]any{"state": req.State, "model": s.Model, "questions": req.Questions}
	if len(req.Images) > 0 {
		payload["images"] = req.Images
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return WireResponse{}, &CallError{Kind: KindDecode, Message: err.Error()}
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return WireResponse{}, &CallError{Kind: KindTransport, Message: err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	if s.PropagateTrace {
		propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(hreq.Header))
	}
	if s.APIKey != "" {
		if s.AuthHeader != "" {
			hreq.Header.Set(s.AuthHeader, s.APIKey)
		} else {
			hreq.Header.Set("Authorization", "Bearer "+s.APIKey)
		}
	}
	c := s.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(hreq)
	if err != nil {
		return WireResponse{}, err // Registry.call classifies timeouts vs transport
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return WireResponse{}, err
	}
	if resp.StatusCode/100 != 2 {
		snippet := string(raw)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return WireResponse{}, &CallError{Kind: KindHTTP, Message: fmt.Sprintf("%d: %s", resp.StatusCode, snippet), Status: resp.StatusCode}
	}
	var wire WireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return WireResponse{}, &CallError{Kind: KindDecode, Message: err.Error()}
	}
	return wire, nil
}

// unsupported stands in for providers not implemented yet (e.g. "openai").
type unsupported struct{ provider string }

func (u unsupported) Call(context.Context, Request) (WireResponse, error) {
	return WireResponse{}, &CallError{Kind: KindUnsupported, Message: "provider " + u.provider + " is not implemented"}
}

// Probe asks GET {BaseURL}/readyz (Decis and dunce-style servers have one;
// hosted APIs usually do not, which reports "unknown").
func (s *SystemOne) Probe(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.BaseURL, "/")+"/readyz", nil)
	if err != nil {
		return "not_ready"
	}
	c := s.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return "not_ready"
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return "ready"
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return "unknown"
	default:
		return "not_ready"
	}
}
