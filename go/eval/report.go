package eval

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/phin-tech/ignatius/go/ignatius"
)

// Report is the result of a run. It holds numbers and names, never a request's content: a miss names the
// item, the question and the two answers, not the state.
type Report struct {
	Items      int               `json:"items"`
	Judged     int               `json:"judged_questions"`
	Baseline   string            `json:"baseline"` // the first route: the others are compared with it
	Candidates []CandidateReport `json:"candidates"`
	Sweeps     []Sweep           `json:"sweeps,omitempty"`
}

type CandidateReport struct {
	Name         string               `json:"name"`
	Mode         string               `json:"mode"`
	Judged       int                  `json:"judged"`
	Correct      int                  `json:"correct"`
	Unanswered   int                  `json:"unanswered"` // judged questions with no answer, counted as wrong
	Accuracy     float64              `json:"accuracy"`
	CI95         [2]float64           `json:"ci95"` // Wilson interval on the accuracy
	ByType       map[string]TypeStats `json:"by_type"`
	AnsweredBy   []AnsweredBy         `json:"answered_by"`
	Tiers        []TierStats          `json:"tiers,omitempty"`   // a cascade
	Members      []MemberStats        `json:"members,omitempty"` // a fan-out
	FailedItems  int                  `json:"failed_items"`      // items the plan could not fully answer
	Failures     map[string]int       `json:"failures,omitempty"`
	InputTokens  int                  `json:"input_tokens"`
	OutputTokens int                  `json:"output_tokens"`
	CostUSD      *float64             `json:"cost_usd,omitempty"`
	CostPer1k    *float64             `json:"cost_usd_per_1k_items,omitempty"`
	LatencyP50   float64              `json:"latency_ms_p50"`
	LatencyP95   float64              `json:"latency_ms_p95"`
	ECE          *float64             `json:"ece_top_probability,omitempty"`
	AUROC        *float64             `json:"auroc_confidence,omitempty"`
	Misses       []Miss               `json:"misses,omitempty"`
	VsBaseline   *Comparison          `json:"vs_baseline,omitempty"`
}

type TypeStats struct {
	Judged   int      `json:"judged"`
	Correct  int      `json:"correct"`
	Accuracy float64  `json:"accuracy"`
	MAE      *float64 `json:"mae,omitempty"` // a score's mean distance from the gold level
}

// AnsweredBy says who gave the final answers and how often they were right: for a cascade, whether the
// cheap tier is right when it keeps an answer.
type AnsweredBy struct {
	Model    string  `json:"model"`
	Answers  int     `json:"answers"`
	Correct  int     `json:"correct"`
	Accuracy float64 `json:"accuracy"`
	Share    float64 `json:"share"` // of the judged questions
}

// TierStats is one tier of a cascade, over all items.
type TierStats struct {
	Tier      int    `json:"tier"`
	Model     string `json:"model"`
	Calls     int    `json:"calls"`
	Asked     int    `json:"questions_asked"`
	Escalated int    `json:"escalated"`
	Settled   int    `json:"settled"`
}

// MemberStats is one member of a fan-out judged on its own answers, for comparison with the combined one.
type MemberStats struct {
	Model    string  `json:"model"`
	Judged   int     `json:"judged"`
	Correct  int     `json:"correct"`
	Accuracy float64 `json:"accuracy"`
}

type Miss struct {
	Item       string   `json:"item"`
	Question   string   `json:"question"`
	Gold       string   `json:"gold"`
	Got        string   `json:"got"`
	Model      string   `json:"model,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
}

// Comparison is a route against the baseline on the same items.
type Comparison struct {
	Baseline  string     `json:"baseline"`
	Delta     float64    `json:"delta_accuracy"` // this minus the baseline, as a fraction
	CI95      [2]float64 `json:"delta_ci95"`     // paired bootstrap over items
	CostRatio *float64   `json:"cost_ratio,omitempty"`
	Verdict   string     `json:"verdict"`
}

// Sweep is what a cascade would have done at other thresholds, computed from each tier's solo answers.
type Sweep struct {
	Candidate string     `json:"candidate"`
	Models    []string   `json:"models"`
	Rows      []SweepRow `json:"rows"`
}

type SweepRow struct {
	Threshold float64   `json:"threshold"`
	Accuracy  float64   `json:"accuracy"`
	Reach     []float64 `json:"reach"` // the share of judged questions that got to tier 1, 2, ...
	CostPer1k *float64  `json:"cost_usd_per_1k_items,omitempty"`
}

// SweepThresholds are the bars a sweep tries, applied to every tier but the last.
var SweepThresholds = []float64{0.5, 0.6, 0.7, 0.8, 0.9, 0.95}

const bootstrapResamples = 2000

func buildReport(items []Item, cands []Candidate, results [][]itemResult, solo map[string][]itemResult, opts Options) *Report {
	rep := &Report{Items: len(items), Baseline: cands[0].Name}
	for _, it := range items {
		rep.Judged += len(it.gold)
	}
	perItem := make([][2][]int, len(cands)) // per candidate: judged and correct per item, for the bootstrap
	for ci, c := range cands {
		cr, judgedPer, correctPer := candidateReport(items, c, results[ci], opts)
		perItem[ci] = [2][]int{judgedPer, correctPer}
		rep.Candidates = append(rep.Candidates, cr)
	}
	for ci := 1; ci < len(cands); ci++ {
		cmp := compare(cands[0].Name, perItem[0], perItem[ci], rep.Candidates[0], rep.Candidates[ci], opts.Seed)
		rep.Candidates[ci].VsBaseline = &cmp
	}
	if len(solo) > 0 {
		for _, c := range cands {
			if c.Plan.Mode == ignatius.ModeCascade {
				rep.Sweeps = append(rep.Sweeps, sweep(items, c, solo))
			}
		}
	}
	return rep
}

func candidateReport(items []Item, c Candidate, res []itemResult, opts Options) (CandidateReport, []int, []int) {
	cr := CandidateReport{Name: c.Name, Mode: c.Plan.Mode, ByType: map[string]TypeStats{}, Failures: map[string]int{}}
	judgedPer, correctPer := make([]int, len(items)), make([]int, len(items))
	type tally struct{ answers, correct int }
	by := map[string]*tally{}
	typeObs := map[string][]observation{}
	var all []observation
	tiers := map[int]*TierStats{}
	members := map[string]*MemberStats{}
	var lat []float64
	var costSum float64
	priced := false
	for i, r := range res {
		lat = append(lat, float64(r.wall.Microseconds())/1000)
		if !r.routed.OK {
			cr.FailedItems++
		}
		for _, f := range r.routed.Failures {
			cr.Failures[f.Error.Kind]++
		}
		for _, rr := range r.routed.Results {
			cr.InputTokens += rr.Usage.InputTokens
			cr.OutputTokens += rr.Usage.OutputTokens
		}
		if r.routed.CostUSD != nil {
			priced = true
			costSum += *r.routed.CostUSD
		}
		for _, h := range r.routed.Trace {
			t := tiers[h.Tier]
			if t == nil {
				t = &TierStats{Tier: h.Tier, Model: h.Model}
				tiers[h.Tier] = t
			}
			t.Calls++
			t.Asked += len(h.Questions)
			t.Escalated += len(h.Escalated)
			t.Settled += len(h.Questions) - len(h.Escalated)
		}
		for _, qid := range sortedKeys(r.obs) {
			o := r.obs[qid]
			cr.Judged++
			judgedPer[i]++
			ts := cr.ByType[o.qtype]
			ts.Judged++
			if o.correct {
				cr.Correct++
				correctPer[i]++
				ts.Correct++
			}
			cr.ByType[o.qtype] = ts
			typeObs[o.qtype] = append(typeObs[o.qtype], o)
			all = append(all, o)
			if !o.answered {
				cr.Unanswered++
				continue
			}
			t := by[o.model]
			if t == nil {
				t = &tally{}
				by[o.model] = t
			}
			t.answers++
			if o.correct {
				t.correct++
			}
			if !o.correct && (opts.Misses < 0 || len(cr.Misses) < opts.Misses) && opts.Misses != 0 {
				cr.Misses = append(cr.Misses, Miss{Item: itemName(items[i], i), Question: qid, Gold: o.gold, Got: o.got, Model: o.model, Confidence: o.confidence})
			}
		}
		for model, obs := range r.memberObs {
			m := members[model]
			if m == nil {
				m = &MemberStats{Model: model}
				members[model] = m
			}
			for _, o := range obs {
				m.Judged++
				if o.correct {
					m.Correct++
				}
			}
		}
	}
	cr.Accuracy = ratio(cr.Correct, cr.Judged)
	lo, hi := wilson(cr.Correct, cr.Judged)
	cr.CI95 = [2]float64{lo, hi}
	for typ, ts := range cr.ByType {
		ts.Accuracy = ratio(ts.Correct, ts.Judged)
		if typ == "score" {
			var sum float64
			n := 0
			for _, o := range typeObs[typ] {
				if o.absErr != nil {
					sum += *o.absErr
					n++
				}
			}
			if n > 0 {
				mae := sum / float64(n)
				ts.MAE = &mae
			}
		}
		cr.ByType[typ] = ts
	}
	for _, model := range sortedKeys(by) {
		t := by[model]
		cr.AnsweredBy = append(cr.AnsweredBy, AnsweredBy{Model: model, Answers: t.answers, Correct: t.correct,
			Accuracy: ratio(t.correct, t.answers), Share: ratio(t.answers, cr.Judged)})
	}
	for _, idx := range sortedIntKeys(tiers) {
		cr.Tiers = append(cr.Tiers, *tiers[idx])
	}
	for _, model := range sortedKeys(members) {
		m := members[model]
		m.Accuracy = ratio(m.Correct, m.Judged)
		cr.Members = append(cr.Members, *m)
	}
	if len(cr.Failures) == 0 {
		cr.Failures = nil
	}
	if priced {
		cr.CostUSD = &costSum
		per1k := costSum / float64(len(items)) * 1000
		cr.CostPer1k = &per1k
	}
	sort.Float64s(lat)
	cr.LatencyP50, cr.LatencyP95 = percentile(lat, 0.5), percentile(lat, 0.95)
	cr.ECE = ece(all)
	cr.AUROC = auroc(all)
	return cr, judgedPer, correctPer
}

func itemName(it Item, i int) string {
	if it.ID != "" {
		return it.ID
	}
	return fmt.Sprintf("#%d", i+1)
}

func sortedIntKeys[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(math.Round(p*float64(len(sorted)-1)))]
}

// wilson is the 95% Wilson score interval for k of n.
func wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.96
	p := float64(k) / float64(n)
	d := 1 + z*z/float64(n)
	c := p + z*z/(2*float64(n))
	m := z * math.Sqrt(p*(1-p)/float64(n)+z*z/(4*float64(n)*float64(n)))
	return (c - m) / d, (c + m) / d
}

// ece is the expected calibration error of the top probability over the answered questions: the gap between
// how sure the model says it is and how often it is right (10 bins; lower is better).
func ece(obs []observation) *float64 {
	var rs []observation
	for _, o := range obs {
		if o.answered && o.pTop != nil {
			rs = append(rs, o)
		}
	}
	if len(rs) == 0 {
		return nil
	}
	const bins = 10
	var total float64
	for b := 0; b < bins; b++ {
		lo, hi := float64(b)/bins, float64(b+1)/bins
		var n, hits int
		var sum float64
		for _, o := range rs {
			p := *o.pTop
			if (p >= lo && p < hi) || (b == bins-1 && p >= 1) {
				n++
				sum += p
				if o.correct {
					hits++
				}
			}
		}
		if n > 0 {
			total += float64(n) / float64(len(rs)) * math.Abs(float64(hits)/float64(n)-sum/float64(n))
		}
	}
	return &total
}

// auroc is the chance a right answer has a higher confidence than a wrong one: 0.5 means the confidence says
// nothing about correctness, 1 means it separates them perfectly.
func auroc(obs []observation) *float64 {
	var pos, neg []float64
	for _, o := range obs {
		if !o.answered || o.confidence == nil {
			continue
		}
		if o.correct {
			pos = append(pos, *o.confidence)
		} else {
			neg = append(neg, *o.confidence)
		}
	}
	if len(pos) == 0 || len(neg) == 0 {
		return nil
	}
	var wins float64
	for _, p := range pos {
		for _, n := range neg {
			switch {
			case p > n:
				wins++
			case p == n:
				wins += 0.5
			}
		}
	}
	v := wins / float64(len(pos)*len(neg))
	return &v
}

// compare judges a route against the baseline on the same items: the accuracy difference with a paired
// bootstrap interval (items are resampled, so the pairing and the clustering of questions within an item are
// respected), the cost ratio, and a one-line verdict.
func compare(base string, a, b [2][]int, baseR, candR CandidateReport, seed int64) Comparison {
	n := len(a[0])
	rng := rand.New(rand.NewSource(seed))
	deltas := make([]float64, 0, bootstrapResamples)
	for r := 0; r < bootstrapResamples; r++ {
		var judged, ca, cb int
		for k := 0; k < n; k++ {
			i := rng.Intn(n)
			judged += a[0][i]
			ca += a[1][i]
			cb += b[1][i]
		}
		if judged > 0 {
			deltas = append(deltas, float64(cb-ca)/float64(judged))
		}
	}
	sort.Float64s(deltas)
	cmp := Comparison{Baseline: base, Delta: candR.Accuracy - baseR.Accuracy}
	if len(deltas) > 0 {
		cmp.CI95 = [2]float64{percentile(deltas, 0.025), percentile(deltas, 0.975)}
	}
	if baseR.CostUSD != nil && candR.CostUSD != nil && *baseR.CostUSD > 0 {
		r := *candR.CostUSD / *baseR.CostUSD
		cmp.CostRatio = &r
	}
	cmp.Verdict = verdict(cmp)
	return cmp
}

func verdict(c Comparison) string {
	pts := func(x float64) string { return fmt.Sprintf("%.1f", x*100) }
	signed := func(x float64) string { return fmt.Sprintf("%+.1f", x*100) }
	var head string
	switch {
	case c.CI95[0] > 0: // the direction is in the words, so the numbers are magnitudes
		head = fmt.Sprintf("better than %s by %s points (95%% CI %s to %s)", c.Baseline, pts(c.Delta), pts(c.CI95[0]), pts(c.CI95[1]))
	case c.CI95[1] < 0:
		head = fmt.Sprintf("worse than %s by %s points (95%% CI %s to %s)", c.Baseline, pts(-c.Delta), pts(-c.CI95[1]), pts(-c.CI95[0]))
	default:
		head = fmt.Sprintf("no detectable difference from %s (%s points, 95%% CI %s to %s)", c.Baseline, signed(c.Delta), signed(c.CI95[0]), signed(c.CI95[1]))
	}
	if c.CostRatio != nil {
		head += fmt.Sprintf(", at %.0f%% of its cost", *c.CostRatio*100)
	}
	return head
}

// sweep replays a cascade at other thresholds from each tier's solo answers: no new calls, and the same rule
// as the cascade route (a tier keeps an answer when its confidence reaches the bar; the last tier answers
// what is left). One threshold is applied to every tier but the last.
func sweep(items []Item, c Candidate, solo map[string][]itemResult) Sweep {
	sw := Sweep{Candidate: c.Name}
	for _, t := range c.Plan.Tiers {
		sw.Models = append(sw.Models, t.Model)
	}
	k := len(sw.Models)
	for _, thr := range SweepThresholds {
		var judged, correct int
		reach := make([]int, k)
		var cost float64
		anyCost := false
		for i, it := range items {
			qids := sortedKeys(it.gold)
			if len(qids) == 0 {
				continue
			}
			pending := qids
			for tier, model := range sw.Models {
				res := solo[model][i]
				if tier > 0 {
					reach[tier] += len(pending)
				}
				if res.routed.CostUSD != nil {
					anyCost = true
					cost += *res.routed.CostUSD * float64(len(pending)) / float64(len(qids))
				}
				var next []string
				for _, qid := range pending {
					a, ok := res.routed.Answers[qid]
					last := tier == k-1
					if ok && (last || (a.Confidence != nil && *a.Confidence >= thr)) {
						judged++
						if judge(it.Questions[qid], it.gold[qid], a).correct {
							correct++
						}
						continue
					}
					if last {
						judged++ // unanswered at the last tier: counted, as wrong
						continue
					}
					next = append(next, qid)
				}
				pending = next
				if len(pending) == 0 {
					break
				}
			}
		}
		row := SweepRow{Threshold: thr, Accuracy: ratio(correct, judged)}
		for tier := 1; tier < k; tier++ {
			row.Reach = append(row.Reach, ratio(reach[tier], judged))
		}
		if anyCost {
			per1k := cost / float64(len(items)) * 1000
			row.CostPer1k = &per1k
		}
		sw.Rows = append(sw.Rows, row)
	}
	return sw
}

// Text renders the report as tables for a terminal.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d items, %d judged questions; each route is compared with %s\n\n", r.Items, r.Judged, r.Baseline)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "route\tmode\taccuracy\t95% CI\tunanswered\tp50 ms\tp95 ms\t$/1k items\tECE\tAUROC")
	for _, c := range r.Candidates {
		cost, ece, au := "-", "-", "-"
		if c.CostPer1k != nil {
			cost = fmt.Sprintf("%.4f", *c.CostPer1k)
		}
		if c.ECE != nil {
			ece = fmt.Sprintf("%.1f%%", *c.ECE*100)
		}
		if c.AUROC != nil {
			au = fmt.Sprintf("%.2f", *c.AUROC)
		}
		fmt.Fprintf(tw, "%s\t%s\t%.1f%% (%d/%d)\t%.0f-%.0f%%\t%d\t%.0f\t%.0f\t%s\t%s\t%s\n", c.Name, c.Mode, c.Accuracy*100, c.Correct, c.Judged,
			c.CI95[0]*100, c.CI95[1]*100, c.Unanswered, c.LatencyP50, c.LatencyP95, cost, ece, au)
	}
	tw.Flush()
	for _, c := range r.Candidates {
		fmt.Fprintf(&b, "\n%s\n", c.Name)
		if c.VsBaseline != nil {
			fmt.Fprintf(&b, "  verdict: %s\n", c.VsBaseline.Verdict)
		}
		if c.FailedItems > 0 {
			fmt.Fprintf(&b, "  %d items were not fully answered; failures: %v\n", c.FailedItems, c.Failures)
		}
		for _, typ := range sortedKeys(c.ByType) {
			ts := c.ByType[typ]
			mae := ""
			if ts.MAE != nil {
				mae = fmt.Sprintf(", mean error %.2f levels", *ts.MAE)
			}
			fmt.Fprintf(&b, "  %-7s %d questions, %.1f%% correct%s\n", typ, ts.Judged, ts.Accuracy*100, mae)
		}
		for _, a := range c.AnsweredBy {
			fmt.Fprintf(&b, "  answered by %-14s %5.1f%% of questions, %.1f%% of those correct\n", a.Model, a.Share*100, a.Accuracy*100)
		}
		for _, t := range c.Tiers {
			fmt.Fprintf(&b, "  tier %d %-14s asked %d, settled %d, escalated %d\n", t.Tier, t.Model, t.Asked, t.Settled, t.Escalated)
		}
		for _, m := range c.Members {
			fmt.Fprintf(&b, "  member %-14s on its own: %.1f%% correct (%d/%d)\n", m.Model, m.Accuracy*100, m.Correct, m.Judged)
		}
		for _, m := range c.Misses {
			conf := ""
			if m.Confidence != nil {
				conf = fmt.Sprintf(" (confidence %.2f)", *m.Confidence)
			}
			fmt.Fprintf(&b, "  miss %s/%s: gold %s, got %s from %s%s\n", m.Item, m.Question, m.Gold, m.Got, m.Model, conf)
		}
	}
	for _, s := range r.Sweeps {
		fmt.Fprintf(&b, "\nsweep of %s (%s): one threshold on every tier but the last\n", s.Candidate, strings.Join(s.Models, " > "))
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "threshold\taccuracy\treached later tiers\t$/1k items")
		for _, row := range s.Rows {
			reach := make([]string, len(row.Reach))
			for i, x := range row.Reach {
				reach[i] = fmt.Sprintf("%.0f%%", x*100)
			}
			cost := "-"
			if row.CostPer1k != nil {
				cost = fmt.Sprintf("%.4f", *row.CostPer1k)
			}
			fmt.Fprintf(tw, "%.2f\t%.1f%%\t%s\t%s\n", row.Threshold, row.Accuracy*100, strings.Join(reach, " / "), cost)
		}
		tw.Flush()
	}
	return b.String()
}
