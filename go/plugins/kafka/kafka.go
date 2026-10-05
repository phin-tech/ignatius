// Package kafka is an optional event sink that produces Ignatius events to a Kafka topic
// (SPEC 14.5). It works with Apache Kafka, Redpanda, MSK and anything else that speaks the
// Kafka protocol. It is its own Go module so the core never depends on a Kafka client: a
// build that wants it imports it for its side effect,
//
//	import _ "github.com/phin-tech/ignatius/go/plugins/kafka"
//
// which registers the "kafka" sink type. Configure it in the gateway's TOML:
//
//	[[events.sinks]]
//	type = "kafka"
//	name = "bus"
//	only = ["request.completed"]            # optional, as for any sink
//	timeout_ms = 5000                       # how long one event may take to be acknowledged; at least 1000
//	[events.sinks.options]
//	brokers = ["kafka-1:9092", "kafka-2:9092"]   # or brokers_env = "KAFKA_BROKERS" (comma separated)
//	topic = "ignatius-events"                    # required; must already exist
//	key_by = "request_id"                        # request_id (default) | client | type | id
//	acks = "all"                                 # all (default) | leader
//	compression = "snappy"                       # none | gzip | snappy | lz4 | zstd (default: the client's, snappy)
//	client_id = "ignatius"                       # optional
//	tls = true                                   # system roots; tls_ca_file adds a PEM CA for a private one
//	tls_ca_file = "/etc/ssl/kafka-ca.pem"
//	sasl = "scram-sha-512"                       # plain | scram-sha-256 | scram-sha-512
//	username_env = "KAFKA_USER"                  # SASL credentials come from the environment, never the config
//	password_env = "KAFKA_PASSWORD"
//
// Each record's value is the same CloudEvents JSON the webhook sink sends; its key is the
// request id by default, so one request's events and the feedback about it land on one
// partition, in order. Headers carry the event type and id (ignatius-event, ignatius-delivery)
// for consumers that filter or dedupe without parsing the value. Delivery is best effort like
// every sink: the client retries within timeout_ms (acks=all with an idempotent producer, so a
// retry does not duplicate), then the event is counted as failed.
package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/phin-tech/ignatius/go/events"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

var knownOptions = map[string]bool{
	"brokers": true, "brokers_env": true, "topic": true, "key_by": true, "acks": true, "compression": true,
	"client_id": true, "tls": true, "tls_ca_file": true, "sasl": true, "username_env": true, "password_env": true,
}

func init() {
	events.RegisterSinkType("kafka", events.SinkType{Validate: validate, New: newSink})
}

func validate(sc events.SinkConfig) error {
	for _, k := range sc.OptKeys() {
		if !knownOptions[k] {
			return fmt.Errorf("unknown option %q", k)
		}
	}
	if sc.OptString("topic") == "" {
		return errors.New("options.topic is required for a kafka sink")
	}
	if sc.TimeoutMS != 0 && sc.TimeoutMS < 1000 {
		return errors.New("timeout_ms must be at least 1000 for a kafka sink (the client's minimum delivery timeout)")
	}
	_, hasList := sc.Options["brokers"]
	hasEnv := sc.OptString("brokers_env") != ""
	switch {
	case hasList == hasEnv:
		return errors.New("set exactly one of options.brokers (a list) and options.brokers_env")
	case hasList:
		if b, ok := sc.OptStrings("brokers"); !ok || len(b) == 0 || containsEmpty(b) {
			return errors.New("options.brokers must be a non-empty list of host:port strings")
		}
	}
	if !oneOf(sc.OptString("key_by"), "", "request_id", "client", "type", "id") {
		return errors.New(`options.key_by must be "request_id", "client", "type" or "id"`)
	}
	if !oneOf(sc.OptString("acks"), "", "all", "leader") {
		return errors.New(`options.acks must be "all" or "leader"`)
	}
	if !oneOf(sc.OptString("compression"), "", "none", "gzip", "snappy", "lz4", "zstd") {
		return errors.New(`options.compression must be "none", "gzip", "snappy", "lz4" or "zstd"`)
	}
	if v, set := sc.Options["tls"]; set {
		if _, isBool := v.(bool); !isBool {
			return errors.New("options.tls must be true or false")
		}
	}
	if sc.OptString("tls_ca_file") != "" && !sc.OptBool("tls") {
		return errors.New("options.tls_ca_file needs options.tls = true")
	}
	mech := sc.OptString("sasl")
	if !oneOf(mech, "", "plain", "scram-sha-256", "scram-sha-512") {
		return errors.New(`options.sasl must be "plain", "scram-sha-256" or "scram-sha-512"`)
	}
	if mech != "" && (sc.OptString("username_env") == "" || sc.OptString("password_env") == "") {
		return errors.New("options.sasl needs options.username_env and options.password_env")
	}
	if mech == "" && (sc.OptString("username_env") != "" || sc.OptString("password_env") != "") {
		return errors.New("options.username_env and options.password_env need options.sasl")
	}
	return nil
}

func oneOf(s string, set ...string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

func containsEmpty(ss []string) bool {
	for _, s := range ss {
		if strings.TrimSpace(s) == "" {
			return true
		}
	}
	return false
}

type sink struct {
	cl      *kgo.Client
	topic   string
	keyBy   string
	timeout time.Duration
}

func newSink(sc events.SinkConfig, getenv func(string) string) (events.Sink, error) {
	var brokers []string
	if name := sc.OptString("brokers_env"); name != "" {
		v := getenv(name)
		if v == "" {
			return nil, fmt.Errorf("environment variable %s is not set", name)
		}
		for _, b := range strings.Split(v, ",") {
			if b = strings.TrimSpace(b); b != "" {
				brokers = append(brokers, b)
			}
		}
	} else {
		brokers, _ = sc.OptStrings("brokers")
	}
	timeout := sc.Timeout()
	opts := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.DefaultProduceTopic(sc.OptString("topic")),
		kgo.RecordDeliveryTimeout(timeout),
		kgo.ProduceRequestTimeout(timeout),
		kgo.ProducerLinger(0), // each Send waits for its own record: do not hold it back to fill a batch
	}
	if id := sc.OptString("client_id"); id != "" {
		opts = append(opts, kgo.ClientID(id))
	} else {
		opts = append(opts, kgo.ClientID("ignatius"))
	}
	if sc.OptString("acks") == "leader" {
		// An idempotent producer needs acks=all.
		opts = append(opts, kgo.RequiredAcks(kgo.LeaderAck()), kgo.DisableIdempotentWrite())
	} else {
		opts = append(opts, kgo.RequiredAcks(kgo.AllISRAcks()))
	}
	switch sc.OptString("compression") {
	case "none":
		opts = append(opts, kgo.ProducerBatchCompression(kgo.NoCompression()))
	case "gzip":
		opts = append(opts, kgo.ProducerBatchCompression(kgo.GzipCompression()))
	case "snappy":
		opts = append(opts, kgo.ProducerBatchCompression(kgo.SnappyCompression()))
	case "lz4":
		opts = append(opts, kgo.ProducerBatchCompression(kgo.Lz4Compression()))
	case "zstd":
		opts = append(opts, kgo.ProducerBatchCompression(kgo.ZstdCompression()))
	}
	if sc.OptBool("tls") {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if path := sc.OptString("tls_ca_file"); path != "" {
			pem, err := os.ReadFile(path)
			if err != nil {
				return nil, errors.New("cannot read options.tls_ca_file")
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("options.tls_ca_file holds no PEM certificate")
			}
			cfg.RootCAs = pool
		}
		opts = append(opts, kgo.DialTLSConfig(cfg))
	}
	if mech := sc.OptString("sasl"); mech != "" {
		user, pass := getenv(sc.OptString("username_env")), getenv(sc.OptString("password_env"))
		if user == "" {
			return nil, fmt.Errorf("environment variable %s is not set", sc.OptString("username_env"))
		}
		if pass == "" {
			return nil, fmt.Errorf("environment variable %s is not set", sc.OptString("password_env"))
		}
		var m sasl.Mechanism
		switch mech {
		case "plain":
			m = plain.Auth{User: user, Pass: pass}.AsMechanism()
		case "scram-sha-256":
			m = scram.Auth{User: user, Pass: pass}.AsSha256Mechanism()
		case "scram-sha-512":
			m = scram.Auth{User: user, Pass: pass}.AsSha512Mechanism()
		}
		opts = append(opts, kgo.SASL(m))
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, errors.New("could not create the Kafka client") // the cause can name a broker or option value
	}
	s := &sink{cl: cl, topic: sc.OptString("topic"), keyBy: sc.OptString("key_by"), timeout: timeout}
	if s.keyBy == "" {
		s.keyBy = "request_id"
	}
	// A topic that does not exist is a typo worth failing startup for, when the cluster can
	// say so. A cluster that cannot be reached or asked is left to the first send, like a broker
	// that is restarting.
	// The check gets 3 seconds: a cluster that rejects our credentials keeps the request retrying, and
	// startup must not hang on it.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := kmsg.NewPtrMetadataRequest()
	req.AllowAutoTopicCreation = false
	t := kmsg.NewMetadataRequestTopic()
	t.Topic = kmsg.StringPtr(s.topic)
	req.Topics = []kmsg.MetadataRequestTopic{t}
	if resp, err := req.RequestWith(ctx, cl); err == nil {
		for _, rt := range resp.Topics {
			if rt.ErrorCode == kerr.UnknownTopicOrPartition.Code {
				cl.Close()
				return nil, errors.New("the topic does not exist on this cluster (create it first; the sink does not create topics)")
			}
		}
	}
	return s, nil
}

// Send produces one record and waits for the broker's acknowledgement. It returns an
// *events.SendError whose Kind is "rejected" (it can never succeed: the record is too large,
// the topic is unknown, or the principal may not write to it), "timeout" or "transport"; the
// client's message, which can name brokers and topics, is never carried.
func (s *sink) Send(ctx context.Context, ev events.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return &events.SendError{Kind: "rejected"}
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout+time.Second) // the client's own delivery timeout fires first
	defer cancel()
	rec := &kgo.Record{
		Key: []byte(s.key(ev, body)), Value: body,
		Headers: []kgo.RecordHeader{
			{Key: "content-type", Value: []byte("application/cloudevents+json")},
			{Key: "ignatius-event", Value: []byte(ev.Type)},
			{Key: "ignatius-delivery", Value: []byte(ev.ID)},
		},
	}
	if err := s.cl.ProduceSync(ctx, rec).FirstErr(); err != nil {
		return classify(err)
	}
	return nil
}

// classify maps a client error to a kind, carrying nothing from its message.
func classify(err error) *events.SendError {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, kgo.ErrRecordTimeout):
		return &events.SendError{Kind: "timeout"}
	case errors.Is(err, kerr.MessageTooLarge), errors.Is(err, kerr.InvalidTopicException),
		errors.Is(err, kerr.TopicAuthorizationFailed), errors.Is(err, kerr.UnknownTopicOrPartition),
		errors.Is(err, kerr.SaslAuthenticationFailed), errors.Is(err, kerr.ClusterAuthorizationFailed):
		return &events.SendError{Kind: "rejected"}
	}
	return &events.SendError{Kind: "transport"}
}

func (s *sink) Close() error { s.cl.Close(); return nil }

// key picks the partitioning key; see the package comment.
func (s *sink) key(ev events.Event, body []byte) string {
	switch s.keyBy {
	case "type":
		return ev.Type
	case "id":
		return ev.ID
	}
	var d struct {
		Data struct {
			RequestID string `json:"request_id"`
			Client    string `json:"client"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &d)
	key := d.Data.RequestID
	if s.keyBy == "client" {
		key = d.Data.Client
	}
	if key == "" {
		return ev.ID
	}
	return key
}
