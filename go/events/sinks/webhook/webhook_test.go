package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/events"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func closeAll(t *testing.T, e *events.Emitter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func hook(opts map[string]any) events.Config {
	o := map[string]any{"url_env": "U"}
	for k, v := range opts {
		o[k] = v
	}
	return events.Config{Sinks: []events.SinkConfig{{Type: "webhook", Options: o}}}
}

type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func (r *recorder) handler(statuses ...int) http.Handler {
	i := 0
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs, r.body = append(r.reqs, req), append(r.body, b)
		status := 200
		if i < len(statuses) {
			status = statuses[i]
		}
		i++
		r.mu.Unlock()
		w.WriteHeader(status)
	})
}

func fast(t *testing.T) {
	t.Helper()
	old := backoff
	backoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { backoff = old })
}

func TestDeliversASignedCloudEvent(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	e, err := events.Build(hook(map[string]any{"secret_env": "S", "auth_env": "A"}),
		env(map[string]string{"U": srv.URL, "S": "shh", "A": "Bearer tok"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := events.New(events.TypeRequestCompleted, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), map[string]any{"request_id": "r1"})
	e.Emit(ev)
	closeAll(t, e)

	if len(rec.reqs) != 1 {
		t.Fatalf("got %d requests", len(rec.reqs))
	}
	r, body := rec.reqs[0], rec.body[0]
	if r.Header.Get("Content-Type") != "application/cloudevents+json" || r.Header.Get("X-Ignatius-Event") != events.TypeRequestCompleted ||
		r.Header.Get("X-Ignatius-Delivery") != ev.ID || r.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("headers: %v", r.Header)
	}
	m := hmac.New(sha256.New, []byte("shh"))
	m.Write([]byte(r.Header.Get("X-Ignatius-Timestamp") + "."))
	m.Write(body)
	if r.Header.Get("X-Ignatius-Signature") != "sha256="+hex.EncodeToString(m.Sum(nil)) {
		t.Error("the signature does not verify with the shared secret")
	}
	var got events.Event
	if err := json.Unmarshal(body, &got); err != nil || got.SpecVersion != "1.0" || got.Source != "ignatius" || got.ID != ev.ID {
		t.Fatalf("body = %s (%v)", body, err)
	}
}

func TestRetriesServerErrorsButNotClientErrors(t *testing.T) {
	fast(t)
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler(503, 429, 200))
	defer srv.Close()
	e, _ := events.Build(hook(nil), env(map[string]string{"U": srv.URL}), nil)
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), nil))
	closeAll(t, e)
	if len(rec.reqs) != 3 {
		t.Fatalf("503, 429 then 200 should be 3 attempts, got %d", len(rec.reqs))
	}
	if rec.reqs[0].Header.Get("X-Ignatius-Delivery") != rec.reqs[2].Header.Get("X-Ignatius-Delivery") {
		t.Error("a retry must carry the same delivery id")
	}

	rec = &recorder{}
	srv2 := httptest.NewServer(rec.handler(400))
	defer srv2.Close()
	e, _ = events.Build(hook(nil), env(map[string]string{"U": srv2.URL}), nil)
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), nil))
	closeAll(t, e)
	if len(rec.reqs) != 1 {
		t.Fatalf("a 400 is final, got %d attempts", len(rec.reqs))
	}

	rec = &recorder{}
	srv3 := httptest.NewServer(rec.handler(500, 500, 500, 500))
	defer srv3.Close()
	e, _ = events.Build(hook(map[string]any{"retries": int64(1)}), env(map[string]string{"U": srv3.URL}), nil)
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), nil))
	closeAll(t, e)
	if len(rec.reqs) != 2 {
		t.Fatalf("retries = 1 is two attempts, got %d", len(rec.reqs))
	}

	rec = &recorder{}
	srv4 := httptest.NewServer(rec.handler(500, 500))
	defer srv4.Close()
	e, _ = events.Build(hook(map[string]any{"retries": int64(0)}), env(map[string]string{"U": srv4.URL}), nil)
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), nil))
	closeAll(t, e)
	if len(rec.reqs) != 1 {
		t.Fatalf("retries = 0 is one attempt, got %d", len(rec.reqs))
	}
}

func TestDoesNotFollowRedirects(t *testing.T) {
	other := &recorder{}
	elsewhere := httptest.NewServer(other.handler())
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	e, _ := events.Build(hook(map[string]any{"auth_env": "A"}), env(map[string]string{"U": srv.URL, "A": "Bearer tok"}), nil)
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), nil))
	closeAll(t, e)
	if len(other.reqs) != 0 {
		t.Fatal("the Authorization header was forwarded to a redirect target")
	}
}

// A failure is logged by kind. The URL (it may hold a token), the secret, a response body and the
// event never reach the log.
func TestFailuresLogOnlyTheKind(t *testing.T) {
	fast(t)
	var logs bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "echo BODY-SENTINEL")
	}))
	url := srv.URL + "/hook?token=URL-SENTINEL"
	e, _ := events.Build(hook(map[string]any{"secret_env": "S", "retries": int64(0)}),
		env(map[string]string{"U": url, "S": "SECRET-SENTINEL"}), slog.New(slog.NewJSONHandler(&logs, nil)))
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), map[string]string{"state": "DATA-SENTINEL"}))
	closeAll(t, e)
	srv.Close()
	// and again with the server gone: a transport error carries the URL in net/http
	e, _ = events.Build(hook(map[string]any{"retries": int64(0)}), env(map[string]string{"U": url}), slog.New(slog.NewJSONHandler(&logs, nil)))
	e.Emit(events.New(events.TypeRequestCompleted, time.Now(), nil))
	closeAll(t, e)
	out := logs.String()
	if !strings.Contains(out, `"error_kind":"http"`) || !strings.Contains(out, `"error_kind":"transport"`) {
		t.Fatalf("expected both failure kinds logged: %s", out)
	}
	for _, s := range []string{"URL-SENTINEL", "SECRET-SENTINEL", "BODY-SENTINEL", "DATA-SENTINEL", "127.0.0.1"} {
		if strings.Contains(out, s) {
			t.Errorf("log leaked %q: %s", s, out)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	bad := map[string]events.Config{
		"no url_env":       {Sinks: []events.SinkConfig{{Type: "webhook"}}},
		"unknown option":   hook(map[string]any{"urll": "x"}),
		"negative retries": hook(map[string]any{"retries": int64(-1)}),
		"retries not int":  hook(map[string]any{"retries": "two"}),
	}
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := hook(map[string]any{"secret_env": "S", "auth_env": "A", "retries": int64(0)}).Validate(); err != nil {
		t.Errorf("valid config: %v", err)
	}
	// A missing or malformed environment value is a startup error that does not echo the value.
	_, err := events.Build(hook(nil), env(nil), nil)
	if err == nil || !strings.Contains(err.Error(), "U is not set") {
		t.Errorf("unset url env: %v", err)
	}
	_, err = events.Build(hook(nil), env(map[string]string{"U": "ftp://tok@host/x"}), nil)
	if err == nil || strings.Contains(err.Error(), "tok") {
		t.Errorf("bad url: %v", err)
	}
	_, err = events.Build(hook(map[string]any{"secret_env": "S"}), env(map[string]string{"U": "http://h/x"}), nil)
	if err == nil || !strings.Contains(err.Error(), "S is not set") {
		t.Errorf("unset secret env: %v", err)
	}
}
