package kafka

// An opt-in integration test against Amazon MSK as emulated by Floci (https://floci.io), which
// runs a real Redpanda broker behind the MSK API. The test creates a cluster through the MSK
// REST API, waits for it to be ACTIVE, asks for its bootstrap brokers the way an application
// would, creates the topic, and sends events through the Kafka sink.
//
// Floci does not publish the broker's port to the host and the broker advertises its container
// name, so the test has to run on the same Docker network. test-msk-floci.sh does that:
//
//	./plugins/kafka/test-msk-floci.sh          # from go/
//
// or set IGNATIUS_TEST_FLOCI_ENDPOINT yourself if you already run somewhere the brokers resolve.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/events"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// mskCall makes an MSK REST call. Floci does not check signatures, but the service is chosen by
// the credential scope, so it must say "kafka".
func mskCall(t *testing.T, endpoint, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, endpoint+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/kafka/aws4_request, SignedHeaders=host, Signature=x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestMSKViaFloci(t *testing.T) {
	endpoint := os.Getenv("IGNATIUS_TEST_FLOCI_ENDPOINT")
	if endpoint == "" {
		t.Skip("set IGNATIUS_TEST_FLOCI_ENDPOINT (see test-msk-floci.sh)")
	}
	name := fmt.Sprintf("ignatius-%d", time.Now().UnixNano())
	code, out := mskCall(t, endpoint, "POST", "/v1/clusters", map[string]any{
		"clusterName": name, "kafkaVersion": "3.6.0", "numberOfBrokerNodes": 1,
		"brokerNodeGroupInfo": map[string]any{"instanceType": "kafka.t3.small", "clientSubnets": []string{"subnet-1"}},
	})
	var created struct {
		ClusterArn string `json:"clusterArn"`
	}
	if code != 200 || json.Unmarshal(out, &created) != nil || created.ClusterArn == "" {
		t.Fatalf("create cluster: %d %s", code, out)
	}
	arn := url.PathEscape(created.ClusterArn)
	t.Cleanup(func() { mskCall(t, endpoint, "DELETE", "/v1/clusters/"+arn, nil) })

	// The cluster is a container starting up: wait for ACTIVE.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		_, out := mskCall(t, endpoint, "GET", "/v1/clusters/"+arn, nil)
		var d struct {
			ClusterInfo struct {
				State string `json:"state"`
			} `json:"clusterInfo"`
		}
		_ = json.Unmarshal(out, &d)
		if d.ClusterInfo.State == "ACTIVE" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster still %q after 3 minutes: %s", d.ClusterInfo.State, out)
		}
		time.Sleep(2 * time.Second)
	}
	_, out = mskCall(t, endpoint, "GET", "/v1/clusters/"+arn+"/bootstrap-brokers", nil)
	var bb struct {
		Plain string `json:"bootstrapBrokerString"`
	}
	if json.Unmarshal(out, &bb) != nil || bb.Plain == "" {
		t.Fatalf("bootstrap brokers: %s", out)
	}
	brokers := strings.Split(bb.Plain, ",")
	t.Logf("bootstrap brokers: %s", bb.Plain)

	// The broker advertises its container name in metadata. Inside the Docker network it
	// resolves; if it does not here, map it to the bootstrap address (needs a writable /etc/hosts).
	admin, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	meta, err := kmsg.NewPtrMetadataRequest().RequestWith(ctx, admin)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	bootHost, _, _ := net.SplitHostPort(brokers[0])
	for _, b := range meta.Brokers {
		if _, err := net.LookupHost(b.Host); err != nil {
			f, ferr := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0)
			if ferr != nil {
				t.Skipf("the broker advertises %q, which does not resolve here and /etc/hosts is not writable; run test-msk-floci.sh", b.Host)
			}
			fmt.Fprintf(f, "%s %s\n", bootHost, b.Host)
			f.Close()
			// Go caches /etc/hosts for a few seconds: wait until the name resolves.
			for i := 0; i < 100; i++ {
				if _, err := net.LookupHost(b.Host); err == nil {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	const topic = "ignatius-events"
	create := kmsg.NewPtrCreateTopicsRequest()
	ct := kmsg.NewCreateTopicsRequestTopic()
	ct.Topic, ct.NumPartitions, ct.ReplicationFactor = topic, 1, 1
	create.Topics = []kmsg.CreateTopicsRequestTopic{ct}
	cr, err := create.RequestWith(ctx, admin)
	if err != nil || (len(cr.Topics) > 0 && cr.Topics[0].ErrorCode != 0 && cr.Topics[0].ErrorCode != 36 /* already exists */) {
		t.Fatalf("create topic: %v %+v", err, cr)
	}

	e, err := events.Build(sinkCfg(brokers, nil), env(nil), nil)
	if err != nil {
		t.Fatalf("build the sink against MSK: %v", err)
	}
	var sent []events.Event
	for i := range 5 {
		ev := events.New(events.TypeRequestCompleted, time.Now(), map[string]any{"request_id": fmt.Sprintf("msk-%d", i), "client": "a"})
		sent = append(sent, ev)
		e.Emit(ev)
	}
	closeAll(t, e)

	got := map[string]*kgo.Record{}
	for _, r := range consume(t, brokers, topic, len(sent)) {
		var ev events.Event
		if json.Unmarshal(r.Value, &ev) != nil {
			t.Fatalf("record %q", r.Value)
		}
		got[ev.ID] = r
	}
	for i, ev := range sent {
		r := got[ev.ID]
		if r == nil {
			t.Fatalf("event %d never arrived", i)
		}
		if string(r.Key) != fmt.Sprintf("msk-%d", i) || header(r, "ignatius-delivery") != ev.ID {
			t.Errorf("event %d: key %q headers %v", i, r.Key, r.Headers)
		}
	}

	// A topic that does not exist fails startup against a real broker too.
	if _, err := events.Build(sinkCfg(brokers, map[string]any{"topic": "no-such-topic"}), env(nil), nil); err == nil ||
		!strings.Contains(err.Error(), "topic does not exist") {
		t.Errorf("missing topic against MSK: %v", err)
	}
}
