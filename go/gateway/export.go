package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/phin-tech/ignatius/go/ignatius"
	"github.com/phin-tech/ignatius/go/store"
)

// ExportJSONL writes one JSON line per question of every stored request of client since
// the given time (SPEC 13.5): the state, the question, the final answer, every model's
// answer including tiers that did not supply the final one (the probabilities are soft
// labels for a student), and the latest feedback. Only an opted-in client has content,
// so only its requests appear.
func ExportJSONL(ctx context.Context, st store.Store, client string, since time.Time, w io.Writer) (int, error) {
	enc := json.NewEncoder(w)
	n := 0
	err := st.Export(ctx, store.ExportFilter{Client: client, Since: since}, func(r store.ExportRow) error {
		var questions map[string]json.RawMessage
		var answers map[string]json.RawMessage
		var results []ignatius.Result
		if err := json.Unmarshal(r.Content.Questions, &questions); err != nil {
			return fmt.Errorf("request %s: stored questions: %w", r.RequestID, err)
		}
		if err := json.Unmarshal(r.Content.Answers, &answers); err != nil {
			return fmt.Errorf("request %s: stored answers: %w", r.RequestID, err)
		}
		if err := json.Unmarshal(r.Content.Results, &results); err != nil {
			return fmt.Errorf("request %s: stored results: %w", r.RequestID, err)
		}
		type modelAnswer struct {
			Model  string          `json:"model"`
			Answer ignatius.Answer `json:"answer"`
		}
		var per []modelAnswer
		for _, res := range results {
			if a, ok := res.Answers[r.QuestionID]; ok {
				per = append(per, modelAnswer{res.Model, a})
			}
		}
		line := map[string]any{
			"request_id": r.RequestID, "question_id": r.QuestionID, "at": r.At.Format(time.RFC3339),
			"route": r.Route, "mode": r.Mode,
			// The images are not stored, so a row whose question was about one has no input a
			// trainer can see. This count says so: skip rows where it is above zero.
			"images":   r.ImageCount,
			"state":    json.RawMessage(r.Content.State),
			"question": questions[r.QuestionID],
			"answer":   answers[r.QuestionID],
			"model":    r.Outcome.Model, "confidence": r.Outcome.Confidence, "threshold": r.Outcome.Threshold,
			"model_answers": per, "feedback": nil,
		}
		fbJSON := func(f store.Feedback) map[string]any {
			fb := map[string]any{"verdict": f.Verdict, "source": f.Source, "client": f.Client, "at": f.At.Format(time.RFC3339)}
			if f.Correct != nil {
				fb["correct"] = json.RawMessage(f.Correct)
			}
			return fb
		}
		// "feedback" is the verdict that stands (a human's over an automated one, then the
		// owner's, then the newest); "all_feedback" has every client's, for a trainer that
		// wants to weigh disagreement itself.
		if f := r.Feedback; f != nil {
			line["feedback"] = fbJSON(*f)
			all := make([]map[string]any, 0, len(r.AllFeedback))
			for _, a := range r.AllFeedback {
				all = append(all, fbJSON(a))
			}
			line["all_feedback"] = all
		}
		n++
		return enc.Encode(line)
	})
	return n, err
}
