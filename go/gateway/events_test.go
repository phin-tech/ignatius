package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/events"
	"github.com/phin-tech/ignatius/go/store"
)

type emitted struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Data struct {
		RequestID  string          `json:"request_id"`
		Client     string          `json:"client"`
		Owner      string          `json:"owner"`
		Route      string          `json:"route"`
		Mode       string          `json:"mode"`
		Images     int             `json:"images"`
		OK         bool            `json:"ok"`
		Verdict    string          `json:"verdict"`
		HasCorrect bool            `json:"has_correct"`
		Correct    json.RawMessage `json:"correct"`
		Model      string          `json:"model"`
		Questions  []struct {
			QuestionID string   `json:"question_id"`
			Model      string   `json:"model"`
			Confidence *float64 `json:"confidence"`
			Threshold  *float64 `json:"threshold"`
		} `json:"questions"`
		Content *struct {
			State any `json:"state"`
		} `json:"content"`
	} `json:"data"`
}

// eventServer builds a store-enabled server with a file sink and returns a function that
// closes it (flushing both queues) and reads back every event.
func eventServer(t *testing.T, models map[string]*fake, mut func(*Config)) (*Server, func() (raw string, evs []emitted)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	s, _, _ := storeServer(t, models, func(c *Config) {
		c.Events = events.Config{Sinks: []events.SinkConfig{{Type: "file", Path: path}}}
		if mut != nil {
			mut(c)
		}
	})
	return s, func() (string, []emitted) {
		s.Flush(context.Background())
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		var out []emitted
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l == "" {
				continue
			}
			var e emitted
			if err := json.Unmarshal([]byte(l), &e); err != nil {
				t.Fatalf("bad event line %q: %v", l, err)
			}
			out = append(out, e)
		}
		return string(b), out
	}
}

func TestRequestEventsCarryMetadataAndContentOnlyForOptedInClients(t *testing.T) {
	cheap, smart := newFake(t, choiceUnsure), newFake(t, choiceSure)
	s, read := eventServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, nil)
	ida := reqID(t, s, choiceReq("cascade:cheap@0.5>smart", sentinel+"-A"), "ka") // a opted in
	idb := reqID(t, s, choiceReq("cascade:cheap@0.5>smart", sentinel+"-B"), "kb") // b did not
	raw, evs := read()

	by := map[string]emitted{}
	for _, e := range evs {
		if e.Type != events.TypeRequestCompleted {
			t.Fatalf("unexpected event %s", e.Type)
		}
		by[e.Data.RequestID] = e
	}
	a, b := by[ida], by[idb]
	if a.Data.Client != "a" || b.Data.Client != "b" || a.Data.Mode != "cascade" || !a.Data.OK || a.Data.Route != "inline" {
		t.Fatalf("a=%+v b=%+v", a.Data, b.Data)
	}
	if len(a.Data.Questions) != 1 || a.Data.Questions[0].Model != "smart" || a.Data.Questions[0].Confidence == nil {
		t.Fatalf("question outcome: %+v", a.Data.Questions)
	}
	if a.Data.Content == nil || !strings.Contains(raw, sentinel+"-A") {
		t.Error("the opted-in client's event should carry its content")
	}
	if b.Data.Content != nil || strings.Contains(raw, sentinel+"-B") {
		t.Errorf("content in a non-opted-in client's event: %s", raw)
	}
	if strings.Contains(raw, sentinel+"-instr") && b.Data.Content != nil {
		t.Error("instructions leaked")
	}
}

func TestFeedbackEventAndItsCorrectedAnswer(t *testing.T) {
	s, read := eventServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, func(c *Config) {
		c.Events.Sinks[0].Only = []string{"feedback.received"}
	})
	ida := reqID(t, s, choiceReq("m", "x"), "ka") // owner a opted in
	idb := reqID(t, s, choiceReq("m", "x"), "kb") // owner b did not
	fb(s, "kb", `{"request_id":"`+ida+`","question_id":"q","verdict":"bad","correct":"b"}`)
	fb(s, "ka", `{"request_id":"`+idb+`","question_id":"q","verdict":"bad","correct":"b"}`)
	_, evs := read()
	if len(evs) != 2 {
		t.Fatalf("only filter: want 2 feedback events and no request events, got %d", len(evs))
	}
	byOwner := map[string]emitted{}
	for _, e := range evs {
		byOwner[e.Data.Owner] = e
	}
	if e := byOwner["a"]; e.Data.Client != "b" || e.Data.Verdict != "bad" || !e.Data.HasCorrect || string(e.Data.Correct) != `"b"` || e.Data.Model != "m" {
		t.Errorf("owner a opted in, so the corrected answer is included: %+v", e.Data)
	}
	if e := byOwner["b"]; !e.Data.HasCorrect || len(e.Data.Correct) != 0 {
		t.Errorf("owner b did not opt in, so only has_correct is sent: %+v", e.Data)
	}
}

func TestNoEventsWithoutSinks(t *testing.T) {
	s, _, _ := storeServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, nil)
	if s.events != nil {
		t.Fatal("no [[events.sinks]] means no emitter")
	}
	reqID(t, s, choiceReq("m", "x"), "ka")
}

// Events work without a store, and a webhook can feed a bridge.
func TestEventsWithoutAStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	s := setup(t, map[string]*fake{"m": newFake(t, choiceSure)}, func(c *Config) {
		c.Events = events.Config{Sinks: []events.SinkConfig{{Type: "file", Path: path}}}
	}, nil)
	do(s, "POST", "/v1/systemone", choiceReq("m", "x"), "")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), events.TypeRequestCompleted) {
		t.Fatalf("no event written: %q", b)
	}
}

func TestEventsConfigParsesFromTOML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "g.toml")
	// The webhook type is a plugin and is not registered in this package's tests: register a stand-in.
	events.RegisterSinkType("gateway-test-hook", events.SinkType{
		New: func(events.SinkConfig, func(string) string) (events.Sink, error) { return nil, nil }})
	os.WriteFile(cfgPath, []byte(`
[models.m]
provider = "systemone"
base_url = "http://127.0.0.1:1"

[events]
queue_size = 50

[[events.sinks]]
type = "gateway-test-hook"
name = "bridge"
only = ["request.completed"]
timeout_ms = 1500
[events.sinks.options]
url_env = "EVENTS_URL"
retries = 1
shards = 3

[[events.sinks]]
type = "file"
path = "-"
`), 0o600)
	c, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	b := c.Events.Sinks[0]
	if c.Events.QueueSize != 50 || len(c.Events.Sinks) != 2 || b.Name != "bridge" || b.TimeoutMS != 1500 ||
		b.OptString("url_env") != "EVENTS_URL" || b.OptInt("retries") != 1 || b.OptInt("shards") != 3 || c.Events.Sinks[1].Path != "-" {
		t.Fatalf("parsed: %+v", c.Events)
	}
	os.WriteFile(cfgPath, []byte("[models.m]\nprovider=\"systemone\"\nbase_url=\"http://x\"\n[[events.sinks]]\ntype=\"nope\"\n"), 0o600)
	if _, err := LoadConfig(cfgPath); err == nil || !strings.Contains(err.Error(), "not compiled into this binary") {
		t.Fatalf("unknown sink type: %v", err)
	}
}

// slowStore delays writes, to show feedback does not race the async writer.
type slowStore struct {
	store.Store
	delay time.Duration
}

func (s slowStore) RecordRequest(ctx context.Context, r store.Request) error {
	time.Sleep(s.delay)
	return s.Store.RecordRequest(ctx, r)
}

func TestFeedbackWaitsForAQueuedRequest(t *testing.T) {
	s, st, _ := storeServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, nil)
	s.writer.Close(context.Background())
	s.writer = newStoreWriter(slowStore{st, 300 * time.Millisecond}, 10, s.log)
	rec := do(s, "POST", "/v1/systemone", choiceReq("m", "x"), "Bearer ka")
	id := rec.Header().Get("X-Request-Id")
	// The row is not written yet: feedback must wait for it, not 404.
	if code, m := fb(s, "kb", `{"request_id":"`+id+`","question_id":"q","verdict":"good"}`); code != 200 {
		t.Fatalf("feedback right after the response: %d %v", code, m)
	}
}

func TestAFullWriteQueueDropsTheRecordNotTheRequest(t *testing.T) {
	s, st, logs := storeServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, nil)
	s.writer.Close(context.Background())
	s.writer = newStoreWriter(slowStore{st, 200 * time.Millisecond}, 1, s.log)
	for range 6 {
		if rec := do(s, "POST", "/v1/systemone", choiceReq("m", "x"), "Bearer ka"); rec.Code != 200 {
			t.Fatalf("a full store queue must not fail a request: %d", rec.Code)
		}
	}
	if s.writer.dropped.Load() < 3 {
		t.Errorf("dropped = %d, want most of the 6 (queue of 1, writer busy)", s.writer.dropped.Load())
	}
	if !bytes.Contains(logs.Bytes(), []byte("write queue full")) {
		t.Errorf("a drop should be logged: %s", logs)
	}
	s.Flush(context.Background())
}

// A setting in the wrong place must fail loudly: a webhook's secret_env left at the top of the
// sink, instead of under its options, would otherwise be dropped and the events sent unsigned.
func TestAMisplacedEventSettingIsAnError(t *testing.T) {
	events.RegisterSinkType("gateway-test-strict", events.SinkType{
		New: func(events.SinkConfig, func(string) string) (events.Sink, error) { return nil, nil }})
	cfgPath := filepath.Join(t.TempDir(), "g.toml")
	write := func(sink string) {
		os.WriteFile(cfgPath, []byte("[models.m]\nprovider=\"systemone\"\nbase_url=\"http://x\"\n[[events.sinks]]\ntype=\"gateway-test-strict\"\n"+sink), 0o600)
	}
	write("secret_env = \"EVENTS_SECRET\"\n[events.sinks.options]\nurl_env = \"U\"\n")
	if _, err := LoadConfig(cfgPath); err == nil || !strings.Contains(err.Error(), "secret_env") || !strings.Contains(err.Error(), "options") {
		t.Fatalf("a top-level secret_env should be rejected, naming it: %v", err)
	}
	write("[events.sinks.options]\nurl_env = \"U\"\nsecret_env = \"EVENTS_SECRET\"\n")
	if _, err := LoadConfig(cfgPath); err != nil {
		t.Fatalf("the same setting under options is fine: %v", err)
	}
	// Other tables stay lenient, as before.
	os.WriteFile(cfgPath, []byte("some_future_key = 1\n[models.m]\nprovider=\"systemone\"\nbase_url=\"http://x\"\n"), 0o600)
	if _, err := LoadConfig(cfgPath); err != nil {
		t.Fatalf("an unknown key outside [events] must not become an error: %v", err)
	}
}
