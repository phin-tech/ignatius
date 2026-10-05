package eval

import (
	"strings"
	"testing"
)

const q = `"questions":{"c":{"type":"choice","instructions":"?","criteria":{"a":"x","b":"y"}},"n":{"type":"noul","instructions":"?"},"s":{"type":"score","instructions":"?","criteria":["l0","l1","l2"]}}`

func TestParseJSONLAcceptsEveryGoldForm(t *testing.T) {
	data := `{"id":"one","state":"hello",` + q + `,"gold":{"c":"a","n":true,"s":2}}

{"state":{"k":"v"},` + q + `,"gold":{"n":"No"}}
{"state":"x",` + q + `,"gold":{"n":1,"c":"b","s":0}}
{"state":"y",` + q + `,"gold":{"n":"yes"}}
`
	items, err := ParseJSONL(strings.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 || items[0].ID != "one" || items[0].gold["c"].text != "a" || !items[0].gold["n"].yes || items[0].gold["s"].level != 2 {
		t.Fatalf("items: %+v", items)
	}
	if items[1].gold["n"].yes || !items[2].gold["n"].yes || !items[3].gold["n"].yes {
		t.Error("noul gold forms: true, 1, yes are yes; no is no")
	}
	if items[1].Request().State == nil || len(items[1].Request().Questions) != 3 {
		t.Error("an item makes its request")
	}
}

func TestParseJSONLAcceptsAnArray(t *testing.T) {
	items, err := ParseJSONL(strings.NewReader(`[{"state":"a",`+q+`,"gold":{"c":"a"}},{"state":"b",`+q+`,"gold":{"c":"b"}}]`), 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("%v %v", items, err)
	}
}

func TestParseJSONLErrorsNameTheLineAndTheProblem(t *testing.T) {
	good := `{"state":"ok",` + q + `,"gold":{"c":"a"}}`
	for name, c := range map[string]struct{ data, want string }{
		"bad json":               {good + "\n{not json}\n", "line 2"},
		"an unknown field":       {good + "\n" + `{"state":"x","gold_answer":{},` + q + `}`, "line 2"},
		"gold for no question":   {`{"state":"x",` + q + `,"gold":{"zzz":"a"}}`, `question "zzz"`},
		"a choice not an option": {`{"state":"x",` + q + `,"gold":{"c":"nope"}}`, "not one of the question's options"},
		"a choice gold not text": {`{"state":"x",` + q + `,"gold":{"c":3}}`, "an option"},
		"a noul gold of 5":       {`{"state":"x",` + q + `,"gold":{"n":5}}`, "true or false"},
		"a noul gold of maybe":   {`{"state":"x",` + q + `,"gold":{"n":"maybe"}}`, "true or false"},
		"a score gold of 1.5":    {`{"state":"x",` + q + `,"gold":{"s":1.5}}`, "whole level"},
		"a score gold too high":  {`{"state":"x",` + q + `,"gold":{"s":3}}`, "outside"},
		"a score gold as text":   {`{"state":"x",` + q + `,"gold":{"s":"two"}}`, "whole level"},
		"no questions":           {`{"state":"x","questions":{},"gold":{}}`, "at least one question"},
		"an empty image":         {`{"state":"x","images":[""],` + q + `,"gold":{"c":"a"}}`, "images[0]"},
		"no gold anywhere":       {`{"state":"x",` + q + `}`, "nothing to judge"},
		"nothing at all":         {"\n\n", "no items"},
	} {
		_, err := ParseJSONL(strings.NewReader(c.data), 0)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error mentioning %q, got %v", name, c.want, err)
		}
	}
}

func TestParseJSONLBoundsTheDataset(t *testing.T) {
	line := `{"state":"x",` + q + `,"gold":{"c":"a"}}` + "\n"
	if _, err := ParseJSONL(strings.NewReader(strings.Repeat(line, 3)), 3); err != nil {
		t.Errorf("exactly at the limit: %v", err)
	}
	if _, err := ParseJSONL(strings.NewReader(strings.Repeat(line, 4)), 3); err == nil || !strings.Contains(err.Error(), "more than 3") {
		t.Errorf("over the limit: %v", err)
	}
}
