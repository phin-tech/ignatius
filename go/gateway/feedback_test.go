package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/store"
)

const (
	choiceSure   = `{"q":{"type":"choice","choice":"a","probabilities":{"a":0.95,"b":0.05},"confidence":0.9}}`
	choiceUnsure = `{"q":{"type":"choice","choice":"a","probabilities":{"a":0.55,"b":0.45},"confidence":0.1}}`
	sentinel     = "SENTINEL-state-7f3a"
)

func choiceReq(model, state string) string {
	return `{"state":"` + state + `","model":"` + model + `","questions":{"q":{"type":"choice","instructions":"` + sentinel + `-instr","criteria":{"a":"first","b":"second"}}}}`
}

// storeServer builds a server with a SQLite store, clients a (opted in, with content
// kept) and b (not), and returns it with the store and the log buffer.
func storeServer(t *testing.T, models map[string]*fake, mut func(*Config)) (*Server, store.Store, *bytes.Buffer) {
	t.Helper()
	env := map[string]string{"K": "up", "KA": "ka", "KB": "kb", "KADM": "adm", "IGNATIUS_STORE_DSN": filepath.Join(t.TempDir(), "s.db")}
	s := setup(t, models, func(c *Config) {
		c.Store = &StoreConfig{}
		c.Clients = []ClientConfig{{Name: "a", KeyEnv: "KA", StoreContent: true}, {Name: "b", KeyEnv: "KB"},
			{Name: "adm", KeyEnv: "KADM", Admin: true}}
		if mut != nil {
			mut(c)
		}
	}, env)
	t.Cleanup(func() { s.Close() })
	var logs bytes.Buffer
	s.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	return s, s.Store(), &logs
}

func reqID(t *testing.T, s *Server, body, key string) string {
	t.Helper()
	rec := do(s, "POST", "/v1/systemone", body, "Bearer "+key)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	s.Flush(context.Background()) // the store is written off the request path
	id := rec.Header().Get("X-Request-Id")
	var out struct {
		Ignatius struct {
			RequestID string `json:"request_id"`
		} `json:"ignatius"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if id == "" || out.Ignatius.RequestID != id {
		t.Fatalf("request id header %q body %q", id, out.Ignatius.RequestID)
	}
	return id
}

func fb(s *Server, key, body string) (int, map[string]any) {
	rec := do(s, "POST", "/v1/ignatius/feedback", body, "Bearer "+key)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func errType(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["type"].(string)
	return s
}

func TestFeedbackNeedsAStore(t *testing.T) {
	s := setup(t, map[string]*fake{"m": newFake(t, choiceSure)}, nil, nil)
	code, m := fb(s, "", `{"request_id":"x","question_id":"q","verdict":"good"}`)
	if code != 404 || errType(m) != "feedback_disabled" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestStoreContentNeedsAStoreTable(t *testing.T) {
	c := Config{Clients: []ClientConfig{{Name: "a", KeyEnv: "K", StoreContent: true}}}
	if err := c.validateStore(); err == nil {
		t.Fatal("store_content without [store] should be a config error")
	}
	c = Config{Store: &StoreConfig{Driver: "sqlite", RetentionDays: -1}}
	if err := c.validateStore(); err == nil {
		t.Fatal("negative retention should be a config error")
	}
	c = Config{Store: &StoreConfig{Driver: "mysql"}}
	if err := c.validateStore(); err == nil {
		t.Fatal("unknown driver should be a config error")
	}
}

func TestFeedbackRoundTrip(t *testing.T) {
	s, st, logs := storeServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, nil)
	id := reqID(t, s, choiceReq("m", "x"), "ka")

	code, m := fb(s, "ka", `{"request_id":"`+id+`","question_id":"q","verdict":"good"}`)
	if code != 200 || m["status"] != "recorded" {
		t.Fatalf("%d %v", code, m)
	}
	// The feedback carries the model, type and confidence that produced the answer.
	got, _ := st.Feedback(context.Background(), "a", time.Time{})
	if len(got) != 1 || got[0].Model != "m" || got[0].Type != "choice" || got[0].Confidence == nil || *got[0].Confidence != 0.9 || got[0].Source != "human" {
		t.Fatalf("feedback = %+v", got)
	}
	// A later verdict replaces it.
	if code, _ := fb(s, "ka", `{"request_id":"`+id+`","question_id":"q","verdict":"bad","correct":"b","source":"automated"}`); code != 200 {
		t.Fatalf("second feedback: %d", code)
	}
	got, _ = st.Feedback(context.Background(), "a", time.Time{})
	if len(got) != 1 || got[0].Verdict != "bad" || string(got[0].Correct) != `"b"` || got[0].Source != "automated" {
		t.Fatalf("feedback after replace = %+v", got)
	}
	// The log line has the verdict, and not the corrected answer.
	if !strings.Contains(logs.String(), `"verdict":"bad"`) || strings.Contains(logs.String(), `"correct"`) {
		t.Errorf("feedback log: %s", logs)
	}
}

// Any authenticated client may give feedback on any request. The feedback counts toward the
// client that made the request, whoever gave it.
func TestFeedbackCrossesClients(t *testing.T) {
	s, st, _ := storeServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, nil)
	id := reqID(t, s, choiceReq("m", "x"), "ka")
	if code, m := fb(s, "kb", `{"request_id":"`+id+`","question_id":"q","verdict":"bad","correct":"b"}`); code != 200 {
		t.Fatalf("another client's feedback: %d %v", code, m)
	}
	ctx := context.Background()
	got, _ := st.Feedback(ctx, "a", time.Time{}) // about a's requests
	if len(got) != 1 || got[0].Client != "b" || got[0].Owner != "a" || got[0].Verdict != "bad" {
		t.Fatalf("feedback about a's requests = %+v", got)
	}
	if other, _ := st.Feedback(ctx, "b", time.Time{}); len(other) != 0 {
		t.Fatalf("feedback is about a's request, not b's: %+v", other)
	}
	// Each client's own verdict is kept: b's does not replace a's, and a's later one replaces a's.
	fb(s, "ka", `{"request_id":"`+id+`","question_id":"q","verdict":"good"}`)
	fb(s, "ka", `{"request_id":"`+id+`","question_id":"q","verdict":"bad"}`)
	if got, _ = st.Feedback(ctx, "a", time.Time{}); len(got) != 2 {
		t.Fatalf("want one live verdict per giver, got %+v", got)
	}
	// The owner's questions are not shown to another client: its `correct` is checked by
	// shape only, so an option the question does not have is accepted from b and refused from a.
	if code, _ := fb(s, "kb", `{"request_id":"`+id+`","question_id":"q","verdict":"bad","correct":"not-an-option"}`); code != 200 {
		t.Errorf("b: %d", code)
	}
	if code, _ := fb(s, "ka", `{"request_id":"`+id+`","question_id":"q","verdict":"bad","correct":"not-an-option"}`); code != 422 {
		t.Errorf("a: %d", code)
	}
}

func TestFeedbackForSomethingThatDoesNotExist(t *testing.T) {
	s, _, _ := storeServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, nil)
	id := reqID(t, s, choiceReq("m", "x"), "ka")
	for name, body := range map[string]string{
		"unknown request":  `{"request_id":"nope","question_id":"q","verdict":"good"}`,
		"unknown question": `{"request_id":"` + id + `","question_id":"zz","verdict":"good"}`,
	} {
		if code, m := fb(s, "ka", body); code != 404 || errType(m) != "unknown_request" {
			t.Errorf("%s: %d %v", name, code, m)
		}
	}
	if code, _ := fb(s, "wrong", `{}`); code != 401 {
		t.Errorf("bad key: %d", code)
	}
}

func TestFeedbackValidation(t *testing.T) {
	s, _, _ := storeServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, nil)
	withContent := reqID(t, s, choiceReq("m", "x"), "ka")
	without := reqID(t, s, choiceReq("m", "x"), "kb")
	for name, c := range map[string]struct {
		key, body string
		want      int
	}{
		"bad verdict":             {"ka", `{"request_id":"` + withContent + `","question_id":"q","verdict":"meh"}`, 422},
		"missing ids":             {"ka", `{"verdict":"good"}`, 422},
		"bad source":              {"ka", `{"request_id":"` + withContent + `","question_id":"q","verdict":"good","source":"robot"}`, 422},
		"unknown field":           {"ka", `{"request_id":"` + withContent + `","question_id":"q","verdict":"good","extra":1}`, 400},
		"option not in criteria":  {"ka", `{"request_id":"` + withContent + `","question_id":"q","verdict":"bad","correct":"zzz"}`, 422},
		"wrong shape for choice":  {"ka", `{"request_id":"` + withContent + `","question_id":"q","verdict":"bad","correct":3}`, 422},
		"valid option":            {"ka", `{"request_id":"` + withContent + `","question_id":"q","verdict":"bad","correct":"b"}`, 200},
		"no content: shape only":  {"kb", `{"request_id":"` + without + `","question_id":"q","verdict":"bad","correct":"anything"}`, 200},
		"no content: wrong shape": {"kb", `{"request_id":"` + without + `","question_id":"q","verdict":"bad","correct":{"x":1}}`, 422},
	} {
		if code, m := fb(s, c.key, c.body); code != c.want {
			t.Errorf("%s: %d %v, want %d", name, code, m, c.want)
		}
	}
	for typ, c := range map[string]struct {
		v    string
		want string
	}{"noul": {`true`, ""}, "noul2": {`0.3`, ""}, "noul3": {`1.5`, "x"}, "score": {`2`, ""}, "score2": {`"hi"`, "x"}} {
		qt := strings.TrimRight(typ, "123")
		if got := checkCorrect([]byte(c.v), qt, nil, "q"); (got != "") != (c.want != "") {
			t.Errorf("%s %s: %q", qt, c.v, got)
		}
	}
}

func TestCascadeOutcomeRecordsTheBar(t *testing.T) {
	cheap, smart := newFake(t, choiceUnsure), newFake(t, choiceSure)
	s, st, _ := storeServer(t, map[string]*fake{"cheap": cheap, "smart": smart}, nil)
	id := reqID(t, s, choiceReq("cascade:cheap@0.5>smart", "x"), "kb")
	if code, _ := fb(s, "kb", `{"request_id":"`+id+`","question_id":"q","verdict":"good"}`); code != 200 {
		t.Fatal(code)
	}
	got, _ := st.Feedback(context.Background(), "b", time.Time{})
	// cheap was at 0.10 < 0.5, so smart supplied the answer; smart is the last tier and has no bar.
	if len(got) != 1 || got[0].Model != "smart" || got[0].Threshold != nil {
		t.Fatalf("feedback = %+v", got)
	}
	id = reqID(t, s, choiceReq("cascade:cheap@0.05>smart", "x"), "kb") // now cheap settles it
	fb(s, "kb", `{"request_id":"`+id+`","question_id":"q","verdict":"bad"}`)
	got, _ = st.Feedback(context.Background(), "b", time.Time{})
	last := got[len(got)-1]
	if last.Model != "cheap" || last.Threshold == nil || *last.Threshold != 0.05 {
		t.Fatalf("feedback = %+v", last)
	}
}

func TestContentIsStoredOnlyForOptedInClientsAndNeverLeaks(t *testing.T) {
	s, st, logs := storeServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, nil)
	ida := reqID(t, s, choiceReq("m", sentinel+"-state"), "ka")
	idb := reqID(t, s, choiceReq("m", sentinel+"-state"), "kb")
	fb(s, "ka", `{"request_id":"`+ida+`","question_id":"q","verdict":"bad","correct":"b"}`)

	ctx := context.Background()
	ca, _ := st.Content(ctx, "a", ida)
	if ca == nil || !strings.Contains(string(ca.State), sentinel) || !strings.Contains(string(ca.Questions), sentinel) {
		t.Fatalf("opted-in client's content missing: %+v", ca)
	}
	if cb, _ := st.Content(ctx, "b", idb); cb != nil {
		t.Fatalf("content stored for a client that did not opt in: %+v", cb)
	}
	if _, _, _, err := st.Outcome(ctx, idb, "q"); err != nil {
		t.Fatalf("metadata should still be stored for every client: %v", err)
	}
	_ = logs // leaks are checked in TestStoredContentNeverReachesTelemetry
	var out bytes.Buffer
	n, err := ExportJSONL(ctx, st, "a", time.Time{}, &out)
	if err != nil || n != 1 {
		t.Fatalf("export: %d %v", n, err)
	}
	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("export line: %v\n%s", err, out.String())
	}
	f := line["feedback"].(map[string]any)
	if line["request_id"] != ida || line["model"] != "m" || f["verdict"] != "bad" || f["correct"] != "b" ||
		!strings.Contains(out.String(), sentinel) || len(line["model_answers"].([]any)) != 1 {
		t.Errorf("export line: %s", out.String())
	}
	if n, _ := ExportJSONL(ctx, st, "b", time.Time{}, &bytes.Buffer{}); n != 0 {
		t.Errorf("client b exported %d rows without any stored content", n)
	}
}

func TestRetentionSweep(t *testing.T) {
	s, st, _ := storeServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, func(c *Config) {
		c.Clients[0].RetentionDays = 7 // a: content after 7 days
		c.Store.RetentionDays = 30     // everything after 30 days
	})
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ida := reqID(t, s, choiceReq("m", "x"), "ka")
	idb := reqID(t, s, choiceReq("m", "x"), "kb")
	fb(s, "ka", `{"request_id":"`+ida+`","question_id":"q","verdict":"good"}`)
	ctx := context.Background()

	now = now.AddDate(0, 0, 3)
	if r, err := s.Sweep(ctx); err != nil || r != (store.PurgeResult{}) {
		t.Fatalf("nothing is old yet: %+v %v", r, err)
	}
	now = now.AddDate(0, 0, 10) // 13 days: past a's 7-day content retention
	r, err := s.Sweep(ctx)
	if err != nil || r.Contents != 1 || r.Requests != 0 {
		t.Fatalf("content sweep = %+v %v", r, err)
	}
	if c, _ := st.Content(ctx, "a", ida); c != nil {
		t.Fatal("a's content survived its retention")
	}
	if _, _, _, err := st.Outcome(ctx, ida, "q"); err != nil {
		t.Fatal("a's metadata should outlive its content")
	}
	now = now.AddDate(0, 0, 30) // past the store-wide 30 days
	if r, err = s.Sweep(ctx); err != nil || r.Requests != 2 || r.Feedback != 1 {
		t.Fatalf("full sweep = %+v %v", r, err)
	}
	for _, c := range [][2]string{{"a", ida}, {"b", idb}} {
		if _, _, _, err := st.Outcome(ctx, c[1], "q"); err == nil {
			t.Errorf("%v survived the store-wide retention", c)
		}
	}
	// Feedback on a request that has been purged is a 404.
	if code, m := fb(s, "ka", `{"request_id":"`+ida+`","question_id":"q","verdict":"good"}`); code != 404 {
		t.Errorf("feedback after retention: %d %v", code, m)
	}
}

func TestNoRetentionUnlessConfigured(t *testing.T) {
	s, st, _ := storeServer(t, map[string]*fake{"m": newFake(t, choiceSure)}, nil)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	id := reqID(t, s, choiceReq("m", "x"), "ka")
	now = now.AddDate(10, 0, 0)
	if r, err := s.Sweep(context.Background()); err != nil || r != (store.PurgeResult{}) {
		t.Fatalf("sweep with no retention deleted %+v %v", r, err)
	}
	if c, _ := st.Content(context.Background(), "a", id); c == nil {
		t.Fatal("content deleted with no retention configured")
	}
	if s.hasRetention() {
		t.Error("hasRetention should be false")
	}
}

func TestRuntimeCredentialsFollowTheirPrincipal(t *testing.T) {
	user := &client{name: "u", storeContent: true}
	session := &client{name: "u", owner: user, session: true}
	minted := &client{name: "u", parent: session}
	if !minted.root().storeContent || minted.root() != user {
		t.Fatal("a key minted from a session must follow the user's store_content")
	}
	if (&client{name: "x"}).root().storeContent {
		t.Fatal("store_content must default to off")
	}
}

func TestCalibrate(t *testing.T) {
	n := 0
	f := func(conf float64, good bool) store.Feedback {
		v := "bad"
		if good {
			v = "good"
		}
		n++
		return store.Feedback{RequestID: fmt.Sprintf("req%d", n), QuestionID: "q", Model: "jeff", Type: "choice", Verdict: v, Confidence: &conf, Source: "human"}
	}
	var fbs []store.Feedback
	for i := 0; i < 30; i++ { // confident answers are right, the shaky ones are not
		fbs = append(fbs, f(0.92, true), f(0.55, i%2 == 0))
	}
	noConf := store.Feedback{RequestID: "noconf", QuestionID: "q", Model: "jeff", Type: "noul", Verdict: "good"}
	rows := Calibrate(append(fbs, noConf), 0.95, 20)
	if len(rows) != 1 || rows[0].Model != "jeff" || rows[0].Labeled != 60 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.Recommended == nil || *r.Recommended != 0.6 {
		t.Fatalf("the lowest bar that gets 95%% with 20 kept is 0.6 (everything >= 0.6 is the confident 30), got %v", r.Recommended)
	}
	byT := map[float64]CalPoint{}
	for _, p := range r.Points {
		byT[p.Threshold] = p
	}
	if p := byT[0.5]; p.Kept != 60 || p.Coverage != 1 {
		t.Errorf("at 0.5 everything is kept: %+v", p)
	}
	if p := byT[0.9]; p.Kept != 30 || p.Accuracy != 1 || p.Coverage != 0.5 {
		t.Errorf("at 0.9: %+v", p)
	}
	strict := Calibrate(fbs, 0.95, 1000)
	if strict[0].Recommended != nil {
		t.Error("too few kept answers must not produce a recommendation")
	}
	// Two clients rating the same decision count once, and the owner's verdict stands.
	var rated []store.Feedback
	for i, f := range fbs {
		f.RequestID, f.QuestionID, f.Client, f.Owner = fmt.Sprintf("r%d", i), "q", "me", "me"
		other := f
		other.Client = "someone-else"
		if other.Verdict == "good" {
			other.Verdict = "bad" // a disagreeing second rater
		}
		rated = append(rated, f, other)
	}
	if got := Calibrate(rated, 0.95, 20); got[0].Labeled != 60 || got[0].Accuracy != 0.75 {
		t.Errorf("a decision rated twice must count once, by the owner's verdict: %+v", got[0])
	}
	var out bytes.Buffer
	WriteCalibration(&out, strict, 0.95, 1000)
	if !strings.Contains(out.String(), "jeff / choice") || !strings.Contains(out.String(), "no bar reaches") {
		t.Errorf("table: %s", out.String())
	}
}

func TestCorrectChoiceAcceptsAListOfOptions(t *testing.T) {
	ct := &store.Content{Questions: []byte(`{"q":{"type":"choice","instructions":"?","criteria":["a","b"]}}`)}
	if msg := checkCorrect([]byte(`"b"`), "choice", ct, "q"); msg != "" {
		t.Errorf("b is an option: %s", msg)
	}
	if msg := checkCorrect([]byte(`"z"`), "choice", ct, "q"); msg == "" {
		t.Error("z is not an option")
	}
}

// SPEC 13.3: an opted-in client's content reaches the store and the admin export, and
// nothing else: no span, metric, log line, or status endpoint. Without 200s from the admin
// endpoints this would pass vacuously, so each response is checked before it is searched.
func TestStoredContentNeverReachesTelemetry(t *testing.T) {
	o := observe(t)
	good := newFake(t, `{"q":{"type":"choice","choice":"`+sentinel+`-label","probabilities":{"`+sentinel+`-label":0.9,"other":0.1},"confidence":0.8}}`)
	bad := newFake(t, choiceSure)
	bad.status, bad.body = 500, `{"error":"echoing your request: `+sentinel+`-upstream"}`
	s, st, logs := storeServer(t, map[string]*fake{"good": good, "bad": bad}, func(c *Config) {
		c.Routes = map[string]map[string]any{"safe": {"mode": "cascade", "tiers": []any{"bad", "good"}}}
	})
	body := func(model string) string {
		q := map[string]any{"q": map[string]any{"type": "choice", "instructions": "Is " + sentinel + "-instr it?",
			"criteria": map[string]string{sentinel + "-label": sentinel + "-desc", "other": "x"}}}
		b, _ := json.Marshal(map[string]any{"state": map[string]any{"text": sentinel + "-state"}, "model": model, "questions": q})
		return string(b)
	}
	var id string
	for _, model := range []string{"good", "safe", "cascade:bad>good", "fan-out:good,bad|vote", "bad"} {
		if rec := do(s, "POST", "/v1/systemone", body(model), "Bearer ka"); model == "good" {
			id = rec.Header().Get("X-Request-Id")
		}
	}
	do(s, "POST", "/v1/route", `{"request":{"state":"`+sentinel+`-state","questions":{"q":{"type":"choice","instructions":"`+sentinel+`-instr","criteria":["`+sentinel+`-label"]}}},"plan":{"mode":"fan_out","models":["good","bad"]}}`, "Bearer ka")
	if code, m := fb(s, "ka", `{"request_id":"`+id+`","question_id":"q","verdict":"bad","correct":"`+sentinel+`-label"}`); code != 200 {
		t.Fatalf("feedback: %d %v", code, m)
	}

	// The content really is in the store, so the absence below means something.
	s.Flush(context.Background())
	if c, _ := st.Content(context.Background(), "a", id); c == nil || !strings.Contains(string(c.State), sentinel+"-state") {
		t.Fatalf("setup: content not stored: %+v", c)
	}
	for name, haystack := range map[string]string{"spans and metrics": o.allTelemetryText(), "log lines": logs.String()} {
		if strings.Contains(haystack, sentinel) {
			t.Errorf("%s leaked stored content:\n%.600s", name, haystack)
		}
	}
	for _, path := range []string{"/v1/stats", "/v1/models", "/v1/routes", "/v1/profiles"} {
		rec := do(s, "GET", path, "", "Bearer adm")
		if rec.Code == 401 || rec.Code == 403 {
			t.Fatalf("setup: %s needs an admin key: %d", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), sentinel) {
			t.Errorf("%s leaked stored content: %.600s", path, rec.Body)
		}
	}
	if stats := do(s, "GET", "/v1/stats", "", "Bearer adm"); stats.Code != 200 || !strings.Contains(stats.Body.String(), `"calls"`) {
		t.Fatalf("setup: stats should be readable: %d %.200s", stats.Code, stats.Body)
	}
}
