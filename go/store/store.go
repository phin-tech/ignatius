// Package store is the optional database behind feedback and opt-in content storage
// (SPEC 13). It has two backends, SQLite (default, pure Go) and Postgres, behind one set
// of plain SQL statements: timestamps are unix milliseconds, JSON is text, and every
// statement is written with ? placeholders that the Postgres backend rewrites.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
	_ "modernc.org/sqlite"             // registers "sqlite"
)

// Driver names accepted in [store] driver.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// ErrNotFound is returned for a request or question the store does not hold.
var ErrNotFound = errors.New("store: not found")

// Outcome is what one question of one request came to: the model that supplied the final
// answer, how confident it was, and the bar a cascade applied to it (nil if none).
type Outcome struct {
	QuestionID string   `json:"question_id"`
	Type       string   `json:"type"`
	Model      string   `json:"model"`
	Confidence *float64 `json:"confidence"`
	Threshold  *float64 `json:"threshold,omitempty"`
}

// Content is what an opted-in client's request left behind. Each field is JSON.
type Content struct {
	State     []byte
	Questions []byte
	Answers   []byte // the final answer per question
	Results   []byte // every model's result, including tiers that did not supply the answer
	Plan      []byte
}

// Request is one routed request. Content is nil unless the client opted in.
type Request struct {
	ID       string
	Client   string
	Layer    string
	Route    string
	Mode     string
	OK       bool
	At       time.Time
	Outcomes []Outcome
	Content  *Content
	// ImageCount is how many images the request carried. The images themselves are never stored,
	// so this is the only trace that a question depended on one.
	ImageCount int
}

// Feedback is one verdict on one question of one request.
type Feedback struct {
	RequestID  string
	QuestionID string
	Client     string // who gave the feedback
	Owner      string // the client that made the request; calibration and retention follow this
	Verdict    string // "good" | "bad"
	Correct    []byte // JSON, optional
	Source     string // "human" | "automated"
	At         time.Time

	// Filled in from the request's outcome when the feedback is recorded.
	Type       string // question type: noul | choice | score
	Model      string
	Confidence *float64
	Threshold  *float64
}

// Audit is one shadow audit (SPEC 13.7): a cascade tier settled a question on its own, and a
// sampled copy of the question was also put to the next tier. Agreed says whether the two gave
// the same answer. It holds no content: no state, question or answer value.
type Audit struct {
	RequestID, QuestionID string
	Client                string // the client that made the request
	Route, Type           string
	Model                 string // the tier that settled the question
	Confidence            *float64
	Threshold             *float64
	AuditModel            string // the next tier, asked in the background
	AuditConfidence       *float64
	Agreed                bool
	At                    time.Time
}

// Resolve picks the verdict that stands for one question when several clients gave one: a
// human's over an automated one over an audit, then the request owner's over anyone else's,
// then the newest.
// It returns nil for an empty list.
func Resolve(fbs []Feedback) *Feedback {
	var best *Feedback
	rank := func(f *Feedback) [3]int64 {
		r := [3]int64{0, 0, f.At.UnixNano()}
		switch f.Source {
		case "human":
			r[0] = 2
		case "automated":
			r[0] = 1
		}
		if f.Client == f.Owner {
			r[1] = 1
		}
		return r
	}
	for i := range fbs {
		f := &fbs[i]
		if best == nil {
			best = f
			continue
		}
		a, b := rank(f), rank(best)
		if a[0] > b[0] || (a[0] == b[0] && (a[1] > b[1] || (a[1] == b[1] && a[2] > b[2]))) {
			best = f
		}
	}
	if best == nil {
		return nil
	}
	c := *best
	return &c
}

// Dedupe keeps one feedback per (request, question), by Resolve, so a decision rated by
// several clients is counted once.
func Dedupe(fbs []Feedback) []Feedback {
	type key struct{ req, q string }
	groups := map[key][]Feedback{}
	var order []key
	for _, f := range fbs {
		k := key{f.RequestID, f.QuestionID}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], f)
	}
	out := make([]Feedback, 0, len(order))
	for _, k := range order {
		out = append(out, *Resolve(groups[k]))
	}
	return out
}

// PurgeFilter selects what Purge deletes. Before is exclusive.
type PurgeFilter struct {
	Client      string // one client; empty = every client
	Before      time.Time
	ContentOnly bool // keep request metadata and feedback, delete stored content
}

// PurgeResult counts deleted rows.
type PurgeResult struct{ Requests, Contents, Feedback, Audits int64 }

// ExportRow is one question of one opted-in request, with its latest feedback.
type ExportRow struct {
	RequestID   string
	QuestionID  string
	At          time.Time
	Client      string
	Route       string
	Mode        string
	ImageCount  int // images the request carried; they are not stored (see Request.ImageCount)
	Outcome     Outcome
	Content     Content
	Feedback    *Feedback  // the verdict that stands (see Resolve), nil if none
	AllFeedback []Feedback // every live verdict, one per giver
}

// ExportFilter selects what Export streams. Client is required.
type ExportFilter struct {
	Client string
	Since  time.Time
}

// Store is the repository the gateway talks to.
type Store interface {
	RecordRequest(ctx context.Context, r Request) error
	// Outcome returns one question's outcome for a request, the client that made the
	// request, and whether that request has stored content.
	Outcome(ctx context.Context, requestID, questionID string) (o Outcome, owner string, hasContent bool, err error)
	// Content returns the stored content of a request owned by client, or nil.
	Content(ctx context.Context, client, requestID string) (*Content, error)
	AddFeedback(ctx context.Context, f Feedback) error
	Purge(ctx context.Context, f PurgeFilter) (PurgeResult, error)
	Export(ctx context.Context, f ExportFilter, fn func(ExportRow) error) error
	// Feedback returns the current (not superseded) feedback rows about a client's
	// requests, from anyone, for calibration and scorecards.
	Feedback(ctx context.Context, client string, since time.Time) ([]Feedback, error)
	// RecordAudits stores shadow-audit results (SPEC 13.7); Audits reads a client's.
	RecordAudits(ctx context.Context, as []Audit) error
	Audits(ctx context.Context, client string, since time.Time) ([]Audit, error)
	Close() error
}

// Open connects, applies migrations and returns a Store. dsn is a file path for sqlite
// (":memory:" works for tests) and a connection string for postgres.
func Open(driver, dsn string) (Store, error) {
	var db *sql.DB
	var err error
	d := &sqlStore{}
	switch driver {
	case DriverSQLite, "":
		d.pg = false
		if dsn == "" {
			dsn = "ignatius.db"
		}
		db, err = sql.Open("sqlite", sqliteDSN(dsn))
		if err == nil {
			db.SetMaxOpenConns(1) // one writer; also keeps ":memory:" a single database
		}
	case DriverPostgres:
		d.pg = true
		if dsn == "" {
			return nil, errors.New("store: postgres needs a connection string")
		}
		db, err = sql.Open("pgx", dsn)
	default:
		return nil, fmt.Errorf("store: unknown driver %q (want %q or %q)", driver, DriverSQLite, DriverPostgres)
	}
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	d.db = db
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: connect to %s %q: %w", driver, redact(driver, dsn), err)
	}
	if err := d.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// redact keeps a postgres connection string's password out of an error message.
func redact(driver, dsn string) string {
	if driver == DriverPostgres {
		return "(connection string)"
	}
	return dsn
}

func sqliteDSN(path string) string {
	if path == ":memory:" {
		return "file::memory:?_pragma=busy_timeout(5000)"
	}
	if strings.HasPrefix(path, "file:") {
		return path
	}
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}

type sqlStore struct {
	db *sql.DB
	pg bool
}

func (s *sqlStore) Close() error { return s.db.Close() }

// q rewrites ? placeholders to $1, $2, ... for Postgres.
func (s *sqlStore) q(query string) string {
	if !s.pg {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }
func nullf(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// migrations are applied in order, once each. Never edit one that has shipped; add another.
var migrations = []string{`
CREATE TABLE requests (
	request_id TEXT PRIMARY KEY,
	client     TEXT NOT NULL,
	layer      TEXT NOT NULL,
	route      TEXT NOT NULL,
	mode       TEXT NOT NULL,
	ok         INTEGER NOT NULL,
	created_ms BIGINT NOT NULL
)`, `
CREATE INDEX requests_client_created ON requests (client, created_ms)`, `
CREATE TABLE outcomes (
	request_id  TEXT NOT NULL,
	question_id TEXT NOT NULL,
	qtype       TEXT NOT NULL,
	model       TEXT NOT NULL,
	confidence  DOUBLE PRECISION,
	threshold   DOUBLE PRECISION,
	PRIMARY KEY (request_id, question_id)
)`, `
CREATE TABLE contents (
	request_id TEXT PRIMARY KEY,
	state      TEXT NOT NULL,
	questions  TEXT NOT NULL,
	answers    TEXT NOT NULL,
	results    TEXT NOT NULL,
	plan       TEXT NOT NULL
)`, `
CREATE TABLE feedback (
	id            TEXT PRIMARY KEY,
	request_id    TEXT NOT NULL,
	question_id   TEXT NOT NULL,
	client        TEXT NOT NULL,
	owner         TEXT NOT NULL,
	verdict       TEXT NOT NULL,
	correct       TEXT,
	source        TEXT NOT NULL,
	qtype         TEXT NOT NULL,
	model         TEXT NOT NULL,
	confidence    DOUBLE PRECISION,
	threshold     DOUBLE PRECISION,
	created_ms    BIGINT NOT NULL,
	superseded_ms BIGINT
)`, `
CREATE INDEX feedback_request ON feedback (request_id, question_id)`, `
CREATE INDEX feedback_owner_created ON feedback (owner, created_ms)`, `
CREATE TABLE audits (
	request_id       TEXT NOT NULL,
	question_id      TEXT NOT NULL,
	client           TEXT NOT NULL,
	route            TEXT NOT NULL,
	qtype            TEXT NOT NULL,
	model            TEXT NOT NULL,
	confidence       DOUBLE PRECISION,
	threshold        DOUBLE PRECISION,
	audit_model      TEXT NOT NULL,
	audit_confidence DOUBLE PRECISION,
	agreed           INTEGER NOT NULL,
	created_ms       BIGINT NOT NULL,
	PRIMARY KEY (request_id, question_id)
)`, `
CREATE INDEX audits_client_created ON audits (client, created_ms)`, `
ALTER TABLE requests ADD COLUMN image_count INTEGER NOT NULL DEFAULT 0`,
}

func (s *sqlStore) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	var have int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&have); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if have > len(migrations) {
		return fmt.Errorf("store: database is at schema version %d but this build knows %d: upgrade ignatius", have, len(migrations))
	}
	for i := have; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO schema_migrations (version) VALUES (?)`), i+1); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *sqlStore) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *sqlStore) RecordRequest(ctx context.Context, r Request) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		ok := 0
		if r.OK {
			ok = 1
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO requests (request_id, client, layer, route, mode, ok, created_ms, image_count) VALUES (?,?,?,?,?,?,?,?)`),
			r.ID, r.Client, r.Layer, r.Route, r.Mode, ok, ms(r.At), r.ImageCount); err != nil {
			return err
		}
		for _, o := range r.Outcomes {
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO outcomes (request_id, question_id, qtype, model, confidence, threshold) VALUES (?,?,?,?,?,?)`),
				r.ID, o.QuestionID, o.Type, o.Model, nullf(o.Confidence), nullf(o.Threshold)); err != nil {
				return err
			}
		}
		if c := r.Content; c != nil {
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO contents (request_id, state, questions, answers, results, plan) VALUES (?,?,?,?,?,?)`),
				r.ID, string(c.State), string(c.Questions), string(c.Answers), string(c.Results), string(c.Plan)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *sqlStore) Outcome(ctx context.Context, requestID, questionID string) (Outcome, string, bool, error) {
	var o Outcome
	var owner string
	var conf, thr sql.NullFloat64
	var hasContent int
	err := s.db.QueryRowContext(ctx, s.q(`
SELECT o.question_id, o.qtype, o.model, o.confidence, o.threshold, r.client,
       (SELECT COUNT(*) FROM contents c WHERE c.request_id = r.request_id)
FROM requests r JOIN outcomes o ON o.request_id = r.request_id
WHERE r.request_id = ? AND o.question_id = ?`), requestID, questionID).
		Scan(&o.QuestionID, &o.Type, &o.Model, &conf, &thr, &owner, &hasContent)
	if errors.Is(err, sql.ErrNoRows) {
		return Outcome{}, "", false, ErrNotFound
	}
	if err != nil {
		return Outcome{}, "", false, err
	}
	if conf.Valid {
		o.Confidence = &conf.Float64
	}
	if thr.Valid {
		o.Threshold = &thr.Float64
	}
	return o, owner, hasContent > 0, nil
}

func (s *sqlStore) Content(ctx context.Context, client, requestID string) (*Content, error) {
	var c Content
	var st, qs, as, rs, pl string
	err := s.db.QueryRowContext(ctx, s.q(`
SELECT c.state, c.questions, c.answers, c.results, c.plan
FROM contents c JOIN requests r ON r.request_id = c.request_id
WHERE c.request_id = ? AND r.client = ?`), requestID, client).Scan(&st, &qs, &as, &rs, &pl)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c = Content{State: []byte(st), Questions: []byte(qs), Answers: []byte(as), Results: []byte(rs), Plan: []byte(pl)}
	return &c, nil
}

func (s *sqlStore) AddFeedback(ctx context.Context, f Feedback) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// A later verdict from the same client replaces the earlier one; the history stays.
		if _, err := tx.ExecContext(ctx, s.q(`UPDATE feedback SET superseded_ms = ?
WHERE request_id = ? AND question_id = ? AND client = ? AND superseded_ms IS NULL`),
			ms(f.At), f.RequestID, f.QuestionID, f.Client); err != nil {
			return err
		}
		var correct any
		if f.Correct != nil {
			correct = string(f.Correct)
		}
		_, err := tx.ExecContext(ctx, s.q(`INSERT INTO feedback
(id, request_id, question_id, client, owner, verdict, correct, source, qtype, model, confidence, threshold, created_ms)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			newID(), f.RequestID, f.QuestionID, f.Client, f.Owner, f.Verdict, correct, f.Source, f.Type, f.Model,
			nullf(f.Confidence), nullf(f.Threshold), ms(f.At))
		return err
	})
}

func scanFeedback(rows *sql.Rows) (Feedback, error) {
	var f Feedback
	var correct sql.NullString
	var conf, thr sql.NullFloat64
	var at int64
	if err := rows.Scan(&f.RequestID, &f.QuestionID, &f.Client, &f.Owner, &f.Verdict, &correct, &f.Source, &f.Type, &f.Model, &conf, &thr, &at); err != nil {
		return f, err
	}
	if correct.Valid {
		f.Correct = []byte(correct.String)
	}
	if conf.Valid {
		f.Confidence = &conf.Float64
	}
	if thr.Valid {
		f.Threshold = &thr.Float64
	}
	f.At = fromMS(at)
	return f, nil
}

const feedbackCols = `request_id, question_id, client, owner, verdict, correct, source, qtype, model, confidence, threshold, created_ms`

func (s *sqlStore) Feedback(ctx context.Context, client string, since time.Time) ([]Feedback, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+feedbackCols+` FROM feedback
WHERE owner = ? AND created_ms >= ? AND superseded_ms IS NULL ORDER BY created_ms`), client, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feedback
	for rows.Next() {
		f, err := scanFeedback(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *sqlStore) Purge(ctx context.Context, f PurgeFilter) (PurgeResult, error) {
	var res PurgeResult
	// which: the rows' client condition, with its arguments.
	which, args := "client = ?", []any{f.Client}
	if f.Client == "" {
		which, args = "1 = 1", nil
	}
	old := which + " AND created_ms < ?"
	oldArgs := append(append([]any{}, args...), ms(f.Before))
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		del := func(query string, a []any) (int64, error) {
			r, err := tx.ExecContext(ctx, s.q(query), a...)
			if err != nil {
				return 0, err
			}
			return r.RowsAffected()
		}
		ids := `(SELECT request_id FROM requests WHERE ` + old + `)`
		var err error
		if res.Contents, err = del(`DELETE FROM contents WHERE request_id IN `+ids, oldArgs); err != nil {
			return err
		}
		if f.ContentOnly {
			return nil
		}
		if _, err = del(`DELETE FROM outcomes WHERE request_id IN `+ids, oldArgs); err != nil {
			return err
		}
		if res.Audits, err = del(`DELETE FROM audits WHERE `+old, oldArgs); err != nil {
			return err
		}
		// Feedback goes with its request even if it arrived later, and on its own age too
		// (by the client the request was made by, whoever gave the feedback).
		fbOld := strings.Replace(old, "client", "owner", 1)
		if res.Feedback, err = del(`DELETE FROM feedback WHERE request_id IN `+ids+` OR (`+fbOld+`)`, append(append([]any{}, oldArgs...), oldArgs...)); err != nil {
			return err
		}
		res.Requests, err = del(`DELETE FROM requests WHERE `+old, oldArgs)
		return err
	})
	return res, err
}

func (s *sqlStore) Export(ctx context.Context, f ExportFilter, fn func(ExportRow) error) error {
	if f.Client == "" {
		return errors.New("store: export needs a client")
	}
	type reqRow struct {
		id, route, mode string
		images          int
		at              time.Time
		c               Content
	}
	// Read the requests first and close the cursor: SQLite has one connection, and the
	// per-request queries below would otherwise wait on it forever.
	rows, err := s.db.QueryContext(ctx, s.q(`
SELECT r.request_id, r.route, r.mode, r.created_ms, r.image_count, c.state, c.questions, c.answers, c.results, c.plan
FROM requests r JOIN contents c ON c.request_id = r.request_id
WHERE r.client = ? AND r.created_ms >= ? ORDER BY r.created_ms, r.request_id`), f.Client, ms(f.Since))
	if err != nil {
		return err
	}
	var reqs []reqRow
	for rows.Next() {
		var r reqRow
		var at int64
		var st, qs, as, rs, pl string
		if err := rows.Scan(&r.id, &r.route, &r.mode, &at, &r.images, &st, &qs, &as, &rs, &pl); err != nil {
			rows.Close()
			return err
		}
		r.at = fromMS(at)
		r.c = Content{State: []byte(st), Questions: []byte(qs), Answers: []byte(as), Results: []byte(rs), Plan: []byte(pl)}
		reqs = append(reqs, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, r := range reqs {
		outs, err := s.outcomesOf(ctx, r.id)
		if err != nil {
			return err
		}
		fbs, err := s.currentFeedback(ctx, r.id)
		if err != nil {
			return err
		}
		for _, o := range outs {
			row := ExportRow{RequestID: r.id, QuestionID: o.QuestionID, At: r.at, Client: f.Client,
				Route: r.route, Mode: r.mode, ImageCount: r.images, Outcome: o, Content: r.c}
			if all := fbs[o.QuestionID]; len(all) > 0 {
				row.Feedback, row.AllFeedback = Resolve(all), all
			}
			if err := fn(row); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *sqlStore) outcomesOf(ctx context.Context, id string) ([]Outcome, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT question_id, qtype, model, confidence, threshold
FROM outcomes WHERE request_id = ? ORDER BY question_id`), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outcome
	for rows.Next() {
		var o Outcome
		var conf, thr sql.NullFloat64
		if err := rows.Scan(&o.QuestionID, &o.Type, &o.Model, &conf, &thr); err != nil {
			return nil, err
		}
		if conf.Valid {
			o.Confidence = &conf.Float64
		}
		if thr.Valid {
			o.Threshold = &thr.Float64
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// currentFeedback returns the live feedback for a request by question: one verdict per
// client that gave one.
func (s *sqlStore) currentFeedback(ctx context.Context, id string) (map[string][]Feedback, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+feedbackCols+` FROM feedback
WHERE request_id = ? AND superseded_ms IS NULL ORDER BY created_ms`), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Feedback{}
	for rows.Next() {
		f, err := scanFeedback(rows)
		if err != nil {
			return nil, err
		}
		out[f.QuestionID] = append(out[f.QuestionID], f)
	}
	return out, rows.Err()
}

func (s *sqlStore) RecordAudits(ctx context.Context, as []Audit) error {
	if len(as) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, a := range as {
			agreed := 0
			if a.Agreed {
				agreed = 1
			}
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO audits
(request_id, question_id, client, route, qtype, model, confidence, threshold, audit_model, audit_confidence, agreed, created_ms)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`),
				a.RequestID, a.QuestionID, a.Client, a.Route, a.Type, a.Model, nullf(a.Confidence), nullf(a.Threshold),
				a.AuditModel, nullf(a.AuditConfidence), agreed, ms(a.At)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *sqlStore) Audits(ctx context.Context, client string, since time.Time) ([]Audit, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT request_id, question_id, client, route, qtype, model, confidence, threshold,
audit_model, audit_confidence, agreed, created_ms FROM audits WHERE client = ? AND created_ms >= ? ORDER BY created_ms`), client, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Audit
	for rows.Next() {
		var a Audit
		var conf, thr, aconf sql.NullFloat64
		var agreed int
		var at int64
		if err := rows.Scan(&a.RequestID, &a.QuestionID, &a.Client, &a.Route, &a.Type, &a.Model, &conf, &thr,
			&a.AuditModel, &aconf, &agreed, &at); err != nil {
			return nil, err
		}
		if conf.Valid {
			a.Confidence = &conf.Float64
		}
		if thr.Valid {
			a.Threshold = &thr.Float64
		}
		if aconf.Valid {
			a.AuditConfidence = &aconf.Float64
		}
		a.Agreed, a.At = agreed != 0, fromMS(at)
		out = append(out, a)
	}
	return out, rows.Err()
}
