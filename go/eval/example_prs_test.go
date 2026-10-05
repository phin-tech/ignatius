package eval

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The sample GitHub PR set is documentation people run, and third-party text: it must keep parsing, keep the
// coverage its README claims, and keep what the builder promises to remove out of it.
func TestTheSampleGitHubPRSet(t *testing.T) {
	f, err := os.Open("../../examples/eval/github-prs/prs.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	items, err := ParseJSONL(f, 0)
	if err != nil {
		t.Fatalf("the sample set must be valid: %v", err)
	}
	if len(items) < 70 {
		t.Errorf("only %d items", len(items))
	}
	email := regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.-]+`)
	mention := regexp.MustCompile(`(^|[^\w/])@[A-Za-z0-9][\w-]*`)
	prefix := regexp.MustCompile(`^\w+(\([^)]*\))?!?:`)
	types := map[string]int{}
	tests := map[bool]int{}
	size := map[float64]int{}
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.ID] || !regexp.MustCompile(`^(vuejs-core|vitejs-vite)-\d+$`).MatchString(it.ID) {
			t.Errorf("item id %q must be unique and name a repository and a PR number", it.ID)
		}
		seen[it.ID] = true
		b, _ := json.Marshal(it.State)
		text := string(b)
		if email.MatchString(text) {
			t.Errorf("%s: an email address is in the text", it.ID)
		}
		for _, m := range mention.FindAllString(text, -1) {
			// @user is the placeholder; @default and @use are CSS at-rules, @experimental is a JSDoc tag
			if !strings.Contains(m, "@user") && !strings.Contains(m, "@default") && !strings.Contains(m, "@use") && !strings.Contains(m, "@experimental") {
				t.Errorf("%s: an @mention is in the text: %q", it.ID, m)
			}
		}
		state := it.State.(map[string]any)
		title, _ := state["title"].(string)
		if prefix.MatchString(title) {
			t.Errorf("%s: the title still carries its commit prefix, which is the answer: %q", it.ID, title)
		}
		desc, _ := state["description"].(string)
		if len(desc) < 80 || strings.Contains(strings.ToLower(desc), "coderabbit") || strings.Contains(desc, "<!--") {
			t.Errorf("%s: the description should be the author's text, at least 80 characters, with no bot summary or comments", it.ID)
		}
		if files, _ := state["changed_files"].([]any); len(files) == 0 || len(files) > 25 {
			t.Errorf("%s: %d listed files", it.ID, len(files))
		}
		types[it.gold["type"].text]++
		tests[it.gold["touches_tests"].yes]++
		size[it.gold["size"].level]++
	}
	for _, ty := range []string{"feature", "bugfix", "performance", "refactor", "docs", "tests", "chore"} {
		if types[ty] < 8 {
			t.Errorf("type %s has %d items", ty, types[ty])
		}
	}
	if tests[true] < 20 || tests[false] < 20 {
		t.Errorf("touches_tests should be balanced: %v", tests)
	}
	for lvl := 0.0; lvl <= 3; lvl++ {
		if size[lvl] < 8 {
			t.Errorf("size level %v has %d items", lvl, size[lvl])
		}
	}
}
