package kafka

// An opt-in round trip against a real broker (Apache Kafka, Redpanda, MSK ...), for the things
// the in-process fake cannot vouch for. The topic must already exist:
//
//	IGNATIUS_TEST_KAFKA_BROKERS=localhost:9092 IGNATIUS_TEST_KAFKA_TOPIC=ignatius-test \
//	  go test ./plugins/kafka -run RealBroker -v

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/events"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestRealBrokerRoundTrip(t *testing.T) {
	brokers, topic := os.Getenv("IGNATIUS_TEST_KAFKA_BROKERS"), os.Getenv("IGNATIUS_TEST_KAFKA_TOPIC")
	if brokers == "" || topic == "" {
		t.Skip("set IGNATIUS_TEST_KAFKA_BROKERS and IGNATIUS_TEST_KAFKA_TOPIC to run against a real broker")
	}
	list := strings.Split(brokers, ",")
	run := fmt.Sprintf("run-%d", time.Now().UnixNano())
	e, err := events.Build(sinkCfg(list, map[string]any{"topic": topic}), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"request_id": run})
	e.Emit(ev)
	closeAll(t, e)
	// The topic may hold older records: read until ours shows up.
	cl, err := kgo.NewClient(kgo.SeedBrokers(list...), kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		var found bool
		cl.PollFetches(ctx).EachRecord(func(r *kgo.Record) {
			var got events.Event
			if json.Unmarshal(r.Value, &got) == nil && got.ID == ev.ID && string(r.Key) == run {
				found = true
			}
		})
		if found {
			return
		}
	}
	t.Fatal("the event never showed up on the topic")
}
