package eval

import (
	"os"
	"strings"
	"testing"
)

// The sample test set shipped in examples/eval is documentation people run: it must keep parsing and
// keep covering what its README says it covers.
func TestTheSampleSupportTriageSet(t *testing.T) {
	f, err := os.Open("../../examples/eval/support-triage.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	items, err := ParseJSONL(f, 0)
	if err != nil {
		t.Fatalf("the sample set must be valid: %v", err)
	}
	if len(items) < 50 {
		t.Errorf("only %d items", len(items))
	}
	ids := map[string]bool{}
	depts := map[string]int{}
	var refundYes, refundNo, churnYes, churnNo int
	urgency := map[float64]int{}
	for _, it := range items {
		if it.ID == "" || ids[it.ID] {
			t.Errorf("item ids must be present and unique: %q", it.ID)
		}
		ids[it.ID] = true
		for _, q := range []string{"department", "refund", "churn", "urgency"} {
			if _, ok := it.gold[q]; !ok {
				t.Errorf("%s has no gold for %s", it.ID, q)
			}
		}
		depts[it.gold["department"].text]++
		if it.gold["refund"].yes {
			refundYes++
		} else {
			refundNo++
		}
		if it.gold["churn"].yes {
			churnYes++
		} else {
			churnNo++
		}
		urgency[it.gold["urgency"].level]++
		if !strings.HasPrefix(it.ID, it.gold["department"].text+"-") {
			t.Errorf("%s: the id should start with its department", it.ID)
		}
	}
	for _, d := range []string{"billing", "technical", "sales", "account"} {
		if depts[d] < 8 {
			t.Errorf("department %s has %d items", d, depts[d])
		}
	}
	if refundYes < 4 || refundNo < 4 || churnYes < 4 || churnNo < 4 {
		t.Errorf("both answers to each yes/no question must appear: refund %d/%d churn %d/%d", refundYes, refundNo, churnYes, churnNo)
	}
	for lvl := 0.0; lvl <= 3; lvl++ {
		if urgency[lvl] == 0 {
			t.Errorf("no item has urgency level %v", lvl)
		}
	}
}
