package webhook_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a buffer a running process can write while the test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The webhook sink is optional. This builds the stock command (cmd/ignatius, which imports
// package standard) and a minimal one (package cli alone, from a throwaway module) and runs the
// same config through each: the stock build starts with a webhook sink; the minimal build refuses
// it and says why, so it cannot make an outbound HTTP call from an event sink.
func TestOnlyTheStockBuildHasTheWebhookSink(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries")
	}
	root, err := filepath.Abs("../../..") // the go/ module
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stock, minimal := filepath.Join(dir, "stock"), filepath.Join(dir, "minimal")
	build := func(cmd *exec.Cmd, what string) {
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", what, err, out)
		}
	}
	c := exec.Command("go", "build", "-o", stock, "./cmd/ignatius")
	c.Dir = root
	build(c, "stock")

	mod := filepath.Join(dir, "minimal-src")
	os.MkdirAll(mod, 0o755)
	os.WriteFile(filepath.Join(mod, "go.mod"), []byte(fmt.Sprintf(`module example.com/minimal

go 1.26.0

require github.com/phin-tech/ignatius/go v0.0.0

replace github.com/phin-tech/ignatius/go => %s
`, root)), 0o644)
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		os.WriteFile(filepath.Join(mod, "go.sum"), sum, 0o644)
	}
	os.WriteFile(filepath.Join(mod, "main.go"), []byte(`package main

import "github.com/phin-tech/ignatius/go/cli"

func main() { cli.Main() }
`), 0o644)
	c = exec.Command("go", "build", "-o", minimal, ".")
	c.Dir = mod
	c.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	build(c, "minimal")

	cfg := filepath.Join(dir, "gw.toml")
	os.WriteFile(cfg, []byte(`
listen = "127.0.0.1:0"
[models.m]
provider = "systemone"
base_url = "http://127.0.0.1:1"
model = "jev-latest"

[[events.sinks]]
type = "webhook"
[events.sinks.options]
url_env = "EVENTS_URL"
`), 0o600)
	env := append(os.Environ(), "EVENTS_URL=http://127.0.0.1:1/hook")

	var stderr syncBuf
	cmd := exec.Command(minimal, "serve", "--config", cfg)
	cmd.Env, cmd.Stderr = env, &stderr
	if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), "not compiled into this binary") ||
		!strings.Contains(stderr.String(), "webhook") || !strings.Contains(stderr.String(), "file") {
		t.Fatalf("the minimal build should refuse the webhook sink and list what it has: %v\n%s", err, stderr.String())
	}

	stderr = syncBuf{}
	cmd = exec.Command(stock, "serve", "--config", cfg)
	cmd.Env, cmd.Stderr = env, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	started := make(chan error, 1)
	go func() { started <- cmd.Wait() }()
	select {
	case err := <-started:
		t.Fatalf("the stock build should start with a webhook sink, but exited: %v\n%s", err, stderr.String())
	case <-time.After(1500 * time.Millisecond):
	}
	if !strings.Contains(stderr.String(), "ignatius listening") {
		t.Errorf("the stock build did not report listening: %s", stderr.String())
	}
}
