// Package webhook is the event sink that POSTs Ignatius events to an HTTP endpoint (SPEC 14).
// It is a plugin like the Kafka and Kinesis sinks: a build that wants it imports it for its
// side effect (package standard does, and so does the stock command), and a build that leaves it
// out cannot make an outbound HTTP call from its event sinks at all.
//
//	[[events.sinks]]
//	type = "webhook"
//	name = "bridge"
//	only = ["request.completed"]        # optional, as for any sink
//	timeout_ms = 5000                   # per attempt
//	[events.sinks.options]
//	url_env = "EVENTS_URL"              # required: the URL is read from the environment (it may hold a token)
//	secret_env = "EVENTS_SECRET"        # optional: sign the body with HMAC-SHA256
//	auth_env = "EVENTS_AUTH"            # optional: sent whole as the Authorization header
//	retries = 2                         # after the first attempt
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/phin-tech/ignatius/go/events"
)

// backoff is the wait before retry n (0-based): 200 ms, 800 ms, 3.2 s. A variable so tests can shorten it.
var backoff = func(attempt int) time.Duration { return 200 * time.Millisecond << (2 * attempt) }

var knownOptions = map[string]bool{"url_env": true, "secret_env": true, "auth_env": true, "retries": true}

func init() {
	events.RegisterSinkType("webhook", events.SinkType{Validate: validate, New: newSink})
}

func validate(sc events.SinkConfig) error {
	for _, k := range sc.OptKeys() {
		if !knownOptions[k] {
			return fmt.Errorf("unknown option %q (want url_env, secret_env, auth_env, retries)", k)
		}
	}
	if sc.OptString("url_env") == "" {
		return errors.New("options.url_env is required for a webhook")
	}
	if v, set := sc.Options["retries"]; set {
		if n, ok := v.(int64); !ok || n < 0 {
			return errors.New("options.retries must be a non-negative integer")
		}
	}
	return nil
}

func parseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("the URL must be an http or https URL") // never echo the value: it may hold a token
	}
	return raw, nil
}

type sink struct {
	url, secret, auth string
	client            *http.Client
	retries           int
	backoff           func(attempt int) time.Duration
}

func newSink(sc events.SinkConfig, getenv func(string) string) (events.Sink, error) {
	urlEnv := sc.OptString("url_env")
	raw := getenv(urlEnv)
	if raw == "" {
		return nil, fmt.Errorf("environment variable %s is not set", urlEnv)
	}
	u, err := parseURL(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", urlEnv, err)
	}
	w := &sink{url: u, retries: 2,
		backoff: backoff}
	if _, set := sc.Options["retries"]; set {
		w.retries = sc.OptInt("retries")
	}
	if name := sc.OptString("secret_env"); name != "" {
		if w.secret = getenv(name); w.secret == "" {
			return nil, fmt.Errorf("environment variable %s is not set", name)
		}
	}
	if name := sc.OptString("auth_env"); name != "" {
		if w.auth = getenv(name); w.auth == "" {
			return nil, fmt.Errorf("environment variable %s is not set", name)
		}
	}
	w.client = &http.Client{Timeout: sc.Timeout(),
		// A redirect could carry the Authorization header to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return w, nil
}

// Send posts the event as application/cloudevents+json. Headers:
//
//	X-Ignatius-Event      the event type
//	X-Ignatius-Delivery   the event id, the same on every retry (an idempotency key)
//	X-Ignatius-Timestamp  unix seconds when this attempt was made
//	X-Ignatius-Signature  sha256=HMAC-SHA256(secret, timestamp + "." + body), if a secret is set
//
// It retries a transport error, a timeout, a 429 and a 5xx, with backoff; any other
// response is final. A 2xx is success.
func (w *sink) Send(ctx context.Context, ev events.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return &events.SendError{Kind: "rejected"}
	}
	var last error
	for attempt := 0; attempt <= w.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(w.backoff(attempt - 1)):
			case <-ctx.Done():
				return last
			}
		}
		last = w.once(ctx, ev, body)
		var se *events.SendError
		if last == nil || (errors.As(last, &se) && se.Kind == "rejected") {
			return last
		}
	}
	return last
}

func (w *sink) once(ctx context.Context, ev events.Event, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return &events.SendError{Kind: "rejected"}
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("User-Agent", "ignatius-events")
	req.Header.Set("X-Ignatius-Event", ev.Type)
	req.Header.Set("X-Ignatius-Delivery", ev.ID)
	req.Header.Set("X-Ignatius-Timestamp", ts)
	if w.secret != "" {
		m := hmac.New(sha256.New, []byte(w.secret))
		m.Write([]byte(ts + "."))
		m.Write(body)
		req.Header.Set("X-Ignatius-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
	}
	if w.auth != "" {
		req.Header.Set("Authorization", w.auth)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		// err can contain the URL, which may hold a token: report only the kind.
		var ne interface{ Timeout() bool }
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return &events.SendError{Kind: "timeout"}
		}
		return &events.SendError{Kind: "transport"}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return &events.SendError{Kind: "http", Status: resp.StatusCode}
	default:
		return &events.SendError{Kind: "rejected", Status: resp.StatusCode}
	}
}

func (w *sink) Close() error { w.client.CloseIdleConnections(); return nil }
