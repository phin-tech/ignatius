package kafka

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"encoding/json"
)

// The point of a plugin is that only a build asking for it has it. This builds both binaries
// and runs the same config through each: the custom build serves a request and a consumer
// reading the topic sees the event for that request id; the core build refuses to start and
// says why.
func TestOnlyTheCustomBuildHasTheSink(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries")
	}
	dir := t.TempDir()
	custom, core := filepath.Join(dir, "ignatius-kafka"), filepath.Join(dir, "ignatius")
	for bin, pkg := range map[string]string{custom: "./cmd/ignatius", core: "../../cmd/ignatius"} {
		if out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, out)
		}
	}
	c := cluster(t, "ignatius-events")
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
type = "kafka"
[events.sinks.options]
brokers_env = "KAFKA_BROKERS"
topic = "ignatius-events"
`, addr, upstream.URL)), 0o600)
	env := append(os.Environ(), "KAFKA_BROKERS="+strings.Join(c.ListenAddrs(), ","))

	var stderr bytes.Buffer
	cmd := exec.Command(core, "serve", "--config", cfg)
	cmd.Env, cmd.Stderr = env, &stderr
	if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), "not compiled into this binary") || !strings.Contains(stderr.String(), "kafka") {
		t.Fatalf("the core build should refuse the kafka sink: %v\n%s", err, stderr.String())
	}

	cmd = exec.Command(custom, "serve", "--config", cfg)
	cmd.Env = env
	stderr.Reset()
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
		t.Fatalf("custom build did not start: %s", stderr.String())
	}
	resp, err := http.Post(base+"/v1/systemone", "application/json", strings.NewReader(
		`{"state":"x","model":"m","questions":{"q":{"type":"choice","instructions":"?","criteria":{"a":"a","b":"b"}}}}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("request: %v %v", resp, err)
	}
	resp.Body.Close()
	reqID := resp.Header.Get("X-Request-Id")
	cmd.Process.Signal(os.Interrupt) // a clean shutdown drains the event queue
	cmd.Wait()

	recs := consume(t, c.ListenAddrs(), "ignatius-events", 1)
	var ev struct {
		Type string `json:"type"`
		Data struct {
			RequestID string `json:"request_id"`
			OK        bool   `json:"ok"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recs[0].Value, &ev); err != nil {
		t.Fatal(err)
	}
	if reqID == "" || ev.Type != "ignatius.request.completed" || ev.Data.RequestID != reqID || !ev.Data.OK || string(recs[0].Key) != reqID {
		t.Fatalf("event = %+v key %q, want request %q\n%s", ev, recs[0].Key, reqID, stderr.String())
	}
}
