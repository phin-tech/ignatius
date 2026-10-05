package ignatius

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type seenReq struct {
	path, auth, ctype string
	body              map[string]any
}

func workersServer(t *testing.T, status int, respond string) (*httptest.Server, *seenReq) {
	t.Helper()
	seen := &seenReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen.path, seen.auth, seen.ctype = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		seen.body = nil // Unmarshal into a map merges: start clean for each request
		json.Unmarshal(raw, &seen.body)
		w.WriteHeader(status)
		io.WriteString(w, respond)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// pngHead is a PNG signature plus a few bytes: enough for the format to be recognized.
const pngHead = "iVBORw0KGgoAAAAA"

var wReq = Request{State: "ticket", Images: []string{pngHead}, Questions: map[string]Question{
	"c": {Type: "choice", Instructions: "?", Criteria: map[string]string{"a": "x", "b": "y"}},
	"n": {Type: "noul", Instructions: "?"},
}}

func TestWorkersAIAddressesTheModelInTheURLAndSendsWhatClefTakes(t *testing.T) {
	srv, seen := workersServer(t, 200, `{"result":{"answers":{
		"c":{"type":"choice","choice":"a","probabilities":{"a":0.9,"b":0.1},"confidence":0.8},
		"n":{"type":"noul","noul":0.7}},"usage":{"input_tokens":12,"output_tokens":0}},"success":true,"errors":[],"messages":[]}`)
	b := &WorkersAI{BaseURL: srv.URL + "/client/v4", AccountID: "ACC123", Model: "@cf/cloudflare/clef-flash", APIKey: "tok"}
	w, err := b.Call(context.Background(), wReq)
	if err != nil {
		t.Fatal(err)
	}
	if seen.path != "/client/v4/accounts/ACC123/ai/run/@cf/cloudflare/clef-flash" || seen.auth != "Bearer tok" || seen.ctype != "application/json" {
		t.Errorf("request: %+v", seen)
	}
	// Clef's input schema requires `model` as well as the URL, and images as data URLs.
	if seen.body["model"] != "clef-flash" {
		t.Errorf("model must be sent as the short name Clef's schema expects: %v", seen.body["model"])
	}
	imgs, _ := seen.body["images"].([]any)
	if seen.body["state"] != "ticket" || len(imgs) != 1 || imgs[0] != "data:image/png;base64,"+pngHead || len(seen.body["questions"].(map[string]any)) != 2 {
		t.Errorf("body: %v", seen.body)
	}
	if w.Answers["c"].Choice != "a" || *w.Answers["n"].Noul != 0.7 || w.Usage.InputTokens != 12 {
		t.Errorf("answers: %+v usage %+v", w.Answers, w.Usage)
	}
	// Without images the field is absent.
	b.Call(context.Background(), Request{State: "t", Questions: wReq.Questions})
	if _, has := seen.body["images"]; has {
		t.Errorf("no images: %v", seen.body)
	}
}

func TestWorkersAIDecodesTolerantlyAndFailsByKind(t *testing.T) {
	for name, c := range map[string]struct {
		status  int
		body    string
		kind    string
		inferTy bool
	}{
		"answers at the top level":   {200, `{"answers":{"n":{"noul":0.9}}}`, "", true},
		"a missing type is inferred": {200, `{"result":{"answers":{"n":{"noul":0.9}}},"success":true}`, "", true},
		"a 200 that says it failed":  {200, `{"result":null,"success":false,"errors":[{"code":7003,"message":"SECRET-DETAIL"}]}`, KindHTTP, false},
		"no answers":                 {200, `{"result":{},"success":true}`, KindDecode, false},
		"not JSON":                   {200, `<html>`, KindDecode, false},
		"unauthorized":               {401, `{"success":false,"errors":[{"code":10000}]}`, KindHTTP, false},
		"rate limited":               {429, `{}`, KindHTTP, false},
	} {
		srv, _ := workersServer(t, c.status, c.body)
		w, err := (&WorkersAI{BaseURL: srv.URL, AccountID: "A", Model: "@cf/cloudflare/clef"}).Call(context.Background(), wReq)
		var ce *CallError
		if c.kind == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			} else if a := w.Answers["n"]; a.Type != "noul" || *a.Noul != 0.9 {
				t.Errorf("%s: %+v", name, a)
			}
			continue
		}
		if err == nil || !asCallError(err, &ce) || ce.Kind != c.kind {
			t.Errorf("%s: want kind %s, got %v", name, c.kind, err)
			continue
		}
		if strings.Contains(ce.Message, "SECRET-DETAIL") {
			t.Errorf("%s: Cloudflare's error text must not be echoed on a 200: %q", name, ce.Message)
		}
		if c.status == 401 && ce.Status != 401 {
			t.Errorf("%s: status %d", name, ce.Status)
		}
	}
}

func TestWorkersAIDoesNotLeakTheAccountIdInAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // nothing listens: a transport error whose text would otherwise carry the URL
	_, err := (&WorkersAI{BaseURL: srv.URL, AccountID: "ACCOUNT-SENTINEL", Model: "@cf/cloudflare/clef"}).Call(context.Background(), wReq)
	if err == nil || strings.Contains(err.Error(), "ACCOUNT-SENTINEL") {
		t.Errorf("the account id must not appear in an error: %v", err)
	}
}

func TestRegistryBuildsWorkersAIFromConfig(t *testing.T) {
	srv, seen := workersServer(t, 200, `{"result":{"answers":{"n":{"type":"noul","noul":0.9}}},"success":true}`)
	env := map[string]string{"CF_ACCOUNT": "ACC9", "CF_TOKEN": "tok9"}
	getenv := func(k string) string { return env[k] }
	reg, err := BuildRegistry(map[string]ModelConfig{"clef": {Provider: "workers-ai", BaseURL: srv.URL, Model: "@cf/cloudflare/clef-flash",
		AccountIDEnv: "CF_ACCOUNT", APIKeyEnv: "CF_TOKEN", Limits: &LimitsConfig{MaxImages: 4, MaxQuestions: 64}}}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{State: "x", Images: []string{pngHead}, Questions: map[string]Question{"n": {Type: "noul", Instructions: "?"}}}
	routed, err := Run(context.Background(), reg, req, Plan{Mode: ModeSingle, Model: "clef"})
	if err != nil || !routed.OK || seen.path != "/accounts/ACC9/ai/run/@cf/cloudflare/clef-flash" || seen.auth != "Bearer tok9" {
		t.Fatalf("%v %+v path=%q auth=%q", err, routed, seen.path, seen.auth)
	}
	for name, m := range map[string]ModelConfig{
		"no model":          {Provider: "workers-ai", AccountIDEnv: "CF_ACCOUNT", APIKeyEnv: "CF_TOKEN"},
		"no account env":    {Provider: "workers-ai", Model: "@cf/x", APIKeyEnv: "CF_TOKEN"},
		"no token env":      {Provider: "workers-ai", Model: "@cf/x", AccountIDEnv: "CF_ACCOUNT"},
		"account env unset": {Provider: "workers-ai", Model: "@cf/x", AccountIDEnv: "NOPE", APIKeyEnv: "CF_TOKEN"},
	} {
		if _, err := BuildRegistry(map[string]ModelConfig{"m": m}, getenv); err == nil {
			t.Errorf("%s: want a startup error", name)
		}
	}
}

// Opt-in: the hosted service itself. This is the check that Clef's real response and image format
// match what the adapter assumes (see WorkersAI).
//
//	IGNATIUS_REAL=1 CLOUDFLARE_ACCOUNT_ID=... CLOUDFLARE_API_TOKEN=... go test ./ignatius -run TestRealClefWorkersAI -v
//	  IGNATIUS_REAL_IMAGE=path/to/image.png   also send this image
//	  IGNATIUS_REAL_CLEF_MODEL=@cf/cloudflare/clef-flash   (the default)
func TestRealClefWorkersAI(t *testing.T) {
	account, token := os.Getenv("CLOUDFLARE_ACCOUNT_ID"), os.Getenv("CLOUDFLARE_API_TOKEN")
	if os.Getenv("IGNATIUS_REAL") != "1" || account == "" || token == "" {
		t.Skip("set IGNATIUS_REAL=1, CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN to run against Workers AI")
	}
	model := os.Getenv("IGNATIUS_REAL_CLEF_MODEL")
	if model == "" {
		model = "@cf/cloudflare/clef-flash"
	}
	req := Request{State: "Invoice 4411 was charged twice. Please refund one or we cancel.", Questions: map[string]Question{
		"department": {Type: "choice", Instructions: "Which team should handle this?",
			Criteria: map[string]string{"billing": "invoices, payments, refunds", "technical": "bugs, outages", "sales": "pricing, contracts"}},
		"churn":   {Type: "noul", Instructions: "Does the user threaten to cancel?"},
		"urgency": {Type: "score", Instructions: "How urgent is this?", Criteria: []string{"no deadline", "this week", "today", "already late"}},
	}}
	if path := os.Getenv("IGNATIUS_REAL_IMAGE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		req.Images = []string{base64Std(b)}
		req.Questions["department"] = Question{Type: "choice", Instructions: "What does the attached image show?",
			Criteria: map[string]string{"billing": "an invoice", "technical": "a screenshot of an error", "other": "anything else"}}
	}
	w, err := (&WorkersAI{AccountID: account, Model: model, APIKey: token}).Call(context.Background(), req)
	if err != nil {
		t.Fatalf("workers ai: %v", err)
	}
	for _, id := range []string{"department", "churn", "urgency"} {
		a, ok := w.Answers[id]
		if !ok {
			t.Errorf("no answer for %s: %+v", id, w.Answers)
			continue
		}
		b, _ := json.Marshal(a)
		t.Logf("%-10s %s", id, b)
	}
	t.Logf("usage: %+v", w.Usage)
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// The confidence policy sits on whichever provider a model uses, the hosted one included.
func TestTheConfidencePolicyAppliesToWorkersAIToo(t *testing.T) {
	srv, _ := workersServer(t, 200, `{"result":{"answers":{"c":{"type":"choice","choice":"a","probabilities":{"a":0.6,"b":0.4},"confidence":0.99}}},"success":true}`)
	reg, err := BuildRegistry(map[string]ModelConfig{"clef": {Provider: "workers-ai", BaseURL: srv.URL, Model: "@cf/cloudflare/clef",
		AccountIDEnv: "A", APIKeyEnv: "T", Confidence: "derived"}}, func(k string) string { return "x" })
	if err != nil {
		t.Fatal(err)
	}
	routed, err := Run(context.Background(), reg, Request{State: "x", Questions: map[string]Question{"c": {Type: "choice", Instructions: "?", Criteria: []string{"a", "b"}}}},
		Plan{Mode: ModeSingle, Model: "clef"})
	if err != nil || !routed.OK {
		t.Fatalf("%v %+v", err, routed)
	}
	a := routed.Answers["c"]
	if *a.Confidence > 0.21 || *a.Confidence < 0.19 || a.NativeConfidence == nil || *a.NativeConfidence != 0.99 {
		t.Errorf("derived over the provider's 0.99: %+v", a)
	}
}

func TestWorkersImageLabelsByWhatTheBytesAre(t *testing.T) {
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	png := b64([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x00"))
	jpg := b64([]byte("\xff\xd8\xff\xe0\x00\x10JFIF"))
	webp := b64([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "))
	gif := b64([]byte("GIF89a\x00\x00\x00\x00\x00\x00"))
	for name, c := range map[string]struct {
		in   string
		want string
		ok   bool
	}{
		"png":                   {png, "data:image/png;base64," + png, true},
		"jpeg":                  {jpg, "data:image/jpeg;base64," + jpg, true},
		"webp":                  {webp, "data:image/webp;base64," + webp, true},
		"a data url passes":     {"DATA:image/gif;base64,AAAA", "DATA:image/gif;base64,AAAA", true},
		"whitespace is dropped": {png[:8] + "\n" + png[8:], "data:image/png;base64," + png, true},
		"gif is refused":        {gif, "", false},
		"not base64":            {"***not base64***", "", false},
		"too short":             {"aGk=", "", false},
		"empty":                 {"", "", false},
	} {
		got, ok := workersImage(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", name, got, ok, c.want, c.ok)
		}
	}
	// An image Clef cannot read fails the call before it is sent, by kind, and the cascade skips the tier.
	srv, seen := workersServer(t, 200, `{"answers":{"n":{"noul":0.9}}}`)
	_, err := (&WorkersAI{BaseURL: srv.URL, AccountID: "A", Model: "@cf/cloudflare/clef"}).Call(context.Background(),
		Request{State: "x", Images: []string{png, gif}, Questions: wReq.Questions})
	var ce *CallError
	if err == nil || !asCallError(err, &ce) || ce.Kind != KindUnsupported || !strings.Contains(ce.Message, "images[1]") {
		t.Errorf("a GIF must be refused by kind, naming it: %v", err)
	}
	if seen.path != "" {
		t.Error("nothing is sent when an image is refused")
	}
}
