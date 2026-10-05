package gateway

// A smoke test of the whole feedback loop against the real TypeSafe API (SPEC 13): a cascade, the
// shadow audit, feedback, calibrate and the export, on Jev's real wire format, confidences and
// latencies. Everything else in this repository uses fakes or emulators.
//
// It costs a few dozen API calls (ignatius.real_n requests, three questions each, each audited
// once), and the key stays in the environment:
//
//	IGNATIUS_REAL=1 TYPESAFE_API_KEY=... go test ./gateway -run TestRealJev -v
//	  IGNATIUS_REAL_N=5                    requests to send (default 5)
//	  IGNATIUS_REAL_BASE_URL=https://...   override the base URL (default https://api.typesafe.ai)
//
// Both cascade tiers are Jev (two aliases for one upstream), so the audit compares Jev with itself:
// it checks the plumbing and shows the real confidences, and says nothing about a cheap model.
// For a real cheap-versus-expensive comparison, point `cheap` at an open-weight model.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
)

func TestRealJevFeedbackLoop(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if os.Getenv("IGNATIUS_REAL") != "1" || key == "" {
		t.Skip("set IGNATIUS_REAL=1 and TYPESAFE_API_KEY to run against real Jev")
	}
	base := os.Getenv("IGNATIUS_REAL_BASE_URL")
	if base == "" {
		base = "https://api.typesafe.ai"
	}
	n := 5
	if v, err := strconv.Atoi(os.Getenv("IGNATIUS_REAL_N")); err == nil && v > 0 {
		n = v
	}

	env := map[string]string{"TYPESAFE_API_KEY": key, "KA": "ka", "IGNATIUS_STORE_DSN": filepath.Join(t.TempDir(), "real.db")}
	cfg := Config{
		Store: &StoreConfig{}, Audit: &AuditConfig{SampleRate: 1, MaxInflight: 4},
		Clients: []ClientConfig{{Name: "a", KeyEnv: "KA", StoreContent: true}},
		Models: map[string]ignatius.ModelConfig{
			"jev-a": {Provider: "systemone", BaseURL: base, Model: "jev-latest", APIKeyEnv: "TYPESAFE_API_KEY", TimeoutMS: 30000},
			"jev-b": {Provider: "systemone", BaseURL: base, Model: "jev-latest", APIKeyEnv: "TYPESAFE_API_KEY", TimeoutMS: 30000},
		},
		// A bar of 0 settles every question that has a confidence at the first tier (a bar of 0.01
		// would escalate a noul at exactly one half, which has confidence 0), so each is audited.
		Routes: map[string]map[string]any{"casc": {"mode": "cascade", "tiers": []any{map[string]any{"model": "jev-a", "threshold": 0}, "jev-b"}}},
	}
	cfg.applyDefaults()
	getenv := func(k string) string { return env[k] }
	reg, err := ignatius.BuildRegistry(cfg.Models, getenv)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, reg, getenv)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var logs bytes.Buffer
	s.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))

	body := `{"state":` + perfState + `,"model":"casc","questions":` + perfQuestions + `}`
	var ids []string
	start := time.Now()
	for i := 0; i < n; i++ {
		rec := do(s, "POST", "/v1/systemone", body, "Bearer ka")
		if rec.Code != 200 {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
		ids = append(ids, rec.Header().Get("X-Request-Id"))
	}
	t.Logf("%d cascade requests through %s in %v", n, base, time.Since(start).Round(time.Millisecond))
	s.Flush(context.Background())

	// 1. The audits: real answers, compared.
	ctx := context.Background()
	as, err := s.Store().Audits(ctx, "a", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 3*n {
		t.Fatalf("want one audit per question (%d), got %d; log: %s", 3*n, len(as), logs.String())
	}
	agreed := map[string][2]int{}
	for _, a := range as {
		c := agreed[a.Type]
		c[1]++
		if a.Agreed {
			c[0]++
		}
		agreed[a.Type] = c
		if a.Model != "jev-a" || a.AuditModel != "jev-b" {
			t.Errorf("audit models: %+v", a)
		}
		if a.Type != "noul" && a.Confidence == nil { // choice and score carry a confidence on the real wire
			t.Errorf("a %s audit has no confidence: %+v", a.Type, a)
		}
	}
	for typ, c := range agreed {
		t.Logf("audit agreement, %-6s %d of %d", typ, c[0], c[1])
	}

	// 2. Feedback on a real request: the model, type and confidence come from the real answer.
	if code, m := fb(s, "ka", `{"request_id":"`+ids[0]+`","question_id":"department","verdict":"good"}`); code != 200 {
		t.Fatalf("feedback: %d %v", code, m)
	}
	got, _ := s.Store().Feedback(ctx, "a", time.Time{})
	if len(got) != 1 || got[0].Model != "jev-a" || got[0].Type != "choice" || got[0].Confidence == nil {
		t.Fatalf("feedback = %+v", got)
	}
	t.Logf("a real choice confidence: %.3f", *got[0].Confidence)

	// 3. Calibrate over the audits, and the training export of the stored content.
	var table bytes.Buffer
	rows := Calibrate(AuditFeedback(as), 0.9, 1)
	WriteCalibration(&table, rows, 0.9, 1)
	t.Logf("calibrate --source audit:%s", table.String())
	if len(rows) == 0 {
		t.Error("calibrate found nothing to sweep")
	}
	var out bytes.Buffer
	lines, err := ExportJSONL(ctx, s.Store(), "a", time.Time{}, &out)
	if err != nil || lines != 3*n {
		t.Fatalf("export: %d lines, %v", lines, err)
	}
	var first map[string]any
	_ = json.Unmarshal([]byte(strings.SplitN(out.String(), "\n", 2)[0]), &first)
	if first["state"] == nil || first["model"] != "jev-a" {
		t.Errorf("export line: %.300s", out.String())
	}
}
