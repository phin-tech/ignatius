package ignatius

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// recorder is a backend that answers every question and remembers each call it got.
type recorder struct {
	mu    sync.Mutex
	calls []Request
	cost  float64
	fail  string // a question id whose call fails
}

func (r *recorder) Call(_ context.Context, req Request) (WireResponse, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.mu.Unlock()
	w := WireResponse{Model: "rec", Answers: map[string]Answer{}, Usage: Usage{InputTokens: 10, OutputTokens: 1}}
	for id := range req.Questions {
		if id == r.fail {
			return WireResponse{}, &CallError{Kind: KindHTTP, Message: "503: boom", Status: 503}
		}
		w.Answers[id] = Answer{Type: "noul", Noul: fp(0.9)}
	}
	if r.cost > 0 {
		c := r.cost
		w.CostUSD = &c
	}
	return w, nil
}

func qs(n int) map[string]Question {
	m := map[string]Question{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("q%03d", i)] = Question{Type: "noul", Instructions: "?"}
	}
	return m
}

func TestLimitedRefusesImagesTheModelCannotTake(t *testing.T) {
	inner := &recorder{}
	imgs := []string{"aGk=", "aGk="}
	for name, c := range map[string]struct {
		limits LimitsConfig
		images []string
		ok     bool
		msg    string
	}{
		"no limits, no images":        {LimitsConfig{}, nil, true, ""},
		"no limits, an image":         {LimitsConfig{}, imgs[:1], false, "takes no images"},
		"room for them":               {LimitsConfig{MaxImages: 4}, imgs, true, ""},
		"exactly the limit":           {LimitsConfig{MaxImages: 2}, imgs, true, ""},
		"one over the limit":          {LimitsConfig{MaxImages: 1}, imgs, false, "at most 1 images"},
		"questions are not the limit": {LimitsConfig{MaxQuestions: 5}, imgs[:1], false, "takes no images"},
	} {
		inner.calls = nil
		_, err := Limited{Inner: inner, Limits: c.limits}.Call(context.Background(), Request{State: "x", Images: c.images, Questions: qs(1)})
		var ce *CallError
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: %v", name, err)
		case !c.ok && (err == nil || !asCallError(err, &ce) || ce.Kind != KindUnsupported || !strings.Contains(ce.Message, c.msg)):
			t.Errorf("%s: want an unsupported error mentioning %q, got %v", name, c.msg, err)
		case !c.ok && len(inner.calls) != 0:
			t.Errorf("%s: an unsupported request must not reach the model", name)
		}
	}
}

func asCallError(err error, target **CallError) bool {
	ce, ok := err.(*CallError)
	*target = ce
	return ok
}

func TestLimitedSplitsTooManyQuestionsAndMergesTheAnswers(t *testing.T) {
	inner := &recorder{cost: 0.5}
	w, err := Limited{Inner: inner, Limits: LimitsConfig{MaxQuestions: 4}}.Call(context.Background(),
		Request{State: "the state", Images: []string{"aGk="}, Questions: qs(10)}) // 10 questions, 4 per call: 4 + 4 + 2
	if err == nil || !strings.Contains(err.Error(), "images") {
		t.Fatalf("with no image allowance the split request is still refused: %v", err)
	}
	w, err = Limited{Inner: inner, Limits: LimitsConfig{MaxQuestions: 4, MaxImages: 1}}.Call(context.Background(),
		Request{State: "the state", Images: []string{"aGk="}, Questions: qs(10)})
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, c := range inner.calls {
		sizes = append(sizes, len(c.Questions))
		if c.State != "the state" || len(c.Images) != 1 {
			t.Errorf("every piece carries the state and the images: %+v", c)
		}
	}
	sort.Ints(sizes)
	if fmt.Sprint(sizes) != "[2 4 4]" {
		t.Errorf("piece sizes = %v, want [2 4 4]", sizes)
	}
	if len(w.Answers) != 10 || w.Usage.InputTokens != 30 || w.Usage.OutputTokens != 3 {
		t.Errorf("merged = %d answers, usage %+v", len(w.Answers), w.Usage)
	}
	if w.CostUSD == nil || *w.CostUSD != 1.5 {
		t.Errorf("the pieces' costs add up: %v", w.CostUSD)
	}
	// The same split every time: the first piece is always the first four ids in order.
	inner.calls = nil
	Limited{Inner: inner, Limits: LimitsConfig{MaxQuestions: 4, MaxImages: 1}}.Call(context.Background(), Request{State: "s", Images: []string{"a"}, Questions: qs(10)})
	for _, c := range inner.calls {
		ids := make([]string, 0)
		for id := range c.Questions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) == 4 && ids[0] != "q000" && ids[0] != "q004" {
			t.Errorf("an unstable split: %v", ids)
		}
	}
}

func TestLimitedIsAllOrNothingAndLeavesSmallRequestsAlone(t *testing.T) {
	inner := &recorder{fail: "q005"}
	_, err := Limited{Inner: inner, Limits: LimitsConfig{MaxQuestions: 4}}.Call(context.Background(), Request{State: "s", Questions: qs(10)})
	var ce *CallError
	if err == nil || !asCallError(err, &ce) || ce.Kind != KindHTTP {
		t.Fatalf("one failing piece fails the whole call with its error: %v", err)
	}
	inner = &recorder{}
	Limited{Inner: inner, Limits: LimitsConfig{MaxQuestions: 4}}.Call(context.Background(), Request{State: "s", Questions: qs(4)})
	if len(inner.calls) != 1 {
		t.Errorf("a request within the limit is one call, got %d", len(inner.calls))
	}
	if _, err := BuildRegistry(map[string]ModelConfig{"x": {Provider: "systemone", BaseURL: "http://x", Limits: &LimitsConfig{MaxImages: -1}}}, func(string) string { return "" }); err == nil {
		t.Error("a negative limit is a startup error")
	}
}

func TestImagesReachTheWireAndOnlyWhenThereAreSome(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		bodies = append(bodies, m)
		w.Write([]byte(`{"model":"clef","answers":{"q000":{"type":"noul","noul":0.9}},"usage":{}}`))
	}))
	defer srv.Close()
	reg, err := BuildRegistry(map[string]ModelConfig{
		"clef": {Provider: "systemone", BaseURL: srv.URL, Model: "clef", Limits: &LimitsConfig{MaxImages: 4, MaxQuestions: 64}},
	}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	run := func(images []string) {
		routed, err := Run(context.Background(), reg, Request{State: "x", Images: images, Questions: qs(1)}, Plan{Mode: ModeSingle, Model: "clef"})
		if err != nil || !routed.OK {
			t.Fatalf("%v %+v", err, routed.Failures)
		}
	}
	run([]string{"aGk=", "d29ybGQ="})
	run(nil)
	if got, _ := bodies[0]["images"].([]any); len(got) != 2 || got[0] != "aGk=" || bodies[0]["model"] != "clef" {
		t.Errorf("with images: %v", bodies[0])
	}
	if _, present := bodies[1]["images"]; present {
		t.Errorf("without images the field is absent, as for a model that has never heard of it: %v", bodies[1])
	}
}

// Same words, different picture: a different question, so the cache must not mix them up.
func TestTheCacheKeyIncludesTheImages(t *testing.T) {
	q := Question{Type: "noul", Instructions: "is it a cat?"}
	none, _ := questionKey("m", "u", "x", hashImages(nil), q)
	a, _ := questionKey("m", "u", "x", hashImages([]string{"AAA"}), q)
	b, _ := questionKey("m", "u", "x", hashImages([]string{"BBB"}), q)
	again, _ := questionKey("m", "u", "x", hashImages([]string{"AAA"}), q)
	ab, _ := questionKey("m", "u", "x", hashImages([]string{"AAA", "BBB"}), q)
	ba, _ := questionKey("m", "u", "x", hashImages([]string{"BBB", "AAA"}), q)
	if a != again {
		t.Error("the same images must give the same key")
	}
	for name, k := range map[string]string{"no images": none, "other image": b, "two images": ab, "reordered": ba} {
		if k == a {
			t.Errorf("%s collides with the single AAA image", name)
		}
	}
	if ab == ba {
		t.Error("image order matters to the model, so it matters to the key")
	}
	if hashImages(nil) != "" {
		t.Error("no images hash to the empty string, so existing keys are unchanged")
	}
}

// The text-only tier cannot take the picture: it fails as "unsupported" and the cascade carries on
// to the tier that can, which answers with the images.
func TestACascadeSkipsATierThatCannotTakeImages(t *testing.T) {
	textOnly := &recorder{}
	vision := &recorder{}
	reg := Registry{
		"jev":  Limited{Inner: textOnly}, // the default: no images
		"clef": Limited{Inner: vision, Limits: LimitsConfig{MaxImages: 4}},
	}
	routed, err := Run(context.Background(), reg, Request{State: "x", Images: []string{"aGk="}, Questions: qs(2)},
		Plan{Mode: ModeCascade, Tiers: []Tier{{Model: "jev"}, {Model: "clef"}}})
	if err != nil || !routed.OK {
		t.Fatalf("%v %+v", err, routed.Failures)
	}
	if len(textOnly.calls) != 0 {
		t.Error("the text-only model must not be called with an image request")
	}
	if len(vision.calls) != 1 || len(vision.calls[0].Images) != 1 || len(vision.calls[0].Questions) != 2 {
		t.Errorf("the vision tier answers both questions with the image: %+v", vision.calls)
	}
	if len(routed.Failures) != 1 || routed.Failures[0].Model != "jev" || routed.Failures[0].Error.Kind != KindUnsupported {
		t.Errorf("the skipped tier is reported as data: %+v", routed.Failures)
	}
	for id, a := range routed.Answers {
		if a.Model != "clef" {
			t.Errorf("%s was answered by %s", id, a.Model)
		}
	}
	// Without images the cheap tier answers as it always did.
	textOnly.calls, vision.calls = nil, nil
	routed, _ = Run(context.Background(), reg, Request{State: "x", Questions: qs(2)}, Plan{Mode: ModeCascade, Tiers: []Tier{{Model: "jev", Threshold: &Threshold{Default: fp(0.5)}}, {Model: "clef"}}})
	if len(textOnly.calls) != 1 || len(vision.calls) != 0 || len(routed.Failures) != 0 {
		t.Errorf("text-only requests are unchanged: jev %d clef %d failures %+v", len(textOnly.calls), len(vision.calls), routed.Failures)
	}
}

func TestTheCacheHandsImagesToTheBackendOnAMiss(t *testing.T) {
	inner := &recorder{}
	c := &Cached{Inner: inner, C: NewCache(CacheConfig{Enabled: true, MaxEntries: 100, TTLMS: 60000}, nil), Alias: "m", UpstreamModel: "u"}
	req := Request{State: "x", Images: []string{"AAA"}, Questions: qs(1)}
	if _, err := c.Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(inner.calls) != 1 || len(inner.calls[0].Images) != 1 {
		t.Fatalf("the miss must carry the images: %+v", inner.calls)
	}
	c.Call(context.Background(), req) // a hit: no call
	if len(inner.calls) != 1 {
		t.Errorf("the same question and image is a hit: %d calls", len(inner.calls))
	}
	req.Images = []string{"BBB"}
	c.Call(context.Background(), req) // another picture: a miss
	if len(inner.calls) != 2 {
		t.Errorf("a different image must miss: %d calls", len(inner.calls))
	}
}

func TestValidateRejectsAnEmptyImage(t *testing.T) {
	if err := (Request{Images: []string{"aGk=", ""}, Questions: qs(1)}).Validate(); err == nil || !strings.Contains(err.Error(), "images[1]") {
		t.Errorf("an empty image must be refused, naming it: %v", err)
	}
	if err := (Request{Images: []string{"aGk="}, Questions: qs(1)}).Validate(); err != nil {
		t.Errorf("a valid image: %v", err)
	}
}
