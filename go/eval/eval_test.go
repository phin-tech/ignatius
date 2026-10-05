package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
)

func fp(v float64) *float64 { return &v }

// scripted is a model whose answers are decided by a function of the item's state ("i7" -> 7).
type scripted struct {
	answer func(idx int, qid string, q ignatius.Question) ignatius.Answer
	usage  ignatius.Usage
	delay  time.Duration
	calls  *int
	mu     *sync.Mutex
}

func (s scripted) Call(ctx context.Context, req ignatius.Request) (ignatius.WireResponse, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ignatius.WireResponse{}, ctx.Err()
		}
	}
	if s.calls != nil {
		s.mu.Lock()
		*s.calls++
		s.mu.Unlock()
	}
	var idx int
	fmt.Sscanf(fmt.Sprint(req.State), "i%d", &idx)
	w := ignatius.WireResponse{Model: "m", Answers: map[string]ignatius.Answer{}, Usage: s.usage}
	for qid, q := range req.Questions {
		w.Answers[qid] = s.answer(idx, qid, q)
	}
	return w, nil
}

// choice answers option a or b with the given confidence.
func choice(pick string, conf float64) ignatius.Answer {
	other := "a"
	if pick == "a" {
		other = "b"
	}
	return ignatius.Answer{Type: "choice", Choice: pick, Probabilities: map[string]float64{pick: 0.5 + conf/2, other: 0.5 - conf/2}, Confidence: fp(conf)}
}

// items makes n items i0..i(n-1), each with one choice question whose gold alternates a, b.
func items(n int) []Item {
	var out []Item
	for i := 0; i < n; i++ {
		gold := "a"
		if i%2 == 1 {
			gold = "b"
		}
		it := Item{ID: fmt.Sprintf("t%d", i), State: fmt.Sprintf("i%d", i),
			Questions: map[string]ignatius.Question{"c": {Type: "choice", Instructions: "?", Criteria: map[string]any{"a": "x", "b": "y"}}},
			Gold:      map[string]any{"c": gold}}
		if err := it.Validate(); err != nil {
			panic(err)
		}
		out = append(out, it)
	}
	return out
}

func goldOf(i int) string {
	if i%2 == 1 {
		return "b"
	}
	return "a"
}

func wrongOf(i int) string {
	if goldOf(i) == "a" {
		return "b"
	}
	return "a"
}

// cheapModel is wrong on every fourth item, and says so with low confidence (0.6); right ones get 0.95.
func cheapModel(calls *int, mu *sync.Mutex) scripted {
	return scripted{calls: calls, mu: mu, usage: ignatius.Usage{InputTokens: 100}, answer: func(idx int, qid string, q ignatius.Question) ignatius.Answer {
		if idx%4 == 0 {
			return choice(wrongOf(idx), 0.6)
		}
		return choice(goldOf(idx), 0.95)
	}}
}

func strongModel() scripted {
	return scripted{usage: ignatius.Usage{InputTokens: 100}, answer: func(idx int, qid string, q ignatius.Question) ignatius.Answer {
		return choice(goldOf(idx), 0.99)
	}}
}

func registry() (ignatius.Registry, *int) {
	calls := 0
	var mu sync.Mutex
	return ignatius.Registry{
		"cheap":  &ignatius.Priced{Inner: cheapModel(&calls, &mu), InPerMTok: 1_000_000}, // $1 per 100 tokens... scaled below
		"strong": &ignatius.Priced{Inner: strongModel(), InPerMTok: 10_000_000},
		"wobbly": ignatius.Registry{}["x"], // placeholder replaced in tests that need it
	}, &calls
}

func single(model string) ignatius.Plan {
	return ignatius.Plan{Mode: ignatius.ModeSingle, Model: model}
}

func TestSingleModelAccuracyMissesAndWho(t *testing.T) {
	reg, _ := registry()
	delete(reg, "wobbly")
	rep, err := Run(context.Background(), reg, items(20), []Candidate{{"cheap", single("cheap")}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Candidates[0]
	if c.Judged != 20 || c.Correct != 15 || c.Accuracy != 0.75 || rep.Items != 20 || rep.Judged != 20 {
		t.Fatalf("accuracy: %+v", c)
	}
	if c.CI95[0] > 0.75 || c.CI95[1] < 0.75 || c.CI95[1]-c.CI95[0] > 0.5 {
		t.Errorf("the 95%% interval must contain the estimate and not be absurd: %v", c.CI95)
	}
	if len(c.Misses) != 5 || c.Misses[0].Item != "t0" || c.Misses[0].Question != "c" || c.Misses[0].Gold != "a" || c.Misses[0].Got != "b" || c.Misses[0].Model != "cheap" {
		t.Errorf("misses: %+v", c.Misses)
	}
	if len(c.AnsweredBy) != 1 || c.AnsweredBy[0].Model != "cheap" || c.AnsweredBy[0].Share != 1 || c.AnsweredBy[0].Accuracy != 0.75 {
		t.Errorf("answered by: %+v", c.AnsweredBy)
	}
	if ts := c.ByType["choice"]; ts.Judged != 20 || ts.Correct != 15 {
		t.Errorf("by type: %+v", c.ByType)
	}
	if c.InputTokens != 2000 || c.CostUSD == nil || c.CostPer1k == nil {
		t.Errorf("tokens and cost: %d %v", c.InputTokens, c.CostUSD)
	}
}

func TestCascadeTiersAndTheVerdictAgainstTheBaseline(t *testing.T) {
	reg, calls := registry()
	delete(reg, "wobbly")
	cands := []Candidate{
		{"strong", single("strong")},
		{"cascade", ignatius.Plan{Mode: ignatius.ModeCascade, Tiers: []ignatius.Tier{{Model: "cheap", Threshold: &ignatius.Threshold{Default: fp(0.8)}}, {Model: "strong"}}}},
	}
	rep, err := Run(context.Background(), reg, items(40), cands, Options{})
	if err != nil {
		t.Fatal(err)
	}
	base, cas := rep.Candidates[0], rep.Candidates[1]
	if base.Accuracy != 1 || cas.Accuracy != 1 {
		t.Fatalf("both are always right: %v %v", base.Accuracy, cas.Accuracy)
	}
	// The cheap tier is wrong, and unsure (0.6 < 0.8), on every fourth item: those 10 go on to the strong tier.
	if len(cas.Tiers) != 2 || cas.Tiers[0].Asked != 40 || cas.Tiers[0].Settled != 30 || cas.Tiers[0].Escalated != 10 ||
		cas.Tiers[1].Asked != 10 || cas.Tiers[1].Model != "strong" || cas.Tiers[0].Calls != 40 {
		t.Errorf("tiers: %+v", cas.Tiers)
	}
	by := map[string]AnsweredBy{}
	for _, a := range cas.AnsweredBy {
		by[a.Model] = a
	}
	if by["cheap"].Answers != 30 || by["cheap"].Accuracy != 1 || by["strong"].Answers != 10 {
		t.Errorf("who answered: %+v", cas.AnsweredBy)
	}
	// Strong costs 10x cheap per token; the cascade pays for cheap on all 40 and strong on 10.
	if cas.VsBaseline == nil || cas.VsBaseline.CostRatio == nil {
		t.Fatalf("a comparison with a cost ratio is expected: %+v", cas.VsBaseline)
	}
	want := (40*1.0 + 10*10.0) / (40 * 10.0)
	if math.Abs(*cas.VsBaseline.CostRatio-want) > 1e-9 {
		t.Errorf("cost ratio = %v, want %v", *cas.VsBaseline.CostRatio, want)
	}
	if !strings.Contains(cas.VsBaseline.Verdict, "no detectable difference") || !strings.Contains(cas.VsBaseline.Verdict, "35% of its cost") {
		t.Errorf("verdict: %q", cas.VsBaseline.Verdict)
	}
	if base.VsBaseline != nil {
		t.Error("the baseline is not compared with itself")
	}
	if *calls != 40 {
		t.Errorf("the cheap model should be called once per item: %d", *calls)
	}
}

func TestTheVerdictCallsABetterAndAWorseRoute(t *testing.T) {
	reg, _ := registry()
	delete(reg, "wobbly")
	// cheap is right on 75%; strong on 100%. Over 80 items the gap is far outside noise.
	rep, err := Run(context.Background(), reg, items(80), []Candidate{{"cheap", single("cheap")}, {"strong", single("strong")}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	v := rep.Candidates[1].VsBaseline
	if v.CI95[0] <= 0 || !strings.HasPrefix(v.Verdict, "better than cheap by 25.0 points") {
		t.Errorf("better: %+v", v)
	}
	rep, _ = Run(context.Background(), reg, items(80), []Candidate{{"strong", single("strong")}, {"cheap", single("cheap")}}, Options{})
	v = rep.Candidates[1].VsBaseline
	if v.CI95[1] >= 0 || !strings.HasPrefix(v.Verdict, "worse than strong by 25.0 points") {
		t.Errorf("worse: %+v", v)
	}
	// Deterministic for a seed.
	r1, _ := Run(context.Background(), reg, items(40), []Candidate{{"strong", single("strong")}, {"cheap", single("cheap")}}, Options{Seed: 5})
	r2, _ := Run(context.Background(), reg, items(40), []Candidate{{"strong", single("strong")}, {"cheap", single("cheap")}}, Options{Seed: 5})
	if r1.Candidates[1].VsBaseline.CI95 != r2.Candidates[1].VsBaseline.CI95 {
		t.Error("the same seed must give the same interval")
	}
}

func TestFanOutIsJudgedAsOneAnswerAndByMember(t *testing.T) {
	reg, _ := registry()
	delete(reg, "wobbly")
	reg["strong2"] = strongModel()
	plan := ignatius.Plan{Mode: ignatius.ModeFanOut, Models: []string{"cheap", "strong", "strong2"}, Reduce: "vote"}
	rep, err := Run(context.Background(), reg, items(40), []Candidate{{"consensus", plan}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Candidates[0]
	if c.Accuracy != 1 { // two strong votes beat one wrong cheap one
		t.Errorf("the vote should be right everywhere: %v", c.Accuracy)
	}
	got := map[string]MemberStats{}
	for _, m := range c.Members {
		got[m.Model] = m
	}
	if got["cheap"].Accuracy != 0.75 || got["strong"].Accuracy != 1 || got["strong2"].Judged != 40 {
		t.Errorf("members on their own: %+v", c.Members)
	}
	if len(c.AnsweredBy) != 1 || c.AnsweredBy[0].Model != "ensemble" {
		t.Errorf("a vote is answered by the ensemble: %+v", c.AnsweredBy)
	}
	// A fan-out with no reducer returns several answers per question and cannot be judged.
	_, err = Run(context.Background(), reg, items(4), []Candidate{{"raw", ignatius.Plan{Mode: ignatius.ModeFanOut, Models: []string{"cheap", "strong"}}}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "must reduce") {
		t.Errorf("a raw fan-out: %v", err)
	}
}

func TestTheSweepReplaysTheCascadeAtOtherThresholds(t *testing.T) {
	reg, calls := registry()
	delete(reg, "wobbly")
	plan := ignatius.Plan{Mode: ignatius.ModeCascade, Tiers: []ignatius.Tier{{Model: "cheap"}, {Model: "strong"}}}
	rep, err := Run(context.Background(), reg, items(40), []Candidate{{"cascade", plan}}, Options{Sweep: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sweeps) != 1 || rep.Sweeps[0].Candidate != "cascade" || len(rep.Sweeps[0].Rows) != len(SweepThresholds) {
		t.Fatalf("sweeps: %+v", rep.Sweeps)
	}
	rows := map[float64]SweepRow{}
	for _, r := range rep.Sweeps[0].Rows {
		rows[r.Threshold] = r
	}
	// The cheap model is wrong at confidence 0.6 on 10 of 40 items, and right at 0.95 on the rest.
	// At a bar of 0.5 it keeps everything (30 right of 40); from 0.7 up it passes the 10 wrong ones on.
	if r := rows[0.5]; r.Accuracy != 0.75 || r.Reach[0] != 0 {
		t.Errorf("at 0.5: %+v", r)
	}
	for _, thr := range []float64{0.7, 0.8, 0.9, 0.95} {
		if r := rows[thr]; r.Accuracy != 1 || r.Reach[0] != 0.25 {
			t.Errorf("at %v: %+v", thr, r)
		}
	}
	if r := rows[0.8]; r.CostPer1k == nil {
		t.Error("the sweep estimates cost when the models are priced")
	} else if r2 := rows[0.5]; *r.CostPer1k <= *r2.CostPer1k {
		t.Errorf("sending 25%% on to the dear tier must cost more than keeping everything: %v vs %v", *r.CostPer1k, *r2.CostPer1k)
	}
	if *calls != 80 { // the cascade run (40) and the cheap model's solo run (40): the sweep adds solo runs, not replays
		t.Errorf("cheap was called %d times", *calls)
	}
	// Without the flag there is no sweep and no extra calls.
	*calls = 0
	rep, _ = Run(context.Background(), reg, items(40), []Candidate{{"cascade", plan}}, Options{})
	if len(rep.Sweeps) != 0 || *calls != 40 {
		t.Errorf("no sweep expected: %d sweeps, %d calls", len(rep.Sweeps), *calls)
	}
}

func TestJudgingEachQuestionType(t *testing.T) {
	it := Item{ID: "x", State: "i0", Questions: map[string]ignatius.Question{
		"n": {Type: "noul", Instructions: "?"},
		"s": {Type: "score", Instructions: "?", Criteria: []any{"l0", "l1", "l2", "l3"}},
		"c": {Type: "choice", Instructions: "?", Criteria: map[string]any{"a": "x", "b": "y"}},
	}, Gold: map[string]any{"n": "yes", "s": 2.0, "c": "a"}}
	if err := it.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := ignatius.Registry{"m": scripted{answer: func(idx int, qid string, q ignatius.Question) ignatius.Answer {
		switch qid {
		case "n":
			return ignatius.Answer{Type: "noul", Noul: fp(0.7)} // yes, right
		case "s":
			return ignatius.Answer{Type: "score", Score: fp(2.4), Probabilities: map[string]float64{"2": 0.6, "3": 0.4}, Confidence: fp(0.5)} // rounds to 2: right
		}
		return choice("b", 0.9) // wrong
	}}}
	rep, err := Run(context.Background(), reg, []Item{it}, []Candidate{{"m", single("m")}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Candidates[0]
	if c.Correct != 2 || c.Judged != 3 || c.ByType["noul"].Correct != 1 || c.ByType["score"].Correct != 1 || c.ByType["choice"].Correct != 0 {
		t.Fatalf("%+v %+v", c, c.ByType)
	}
	if mae := c.ByType["score"].MAE; mae == nil || math.Abs(*mae-0.4) > 1e-9 {
		t.Errorf("a score's mean error: %v", mae)
	}
	if len(c.Misses) != 1 || c.Misses[0].Question != "c" || c.Misses[0].Gold != "a" || c.Misses[0].Got != "b" {
		t.Errorf("misses: %+v", c.Misses)
	}
}

func TestAQuestionWithNoGoldIsAskedButNotJudged(t *testing.T) {
	it := Item{State: "i0", Questions: map[string]ignatius.Question{
		"c":     {Type: "choice", Instructions: "?", Criteria: map[string]any{"a": "x", "b": "y"}},
		"extra": {Type: "noul", Instructions: "?"},
	}, Gold: map[string]any{"c": "a"}}
	if err := it.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := ignatius.Registry{"m": scripted{answer: func(idx int, qid string, q ignatius.Question) ignatius.Answer {
		if q.Type == "noul" {
			return ignatius.Answer{Type: "noul", Noul: fp(0.9)}
		}
		return choice("a", 0.9)
	}}}
	rep, err := Run(context.Background(), reg, []Item{it}, []Candidate{{"m", single("m")}}, Options{})
	if err != nil || rep.Candidates[0].Judged != 1 || rep.Judged != 1 {
		t.Fatalf("%v %+v", err, rep.Candidates[0])
	}
}

func TestAnUnansweredQuestionCountsAsWrong(t *testing.T) {
	reg := ignatius.Registry{"m": scripted{answer: func(idx int, qid string, q ignatius.Question) ignatius.Answer {
		return ignatius.Answer{Type: "choice"} // no choice at all
	}}}
	rep, err := Run(context.Background(), reg, items(4), []Candidate{{"m", single("m")}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Candidates[0]
	if c.Correct != 0 || c.Unanswered != 4 || c.Accuracy != 0 {
		t.Errorf("%+v", c)
	}
}

func TestProgressContextAndPlanErrors(t *testing.T) {
	reg, _ := registry()
	delete(reg, "wobbly")
	var last, calls int
	var mu sync.Mutex
	rep, err := Run(context.Background(), reg, items(10), []Candidate{{"a", single("cheap")}, {"b", single("strong")}}, Options{Progress: func(done, total int) {
		mu.Lock()
		calls++
		last = done
		mu.Unlock()
		if total != 20 {
			t.Errorf("total = %d, want 2 routes x 10 items", total)
		}
	}})
	if err != nil || rep == nil || calls != 20 || last != 20 {
		t.Fatalf("progress: %d calls, last %d, err %v", calls, last, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	slow := ignatius.Registry{"s": scripted{delay: 50 * time.Millisecond, answer: func(int, string, ignatius.Question) ignatius.Answer { return choice("a", 0.9) }}}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := Run(ctx, slow, items(50), []Candidate{{"s", single("s")}}, Options{Concurrency: 2}); err == nil {
		t.Error("a cancelled run must return an error, not a partial report")
	}

	_, err = Run(context.Background(), reg, items(3), []Candidate{{"ghost", single("nope")}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("a plan that cannot run names the route: %v", err)
	}
	if _, err := Run(context.Background(), reg, items(3), []Candidate{{"x", single("cheap")}, {"x", single("strong")}}, Options{}); err == nil {
		t.Error("duplicate route names must be refused")
	}
	if _, err := Run(context.Background(), reg, nil, []Candidate{{"x", single("cheap")}}, Options{}); err == nil {
		t.Error("no items")
	}
}

func TestCalibrationAndFailureAccounting(t *testing.T) {
	reg, _ := registry()
	delete(reg, "wobbly")
	rep, _ := Run(context.Background(), reg, items(40), []Candidate{{"cheap", single("cheap")}}, Options{})
	c := rep.Candidates[0]
	if c.AUROC == nil || *c.AUROC < 0.99 { // the wrong ones are exactly the low-confidence ones
		t.Errorf("confidence separates right from wrong here: %v", c.AUROC)
	}
	if c.ECE == nil || *c.ECE < 0 || *c.ECE > 1 {
		t.Errorf("ece: %v", c.ECE)
	}
	// A model that errors: every item fails, the failures are counted by kind, and nothing is judged right.
	bad := ignatius.Registry{"down": failing{}}
	rep, err := Run(context.Background(), bad, items(5), []Candidate{{"down", single("down")}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c = rep.Candidates[0]
	if c.FailedItems != 5 || c.Failures["http"] != 5 || c.Unanswered != 5 || c.Correct != 0 {
		t.Errorf("%+v", c)
	}
}

type failing struct{}

func (failing) Call(context.Context, ignatius.Request) (ignatius.WireResponse, error) {
	return ignatius.WireResponse{}, &ignatius.CallError{Kind: ignatius.KindHTTP, Message: "503: boom", Status: 503}
}

// A report holds numbers and names, never the request's content.
func TestAReportHoldsNoContent(t *testing.T) {
	reg, _ := registry()
	delete(reg, "wobbly")
	its := items(8)
	for i := range its {
		its[i].State = fmt.Sprintf("i%d SECRET-STATE-%d", i, i)
	}
	// the scripted model reads the index from the front of the state
	rep, err := Run(context.Background(), reg, its, []Candidate{{"cheap", single("cheap")}, {"strong", single("strong")}}, Options{Sweep: false})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rep)
	if strings.Contains(string(b), "SECRET") || strings.Contains(rep.Text(), "SECRET") {
		t.Errorf("content in the report: %s", b)
	}
	if !strings.Contains(rep.Text(), "verdict:") || !strings.Contains(rep.Text(), "miss ") {
		t.Errorf("the text report should have a verdict and misses:\n%s", rep.Text())
	}
}
