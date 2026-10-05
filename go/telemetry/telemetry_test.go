package telemetry_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/telemetry"
	"go.opentelemetry.io/otel/trace"
)

func f64(v float64) *float64 { return &v }

func TestValidate(t *testing.T) {
	for name, c := range map[string]telemetry.Config{
		"grpc not supported":    {OTLPEndpoint: "http://c:4318", OTLPProtocol: "grpc"},
		"endpoint not a URL":    {OTLPEndpoint: "collector:4318"},
		"endpoint wrong scheme": {OTLPEndpoint: "ftp://c:4318"},
		"ratio above 1":         {OTLPEndpoint: "http://c:4318", TracesSampleRatio: f64(1.5)},
		"ratio below 0":         {Prometheus: true, TracesSampleRatio: f64(-0.1)},
		"negative interval":     {Prometheus: true, MetricsIntervalMS: -1},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	for name, c := range map[string]telemetry.Config{
		"empty is fine (off)": {},
		"http/protobuf":       {OTLPEndpoint: "https://c:4318", OTLPProtocol: "http/protobuf", TracesSampleRatio: f64(0.1)},
		"prometheus only":     {Prometheus: true},
		"explicit ratio of 0": {OTLPEndpoint: "http://c", TracesSampleRatio: f64(0)},
	} {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSetupWithNothingConfiguredIsANoOp(t *testing.T) {
	p, err := telemetry.Setup(context.Background(), telemetry.Config{}, func(string) string { return "" }, "test")
	if err != nil || p.MetricsHandler != nil || p.Shutdown(context.Background()) != nil {
		t.Fatalf("%v %+v", err, p)
	}
	if _, err := telemetry.Setup(context.Background(), telemetry.Config{OTLPProtocol: "grpc", Prometheus: true}, func(string) string { return "" }, "t"); err == nil {
		t.Error("an invalid config must be rejected by Setup too")
	}
}

// collector is a fake OTLP/HTTP endpoint that records what it is sent.
type collector struct {
	*httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	auth  map[string]string
	body  map[string][]byte
	check string // the last X-Check request header, for env-var pass-through tests
}

func newCollector(t *testing.T) *collector {
	c := &collector{hits: map[string]int{}, auth: map[string]string{}, body: map[string][]byte{}}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.hits[r.URL.Path]++
		c.auth[r.URL.Path] = r.Header.Get("Authorization")
		if v := r.Header.Get("X-Check"); v != "" {
			c.check = v
		}
		c.body[r.URL.Path] = append(c.body[r.URL.Path], b...)
		c.mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *collector) snapshot() (hits map[string]int, auth map[string]string, body map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hits, auth, body = map[string]int{}, map[string]string{}, map[string]string{}
	for k, v := range c.hits {
		hits[k], auth[k], body[k] = v, c.auth[k], string(c.body[k])
	}
	return
}

// buildGateway compiles the real binary once for the subprocess tests.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func buildGateway(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ignatius-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "ignatius")
		out, err := exec.Command("go", "build", "-o", binPath, "github.com/phin-tech/ignatius/go/cmd/ignatius").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// The real binary, real SDK and exporters, a fake collector, and a request carrying a
// secret. Run as a subprocess because OpenTelemetry's globals bind once per process,
// so an in-process test would depend on test order. It also covers main's wiring and
// the flush on SIGTERM.
func TestBinaryExportsToCollectorAndPrometheus(t *testing.T) {
	const secret = "SENTINEL-E2E-5d2e"
	bin := buildGateway(t)
	col := newCollector(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"answers":{"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":5,"output_tokens":2}}`)
	}))
	defer up.Close()
	addr := freeAddr(t)
	cfgPath := filepath.Join(t.TempDir(), "ignatius.toml")
	cfg := fmt.Sprintf(`listen = %q
default_route = "m"

[telemetry]
service_name = "ignatius-test"
otlp_endpoint = %q
otlp_headers_env = "OTLP_AUTH"
prometheus = true
metrics_interval_ms = 50

[models.m]
provider = "systemone"
base_url = %q
price_input_per_mtok = 1.0
`, addr, col.URL+"/", up.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve", "--config", cfgPath)
	cmd.Env = []string{"OTLP_AUTH=Authorization=Bearer collector-token", "OTEL_RESOURCE_ATTRIBUTES=deployment.environment=e2e"}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	base := "http://" + addr
	for i := 0; ; i++ {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			break
		}
		if i > 100 {
			t.Fatalf("gateway did not start:\n%s", stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	body := `{"state":"` + secret + `","model":"m","questions":{"q":{"type":"noul","instructions":"` + secret + `"}}}`
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", base+"/v1/systemone", strings.NewReader(body))
		req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("request %d: %v %v", i, err, resp)
		}
		resp.Body.Close()
	}
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(raw)
	for _, want := range []string{
		"ignatius_requests_total",                  // counters end in _total
		"ignatius_request_duration_seconds_bucket", // durations in base-unit seconds, as a histogram
		"ignatius_model_call_duration_seconds",
		"ignatius_answer_confidence",
		`route="m"`, `mode="single"`, `client="anonymous"`,
		"ignatius_cost_usd_total",      // a lowercase unit, then _total
		`deployment_environment="e2e"`, // OTEL_RESOURCE_ATTRIBUTES is honored
		`service_name="ignatius-test"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics is missing %q:\n%.1500s", want, text)
		}
	}
	if strings.Contains(text, secret) {
		t.Error("/metrics leaked request content")
	}
	if strings.Contains(text, "otel_scope_") {
		t.Errorf("per-series scope labels are noise and should be off:\n%.500s", text)
	}

	// SIGTERM must flush both exporters to the collector before the process exits
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("gateway did not exit on SIGTERM:\n%s", stderr.String())
	}
	hits, auth, bodies := col.snapshot()
	if hits["/v1/traces"] == 0 || hits["/v1/metrics"] == 0 {
		t.Fatalf("both signals must reach the collector after shutdown, saw %v\n%s", hits, stderr.String())
	}
	if auth["/v1/traces"] != "Bearer collector-token" || auth["/v1/metrics"] != "Bearer collector-token" {
		t.Errorf("headers from the env var must reach the collector: %v", auth)
	}
	for path, b := range bodies {
		if strings.Contains(b, secret) {
			t.Errorf("OTLP payload to %s contains request content", path)
		}
	}
	for _, want := range []string{"ignatius-test", "ignatius.request", "0af7651916cd43dd8448eb211c80319c"} {
		if !strings.Contains(bodies["/v1/traces"], want) && !strings.Contains(bodies["/v1/traces"], hexToRaw(want)) {
			t.Errorf("the trace payload should contain %q", want)
		}
	}
	if strings.Contains(stderr.String(), secret) {
		t.Error("the gateway's log lines leaked request content")
	}
}

// hexToRaw is the byte form of a hex trace id, as OTLP/protobuf carries it.
func hexToRaw(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil {
		return "\x00no-such-bytes\x00"
	}
	return string(b)
}

func TestPrometheusOnlyNeedsNoCollector(t *testing.T) {
	prov, err := telemetry.Setup(context.Background(), telemetry.Config{Prometheus: true}, func(string) string { return "" }, "t")
	if err != nil || prov.MetricsHandler == nil {
		t.Fatalf("%v", err)
	}
	defer prov.Shutdown(context.Background())
	rec := httptest.NewRecorder()
	prov.MetricsHandler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Errorf("%d", rec.Code)
	}
}

// sampled counts the OTLP trace posts after the providers are flushed.
func exportedTraces(t *testing.T, ratio float64, start func(ctx context.Context, tp trace.TracerProvider)) int {
	t.Helper()
	col := newCollector(t)
	prov, err := telemetry.Setup(context.Background(), telemetry.Config{OTLPEndpoint: col.URL, TracesSampleRatio: &ratio, MetricsIntervalMS: 50},
		func(string) string { return "" }, "t")
	if err != nil {
		t.Fatal(err)
	}
	start(context.Background(), prov.TracerProvider) // the provider itself, not the process-wide global
	prov.Shutdown(context.Background())
	hits, _, _ := col.snapshot()
	return hits["/v1/traces"]
}

func TestSamplingRatio(t *testing.T) {
	root := func(ctx context.Context, tp trace.TracerProvider) {
		for i := 0; i < 20; i++ {
			_, sp := tp.Tracer("t").Start(ctx, "root")
			sp.End()
		}
	}
	if n := exportedTraces(t, 0, root); n != 0 {
		t.Errorf("ratio 0 must export no new traces, saw %d posts", n)
	}
	if n := exportedTraces(t, 1, root); n == 0 { // the control: the same harness does export at 1.0
		t.Error("ratio 1 must export traces (control for the test above)")
	}
	// a request that continues a sampled caller's trace is kept even at ratio 0
	remote := func(ctx context.Context, tp trace.TracerProvider) {
		tid, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
		sid, _ := trace.SpanIDFromHex("b7ad6b7169203331")
		ctx = trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true}))
		_, sp := tp.Tracer("t").Start(ctx, "child-of-sampled-parent")
		sp.End()
	}
	if n := exportedTraces(t, 0, remote); n == 0 {
		t.Error("a continued sampled trace must be kept even at ratio 0 (ParentBased)")
	}
}

// What happens to the standard OTEL_* variables, so the docs can say it exactly. The
// endpoint and protocol come from the TOML. An exporter setting that the TOML leaves
// unset, such as request headers, still comes from the standard variable.
func TestExporterEnvVarsFillGapsButTheEndpointComesFromConfig(t *testing.T) {
	bin := buildGateway(t)
	col, decoy := newCollector(t), newCollector(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"answers":{"q":{"type":"noul","noul":0.9}}}`)
	}))
	defer up.Close()
	addr := freeAddr(t)
	cfgPath := filepath.Join(t.TempDir(), "ignatius.toml")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`listen = %q
default_route = "m"
[telemetry]
otlp_endpoint = %q
metrics_interval_ms = 50
[models.m]
provider = "systemone"
base_url = %q
`, addr, col.URL, up.URL)), 0o600)
	cmd := exec.Command(bin, "serve", "--config", cfgPath)
	cmd.Env = []string{"OTEL_EXPORTER_OTLP_HEADERS=x-check=from-env", "OTEL_EXPORTER_OTLP_ENDPOINT=" + decoy.URL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	for i := 0; ; i++ {
		if r, err := http.Get("http://" + addr + "/healthz"); err == nil {
			r.Body.Close()
			break
		}
		if i > 100 {
			t.Fatal("gateway did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp, err := http.Post("http://"+addr+"/v1/systemone", "application/json",
		strings.NewReader(`{"state":"x","model":"m","questions":{"q":{"type":"noul","instructions":"?"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cmd.Process.Signal(syscall.SIGTERM)
	cmd.Wait()

	hits, _, _ := col.snapshot()
	if hits["/v1/traces"] == 0 {
		t.Fatalf("the endpoint from the TOML must be used, saw %v", hits)
	}
	if dh, _, _ := decoy.snapshot(); len(dh) != 0 {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT must NOT override the TOML endpoint, but the decoy got %v", dh)
	}
	col.mu.Lock()
	defer col.mu.Unlock()
	if col.check != "from-env" {
		t.Errorf("OTEL_EXPORTER_OTLP_HEADERS should still apply when the TOML sets no headers, got %q", col.check)
	}
}
