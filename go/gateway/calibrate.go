package gateway

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/phin-tech/ignatius/go/store"
)

// CalibrationThresholds are the bars Calibrate sweeps.
var CalibrationThresholds = []float64{0.5, 0.6, 0.7, 0.8, 0.85, 0.9, 0.95}

// CalPoint is one threshold's outcome for a model and question type: among the answers at
// or above the bar (the ones a cascade tier would keep), how many were judged good, and
// what share of all labeled answers that is. 1 - Coverage is the escalation rate.
type CalPoint struct {
	Threshold float64 `json:"threshold"`
	Kept      int     `json:"kept"`
	Accuracy  float64 `json:"accuracy"` // good / kept; 0 when nothing is kept
	Coverage  float64 `json:"coverage"`
}

// CalRow is the sweep for one (model, question type).
type CalRow struct {
	Model       string     `json:"model"`
	Type        string     `json:"type"`
	Labeled     int        `json:"labeled"`  // feedback rows with a confidence
	Accuracy    float64    `json:"accuracy"` // good / labeled, no threshold
	Points      []CalPoint `json:"points"`
	Recommended *float64   `json:"recommended"` // lowest bar that reaches the target with enough kept answers
}

// AuditFeedback turns shadow audits (SPEC 13.7) into feedback rows so Calibrate can use them: a
// tier that agreed with the next tier counts as right, one that disagreed as wrong. It is a
// weaker label than a person's (the next tier can be the wrong one) and ranks below it when both
// exist for a question.
func AuditFeedback(as []store.Audit) []store.Feedback {
	out := make([]store.Feedback, 0, len(as))
	for _, a := range as {
		v := "bad"
		if a.Agreed {
			v = "good"
		}
		out = append(out, store.Feedback{RequestID: a.RequestID, QuestionID: a.QuestionID, Owner: a.Client, Verdict: v,
			Source: "audit", Type: a.Type, Model: a.Model, Confidence: a.Confidence, Threshold: a.Threshold, At: a.At})
	}
	return out
}

// Calibrate sweeps thresholds over feedback. A verdict of "good" counts as a correct
// answer, "bad" as a wrong one. Rows without a confidence (noul answers carry none) cannot
// be placed against a bar and are left out. target is the accuracy a kept answer should
// reach; a bar is only recommended once at least minKept answers clear it.
func Calibrate(fbs []store.Feedback, target float64, minKept int) []CalRow {
	type key struct{ model, typ string }
	groups := map[key][]store.Feedback{}
	for _, f := range store.Dedupe(fbs) { // a decision rated by several clients counts once
		if f.Confidence == nil {
			continue
		}
		k := key{f.Model, f.Type}
		groups[k] = append(groups[k], f)
	}
	var rows []CalRow
	for k, g := range groups {
		row := CalRow{Model: k.model, Type: k.typ, Labeled: len(g)}
		good := 0
		for _, f := range g {
			if f.Verdict == "good" {
				good++
			}
		}
		row.Accuracy = float64(good) / float64(len(g))
		for _, t := range CalibrationThresholds {
			kept, keptGood := 0, 0
			for _, f := range g {
				if *f.Confidence >= t {
					kept++
					if f.Verdict == "good" {
						keptGood++
					}
				}
			}
			p := CalPoint{Threshold: t, Kept: kept, Coverage: float64(kept) / float64(len(g))}
			if kept > 0 {
				p.Accuracy = float64(keptGood) / float64(kept)
			}
			row.Points = append(row.Points, p)
			if row.Recommended == nil && kept >= minKept && p.Accuracy >= target {
				v := t
				row.Recommended = &v
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].Type < rows[j].Type
	})
	return rows
}

// WriteCalibration prints the sweep as tables, one per model and question type.
func WriteCalibration(w io.Writer, rows []CalRow, target float64, minKept int) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no feedback with a confidence yet")
		return
	}
	for _, r := range rows {
		fmt.Fprintf(w, "\n%s / %s: %d labeled, %.0f%% good overall\n", r.Model, r.Type, r.Labeled, r.Accuracy*100)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "threshold\tkept\taccuracy\tescalated")
		for _, p := range r.Points {
			acc := "-"
			if p.Kept > 0 {
				acc = fmt.Sprintf("%.0f%%", p.Accuracy*100)
			}
			fmt.Fprintf(tw, "%.2f\t%d\t%s\t%.0f%%\n", p.Threshold, p.Kept, acc, (1-p.Coverage)*100)
		}
		_ = tw.Flush()
		if r.Recommended != nil {
			fmt.Fprintf(w, "lowest bar reaching %.0f%% accuracy with at least %d kept: %.2f\n", target*100, minKept, *r.Recommended)
		} else {
			fmt.Fprintf(w, "no bar reaches %.0f%% accuracy with at least %d kept answers (more feedback, or escalate this model)\n", target*100, minKept)
		}
	}
}
