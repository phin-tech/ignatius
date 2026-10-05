package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func closeAll(t *testing.T, e *Emitter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// block is a test sink that holds every Send until released, to show queues and isolation.
type block struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func newBlock() *block { return &block{release: make(chan struct{}), started: make(chan struct{})} }

func (b *block) Send(ctx context.Context, e Event) error {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return nil
}
func (b *block) Close() error { return nil }

// fail is a test sink that always fails the way a real one reports it: by kind.
type fail struct{ err error }

func (f fail) Send(context.Context, Event) error { return f.err }
func (f fail) Close() error                      { return nil }

func register(t *testing.T, name string, s Sink) {
	t.Helper()
	RegisterSinkType(name, SinkType{New: func(SinkConfig, func(string) string) (Sink, error) { return s, nil }})
}

func TestOnlyFiltersByType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	e, err := Build(Config{Sinks: []SinkConfig{{Type: "file", Path: path, Only: []string{"feedback.received"}}}}, env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Enabled(TypeRequestCompleted) || !e.Enabled(TypeFeedbackReceived) {
		t.Error("Enabled should follow the filter")
	}
	e.Emit(New(TypeRequestCompleted, time.Now(), 1))
	e.Emit(New(TypeFeedbackReceived, time.Now(), 2))
	closeAll(t, e)
	b, _ := os.ReadFile(path)
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) != 1 || !strings.Contains(lines[0], TypeFeedbackReceived) {
		t.Fatalf("file = %q", b)
	}
}

func TestFileSinkAppendsJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	for range 2 { // reopening appends
		e, _ := Build(Config{Sinks: []SinkConfig{{Type: "file", Path: path}}}, env(nil), nil)
		e.Emit(New(TypeRequestCompleted, time.Now(), map[string]int{"n": 1}))
		e.Emit(New(TypeFeedbackReceived, time.Now(), map[string]int{"n": 2}))
		closeAll(t, e)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %d: %s", len(lines), b)
	}
	for _, l := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(l), &ev); err != nil || ev.SpecVersion != "1.0" {
			t.Fatalf("bad line %q: %v", l, err)
		}
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", st.Mode().Perm())
	}
	if _, err := Build(Config{Sinks: []SinkConfig{{Type: "file", Path: filepath.Join(path, "no", "dir")}}}, env(nil), nil); err == nil || strings.Contains(err.Error(), "no/dir") {
		t.Errorf("an unwritable path is a startup error that does not echo it: %v", err)
	}
}

func TestAFullQueueDropsAndNeverBlocks(t *testing.T) {
	b := newBlock()
	register(t, "test-block-queue", b)
	var logs bytes.Buffer
	e, _ := Build(Config{QueueSize: 2, Sinks: []SinkConfig{{Type: "test-block-queue"}}}, env(nil), slog.New(slog.NewJSONHandler(&logs, nil)))
	done := make(chan struct{})
	go func() {
		for range 50 { // the sender is stuck on the first event; the queue holds 2
			e.Emit(New(TypeRequestCompleted, time.Now(), nil))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on a slow sink")
	}
	if n := e.runners[0].dropped.Load(); n < 40 {
		t.Errorf("dropped = %d, want most of the 50", n)
	}
	if !strings.Contains(logs.String(), "queue full") {
		t.Errorf("a drop should be logged once: %s", logs.String())
	}
	close(b.release)
	closeAll(t, e)
}

func TestSlowSinkDoesNotHoldUpAnother(t *testing.T) {
	b := newBlock()
	register(t, "test-block-isolation", b)
	path := filepath.Join(t.TempDir(), "e.jsonl")
	e, _ := Build(Config{Sinks: []SinkConfig{{Type: "test-block-isolation"}, {Type: "file", Path: path}}}, env(nil), nil)
	e.Emit(New(TypeRequestCompleted, time.Now(), nil))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if data, _ := os.ReadFile(path); len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the file sink waited on the stuck sink")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(b.release)
	closeAll(t, e)
}

func TestCloseDeliversWhatIsQueuedAndThenIgnoresEmit(t *testing.T) {
	sink := &collect{}
	register(t, "test-collect-close", sink)
	e, _ := Build(Config{Sinks: []SinkConfig{{Type: "test-collect-close"}}}, env(nil), nil)
	for range 20 {
		e.Emit(New(TypeRequestCompleted, time.Now(), nil))
	}
	closeAll(t, e)
	if len(sink.got) != 20 {
		t.Fatalf("close flushed %d of 20", len(sink.got))
	}
	e.Emit(New(TypeRequestCompleted, time.Now(), nil)) // after Close: no panic, no delivery
	var nilEmitter *Emitter
	nilEmitter.Emit(New(TypeRequestCompleted, time.Now(), nil))
	if nilEmitter.Enabled(TypeRequestCompleted) {
		t.Error("a nil emitter enables nothing")
	}
	if len(sink.got) != 20 {
		t.Error("an event emitted after Close was delivered")
	}
}

// A failure is logged by kind: a sink's error message is never printed, because it can carry a
// URL, a credential or a body.
func TestFailuresLogOnlyTheKind(t *testing.T) {
	var logs bytes.Buffer
	register(t, "test-fail", fail{err: errors.New("dial https://hook.example/?token=URL-SENTINEL: refused")})
	register(t, "test-fail-kind", fail{err: &SendError{Kind: "http", Status: 503}})
	e, _ := Build(Config{Sinks: []SinkConfig{{Type: "test-fail"}, {Type: "test-fail-kind"}}}, env(nil), slog.New(slog.NewJSONHandler(&logs, nil)))
	e.Emit(New(TypeRequestCompleted, time.Now(), map[string]string{"state": "DATA-SENTINEL"}))
	closeAll(t, e)
	out := logs.String()
	if !strings.Contains(out, `"error_kind":"error"`) || !strings.Contains(out, `"error_kind":"http"`) {
		t.Fatalf("expected both failures logged by kind: %s", out)
	}
	for _, s := range []string{"URL-SENTINEL", "DATA-SENTINEL", "hook.example"} {
		if strings.Contains(out, s) {
			t.Errorf("log leaked %q: %s", s, out)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	bad := map[string]Config{
		"unknown type":      {Sinks: []SinkConfig{{Type: "kafka"}}},
		"file needs a path": {Sinks: []SinkConfig{{Type: "file"}}},
		"duplicate name":    {Sinks: []SinkConfig{{Type: "file", Path: "-", Name: "x"}, {Type: "file", Path: "-", Name: "x"}}},
		"unknown event":     {Sinks: []SinkConfig{{Type: "file", Path: "-", Only: []string{"nope"}}}},
		"negative queue":    {QueueSize: -1},
		"negative timeout":  {Sinks: []SinkConfig{{Type: "file", Path: "-", TimeoutMS: -1}}},
		"negative workers":  {Sinks: []SinkConfig{{Type: "file", Path: "-", Workers: -1}}},
	}
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := (Config{Sinks: []SinkConfig{{Type: "file", Path: "-"}}}).Validate(); err != nil {
		t.Errorf("a valid config: %v", err)
	}
}

type collect struct {
	mu  sync.Mutex
	got []Event
}

func (c *collect) Send(_ context.Context, e Event) error {
	c.mu.Lock()
	c.got = append(c.got, e)
	c.mu.Unlock()
	return nil
}
func (c *collect) Close() error { return nil }

func TestAPluginSinkTypeRegistersItselfAndIsBuiltFromConfig(t *testing.T) {
	sink := &collect{}
	var seen SinkConfig
	RegisterSinkType("test-plugin", SinkType{
		Validate: func(s SinkConfig) error {
			if s.OptString("stream") == "" {
				return errors.New("options.stream is required")
			}
			return nil
		},
		New: func(s SinkConfig, getenv func(string) string) (Sink, error) { seen = s; return sink, nil },
	})
	// A plugin's own validation runs, with the sink's name in the message.
	err := Config{Sinks: []SinkConfig{{Type: "test-plugin", Name: "p"}}}.Validate()
	if err == nil || !strings.Contains(err.Error(), `"p"`) || !strings.Contains(err.Error(), "options.stream is required") {
		t.Fatalf("validate: %v", err)
	}
	cfg := Config{Sinks: []SinkConfig{{Type: "test-plugin", Options: map[string]any{"stream": "events", "shards": int64(4)}}}}
	e, err := Build(cfg, env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Emit(New(TypeRequestCompleted, time.Now(), nil))
	closeAll(t, e)
	if len(sink.got) != 1 || seen.OptString("stream") != "events" || seen.OptInt("shards") != 4 || seen.OptInt("missing") != 0 {
		t.Fatalf("got %d events, options %+v", len(sink.got), seen.Options)
	}
	if k := seen.OptKeys(); len(k) != 2 || k[0] != "shards" || k[1] != "stream" {
		t.Errorf("OptKeys = %v", k)
	}
}

func TestAnUnknownSinkTypeSaysWhatThisBinaryHas(t *testing.T) {
	err := Config{Sinks: []SinkConfig{{Type: "kinesis"}}}.Validate()
	if err == nil || !strings.Contains(err.Error(), "not compiled into this binary") ||
		!strings.Contains(err.Error(), "file") || !strings.Contains(err.Error(), "SPEC 14.5") {
		t.Fatalf("%v", err)
	}
}

func TestRegisteringTwiceOrBadlyPanics(t *testing.T) {
	for name, f := range map[string]func(){
		"duplicate": func() {
			RegisterSinkType("file", SinkType{New: func(SinkConfig, func(string) string) (Sink, error) { return nil, nil }})
		},
		"no name": func() {
			RegisterSinkType("", SinkType{New: func(SinkConfig, func(string) string) (Sink, error) { return nil, nil }})
		},
		"no New": func() { RegisterSinkType("test-no-new", SinkType{}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s should panic", name)
				}
			}()
			f()
		}()
	}
	if got := SinkTypes(); len(got) < 2 || got[0] > got[len(got)-1] {
		t.Errorf("SinkTypes = %v", got)
	}
}

func TestOptionHelpers(t *testing.T) {
	s := SinkConfig{Options: map[string]any{"on": true, "hosts": []any{"a", "b"}, "mixed": []any{"a", int64(1)}, "name": "x"}}
	if !s.OptBool("on") || s.OptBool("name") || s.OptBool("missing") {
		t.Error("OptBool")
	}
	if h, ok := s.OptStrings("hosts"); !ok || len(h) != 2 || h[1] != "b" {
		t.Errorf("OptStrings = %v %v", h, ok)
	}
	for _, k := range []string{"mixed", "name", "missing"} {
		if _, ok := s.OptStrings(k); ok {
			t.Errorf("OptStrings(%s) should be false", k)
		}
	}
}

func TestTimeoutDefaultsToFiveSecondsAndCanBeLowered(t *testing.T) {
	if got := (SinkConfig{}).Timeout(); got != 5*time.Second {
		t.Errorf("default = %v", got)
	}
	if got := (SinkConfig{TimeoutMS: 700}).Timeout(); got != 700*time.Millisecond {
		t.Errorf("a configured timeout must be honored, got %v", got)
	}
	if got := (SinkConfig{TimeoutMS: 60000}).Timeout(); got != time.Minute {
		t.Errorf("and raised, got %v", got)
	}
}
