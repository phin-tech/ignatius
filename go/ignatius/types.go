// Package ignatius orchestrates calls to System One decision models: single,
// fan_out (with optional reducers) and cascade, over normalized types. See
// SPEC.md at the repository root; the types and behavior here follow it.
package ignatius

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	ModeSingle  = "single"
	ModeFanOut  = "fan_out"
	ModeCascade = "cascade"
)

// Request is the normalized input: one state and typed questions. Images are optional base64
// PNG, JPEG or WebP images shared by every question (SPEC 3.1); only a model whose limits allow
// images is sent them, and a request is never sent to a model with its images silently dropped.
type Request struct {
	State     any                 `json:"state"`
	Images    []string            `json:"images,omitempty"`
	Questions map[string]Question `json:"questions"`
}

type Question struct {
	Type         string `json:"type"` // noul | choice | score
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Answer is a normalized answer. Confidence is already normalized (SPEC 1.1).
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"` // score levels, passed through for Jev-compatible clients
	Confidence    *float64           `json:"confidence"`
	// NativeConfidence is the provider's own number when the model's confidence policy replaced
	// it with a derived one (SPEC 1.2).
	NativeConfidence *float64 `json:"native_confidence,omitempty"`
	Model            string   `json:"model,omitempty"`
	Contributors     []string `json:"contributors,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Result struct {
	Model     string            `json:"model"`
	Answers   map[string]Answer `json:"answers"`
	Usage     Usage             `json:"usage"`
	LatencyMS int64             `json:"latency_ms"`
	CostUSD   *float64          `json:"cost_usd,omitempty"` // nil = no price configured
	Cached    int               `json:"cached,omitempty"`   // questions served from the cache
}

// Failure kinds.
const (
	KindTimeout     = "timeout"
	KindTransport   = "transport"
	KindHTTP        = "http"
	KindDecode      = "decode"
	KindUnsupported = "unsupported"
	KindCircuitOpen = "circuit_open"
)

type ErrorBody struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type Failure struct {
	Model     string    `json:"model"`
	Error     ErrorBody `json:"error"`
	LatencyMS int64     `json:"latency_ms"`
}

// HopQuestion explains what happened to one question at one cascade tier.
type HopQuestion struct {
	ID         string   `json:"id"`
	Confidence *float64 `json:"confidence"`          // nil if the tier gave no answer
	Threshold  *float64 `json:"threshold,omitempty"` // the bar applied; nil on the last tier
	Escalated  bool     `json:"escalated"`
}

// Hop records one cascade tier that was actually called.
type Hop struct {
	Tier      int           `json:"tier"`
	Model     string        `json:"model"`
	Questions []string      `json:"questions"`
	Escalated []string      `json:"escalated"`
	LatencyMS int64         `json:"latency_ms"`
	Error     *ErrorBody    `json:"error,omitempty"` // set when the tier failed
	Detail    []HopQuestion `json:"detail,omitempty"`
}

// Routed is the envelope every mode returns.
type Routed struct {
	Mode     string            `json:"mode"`
	OK       bool              `json:"ok"`
	Answers  map[string]Answer `json:"answers"`
	Results  []Result          `json:"results"`
	Failures []Failure         `json:"failures"`
	Trace    []Hop             `json:"trace,omitempty"`
	CostUSD  *float64          `json:"cost_usd,omitempty"` // sum over priced results
}

// Threshold is a number (all question types) or a per-type map. Missing types
// fall back to Default, then to the enclosing threshold.
type Threshold struct {
	Default *float64
	ByType  map[string]float64
}

func (t *Threshold) UnmarshalJSON(b []byte) error {
	var n float64
	if err := json.Unmarshal(b, &n); err == nil {
		t.Default = &n
		return nil
	}
	var m map[string]float64
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("threshold must be a number or a {noul,choice,score} map")
	}
	t.ByType = m
	return nil
}

func (t Threshold) MarshalJSON() ([]byte, error) {
	if t.ByType != nil {
		return json.Marshal(t.ByType)
	}
	return json.Marshal(t.Default)
}

func (t *Threshold) lookup(qtype string) (float64, bool) {
	if t == nil {
		return 0, false
	}
	if v, ok := t.ByType[qtype]; ok {
		return v, true
	}
	if t.Default != nil {
		return *t.Default, true
	}
	return 0, false
}

// Tier is a cascade stage. A bare string in JSON is shorthand for {model}.
type Tier struct {
	Model     string     `json:"model"`
	Threshold *Threshold `json:"threshold,omitempty"`
}

func (t *Tier) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*t = Tier{Model: s}
		return nil
	}
	type plain Tier
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*t = Tier(p)
	return nil
}

// Plan says how to route a Request (SPEC 6.0).
type Plan struct {
	Mode       string     `json:"mode"`
	Model      string     `json:"model,omitempty"`
	Models     []string   `json:"models,omitempty"`
	Tiers      []Tier     `json:"tiers,omitempty"`
	Threshold  *Threshold `json:"threshold,omitempty"`
	Reduce     string     `json:"reduce,omitempty"`
	MinSuccess int        `json:"min_success,omitempty"`
	TimeoutMS  int        `json:"timeout_ms,omitempty"`
}

// PlanError lists everything wrong with a Plan (the caller's mistake: HTTP 422).
type PlanError struct{ Problems []string }

func (e *PlanError) Error() string { return "invalid plan: " + strings.Join(e.Problems, "; ") }

// Validate checks the caller's Request (HTTP 422 territory).
func (r Request) Validate() error {
	if len(r.Questions) == 0 {
		return fmt.Errorf("at least one question is required")
	}
	for i, img := range r.Images {
		if img == "" {
			return fmt.Errorf("images[%d] is empty: images are base64 strings", i)
		}
	}
	for id, q := range r.Questions {
		switch q.Type {
		case "noul", "choice", "score":
		default:
			return fmt.Errorf("question %q: type must be noul, choice or score, got %q", id, q.Type)
		}
	}
	return nil
}
