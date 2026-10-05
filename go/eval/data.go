// Package eval runs a labeled test set through routes (cascades, fan-outs, single models, inline
// routes) and judges what comes back (SPEC 15): accuracy with a confidence interval, what each
// tier of a cascade or member of a fan-out contributed, cost and latency, whether confidence tracks
// correctness, a paired comparison of every route against the first, and for cascades a threshold
// sweep. It runs the real plans through the real library, so what it measures is what production does.
package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/phin-tech/ignatius/go/ignatius"
)

// Item is one labeled example: a request and, per question, the answer it should get.
//
//	{"id": "t17", "state": "I was charged twice", "questions": {"billing": {"type": "noul", "instructions": "..."}},
//	 "gold": {"billing": true}}
//
// Gold is by question id, and a question with no gold is asked but not judged. A choice's gold is
// an option; a noul's is true or false ("yes" and "no" also work); a score's is a level index.
type Item struct {
	ID        string                       `json:"id,omitempty"`
	State     any                          `json:"state"`
	Images    []string                     `json:"images,omitempty"`
	Questions map[string]ignatius.Question `json:"questions"`
	Gold      map[string]any               `json:"gold"`

	gold map[string]goldValue // Gold, checked and normalized by Validate
}

type goldValue struct {
	text  string  // choice
	yes   bool    // noul
	level float64 // score
}

// Request is what is sent for the item.
func (it Item) Request() ignatius.Request {
	return ignatius.Request{State: it.State, Images: it.Images, Questions: it.Questions}
}

// Validate checks the item and normalizes its gold. It must be called before the item is judged.
func (it *Item) Validate() error {
	if err := it.Request().Validate(); err != nil {
		return err
	}
	it.gold = map[string]goldValue{}
	for qid, raw := range it.Gold {
		q, ok := it.Questions[qid]
		if !ok {
			return fmt.Errorf("gold names question %q, which the item does not ask", qid)
		}
		g, err := normalizeGold(q, raw)
		if err != nil {
			return fmt.Errorf("question %q: %w", qid, err)
		}
		it.gold[qid] = g
	}
	return nil
}

func normalizeGold(q ignatius.Question, raw any) (goldValue, error) {
	switch q.Type {
	case "choice":
		s, ok := raw.(string)
		if !ok {
			return goldValue{}, fmt.Errorf("the gold answer to a choice is an option, a string, got %v", raw)
		}
		switch opts := q.Criteria.(type) {
		case map[string]any:
			if _, found := opts[s]; !found {
				return goldValue{}, fmt.Errorf("gold %q is not one of the question's options", s)
			}
		case []any:
			found := false
			for _, o := range opts {
				found = found || o == s
			}
			if !found {
				return goldValue{}, fmt.Errorf("gold %q is not one of the question's options", s)
			}
		}
		return goldValue{text: s}, nil
	case "noul":
		switch v := raw.(type) {
		case bool:
			return goldValue{yes: v}, nil
		case float64:
			if v == 0 || v == 1 {
				return goldValue{yes: v == 1}, nil
			}
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "yes", "true":
				return goldValue{yes: true}, nil
			case "no", "false":
				return goldValue{}, nil
			}
		}
		return goldValue{}, fmt.Errorf("the gold answer to a noul is true or false (or yes or no), got %v", raw)
	case "score":
		n, ok := raw.(float64)
		if !ok || n != math.Trunc(n) {
			return goldValue{}, fmt.Errorf("the gold answer to a score is a whole level index, got %v", raw)
		}
		if levels, ok := q.Criteria.([]any); ok && (n < 0 || n > float64(len(levels)-1)) {
			return goldValue{}, fmt.Errorf("gold level %v is outside the question's %d levels", n, len(levels))
		}
		return goldValue{level: n}, nil
	}
	return goldValue{}, fmt.Errorf("unknown question type %q", q.Type)
}

// ParseJSONL reads one Item per line (a JSON array of items is accepted too). Blank lines are skipped. An
// error names the line. maxItems bounds the dataset (0 = unbounded).
func ParseJSONL(r io.Reader, maxItems int) ([]Item, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var items []Item
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("the dataset is not a JSON array of items: %w", err)
		}
		for i := range items {
			if err := items[i].Validate(); err != nil {
				return nil, fmt.Errorf("item %d: %w", i+1, err)
			}
		}
	} else {
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for line := 1; sc.Scan(); line++ {
			text := bytes.TrimSpace(sc.Bytes())
			if len(text) == 0 {
				continue
			}
			var it Item
			dec := json.NewDecoder(bytes.NewReader(text))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&it); err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			if err := it.Validate(); err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			items = append(items, it)
			if maxItems > 0 && len(items) > maxItems {
				return nil, fmt.Errorf("more than %d items", maxItems)
			}
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	if maxItems > 0 && len(items) > maxItems {
		return nil, fmt.Errorf("more than %d items", maxItems)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("the dataset has no items")
	}
	judged := 0
	for _, it := range items {
		judged += len(it.gold)
	}
	if judged == 0 {
		return nil, fmt.Errorf("no item has a gold answer, so there is nothing to judge")
	}
	return items, nil
}
