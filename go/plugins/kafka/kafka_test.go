package kafka

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/events"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func closeAll(t *testing.T, e *events.Emitter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func cluster(t *testing.T, topic string, opts ...kfake.Opt) *kfake.Cluster {
	t.Helper()
	c, err := kfake.NewCluster(append([]kfake.Opt{kfake.NumBrokers(1), kfake.SeedTopics(1, topic)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func sinkCfg(brokers []string, opts map[string]any) events.Config {
	o := map[string]any{"topic": "ignatius-events"}
	list := make([]any, len(brokers))
	for i, b := range brokers {
		list[i] = b
	}
	o["brokers"] = list
	for k, v := range opts {
		o[k] = v
	}
	return events.Config{Sinks: []events.SinkConfig{{Type: "kafka", Options: o}}}
}

// consume reads n records from the topic, from the start, with any extra client options.
func consume(t *testing.T, brokers []string, topic string, n int, extra ...kgo.Opt) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart())}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var got []*kgo.Record
	for len(got) < n {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("read %d of %d records", len(got), n)
		}
		fetches.EachRecord(func(r *kgo.Record) { got = append(got, r) })
	}
	return got
}

func header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func TestProducesTheCloudEventKeyedByRequest(t *testing.T) {
	c := cluster(t, "ignatius-events")
	e, err := events.Build(sinkCfg(c.ListenAddrs(), nil), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	var sent []events.Event
	for i := range 5 {
		ev := events.New(events.TypeRequestCompleted, time.Now(), map[string]any{"request_id": fmt.Sprintf("req-%d", i), "client": "a"})
		sent = append(sent, ev)
		e.Emit(ev)
	}
	closeAll(t, e)

	recs := consume(t, c.ListenAddrs(), "ignatius-events", len(sent))
	byID := map[string]*kgo.Record{}
	for _, r := range recs {
		var ev events.Event
		if err := json.Unmarshal(r.Value, &ev); err != nil || ev.SpecVersion != "1.0" {
			t.Fatalf("record %q: %v", r.Value, err)
		}
		byID[ev.ID] = r
	}
	for i, ev := range sent {
		r := byID[ev.ID]
		if r == nil {
			t.Fatalf("event %d never arrived", i)
		}
		if string(r.Key) != fmt.Sprintf("req-%d", i) || header(r, "ignatius-event") != events.TypeRequestCompleted ||
			header(r, "ignatius-delivery") != ev.ID || header(r, "content-type") != "application/cloudevents+json" {
			t.Errorf("event %d: key %q headers %v", i, r.Key, r.Headers)
		}
	}
}

func TestKeyBy(t *testing.T) {
	for by, want := range map[string]string{"client": "a", "type": events.TypeRequestCompleted, "request_id": "req-1"} {
		c := cluster(t, "ignatius-events")
		e, err := events.Build(sinkCfg(c.ListenAddrs(), map[string]any{"key_by": by}), env(nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		e.Emit(events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"request_id": "req-1", "client": "a"}))
		closeAll(t, e)
		if recs := consume(t, c.ListenAddrs(), "ignatius-events", 1); string(recs[0].Key) != want {
			t.Errorf("key_by %s: key %q, want %q", by, recs[0].Key, want)
		}
	}
	c := cluster(t, "ignatius-events") // an event with no request id falls back to its own id
	e, _ := events.Build(sinkCfg(c.ListenAddrs(), nil), env(nil), nil)
	ev := events.New(events.TypeRequestCompleted, time.Now(), map[string]string{})
	e.Emit(ev)
	closeAll(t, e)
	if recs := consume(t, c.ListenAddrs(), "ignatius-events", 1); string(recs[0].Key) != ev.ID {
		t.Errorf("fallback key %q, want %s", recs[0].Key, ev.ID)
	}
}

func TestBrokersFromTheEnvironmentAndAcksLeader(t *testing.T) {
	c := cluster(t, "ignatius-events")
	cfg := events.Config{Sinks: []events.SinkConfig{{Type: "kafka", Options: map[string]any{
		"brokers_env": "BROKERS", "topic": "ignatius-events", "acks": "leader", "compression": "zstd", "client_id": "test"}}}}
	e, err := events.Build(cfg, env(map[string]string{"BROKERS": strings.Join(c.ListenAddrs(), ", ")}), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Emit(events.New(events.TypeFeedbackReceived, time.Now(), map[string]string{"request_id": "r"}))
	closeAll(t, e)
	consume(t, c.ListenAddrs(), "ignatius-events", 1)
	if _, err := events.Build(cfg, env(nil), nil); err == nil || !strings.Contains(err.Error(), "BROKERS is not set") {
		t.Errorf("unset brokers_env: %v", err)
	}
}

func TestAMissingTopicFailsStartupAndAnUnreachableClusterDoesNot(t *testing.T) {
	c := cluster(t, "ignatius-events")
	_, err := events.Build(sinkCfg(c.ListenAddrs(), map[string]any{"topic": "typo-topic"}), env(nil), nil)
	if err == nil || !strings.Contains(err.Error(), "topic does not exist") {
		t.Fatalf("missing topic: %v", err)
	}
	// Nothing listens here: startup succeeds (the broker may be restarting), and an event fails by kind.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	var logs bytes.Buffer
	cfg := sinkCfg([]string{dead}, nil)
	cfg.Sinks[0].TimeoutMS = 1000
	start := time.Now()
	e, err := events.Build(cfg, env(nil), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("an unreachable cluster must not fail startup: %v", err)
	}
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"state": "DATA-SENTINEL"}))
	closeAll(t, e)
	out := logs.String()
	if !strings.Contains(out, `"error_kind":"timeout"`) && !strings.Contains(out, `"error_kind":"transport"`) {
		t.Fatalf("expected a timeout or transport failure: %s", out)
	}
	for _, s := range []string{dead, "DATA-SENTINEL", "ignatius-events"} {
		if strings.Contains(out, s) {
			t.Errorf("log leaked %q: %s", s, out)
		}
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("took %v, far longer than timeout_ms", d)
	}
}

func TestSASL(t *testing.T) {
	c := cluster(t, "ignatius-events", kfake.EnableSASL(), kfake.Superuser("PLAIN", "ign", "s3cret"))
	good := map[string]string{"U": "ign", "P": "s3cret"}
	opts := map[string]any{"sasl": "plain", "username_env": "U", "password_env": "P"}
	e, err := events.Build(sinkCfg(c.ListenAddrs(), opts), env(good), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"request_id": "r"}))
	closeAll(t, e)
	consume(t, c.ListenAddrs(), "ignatius-events", 1, kgo.SASL(plainMech("ign", "s3cret")))

	// A wrong password never delivers, and the password is not in the log.
	var logs bytes.Buffer
	cfg := sinkCfg(c.ListenAddrs(), opts)
	cfg.Sinks[0].TimeoutMS = 1500
	e, err = events.Build(cfg, env(map[string]string{"U": "ign", "P": "WRONG-PASSWORD"}), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("a bad credential is found on first use, not at startup: %v", err)
	}
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"request_id": "r2"}))
	closeAll(t, e)
	if !strings.Contains(logs.String(), "delivery failed") || strings.Contains(logs.String(), "WRONG-PASSWORD") || strings.Contains(logs.String(), "s3cret") {
		t.Errorf("log: %s", logs.String())
	}
	if _, err := events.Build(sinkCfg(c.ListenAddrs(), opts), env(map[string]string{"U": "ign"}), nil); err == nil || !strings.Contains(err.Error(), "P is not set") {
		t.Errorf("unset password env: %v", err)
	}
}

func TestTLSWithAPrivateCA(t *testing.T) {
	caPath, serverCfg := selfSigned(t)
	c := cluster(t, "ignatius-events", kfake.TLS(serverCfg))
	opts := map[string]any{"tls": true, "tls_ca_file": caPath}
	e, err := events.Build(sinkCfg(c.ListenAddrs(), opts), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"request_id": "r"}))
	closeAll(t, e)
	pool := x509.NewCertPool()
	pem, _ := os.ReadFile(caPath)
	pool.AppendCertsFromPEM(pem)
	consume(t, c.ListenAddrs(), "ignatius-events", 1, kgo.DialTLSConfig(&tls.Config{RootCAs: pool}))

	// Without the CA the certificate is not trusted: startup succeeds (it cannot tell), delivery fails.
	cfg := sinkCfg(c.ListenAddrs(), map[string]any{"tls": true})
	cfg.Sinks[0].TimeoutMS = 1500
	var logs bytes.Buffer
	e, err = events.Build(cfg, env(nil), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"request_id": "r2"}))
	closeAll(t, e)
	if !strings.Contains(logs.String(), "delivery failed") {
		t.Errorf("an untrusted certificate must not deliver: %s", logs.String())
	}
	bad := filepath.Join(t.TempDir(), "nope.pem")
	os.WriteFile(bad, []byte("not a pem"), 0o600)
	if _, err := events.Build(sinkCfg(c.ListenAddrs(), map[string]any{"tls": true, "tls_ca_file": bad}), env(nil), nil); err == nil || strings.Contains(err.Error(), bad) {
		t.Errorf("a bad CA file is a startup error that does not echo the path: %v", err)
	}
}

func TestClassify(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"too large": {kerr.MessageTooLarge, "rejected"},
		"no topic":  {kerr.UnknownTopicOrPartition, "rejected"},
		"denied":    {kerr.TopicAuthorizationFailed, "rejected"},
		"sasl":      {kerr.SaslAuthenticationFailed, "rejected"},
		"timeout":   {kgo.ErrRecordTimeout, "timeout"},
		"deadline":  {context.DeadlineExceeded, "timeout"},
		"network":   {errors.New("dial tcp 10.0.0.1:9092: refused"), "transport"},
		"closed":    {kgo.ErrClientClosed, "transport"},
	} {
		if got := classify(c.err); got.Kind != c.want {
			t.Errorf("%s: %q, want %q", name, got.Kind, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	base := func(m map[string]any) events.SinkConfig {
		o := map[string]any{"topic": "t", "brokers": []any{"h:9092"}}
		for k, v := range m {
			if v == nil {
				delete(o, k)
			} else {
				o[k] = v
			}
		}
		return events.SinkConfig{Type: "kafka", Options: o}
	}
	lowTimeout := base(nil)
	lowTimeout.TimeoutMS = 500
	bad := map[string]events.SinkConfig{
		"no topic":                     base(map[string]any{"topic": nil}),
		"no brokers":                   base(map[string]any{"brokers": nil}),
		"both brokers forms":           base(map[string]any{"brokers_env": "B"}),
		"empty brokers":                base(map[string]any{"brokers": []any{}}),
		"blank broker":                 base(map[string]any{"brokers": []any{" "}}),
		"brokers not strings":          base(map[string]any{"brokers": []any{int64(1)}}),
		"unknown option":               base(map[string]any{"topc": "x"}),
		"bad key_by":                   base(map[string]any{"key_by": "shard"}),
		"bad acks":                     base(map[string]any{"acks": "none"}),
		"bad compression":              base(map[string]any{"compression": "brotli"}),
		"tls not a bool":               base(map[string]any{"tls": "yes"}),
		"ca without tls":               base(map[string]any{"tls_ca_file": "/x"}),
		"bad sasl":                     base(map[string]any{"sasl": "gssapi", "username_env": "U", "password_env": "P"}),
		"sasl without credentials":     base(map[string]any{"sasl": "plain"}),
		"credentials without sasl":     base(map[string]any{"username_env": "U", "password_env": "P"}),
		"timeout below client minimum": lowTimeout,
	}
	for name, sc := range bad {
		if err := (events.Config{Sinks: []events.SinkConfig{sc}}).Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	for name, sc := range map[string]events.SinkConfig{
		"minimal": base(nil),
		"env":     base(map[string]any{"brokers": nil, "brokers_env": "B"}),
		"full": base(map[string]any{"key_by": "client", "acks": "leader", "compression": "lz4", "client_id": "x", "tls": true,
			"tls_ca_file": "/x", "sasl": "scram-sha-512", "username_env": "U", "password_env": "P"}),
	} {
		if err := (events.Config{Sinks: []events.SinkConfig{sc}}).Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// selfSigned makes a CA-and-server certificate for 127.0.0.1, writes it as a PEM CA file, and
// returns a server TLS config that presents it.
func selfSigned(t *testing.T) (caPath string, server *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ignatius-test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caPath = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return caPath, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}
