package kinesis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"github.com/phin-tech/ignatius/go/events"
)

// fakeKinesis speaks enough of the Kinesis JSON protocol for PutRecord and
// DescribeStreamSummary.
type fakeKinesis struct {
	*httptest.Server
	mu        sync.Mutex
	puts      []put
	missing   bool // answer ResourceNotFoundException
	denied    bool // answer AccessDeniedException to describe
	rejectPut bool
}

type put struct {
	Stream, PartitionKey string
	Data                 []byte
}

func newFake(t *testing.T) *fakeKinesis {
	t.Helper()
	f := &fakeKinesis{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		fail := func(code string) {
			w.Header().Set("X-Amzn-Errortype", code)
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"__type": code, "message": "ERR-SENTINEL stream leaked-name"})
		}
		switch r.Header.Get("X-Amz-Target") {
		case "Kinesis_20131202.DescribeStreamSummary":
			switch {
			case f.missing:
				fail("ResourceNotFoundException")
			case f.denied:
				fail("AccessDeniedException")
			default:
				io.WriteString(w, `{"StreamDescriptionSummary":{"StreamName":"x","StreamStatus":"ACTIVE","RetentionPeriodHours":24,"StreamARN":"arn:aws:kinesis:us-east-1:1:stream/x","OpenShardCount":1,"StreamModeDetails":{"StreamMode":"ON_DEMAND"},"EncryptionType":"NONE","StreamCreationTimestamp":1}}`)
			}
		case "Kinesis_20131202.PutRecord":
			if f.rejectPut {
				fail("AccessDeniedException")
				return
			}
			var in struct {
				StreamName, PartitionKey string
				Data                     []byte // base64 on the wire
			}
			_ = json.Unmarshal(body, &in)
			f.puts = append(f.puts, put{in.StreamName, in.PartitionKey, in.Data})
			io.WriteString(w, `{"SequenceNumber":"1","ShardId":"shardId-000000000000"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func awsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func cfgFor(f *fakeKinesis, opts map[string]any) events.Config {
	o := map[string]any{"stream": "ignatius-events", "endpoint_env": "EP"}
	for k, v := range opts {
		o[k] = v
	}
	return events.Config{Sinks: []events.SinkConfig{{Type: "kinesis", Options: o}}}
}

func closeAll(t *testing.T, e *events.Emitter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPutsTheCloudEventPartitionedByRequest(t *testing.T) {
	awsEnv(t)
	f := newFake(t)
	e, err := events.Build(cfgFor(f, nil), env(map[string]string{"EP": f.URL}), nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"request_id": "req-1", "client": "a"})
	e.Emit(ev)
	closeAll(t, e)
	if len(f.puts) != 1 {
		t.Fatalf("puts = %d", len(f.puts))
	}
	p := f.puts[0]
	var got events.Event
	if err := json.Unmarshal(p.Data, &got); err != nil || got.ID != ev.ID || got.Type != events.TypeRequestCompleted || got.SpecVersion != "1.0" {
		t.Fatalf("record data = %s (%v)", p.Data, err)
	}
	if p.Stream != "ignatius-events" || p.PartitionKey != "req-1" {
		t.Errorf("stream %q partition %q", p.Stream, p.PartitionKey)
	}
}

func TestPartitionBy(t *testing.T) {
	awsEnv(t)
	for by, want := range map[string]string{"client": "a", "type": events.TypeRequestCompleted, "request_id": "req-1"} {
		f := newFake(t)
		e, err := events.Build(cfgFor(f, map[string]any{"partition_by": by}), env(map[string]string{"EP": f.URL}), nil)
		if err != nil {
			t.Fatal(err)
		}
		e.Emit(events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"request_id": "req-1", "client": "a"}))
		closeAll(t, e)
		if len(f.puts) != 1 || f.puts[0].PartitionKey != want {
			t.Errorf("partition_by %s: %+v, want key %q", by, f.puts, want)
		}
	}
	// "id", and the fallback for an event with no request id, use the event id.
	f := newFake(t)
	e, _ := events.Build(cfgFor(f, nil), env(map[string]string{"EP": f.URL}), nil)
	ev := events.New(events.TypeRequestCompleted, time.Now(), map[string]string{})
	e.Emit(ev)
	closeAll(t, e)
	if len(f.puts) != 1 || f.puts[0].PartitionKey != ev.ID {
		t.Errorf("fallback key: %+v, want %s", f.puts, ev.ID)
	}
}

func TestAMissingStreamFailsStartupButNoPermissionDoesNot(t *testing.T) {
	awsEnv(t)
	f := newFake(t)
	f.missing = true
	_, err := events.Build(cfgFor(f, nil), env(map[string]string{"EP": f.URL}), nil)
	if err == nil || !strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("missing stream: %v", err)
	}
	f.missing, f.denied = false, true
	e, err := events.Build(cfgFor(f, nil), env(map[string]string{"EP": f.URL}), nil)
	if err != nil {
		t.Fatalf("describe denied must not fail startup: %v", err)
	}
	closeAll(t, e)
}

func TestFailuresLogOnlyTheKind(t *testing.T) {
	awsEnv(t)
	f := newFake(t)
	f.rejectPut = true
	var logs bytes.Buffer
	e, err := events.Build(cfgFor(f, nil), env(map[string]string{"EP": f.URL}), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"state": "DATA-SENTINEL"}))
	closeAll(t, e)
	out := logs.String()
	if !strings.Contains(out, `"error_kind":"rejected"`) {
		t.Fatalf("expected a rejected failure: %s", out)
	}
	for _, s := range []string{"ERR-SENTINEL", "leaked-name", "DATA-SENTINEL", f.URL} {
		if strings.Contains(out, s) {
			t.Errorf("log leaked %q: %s", s, out)
		}
	}
}

func TestClassify(t *testing.T) {
	api := func(code string, fault smithy.ErrorFault) error {
		return &smithy.GenericAPIError{Code: code, Message: "m", Fault: fault}
	}
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"throughput": {api("ProvisionedThroughputExceededException", smithy.FaultClient), "throttled"},
		"throttling": {api("ThrottlingException", smithy.FaultClient), "throttled"},
		"denied":     {api("AccessDeniedException", smithy.FaultClient), "rejected"},
		"server":     {api("InternalFailure", smithy.FaultServer), "transport"},
		"timeout":    {context.DeadlineExceeded, "timeout"},
		"network":    {errors.New("dial tcp: refused"), "transport"},
	} {
		if got := classify(c.err); got.Kind != c.want {
			t.Errorf("%s: %q, want %q", name, got.Kind, c.want)
		}
	}
}

func TestAnOversizedRecordIsRejectedBeforeTheNetwork(t *testing.T) {
	awsEnv(t)
	f := newFake(t)
	s, err := newSink(cfgFor(f, nil).Sinks[0], env(map[string]string{"EP": f.URL}))
	if err != nil {
		t.Fatal(err)
	}
	big := events.New(events.TypeRequestCompleted, time.Now(), strings.Repeat("x", maxRecordBytes+1))
	if err := s.Send(context.Background(), big); err == nil || err.(*events.SendError).Kind != "rejected" {
		t.Fatalf("oversized: %v", err)
	}
	if len(f.puts) != 0 {
		t.Error("an oversized record must not be sent")
	}
}

func TestValidate(t *testing.T) {
	bad := map[string]events.SinkConfig{
		"no stream":      {Type: "kinesis"},
		"unknown option": {Type: "kinesis", Options: map[string]any{"stream": "s", "strem": "x"}},
		"bad partition":  {Type: "kinesis", Options: map[string]any{"stream": "s", "partition_by": "shard"}},
	}
	for name, sc := range bad {
		if err := (events.Config{Sinks: []events.SinkConfig{sc}}).Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	ok := events.SinkConfig{Type: "kinesis", Options: map[string]any{"stream": "s", "region": "eu-west-1", "partition_by": "client"}}
	if err := (events.Config{Sinks: []events.SinkConfig{ok}}).Validate(); err != nil {
		t.Errorf("valid config: %v", err)
	}
	// The endpoint env var must be set, and the error does not echo anything else.
	awsEnv(t)
	_, err := events.Build(events.Config{Sinks: []events.SinkConfig{{Type: "kinesis", Options: map[string]any{"stream": "s", "endpoint_env": "EP"}}}}, env(nil), nil)
	if err == nil || !strings.Contains(err.Error(), "EP is not set") {
		t.Errorf("unset endpoint env: %v", err)
	}
}
