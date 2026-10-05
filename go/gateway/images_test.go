package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/events"
	"github.com/phin-tech/ignatius/go/ignatius"
)

const imgSentinel = "IMG-SENTINEL-AAAA"

func withLimits(c *Config, name string, l ignatius.LimitsConfig) {
	m := c.Models[name]
	m.Limits = &l
	c.Models[name] = m
}

func imageBody(model, state string, images ...string) string {
	b, _ := json.Marshal(map[string]any{"state": state, "model": model, "images": images,
		"questions": map[string]any{"q": map[string]any{"type": "choice", "instructions": "?", "criteria": map[string]string{"a": "x", "b": "y"}}}})
	return string(b)
}

func lastSeen(f *fake) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		return ""
	}
	return f.seen[len(f.seen)-1]
}

// Clef through Ollama is a systemone model with an image allowance; Jev is a systemone model without.
func TestAnImageRequestReachesOnlyAModelThatTakesImages(t *testing.T) {
	jev, clef := newFake(t, choiceSure), newFake(t, choiceSure)
	s := setup(t, map[string]*fake{"jev": jev, "clef": clef}, func(c *Config) {
		withLimits(c, "clef", ignatius.LimitsConfig{MaxImages: 4, MaxQuestions: 64})
	}, map[string]string{"K": "up"})

	rec := do(s, "POST", "/v1/systemone", imageBody("clef", "ticket", imgSentinel, "BBB="), "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var up map[string]any
	json.Unmarshal([]byte(lastSeen(clef)), &up)
	if imgs, _ := up["images"].([]any); len(imgs) != 2 || imgs[0] != imgSentinel || up["state"] != "ticket" {
		t.Fatalf("the model must receive the images: %s", lastSeen(clef))
	}

	// A model with no image allowance is never called with an image request, and the gateway says why.
	// This is the caller's request, not an upstream failure, so it is a 422 a client will not retry.
	rec = do(s, "POST", "/v1/systemone", imageBody("jev", "ticket", imgSentinel), "")
	if rec.Code != 422 || jev.posts != 0 || !strings.Contains(rec.Body.String(), "images_not_supported") || !strings.Contains(rec.Body.String(), "unsupported") {
		t.Fatalf("a text-only model: %d %s (upstream calls: %d)", rec.Code, rec.Body, jev.posts)
	}
	if strings.Contains(rec.Body.String(), imgSentinel) {
		t.Error("the error body must not echo the image")
	}

	// A model that takes images but is genuinely failing is still a gateway failure: 502.
	clef.mu.Lock()
	clef.status = 503
	clef.mu.Unlock()
	if rec := do(s, "POST", "/v1/systemone", imageBody("clef", "ticket", imgSentinel), ""); rec.Code != 502 {
		t.Errorf("an upstream failure with images stays a 502: %d %s", rec.Code, rec.Body)
	}
}

func TestACascadeFromATextOnlyTierToAVisionTier(t *testing.T) {
	jev, clef := newFake(t, choiceSure), newFake(t, choiceSure)
	s := setup(t, map[string]*fake{"jev": jev, "clef": clef}, func(c *Config) {
		withLimits(c, "clef", ignatius.LimitsConfig{MaxImages: 4})
	}, map[string]string{"K": "up"})

	rec := do(s, "POST", "/v1/systemone", imageBody("cascade:jev@0.5>clef", "ticket", imgSentinel), "")
	m := decode(t, rec)
	ign := m["ignatius"].(map[string]any)
	src := ign["sources"].(map[string]any)["q"].(map[string]any)
	if rec.Code != 200 || src["model"] != "clef" || jev.posts != 0 || !strings.Contains(lastSeen(clef), imgSentinel) {
		t.Fatalf("%d %s (jev calls %d)", rec.Code, rec.Body, jev.posts)
	}
	fails := ign["failures"].([]any)
	if len(fails) != 1 || fails[0].(map[string]any)["model"] != "jev" ||
		fails[0].(map[string]any)["error"].(map[string]any)["kind"] != "unsupported" {
		t.Errorf("the skipped tier is reported: %v", fails)
	}

	// The same cascade without images is untouched: the cheap tier answers alone.
	clef.mu.Lock()
	clef.seen = nil
	clef.mu.Unlock()
	rec = do(s, "POST", "/v1/systemone", imageBody("cascade:jev@0.5>clef", "ticket"), "")
	m = decode(t, rec)
	if rec.Code != 200 || m["ignatius"].(map[string]any)["sources"].(map[string]any)["q"].(map[string]any)["model"] != "jev" || len(clef.seen) != 0 {
		t.Fatalf("text-only requests are unchanged: %d %s", rec.Code, rec.Body)
	}
}

func TestImagesOnTheNativeRouteAndValidation(t *testing.T) {
	clef := newFake(t, choiceSure)
	s := setup(t, map[string]*fake{"clef": clef}, func(c *Config) {
		withLimits(c, "clef", ignatius.LimitsConfig{MaxImages: 4})
	}, map[string]string{"K": "up"})
	l2 := `{"request":{"state":"x","images":["` + imgSentinel + `"],"questions":{"q":{"type":"choice","instructions":"?","criteria":{"a":"x","b":"y"}}}},"route":"clef"}`
	if rec := do(s, "POST", "/v1/route", l2, ""); rec.Code != 200 || !strings.Contains(lastSeen(clef), imgSentinel) {
		t.Fatalf("/v1/route: %d %s", rec.Code, rec.Body)
	}
	for name, body := range map[string]string{
		"an empty image":   imageBody("clef", "x", ""),
		"one empty of two": imageBody("clef", "x", "aGk=", ""),
	} {
		rec := do(s, "POST", "/v1/systemone", body, "")
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), "images[") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

// Pictures are input, not content a gateway keeps: they are in no stored content, no event, no log
// line and no stats, even for a client that opted in to storing its state and questions.
func TestImagesAreNeverStoredEmittedOrLogged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	clef := newFake(t, choiceSure)
	s, st, logs := storeServer(t, map[string]*fake{"clef": clef}, func(c *Config) {
		withLimits(c, "clef", ignatius.LimitsConfig{MaxImages: 4})
		c.Events = events.Config{Sinks: []events.SinkConfig{{Type: "file", Path: path}}}
	})
	rec := do(s, "POST", "/v1/systemone", imageBody("clef", sentinel+"-state", imgSentinel), "Bearer ka") // a opted in
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(lastSeen(clef), imgSentinel) {
		t.Fatal("setup: the model must have received the image")
	}
	s.Flush(context.Background())
	id := rec.Header().Get("X-Request-Id")
	ct, _ := st.Content(context.Background(), "a", id)
	if ct == nil || !strings.Contains(string(ct.State), sentinel) {
		t.Fatalf("setup: the opted-in client's content must be stored: %+v", ct)
	}
	stored := string(ct.State) + string(ct.Questions) + string(ct.Answers) + string(ct.Results) + string(ct.Plan)
	if strings.Contains(stored, imgSentinel) {
		t.Error("an image is in the stored content")
	}
	var stats string
	if r := do(s, "GET", "/v1/stats", "", "Bearer adm"); r.Code == 200 {
		stats = r.Body.String()
	} else {
		t.Fatalf("setup: stats %d", r.Code)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ev, _ := os.ReadFile(path)
	if !strings.Contains(string(ev), sentinel) {
		t.Fatal("setup: the opted-in event should carry the state")
	}
	for name, h := range map[string]string{"events": string(ev), "logs": logs.String(), "stats": stats} {
		if strings.Contains(h, imgSentinel) {
			t.Errorf("an image leaked into %s: %.300s", name, h)
		}
	}
}

func TestAnAuditCarriesTheImagesToTheNextTier(t *testing.T) {
	a, b := newFake(t, choiceSure), newFake(t, choiceSure)
	s, st := auditServer(t, map[string]*fake{"clef-flash": a, "clef": b}, 1, func(c *Config) {
		withLimits(c, "clef-flash", ignatius.LimitsConfig{MaxImages: 4})
		withLimits(c, "clef", ignatius.LimitsConfig{MaxImages: 4})
	})
	rec := do(s, "POST", "/v1/systemone", imageBody("cascade:clef-flash@0.5>clef", "x", imgSentinel), "Bearer kb")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if as := audits(t, s, st, "b"); len(as) != 1 || !as[0].Agreed {
		t.Fatalf("audits = %+v", as)
	}
	if !strings.Contains(lastSeen(b), imgSentinel) {
		t.Errorf("the audit of an image request must carry the image to the next tier: %s", lastSeen(b))
	}
}

// Telemetry is where an image would be most surprising: a span or metric attribute that copied a
// request field. Run an image request through a cascade and its shadow audit, both with the real
// library's spans, and search everything either one emitted.
func TestImagesAreNeverInTelemetry(t *testing.T) {
	o := observe(t)
	a, b := newFake(t, choiceSure), newFake(t, choiceSure)
	s, st := auditServer(t, map[string]*fake{"clef-flash": a, "clef": b}, 1, func(c *Config) {
		withLimits(c, "clef-flash", ignatius.LimitsConfig{MaxImages: 4})
		withLimits(c, "clef", ignatius.LimitsConfig{MaxImages: 4})
	})
	for _, model := range []string{"cascade:clef-flash@0.5>clef", "clef", "fan-out:clef-flash,clef|vote"} {
		if rec := do(s, "POST", "/v1/systemone", imageBody(model, "x", imgSentinel), "Bearer kb"); rec.Code != 200 {
			t.Fatalf("%s: %d %s", model, rec.Code, rec.Body)
		}
	}
	// An unsupported request exercises the failure path's spans as well.
	do(s, "POST", "/v1/systemone", imageBody("cascade:clef-flash@0.5>clef", "x", imgSentinel, "b", "c", "d", "e"), "Bearer kb")
	if as := audits(t, s, st, "b"); len(as) == 0 {
		t.Fatal("setup: the cascade should have been audited, so the audit's spans exist")
	}
	text := o.allTelemetryText()
	if !strings.Contains(text, "ignatius.cascade.tier") {
		t.Fatalf("setup: the cascade's spans were not captured: %.300s", text)
	}
	if strings.Contains(text, imgSentinel) {
		t.Errorf("an image reached a span or metric: %.600s", text)
	}
}

// The images are not stored, but the fact that there were some must be: an export row about "the
// attached image" has no input a trainer can see, and the count is how it knows to skip it.
func TestTheImageCountIsKeptWhereTheImagesAreNot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	clef := newFake(t, choiceSure)
	s, st, _ := storeServer(t, map[string]*fake{"clef": clef}, func(c *Config) {
		withLimits(c, "clef", ignatius.LimitsConfig{MaxImages: 4})
		c.Events = events.Config{Sinks: []events.SinkConfig{{Type: "file", Path: path}}}
	})
	for _, imgs := range [][]string{{imgSentinel, "BBB="}, {}} {
		if rec := do(s, "POST", "/v1/systemone", imageBody("clef", "x", imgs...), "Bearer ka"); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
	s.Flush(context.Background())
	var out bytes.Buffer
	if n, err := ExportJSONL(context.Background(), st, "a", time.Time{}, &out); err != nil || n != 2 {
		t.Fatalf("export: %d %v", n, err)
	}
	counts := map[float64]int{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var line map[string]any
		json.Unmarshal([]byte(l), &line)
		counts[line["images"].(float64)]++
	}
	if counts[2] != 1 || counts[0] != 1 {
		t.Errorf("the export must say how many images each row's request had: %v", counts)
	}
	if strings.Contains(out.String(), imgSentinel) {
		t.Error("the export holds an image")
	}
	s.Close()
	ev, _ := os.ReadFile(path)
	var withImages, without int
	for _, l := range strings.Split(strings.TrimSpace(string(ev)), "\n") {
		var e emitted
		json.Unmarshal([]byte(l), &e)
		switch e.Data.Images {
		case 2:
			withImages++
		case 0:
			without++
		}
	}
	if withImages != 1 || without != 1 {
		t.Errorf("events must carry the image count: %d with two, %d with none\n%s", withImages, without, ev)
	}
	if strings.Contains(string(ev), imgSentinel) {
		t.Error("an event holds an image")
	}
}
