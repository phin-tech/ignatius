// Package events emits what the gateway does as CloudEvents-shaped JSON to sinks: an HTTP
// webhook, or a JSONL file or stdout (SPEC 14). Kafka, Kinesis and the like are reached
// through a bridge in front of the webhook, or a log shipper reading the file.
//
// Delivery is best effort. Each sink has its own bounded in-memory queue and sender, so a
// slow sink never slows a request or another sink; when a queue is full the event is
// dropped and counted. Events are lost if the process dies.
//
// Privacy (SPEC 14.3): this package carries whatever the caller puts in Data. The gateway
// puts metadata in every event and content only for clients that opted in. Errors and logs
// here name the sink and a failure kind, never a URL, header or body.
package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Event types.
const (
	TypeRequestCompleted = "ignatius.request.completed"
	TypeFeedbackReceived = "ignatius.feedback.received"
)

var knownTypes = map[string]bool{TypeRequestCompleted: true, TypeFeedbackReceived: true}

// Event is a CloudEvents 1.0 structured-mode envelope.
type Event struct {
	SpecVersion     string    `json:"specversion"`
	ID              string    `json:"id"`
	Source          string    `json:"source"`
	Type            string    `json:"type"`
	Time            time.Time `json:"time"`
	DataContentType string    `json:"datacontenttype"`
	Data            any       `json:"data"`
}

// New builds an event with a fresh id (the id is also the webhook's idempotency key).
func New(typ string, at time.Time, data any) Event {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return Event{SpecVersion: "1.0", ID: hex.EncodeToString(b), Source: "ignatius", Type: typ,
		Time: at.UTC(), DataContentType: "application/json", Data: data}
}

// Sink delivers one event, retrying as it sees fit, and returns an error of kind only.
type Sink interface {
	Send(ctx context.Context, e Event) error
	Close() error
}

// SinkConfig is one [[events.sinks]] entry.
type SinkConfig struct {
	Type string `toml:"type"` // "file", or a registered plugin type such as "webhook"
	Name string `toml:"name"` // metric and log label; default "<type>-<index>"
	// Only limits the sink to these event types ("request.completed" or the full
	// "ignatius.request.completed"). Empty means every type.
	Only []string `toml:"only"`

	// TimeoutMS bounds one delivery attempt; a sink reads it through Timeout (default 5000).
	TimeoutMS int `toml:"timeout_ms"`

	// file: a path, or "-" for stdout.
	Path string `toml:"path"`

	// Options are the settings of a sink compiled in as a plugin (SPEC 14.5), as the
	// [events.sinks.options] table. The file sink ignores it.
	Options map[string]any `toml:"options"`

	// QueueSize overrides [events] queue_size for this sink. Workers is the number of
	// concurrent senders (default 1, which keeps order); more raises throughput.
	QueueSize int `toml:"queue_size"`
	Workers   int `toml:"workers"`
}

// Config is the [events] table.
type Config struct {
	QueueSize int          `toml:"queue_size"` // per sink; default 1000
	Sinks     []SinkConfig `toml:"sinks"`
}

// Validate checks the config without touching the environment.
func (c Config) Validate() error {
	if c.QueueSize < 0 {
		return errors.New("events: queue_size cannot be negative")
	}
	names := map[string]bool{}
	for i, s := range c.Sinks {
		name := s.name(i)
		switch {
		case names[name]:
			return fmt.Errorf("events: sink name %q is used twice", name)
		case !registered(s.Type):
			return fmt.Errorf("events: sink %q: type %q is not compiled into this binary (it has: %s); a plugin type needs a build that imports it, see SPEC 14.5",
				name, s.Type, strings.Join(SinkTypes(), ", "))
		case s.TimeoutMS < 0 || s.QueueSize < 0 || s.Workers < 0:
			return fmt.Errorf("events: sink %q: timeout_ms, queue_size and workers cannot be negative", name)
		}
		if t := lookup(s.Type); t.Validate != nil {
			if err := t.Validate(s); err != nil {
				return fmt.Errorf("events: sink %q: %w", name, err)
			}
		}
		names[name] = true
		for _, o := range s.Only {
			if !knownTypes[fullType(o)] {
				return fmt.Errorf("events: sink %q: unknown event type %q (want request.completed or feedback.received)", name, o)
			}
		}
	}
	return nil
}

func (s SinkConfig) name(i int) string {
	if s.Name != "" {
		return s.Name
	}
	return fmt.Sprintf("%s-%d", s.Type, i)
}

func fullType(t string) string {
	if strings.HasPrefix(t, "ignatius.") {
		return t
	}
	return "ignatius." + t
}

var meter = otel.Meter("github.com/phin-tech/ignatius/go/events")

var (
	sentCounter, _    = meter.Int64Counter("ignatius.events.sent", metric.WithDescription("Events delivered to a sink."))
	failedCounter, _  = meter.Int64Counter("ignatius.events.failed", metric.WithDescription("Events a sink gave up on after its retries."))
	droppedCounter, _ = meter.Int64Counter("ignatius.events.dropped", metric.WithDescription("Events dropped because a sink's queue was full."))
)

type runner struct {
	name    string
	sink    Sink
	only    map[string]bool // nil = all
	q       chan Event
	dropped atomic.Int64
	wg      sync.WaitGroup
}

// Emitter fans events out to its sinks.
type Emitter struct {
	log     *slog.Logger
	mu      sync.RWMutex // guards closed against Emit racing Close
	closed  bool
	runners []*runner
}

// Build makes an Emitter from cfg. getenv resolves the *_env settings. A sink whose
// environment is incomplete is a startup error, so a typo never silently drops events.
func Build(cfg Config, getenv func(string) string, log *slog.Logger) (*Emitter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	e := &Emitter{log: log}
	for i, sc := range cfg.Sinks {
		name := sc.name(i)
		sink, err := lookup(sc.Type).New(sc, getenv)
		if err != nil {
			_ = e.Close(context.Background())
			return nil, fmt.Errorf("events: sink %q: %w", name, err)
		}
		size := firstPositive(sc.QueueSize, cfg.QueueSize, 1000)
		r := &runner{name: name, sink: sink, q: make(chan Event, size)}
		if len(sc.Only) > 0 {
			r.only = map[string]bool{}
			for _, o := range sc.Only {
				r.only[fullType(o)] = true
			}
		}
		for w := 0; w < firstPositive(sc.Workers, 1); w++ {
			r.wg.Add(1)
			go e.run(r)
		}
		e.runners = append(e.runners, r)
	}
	return e, nil
}

func firstPositive(vs ...int) int {
	for _, v := range vs {
		if v > 0 {
			return v
		}
	}
	return 0
}

// SetLogger replaces the logger. Call it before emitting.
func (e *Emitter) SetLogger(l *slog.Logger) {
	if e != nil && l != nil {
		e.log = l
	}
}

// Enabled reports whether any sink will receive an event of this type, so a caller can
// skip building an expensive payload.
func (e *Emitter) Enabled(typ string) bool {
	if e == nil {
		return false
	}
	for _, r := range e.runners {
		if r.only == nil || r.only[typ] {
			return true
		}
	}
	return false
}

// Emit queues the event for every sink that wants it. It never blocks and never fails.
func (e *Emitter) Emit(ev Event) {
	if e == nil {
		return
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return
	}
	for _, r := range e.runners {
		if r.only != nil && !r.only[ev.Type] {
			continue
		}
		select {
		case r.q <- ev:
		default:
			n := r.dropped.Add(1)
			droppedCounter.Add(context.Background(), 1, metric.WithAttributes(attribute.String("sink", r.name)))
			if n == 1 || n%1000 == 0 { // do not flood the log when a sink is down
				e.log.Warn("events: sink queue full, event dropped", "sink", r.name, "dropped_total", n)
			}
		}
	}
}

func (e *Emitter) run(r *runner) {
	defer r.wg.Done()
	for ev := range r.q {
		attrs := metric.WithAttributes(attribute.String("sink", r.name), attribute.String("type", ev.Type))
		if err := r.sink.Send(context.Background(), ev); err != nil {
			failedCounter.Add(context.Background(), 1, attrs)
			e.log.Warn("events: delivery failed", "sink", r.name, "type", ev.Type, "error_kind", kindOf(err))
			continue
		}
		sentCounter.Add(context.Background(), 1, attrs)
	}
}

// Close stops accepting events, delivers what is queued until ctx is done, then closes the
// sinks. Events still queued when ctx expires are lost.
func (e *Emitter) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	for _, r := range e.runners {
		close(r.q)
	}
	e.mu.Unlock()
	done := make(chan struct{})
	go func() {
		for _, r := range e.runners {
			r.wg.Wait()
		}
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = errors.New("events: shut down with events still queued")
	}
	for _, r := range e.runners {
		_ = r.sink.Close()
	}
	return err
}

// SendError says why a delivery failed without carrying anything from the request, the
// response or the URL.
type SendError struct {
	Kind   string // "timeout" | "transport" | "http" | "rejected"
	Status int    // for "http" and "rejected"
}

func (e *SendError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s %d", e.Kind, e.Status)
	}
	return e.Kind
}

func kindOf(err error) string {
	var se *SendError
	if errors.As(err, &se) {
		return se.Kind
	}
	return "error"
}
