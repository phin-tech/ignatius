package kinesis

// Integration tests against a local AWS emulator such as Floci (https://floci.io), which
// speaks the real Kinesis API. They are opt-in because they need a running emulator:
//
//	docker run -d --rm -p 127.0.0.1:4566:4566 floci/floci:latest
//	IGNATIUS_TEST_KINESIS_ENDPOINT=http://127.0.0.1:4566 go test ./plugins/kinesis -run Emulator -v
//
// Unlike the fake in kinesis_test.go, the emulator keeps the records, so these read them
// back through the AWS SDK: what the plugin wrote is what a consumer would see.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	ktypes "github.com/aws/aws-sdk-go-v2/service/kinesis/types"
	"github.com/phin-tech/ignatius/go/events"
)

func emulator(t *testing.T) (*kinesis.Client, string) {
	t.Helper()
	endpoint := os.Getenv("IGNATIUS_TEST_KINESIS_ENDPOINT")
	if endpoint == "" {
		t.Skip("set IGNATIUS_TEST_KINESIS_ENDPOINT to a Kinesis emulator (see the comment in emulator_test.go)")
	}
	awsEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return kinesis.NewFromConfig(cfg, func(o *kinesis.Options) { o.BaseEndpoint = aws.String(endpoint) }), endpoint
}

func createStream(t *testing.T, c *kinesis.Client, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.CreateStream(ctx, &kinesis.CreateStreamInput{StreamName: &name, ShardCount: aws.Int32(1)}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	t.Cleanup(func() { c.DeleteStream(ctx, &kinesis.DeleteStreamInput{StreamName: &name}) })
	for range 50 {
		out, err := c.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamName: &name})
		if err == nil && out.StreamDescriptionSummary.StreamStatus == ktypes.StreamStatusActive {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("stream never became ACTIVE")
}

// readAll returns every record in the stream's only shard, waiting for want of them.
func readAll(t *testing.T, c *kinesis.Client, stream string, want int) []ktypes.Record {
	t.Helper()
	ctx := context.Background()
	shards, err := c.ListShards(ctx, &kinesis.ListShardsInput{StreamName: &stream})
	if err != nil || len(shards.Shards) == 0 {
		t.Fatalf("list shards: %v", err)
	}
	it, err := c.GetShardIterator(ctx, &kinesis.GetShardIteratorInput{StreamName: &stream,
		ShardId: shards.Shards[0].ShardId, ShardIteratorType: ktypes.ShardIteratorTypeTrimHorizon})
	if err != nil {
		t.Fatal(err)
	}
	var got []ktypes.Record
	iter := it.ShardIterator
	for range 50 {
		out, err := c.GetRecords(ctx, &kinesis.GetRecordsInput{ShardIterator: iter})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out.Records...)
		if len(got) >= want {
			return got
		}
		iter = out.NextShardIterator
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("read %d of %d records", len(got), want)
	return nil
}

func TestEmulatorRoundTrip(t *testing.T) {
	c, endpoint := emulator(t)
	stream := fmt.Sprintf("ignatius-test-%d", time.Now().UnixNano())
	createStream(t, c, stream)

	e, err := events.Build(events.Config{Sinks: []events.SinkConfig{{Type: "kinesis",
		Options: map[string]any{"stream": stream, "endpoint_env": "EP"}}}}, env(map[string]string{"EP": endpoint}), nil)
	if err != nil {
		t.Fatal(err)
	}
	var sent []events.Event
	for i := range 5 {
		ev := events.New(events.TypeRequestCompleted, time.Now(), map[string]any{"request_id": fmt.Sprintf("req-%d", i), "n": i})
		sent = append(sent, ev)
		e.Emit(ev)
	}
	closeAll(t, e)

	recs := readAll(t, c, stream, len(sent))
	ids, keys := map[string]bool{}, map[string]bool{}
	for _, r := range recs {
		var ev events.Event
		if err := json.Unmarshal(r.Data, &ev); err != nil || ev.SpecVersion != "1.0" || ev.Type != events.TypeRequestCompleted {
			t.Fatalf("record %q: %v", r.Data, err)
		}
		ids[ev.ID] = true
		keys[*r.PartitionKey] = true
	}
	var wantKeys, gotKeys []string
	for i, ev := range sent {
		if !ids[ev.ID] {
			t.Errorf("event %d (%s) never arrived", i, ev.ID)
		}
		wantKeys = append(wantKeys, fmt.Sprintf("req-%d", i))
	}
	for k := range keys {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	if fmt.Sprint(gotKeys) != fmt.Sprint(wantKeys) {
		t.Errorf("partition keys = %v, want the request ids %v", gotKeys, wantKeys)
	}
}

func TestEmulatorMissingStreamFailsStartup(t *testing.T) {
	_, endpoint := emulator(t)
	_, err := events.Build(events.Config{Sinks: []events.SinkConfig{{Type: "kinesis",
		Options: map[string]any{"stream": "no-such-stream-" + fmt.Sprint(time.Now().UnixNano()), "endpoint_env": "EP"}}}},
		env(map[string]string{"EP": endpoint}), nil)
	if err == nil {
		t.Fatal("a stream that does not exist must fail startup")
	}
	t.Log(err)
}

// The whole path: the custom binary serves a request, and a consumer reading the stream sees
// the event for exactly that request id (the one in the response header).
func TestEmulatorCustomBinaryEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	c, endpoint := emulator(t)
	stream := fmt.Sprintf("ignatius-e2e-%d", time.Now().UnixNano())
	createStream(t, c, stream)

	dir := t.TempDir()
	bin := filepath.Join(dir, "ignatius-kinesis")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/ignatius").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			return
		}
		fmt.Fprint(w, `{"model":"m","answers":{"q":{"type":"choice","choice":"a","probabilities":{"a":0.9,"b":0.1},"confidence":0.8}},"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	cfg := filepath.Join(dir, "gw.toml")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`
listen = %q
[models.m]
provider = "systemone"
base_url = %q
model = "jev-latest"
[[events.sinks]]
type = "kinesis"
[events.sinks.options]
stream = %q
endpoint_env = "KINESIS_ENDPOINT"
`, addr, upstream.URL, stream)), 0o600)
	cmd := exec.Command(bin, "serve", "--config", cfg)
	cmd.Env = append(os.Environ(), "KINESIS_ENDPOINT="+endpoint)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	base := "http://" + addr
	up := false
	for i := 0; i < 50 && !up; i++ {
		if r, err := http.Get(base + "/healthz"); err == nil {
			r.Body.Close()
			up = true
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !up {
		t.Fatalf("did not start: %s", stderr.String())
	}
	resp, err := http.Post(base+"/v1/systemone", "application/json", strings.NewReader(
		`{"state":"x","model":"m","questions":{"q":{"type":"choice","instructions":"?","criteria":{"a":"a","b":"b"}}}}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("request: %v %v", resp, err)
	}
	resp.Body.Close()
	reqID := resp.Header.Get("X-Request-Id")
	cmd.Process.Signal(os.Interrupt)
	cmd.Wait()

	recs := readAll(t, c, stream, 1)
	var ev struct {
		Type string `json:"type"`
		Data struct {
			RequestID string `json:"request_id"`
			Mode      string `json:"mode"`
			OK        bool   `json:"ok"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recs[0].Data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != events.TypeRequestCompleted || ev.Data.RequestID != reqID || reqID == "" || ev.Data.Mode != "single" || !ev.Data.OK {
		t.Fatalf("event = %+v, want request %q\n%s", ev, reqID, stderr.String())
	}
	if *recs[0].PartitionKey != reqID {
		t.Errorf("partition key %q, want the request id", *recs[0].PartitionKey)
	}
}
