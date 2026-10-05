package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// backends runs a test against SQLite always, and against Postgres when
// IGNATIUS_TEST_POSTGRES_DSN points at a scratch database.
func backends(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Run("sqlite", func(t *testing.T) {
		s, err := Open(DriverSQLite, filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		fn(t, s)
	})
	if dsn := os.Getenv("IGNATIUS_TEST_POSTGRES_DSN"); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			s, err := Open(DriverPostgres, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// A shared database keeps rows between tests (SQLite gets a fresh file): clear it.
			if _, err := s.Purge(context.Background(), PurgeFilter{Before: time.Now().Add(1000 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			fn(t, s)
		})
	}
}

func f64(v float64) *float64 { return &v }

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func sample(id, client string, at time.Time, content bool) Request {
	r := Request{ID: id, Client: client, Layer: "L1", Route: "fast", Mode: "cascade", OK: true, At: at,
		Outcomes: []Outcome{{QuestionID: "q", Type: "choice", Model: "jeff", Confidence: f64(0.9), Threshold: f64(0.8)}}}
	if content {
		r.Content = &Content{State: []byte(`"hello"`), Questions: []byte(`{"q":{"type":"choice"}}`),
			Answers: []byte(`{"q":{"choice":"a"}}`), Results: []byte(`[]`), Plan: []byte(`{"mode":"single"}`)}
	}
	return r
}

func TestRequestOwnership(t *testing.T) {
	backends(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id := "own-" + newID()
		if err := s.RecordRequest(ctx, sample(id, "alice", t0, true)); err != nil {
			t.Fatal(err)
		}
		o, owner, hasContent, err := s.Outcome(ctx, id, "q")
		if err != nil || owner != "alice" || !hasContent || o.Model != "jeff" || *o.Confidence != 0.9 || *o.Threshold != 0.8 {
			t.Fatalf("outcome = %+v owner=%q content=%v err=%v", o, owner, hasContent, err)
		}
		// An unknown request and an unknown question are not found.
		for _, c := range [][2]string{{"nope", "q"}, {id, "nope"}} {
			if _, _, _, err := s.Outcome(ctx, c[0], c[1]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%v: err = %v, want ErrNotFound", c, err)
			}
		}
		if c, err := s.Content(ctx, "bob", id); err != nil || c != nil {
			t.Fatalf("another client read content: %v %v", c, err)
		}
		if c, err := s.Content(ctx, "alice", id); err != nil || string(c.State) != `"hello"` {
			t.Fatalf("content = %+v %v", c, err)
		}
	})
}

func TestFeedbackSupersedes(t *testing.T) {
	backends(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id := "fb-" + newID()
		if err := s.RecordRequest(ctx, sample(id, "alice", t0, false)); err != nil {
			t.Fatal(err)
		}
		add := func(verdict string, correct []byte, at time.Time) {
			t.Helper()
			if err := s.AddFeedback(ctx, Feedback{RequestID: id, QuestionID: "q", Client: "alice", Owner: "alice", Verdict: verdict,
				Correct: correct, Source: "human", At: at, Model: "jeff", Confidence: f64(0.9)}); err != nil {
				t.Fatal(err)
			}
		}
		add("good", nil, t0)
		add("bad", []byte(`"b"`), t0.Add(time.Minute))
		got, err := s.Feedback(ctx, "alice", t0.Add(-time.Hour))
		if err != nil || len(got) != 1 || got[0].Verdict != "bad" || string(got[0].Correct) != `"b"` || got[0].Model != "jeff" {
			t.Fatalf("feedback = %+v %v", got, err)
		}
	})
}

func TestPurge(t *testing.T) {
	backends(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		old, fresh, other := "old-"+newID(), "new-"+newID(), "oth-"+newID()
		for _, r := range []Request{sample(old, "alice", t0, true), sample(fresh, "alice", t0.Add(48*time.Hour), true), sample(other, "bob", t0, true)} {
			if err := s.RecordRequest(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		fb := func(id, client string, at time.Time) {
			if err := s.AddFeedback(ctx, Feedback{RequestID: id, QuestionID: "q", Client: client, Owner: client, Verdict: "good", Source: "human", At: at, Model: "jeff"}); err != nil {
				t.Fatal(err)
			}
		}
		fb(old, "alice", t0)
		fb(old, "alice", t0.Add(40*time.Hour)) // late: newer than the cut, but about an old request
		cut := t0.Add(24 * time.Hour)

		// Content only: the request and its feedback stay.
		res, err := s.Purge(ctx, PurgeFilter{Client: "alice", Before: cut, ContentOnly: true})
		if err != nil || res.Contents != 1 || res.Requests != 0 {
			t.Fatalf("content-only purge = %+v %v", res, err)
		}
		if _, _, _, err := s.Outcome(ctx, old, "q"); err != nil {
			t.Fatalf("metadata gone after a content-only purge: %v", err)
		}
		if c, _ := s.Content(ctx, "alice", old); c != nil {
			t.Fatal("content survived")
		}

		res, err = s.Purge(ctx, PurgeFilter{Client: "alice", Before: cut})
		if err != nil || res.Requests != 1 || res.Feedback != 2 { // the late verdict superseded the first: two rows in all
			t.Fatalf("purge = %+v %v", res, err)
		}
		if got, _ := s.Feedback(ctx, "alice", time.Time{}); len(got) != 0 {
			t.Fatalf("feedback about a purged request survived: %+v", got)
		}
		if _, _, _, err := s.Outcome(ctx, old, "q"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old request still there: %v", err)
		}
		if _, _, _, err := s.Outcome(ctx, fresh, "q"); err != nil {
			t.Fatalf("fresh request purged: %v", err)
		}
		if _, _, _, err := s.Outcome(ctx, other, "q"); err != nil {
			t.Fatalf("another client's request purged: %v", err)
		}

		// Every client.
		if res, err = s.Purge(ctx, PurgeFilter{Before: cut.Add(100 * time.Hour)}); err != nil || res.Requests != 2 {
			t.Fatalf("all-clients purge = %+v %v", res, err)
		}
	})
}

func TestImageCountIsKeptAndExported(t *testing.T) {
	backends(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		for i, n := range []int{0, 3} {
			r := sample(fmt.Sprintf("img%d-%s", i, newID()), "alice", t0.Add(time.Duration(i)*time.Minute), true)
			r.ImageCount = n
			if err := s.RecordRequest(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		var got []int
		if err := s.Export(ctx, ExportFilter{Client: "alice"}, func(r ExportRow) error { got = append(got, r.ImageCount); return nil }); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != "[0 3]" {
			t.Errorf("image counts = %v, want [0 3]", got)
		}
	})
}

func TestExport(t *testing.T) {
	backends(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id := "ex-" + newID()
		if err := s.RecordRequest(ctx, sample(id, "alice", t0, true)); err != nil {
			t.Fatal(err)
		}
		plain := "pl-" + newID()
		if err := s.RecordRequest(ctx, sample(plain, "alice", t0, false)); err != nil { // no content: not exported
			t.Fatal(err)
		}
		if err := s.AddFeedback(ctx, Feedback{RequestID: id, QuestionID: "q", Client: "alice", Owner: "alice", Verdict: "bad",
			Correct: []byte(`"b"`), Source: "human", At: t0, Model: "jeff"}); err != nil {
			t.Fatal(err)
		}
		var rows []ExportRow
		err := s.Export(ctx, ExportFilter{Client: "alice", Since: t0.Add(-time.Hour)}, func(r ExportRow) error {
			rows = append(rows, r)
			return nil
		})
		if err != nil || len(rows) != 1 || rows[0].RequestID != id || rows[0].Feedback == nil ||
			string(rows[0].Feedback.Correct) != `"b"` || string(rows[0].Content.State) != `"hello"` {
			t.Fatalf("export = %+v %v", rows, err)
		}
		if err := s.Export(ctx, ExportFilter{}, nil); err == nil {
			t.Fatal("export without a client should fail")
		}
	})
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	for range 2 {
		s, err := Open(DriverSQLite, path)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}

func TestUnknownDriver(t *testing.T) {
	if _, err := Open("mysql", "x"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestResolveAndDedupe(t *testing.T) {
	mk := func(client, source, verdict string, at time.Time) Feedback {
		return Feedback{RequestID: "r", QuestionID: "q", Client: client, Owner: "alice", Source: source, Verdict: verdict, At: at}
	}
	t1, t2 := t0, t0.Add(time.Hour)
	cases := []struct {
		name string
		in   []Feedback
		want string // the winning verdict
	}{
		{"human beats automated", []Feedback{mk("alice", "automated", "good", t2), mk("bob", "human", "bad", t1)}, "bad"},
		{"the owner beats another client", []Feedback{mk("bob", "human", "bad", t2), mk("alice", "human", "good", t1)}, "good"},
		{"then the newest", []Feedback{mk("bob", "human", "good", t1), mk("carol", "human", "bad", t2)}, "bad"},
	}
	for _, c := range cases {
		if got := Resolve(c.in); got == nil || got.Verdict != c.want {
			t.Errorf("%s: %+v, want %s", c.name, got, c.want)
		}
	}
	if Resolve(nil) != nil {
		t.Error("nothing to resolve")
	}
	two := []Feedback{mk("alice", "human", "good", t1), mk("bob", "human", "bad", t2)}
	other := Feedback{RequestID: "r2", QuestionID: "q", Client: "bob", Owner: "alice", Source: "human", Verdict: "good", At: t1}
	if got := Dedupe(append(two, other)); len(got) != 2 {
		t.Fatalf("two questions, three verdicts: want 2, got %d", len(got))
	}
}

func TestExportKeepsEveryGiversVerdict(t *testing.T) {
	backends(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id := "mg-" + newID()
		if err := s.RecordRequest(ctx, sample(id, "alice", t0, true)); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct{ client, verdict string }{{"alice", "good"}, {"bob", "bad"}} {
			if err := s.AddFeedback(ctx, Feedback{RequestID: id, QuestionID: "q", Client: c.client, Owner: "alice",
				Verdict: c.verdict, Source: "human", At: t0, Type: "choice", Model: "jeff"}); err != nil {
				t.Fatal(err)
			}
		}
		var row ExportRow
		_ = s.Export(ctx, ExportFilter{Client: "alice"}, func(r ExportRow) error { row = r; return nil })
		if row.Feedback == nil || row.Feedback.Verdict != "good" || len(row.AllFeedback) != 2 {
			t.Fatalf("the owner's verdict stands and both are listed: %+v", row)
		}
	})
}

func TestAudits(t *testing.T) {
	backends(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id := "au-" + newID()
		if err := s.RecordRequest(ctx, sample(id, "alice", t0, false)); err != nil {
			t.Fatal(err)
		}
		a := Audit{RequestID: id, QuestionID: "q", Client: "alice", Route: "fast", Type: "choice", Model: "jeff",
			Confidence: f64(0.9), Threshold: f64(0.8), AuditModel: "jev", AuditConfidence: f64(0.7), Agreed: true, At: t0}
		b := a
		b.QuestionID, b.Agreed, b.AuditConfidence = "q2", false, nil
		if err := s.RecordAudits(ctx, []Audit{a, b}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordAudits(ctx, nil); err != nil {
			t.Fatalf("nothing to record is not an error: %v", err)
		}
		other := a
		other.RequestID, other.Client = "x-"+id, "bob"
		if err := s.RecordAudits(ctx, []Audit{other}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Audits(ctx, "alice", t0.Add(-time.Hour))
		if err != nil || len(got) != 2 {
			t.Fatalf("audits = %+v %v", got, err)
		}
		byQ := map[string]Audit{}
		for _, g := range got {
			byQ[g.QuestionID] = g
		}
		if g := byQ["q"]; !g.Agreed || g.AuditModel != "jev" || *g.AuditConfidence != 0.7 || *g.Threshold != 0.8 || g.Model != "jeff" || g.Route != "fast" {
			t.Errorf("q = %+v", g)
		}
		if g := byQ["q2"]; g.Agreed || g.AuditConfidence != nil {
			t.Errorf("q2 = %+v", g)
		}
		if got, _ := s.Audits(ctx, "alice", t0.Add(time.Hour)); len(got) != 0 {
			t.Errorf("since should filter: %+v", got)
		}
		// Content-only purge leaves audits (they hold no content); a full purge removes them.
		if res, err := s.Purge(ctx, PurgeFilter{Client: "alice", Before: t0.Add(time.Hour), ContentOnly: true}); err != nil || res.Audits != 0 {
			t.Fatalf("content-only purge touched audits: %+v %v", res, err)
		}
		if res, err := s.Purge(ctx, PurgeFilter{Client: "alice", Before: t0.Add(time.Hour)}); err != nil || res.Audits != 2 {
			t.Fatalf("purge = %+v %v", res, err)
		}
		if got, _ := s.Audits(ctx, "bob", time.Time{}); len(got) != 1 {
			t.Errorf("another client's audit was purged: %+v", got)
		}
	})
}

// A database made before audits existed upgrades in place.
func TestAnOlderDatabaseGainsTheAuditsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(DriverSQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	ss := s.(*sqlStore)
	ctx := context.Background()
	for _, q := range []string{`DROP TABLE audits`, `ALTER TABLE requests DROP COLUMN image_count`, `DELETE FROM schema_migrations WHERE version >= 8`} {
		if _, err := ss.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(DriverSQLite, path)
	if err != nil {
		t.Fatalf("reopening an older database: %v", err)
	}
	defer s.Close()
	if err := s.RecordAudits(ctx, []Audit{{RequestID: "r", QuestionID: "q", Client: "c", Route: "r", Type: "noul", Model: "m", AuditModel: "n", At: t0}}); err != nil {
		t.Fatalf("the audits table was not created on upgrade: %v", err)
	}
}
