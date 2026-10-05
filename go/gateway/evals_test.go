package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
)

// evalFake is an upstream whose answer depends on the state ("i7" is item 7): right on most items, wrong on every
// fourth, and unsure when wrong; it can be held to keep a run in progress.
func evalFake(t *testing.T, hold chan struct{}, perfect bool) *fake {
	t.Helper()
	f := &fake{status: 200, ready: 200}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if hold != nil {
			<-hold
		}
		var in struct {
			State     string                       `json:"state"`
			Questions map[string]ignatius.Question `json:"questions"`
		}
		json.Unmarshal(raw, &in)
		var idx int
		fmt.Sscanf(in.State, "i%d", &idx)
		gold := "a"
		if idx%2 == 1 {
			gold = "b"
		}
		pick, other, conf := gold, "b", 0.95
		if gold == "b" {
			other = "a"
		}
		if !perfect && idx%4 == 0 { // wrong, and not sure
			pick, other, conf = other, gold, 0.6
		}
		answers := map[string]any{}
		for qid := range in.Questions {
			answers[qid] = map[string]any{"type": "choice", "choice": pick, "confidence": conf,
				"probabilities": map[string]float64{pick: 0.5 + conf/2, other: 0.5 - conf/2}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers, "usage": map[string]int{"input_tokens": 100, "output_tokens": 1}})
	}))
	t.Cleanup(f.Close)
	return f
}

func evalData(n int, secret string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		gold := "a"
		if i%2 == 1 {
			gold = "b"
		}
		fmt.Fprintf(&b, `{"id":"t%d","state":"i%d %s","questions":{"c":{"type":"choice","instructions":"?","criteria":{"a":"x","b":"y"}}},"gold":{"c":"%s"}}`+"\n", i, i, secret, gold)
	}
	return b.String()
}

func evalServer(t *testing.T, enabled bool, hold chan struct{}, mut func(*Config)) (*Server, *bytes.Buffer) {
	t.Helper()
	cheap, strong := evalFake(t, hold, false), evalFake(t, nil, true)
	s := setup(t, map[string]*fake{"cheap": cheap, "strong": strong}, func(c *Config) {
		c.Eval.Enabled = enabled
		c.Clients = []ClientConfig{{Name: "adm", KeyEnv: "KADM", Admin: true}, {Name: "plain", KeyEnv: "KPL"}}
		if mut != nil {
			mut(c)
		}
	}, map[string]string{"K": "up", "KADM": "adm", "KPL": "pl"})
	t.Cleanup(func() { s.Close() })
	var logs bytes.Buffer
	s.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	return s, &logs
}

func startEval(s *Server, key string, body map[string]any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	rec := do(s, "POST", "/v1/evals", string(b), "Bearer "+key)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func pollEval(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	for i := 0; i < 300; i++ {
		rec := do(s, "GET", "/v1/evals/"+id, "", "Bearer adm")
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		if rec.Code == 200 && m["status"] != "running" {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the evaluation never finished")
	return nil
}

func TestEvaluationsAreOffByDefaultAndAdminOnly(t *testing.T) {
	s, _ := evalServer(t, false, nil, nil)
	code, m := startEval(s, "adm", map[string]any{"routes": []string{"cheap"}, "data": evalData(4, "x")})
	if code != 404 || errType(m) != "eval_disabled" {
		t.Errorf("off by default: %d %v", code, m)
	}
	for _, p := range []string{"/v1/evals", "/v1/evals/x"} {
		if rec := do(s, "GET", p, "", "Bearer adm"); rec.Code != 404 {
			t.Errorf("GET %s while off: %d", p, rec.Code)
		}
	}
	s, _ = evalServer(t, true, nil, nil)
	if code, m := startEval(s, "pl", map[string]any{"routes": []string{"cheap"}, "data": evalData(4, "x")}); code != 403 || errType(m) != "admin_required" {
		t.Errorf("a non-admin key: %d %v", code, m)
	}
	if rec := do(s, "GET", "/v1/evals", "", "Bearer pl"); rec.Code != 403 {
		t.Errorf("a non-admin may not list: %d", rec.Code)
	}
	if rec := do(s, "GET", "/v1/evals", "", ""); rec.Code != 403 {
		t.Errorf("no credential: %d", rec.Code)
	}
	// The status page learns whether to show its panel from the stats.
	stats := decode(t, do(s, "GET", "/v1/stats", "", "Bearer adm"))
	if stats["eval_enabled"] != true {
		t.Errorf("stats should say evaluations are on: %v", stats["eval_enabled"])
	}
}

func TestAnEvaluationRunsAndTheReportComesBack(t *testing.T) {
	s, logs := evalServer(t, true, nil, func(c *Config) {
		c.Models["strong"] = withPrice(c.Models["strong"], 10)
		c.Models["cheap"] = withPrice(c.Models["cheap"], 1)
	})
	code, m := startEval(s, "adm", map[string]any{
		"routes": []string{"strong", "cascade:cheap@0.8>strong"}, "data": evalData(40, "SECRET-STATE"), "sweep": true})
	if code != 202 || m["status"] != "running" || m["items"].(float64) != 40 || m["id"] == "" {
		t.Fatalf("start: %d %v", code, m)
	}
	done := pollEval(t, s, m["id"].(string))
	if done["status"] != "done" || done["error"] != nil {
		t.Fatalf("finished as %v: %v", done["status"], done["error"])
	}
	rep := done["report"].(map[string]any)
	cands := rep["candidates"].([]any)
	if rep["baseline"] != "strong" || len(cands) != 2 || rep["items"].(float64) != 40 || rep["judged_questions"].(float64) != 40 {
		t.Fatalf("report: %v", rep)
	}
	base, cas := cands[0].(map[string]any), cands[1].(map[string]any)
	if base["accuracy"].(float64) != 1 || cas["accuracy"].(float64) != 1 {
		t.Errorf("accuracy: strong %v cascade %v", base["accuracy"], cas["accuracy"])
	}
	tiers := cas["tiers"].([]any)
	if len(tiers) != 2 || tiers[0].(map[string]any)["escalated"].(float64) != 10 {
		t.Errorf("the cheap tier is unsure on every fourth item: %v", tiers)
	}
	if cas["vs_baseline"].(map[string]any)["cost_ratio"] == nil || len(rep["sweeps"].([]any)) != 1 {
		t.Errorf("a cost ratio and a sweep are expected: %v", cas["vs_baseline"])
	}
	// The dataset is content: it is in no report, no log line and no listing.
	body, _ := json.Marshal(done)
	listing := do(s, "GET", "/v1/evals", "", "Bearer adm").Body.String()
	for name, h := range map[string]string{"report": string(body), "logs": logs.String(), "listing": listing} {
		if strings.Contains(h, "SECRET-STATE") {
			t.Errorf("the dataset leaked into the %s", name)
		}
	}
	if !strings.Contains(logs.String(), `"msg":"eval_started"`) || !strings.Contains(logs.String(), `"msg":"eval_finished"`) {
		t.Errorf("a run should log its start and finish: %s", logs)
	}
	if strings.Contains(listing, `"report"`) {
		t.Error("the listing holds summaries, not reports")
	}
}

func withPrice(m ignatius.ModelConfig, perMTok float64) ignatius.ModelConfig {
	m.PriceInputPerMTok = &perMTok
	return m
}

func TestOnlyOneEvaluationRunsAtATimeAndItCanBeCancelled(t *testing.T) {
	hold := make(chan struct{})
	s, _ := evalServer(t, true, hold, nil)
	code, m := startEval(s, "adm", map[string]any{"routes": []string{"cheap"}, "data": evalData(6, "x"), "concurrency": 2})
	if code != 202 {
		t.Fatalf("%d %v", code, m)
	}
	id := m["id"].(string)
	if code, m := startEval(s, "adm", map[string]any{"routes": []string{"strong"}, "data": evalData(2, "x")}); code != 409 || errType(m) != "eval_busy" {
		t.Errorf("a second run while one is running: %d %v", code, m)
	}
	mid := decode(t, do(s, "GET", "/v1/evals/"+id, "", "Bearer adm"))
	if mid["status"] != "running" || mid["report"] != nil {
		t.Errorf("a run in progress has no report yet: %v", mid)
	}
	if rec := do(s, "DELETE", "/v1/evals/"+id, "", "Bearer adm"); rec.Code != 202 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	close(hold) // let the held model calls return
	done := pollEval(t, s, id)
	if done["status"] != "canceled" || done["report"] != nil {
		t.Errorf("a cancelled run: %v", done["status"])
	}
	// Now it is finished, so another can start, and a finished run can be forgotten.
	if code, _ := startEval(s, "adm", map[string]any{"routes": []string{"strong"}, "data": evalData(4, "x")}); code != 202 {
		t.Errorf("a new run after the first finished: %d", code)
	}
	if rec := do(s, "DELETE", "/v1/evals/"+id, "", "Bearer adm"); rec.Code != 204 {
		t.Errorf("forgetting a finished run: %d", rec.Code)
	}
	if rec := do(s, "GET", "/v1/evals/"+id, "", "Bearer adm"); rec.Code != 404 {
		t.Errorf("a forgotten run: %d", rec.Code)
	}
}

func TestEvaluationRequestsAreValidatedBeforeAnythingRuns(t *testing.T) {
	cheap := newFake(t, choiceSure)
	s := setup(t, map[string]*fake{"cheap": cheap}, func(c *Config) {
		c.Eval = EvalConfig{Enabled: true, MaxItems: 5}
		c.Clients = []ClientConfig{{Name: "adm", KeyEnv: "KADM", Admin: true}, {Name: "narrow", KeyEnv: "KN", Admin: true, Routes: []string{"cheap"}}}
	}, map[string]string{"K": "up", "KADM": "adm", "KN": "narrow"})
	for name, c := range map[string]struct {
		body map[string]any
		want int
		typ  string
	}{
		"no routes":          {map[string]any{"data": evalData(2, "x")}, 422, "invalid_request"},
		"too many routes":    {map[string]any{"routes": []string{"cheap", "a", "b", "c", "d", "e"}, "data": evalData(2, "x")}, 422, "invalid_request"},
		"no data":            {map[string]any{"routes": []string{"cheap"}}, 422, "invalid_request"},
		"data and items":     {map[string]any{"routes": []string{"cheap"}, "data": evalData(1, "x"), "items": []any{map[string]any{}}}, 422, "invalid_request"},
		"an unknown route":   {map[string]any{"routes": []string{"ghost"}, "data": evalData(2, "x")}, 422, "unknown_model"},
		"a bad dataset line": {map[string]any{"routes": []string{"cheap"}, "data": "{not json}\n"}, 422, "invalid_dataset"},
		"gold not an option": {map[string]any{"routes": []string{"cheap"}, "data": `{"state":"x","questions":{"c":{"type":"choice","instructions":"?","criteria":{"a":"x"}}},"gold":{"c":"zzz"}}`}, 422, "invalid_dataset"},
		"over max_items":     {map[string]any{"routes": []string{"cheap"}, "data": evalData(6, "x")}, 422, "invalid_dataset"},
	} {
		code, m := startEval(s, "adm", c.body)
		if code != c.want || errType(m) != c.typ {
			t.Errorf("%s: %d %v, want %d %s", name, code, m, c.want, c.typ)
		}
	}
	if code, m := startEval(s, "narrow", map[string]any{"routes": []string{"cascade:cheap>cheap"}, "data": evalData(2, "x")}); code != 403 {
		t.Errorf("an admin with a route allowlist may not evaluate what it may not use: %d %v", code, m)
	}
	if rec := do(s, "POST", "/v1/evals", "{", "Bearer adm"); rec.Code != 400 {
		t.Errorf("bad json: %d", rec.Code)
	}
	if rec := do(s, "GET", "/v1/evals", "", "Bearer adm"); strings.Contains(rec.Body.String(), `"id"`) {
		t.Errorf("nothing should have started: %s", rec.Body)
	}
}

func TestAnEvaluationTakesItemsAsJSONToo(t *testing.T) {
	s, _ := evalServer(t, true, nil, nil)
	items := []any{}
	for i := 0; i < 4; i++ {
		items = append(items, map[string]any{"state": fmt.Sprintf("i%d", i), "gold": map[string]any{"c": "a"},
			"questions": map[string]any{"c": map[string]any{"type": "choice", "instructions": "?", "criteria": map[string]string{"a": "x", "b": "y"}}}})
	}
	code, m := startEval(s, "adm", map[string]any{"routes": []string{"strong"}, "items": items})
	if code != 202 {
		t.Fatalf("%d %v", code, m)
	}
	if done := pollEval(t, s, m["id"].(string)); done["status"] != "done" {
		t.Errorf("%v %v", done["status"], done["error"])
	}
}

func TestClosingTheServerCancelsARunningEvaluation(t *testing.T) {
	hold := make(chan struct{})
	s, _ := evalServer(t, true, hold, nil)
	code, _ := startEval(s, "adm", map[string]any{"routes": []string{"cheap"}, "data": evalData(6, "x"), "concurrency": 2})
	if code != 202 {
		t.Fatal(code)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.Close() }()
	time.Sleep(50 * time.Millisecond)
	close(hold)
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("Close must not wait for an evaluation to finish on its own")
	}
	_ = context.Background
}
