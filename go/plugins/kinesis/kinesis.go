// Package kinesis is an optional event sink that writes Ignatius events to an Amazon
// Kinesis data stream (SPEC 14.5). It is its own Go module so the core never depends on the
// AWS SDK: a build that wants it imports it for its side effect,
//
//	import _ "github.com/phin-tech/ignatius/go/plugins/kinesis"
//
// which registers the "kinesis" sink type, the way a Caddy build adds a module. Configure it
// in the gateway's TOML:
//
//	[[events.sinks]]
//	type = "kinesis"
//	name = "stream"
//	only = ["request.completed"]            # optional, as for any sink
//	[events.sinks.options]
//	stream = "ignatius-events"              # required: the stream name
//	region = "us-east-1"                    # optional; else AWS_REGION or the usual chain
//	partition_by = "request_id"             # request_id (default) | client | type | id
//	endpoint_env = "KINESIS_ENDPOINT"       # optional env var holding a custom endpoint (LocalStack)
//
// Credentials are never in the config: the AWS default chain supplies them (environment,
// shared config, web identity, ECS or instance role). Each record is the same CloudEvents
// JSON the webhook sink sends. Delivery is best effort like every sink: the SDK retries
// throttling and transient errors, then the event is counted as failed.
package kinesis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/aws/smithy-go"
	"github.com/phin-tech/ignatius/go/events"
)

// maxRecordBytes is Kinesis's limit on one record's data.
const maxRecordBytes = 1 << 20

var knownOptions = map[string]bool{"stream": true, "region": true, "partition_by": true, "endpoint_env": true}

func init() {
	events.RegisterSinkType("kinesis", events.SinkType{Validate: validate, New: newSink})
}

func validate(sc events.SinkConfig) error {
	for _, k := range sc.OptKeys() {
		if !knownOptions[k] {
			return fmt.Errorf("unknown option %q (want stream, region, partition_by, endpoint_env)", k)
		}
	}
	if sc.OptString("stream") == "" {
		return errors.New("options.stream is required for a kinesis sink")
	}
	switch sc.OptString("partition_by") {
	case "", "request_id", "client", "type", "id":
	default:
		return errors.New(`options.partition_by must be "request_id", "client", "type" or "id"`)
	}
	return nil
}

type sink struct {
	api         *kinesis.Client
	stream      string
	partitionBy string
	timeout     time.Duration
}

func newSink(sc events.SinkConfig, getenv func(string) string) (events.Sink, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var opts []func(*config.LoadOptions) error
	if r := sc.OptString("region"); r != "" {
		opts = append(opts, config.WithRegion(r))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, errors.New("could not load the AWS configuration")
	}
	var endpoint *string
	if name := sc.OptString("endpoint_env"); name != "" {
		v := getenv(name)
		if v == "" {
			return nil, fmt.Errorf("environment variable %s is not set", name)
		}
		endpoint = aws.String(v)
	}
	s := &sink{
		stream:      sc.OptString("stream"),
		partitionBy: sc.OptString("partition_by"),
		timeout:     sc.Timeout(),
		api:         kinesis.NewFromConfig(cfg, func(o *kinesis.Options) { o.BaseEndpoint = endpoint }),
	}
	if s.partitionBy == "" {
		s.partitionBy = "request_id"
	}
	// A stream name that does not exist is a typo worth failing startup for. Anything else
	// (no permission to describe it, credentials that are not ready yet) is left to the first send.
	_, err = s.api.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamName: &s.stream})
	var nf interface{ ErrorCode() string }
	if errors.As(err, &nf) && nf.ErrorCode() == "ResourceNotFoundException" {
		return nil, errors.New("the stream does not exist in this account and region")
	}
	return s, nil
}

// Send writes one record. It returns an *events.SendError whose Kind is "rejected" (the
// request can never succeed: no such stream, no permission, record too large), "throttled",
// "timeout" or "transport"; the AWS message, which can echo names, is never carried.
func (s *sink) Send(ctx context.Context, ev events.Event) error {
	body, err := json.Marshal(ev)
	if err != nil || len(body) > maxRecordBytes {
		return &events.SendError{Kind: "rejected"}
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	_, err = s.api.PutRecord(ctx, &kinesis.PutRecordInput{
		StreamName: &s.stream, Data: body, PartitionKey: aws.String(s.partitionKey(ev, body)),
	})
	if err == nil {
		return nil
	}
	return classify(err)
}

// classify maps an SDK error to a kind, carrying nothing from the error's message.
func classify(err error) *events.SendError {
	if errors.Is(err, context.DeadlineExceeded) {
		return &events.SendError{Kind: "timeout"}
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch code := api.ErrorCode(); {
		case code == "ProvisionedThroughputExceededException" || strings.Contains(code, "Throttl"):
			return &events.SendError{Kind: "throttled"}
		case api.ErrorFault() == smithy.FaultClient:
			return &events.SendError{Kind: "rejected"}
		}
	}
	return &events.SendError{Kind: "transport"}
}

func (s *sink) Close() error { return nil }

// partitionKey picks the shard key. request_id keeps one request's events, and the feedback
// about it, in order on one shard; it falls back to the event id for an event without one.
func (s *sink) partitionKey(ev events.Event, body []byte) string {
	switch s.partitionBy {
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
	if s.partitionBy == "client" {
		key = d.Data.Client
	}
	if key == "" {
		return ev.ID
	}
	return key
}

func (s *sink) String() string { return "kinesis:" + s.stream }
