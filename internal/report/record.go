// Package report watches how a client (an MTN API user or an Airtel app) uses Mbongo and
// points out integration mistakes. It records every call a client makes and every callback
// Mbongo sends it, run by run.
package report

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/migrate"
)

// Outcomes a client can learn about a payment, the same for both operators.
const (
	Pending    = "pending"
	Successful = "successful"
	Failed     = "failed"
)

// Call is one request a client made and what it got back. Status 0 means no answer reached
// the client: the connection was dropped, or the client hung up first.
type Call struct {
	ID        int64     `json:"id"`
	Run       int64     `json:"run"`
	Operator  string    `json:"operator"`
	Client    string    `json:"client"`
	Operation string    `json:"operation"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Reference string    `json:"reference,omitempty"`
	Order     string    `json:"order,omitempty"`
	RefundOf  string    `json:"refundOf,omitempty"`
	Phone     string    `json:"phone,omitempty"`
	Amount    string    `json:"amount,omitempty"`
	Currency  string    `json:"currency,omitempty"`
	Token     string    `json:"token,omitempty"`
	Status    int       `json:"status"`
	Outcome   string    `json:"outcome,omitempty"`
	Fault     string    `json:"fault,omitempty"`
	At        time.Time `json:"at"`
	Millis    int64     `json:"ms"`
}

// Callback is one callback sent to a client. HTTPStatus 0 means it never got an answer.
type Callback struct {
	ID         int64     `json:"id"`
	Run        int64     `json:"run"`
	Operator   string    `json:"operator"`
	Client     string    `json:"client"`
	Reference  string    `json:"reference"`
	Outcome    string    `json:"outcome"`
	HTTPStatus int       `json:"httpStatus"`
	Error      string    `json:"error,omitempty"`
	Fault      string    `json:"fault,omitempty"`
	At         time.Time `json:"at"`
	Millis     int64     `json:"ms"`
}

// Exchange is a call as the operator's Describe sees it, once answered.
type Exchange struct {
	Request *http.Request
	Body    []byte
	Status  int
	Answer  []byte
}

// Describe tells, in the operator's own terms, who made a call and about which payment.
type Describe func(ctx context.Context, e Exchange) Call

const schema = `
CREATE TABLE report_runs (
    id         INTEGER PRIMARY KEY,
    started_at TEXT NOT NULL
);
CREATE TABLE report_calls (
    id        INTEGER PRIMARY KEY,
    run       INTEGER NOT NULL REFERENCES report_runs (id),
    operator  TEXT NOT NULL,
    client    TEXT NOT NULL,
    operation TEXT NOT NULL,
    method    TEXT NOT NULL,
    path      TEXT NOT NULL,
    reference TEXT NOT NULL,
    order_ref TEXT NOT NULL,
    refund_of TEXT NOT NULL,
    phone     TEXT NOT NULL,
    amount    TEXT NOT NULL,
    currency  TEXT NOT NULL,
    token     TEXT NOT NULL,
    status    INTEGER NOT NULL,
    outcome   TEXT NOT NULL,
    fault     TEXT NOT NULL,
    at        TEXT NOT NULL,
    ms        INTEGER NOT NULL
);
CREATE INDEX report_calls_by_run ON report_calls (run, operator, client);
CREATE TABLE report_callbacks (
    id          INTEGER PRIMARY KEY,
    run         INTEGER NOT NULL REFERENCES report_runs (id),
    operator    TEXT NOT NULL,
    client      TEXT NOT NULL,
    reference   TEXT NOT NULL,
    outcome     TEXT NOT NULL,
    http_status INTEGER NOT NULL,
    error       TEXT NOT NULL,
    fault       TEXT NOT NULL,
    at          TEXT NOT NULL,
    ms          INTEGER NOT NULL
);
CREATE INDEX report_callbacks_by_run ON report_callbacks (run, operator, client);
`

const timeLayout = "2006-01-02T15:04:05.000000000Z"

// Recorder keeps the journal. A nil Recorder records nothing.
type Recorder struct {
	db    *sql.DB
	clock clock.Clock
}

// New opens the journal; the first run starts with it.
func New(ctx context.Context, db *sql.DB, c clock.Clock) (*Recorder, error) {
	if err := migrate.Run(ctx, db, "report", []string{schema}); err != nil {
		return nil, err
	}
	rec := &Recorder{db: db, clock: c}
	_, err := db.ExecContext(ctx, `INSERT INTO report_runs (id, started_at)
		SELECT 1, ? WHERE NOT EXISTS (SELECT 1 FROM report_runs)`, rec.now())
	return rec, err
}

func (rec *Recorder) now() string { return rec.clock.Now().UTC().Format(timeLayout) }

type noteKey struct{}

type note struct{ fault string }

// NoteFault marks the call being recorded as failed on purpose, so no check blames the client
// for what a rule did.
func NoteFault(ctx context.Context, fault string) {
	if n, ok := ctx.Value(noteKey{}).(*note); ok {
		n.fault = fault
	}
}

const maxBody = 64 << 10

// Wrap records each call to one of the operator's routes, with what the client got back.
func (rec *Recorder) Wrap(operator, operation string, describe Describe, next http.Handler) http.Handler {
	if rec == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := rec.clock.Now()
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
		r.Body = readCloser{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		n := &note{}
		r = r.WithContext(context.WithValue(r.Context(), noteKey{}, n))
		out := &watched{ResponseWriter: w}
		defer func() {
			err := recover()
			status := out.status
			switch {
			case err == http.ErrAbortHandler: //nolint:errorlint // recover returns the value itself
				status = 0
			case err != nil && !out.wrote:
				status = http.StatusInternalServerError
			case !out.wrote && r.Context().Err() != nil:
				status = 0
			case !out.wrote:
				status = http.StatusOK
			}
			c := describe(r.Context(), Exchange{Request: r, Body: body, Status: status, Answer: out.answer.Bytes()})
			c.Operator, c.Operation, c.Method, c.Path, c.Status, c.Fault = operator, operation, r.Method, r.URL.Path, status, n.fault
			c.At, c.Millis = start, rec.clock.Now().Sub(start).Milliseconds()
			// The client's answer is already out; a journal error must not change it.
			_ = rec.addCall(context.WithoutCancel(r.Context()), c)
			if err != nil {
				panic(err)
			}
		}()
		next.ServeHTTP(out, r)
	})
}

// Callback records a callback sent to a client.
func (rec *Recorder) Callback(ctx context.Context, cb Callback) error {
	if rec == nil {
		return nil
	}
	_, err := rec.db.ExecContext(ctx, `INSERT INTO report_callbacks
		(run, operator, client, reference, outcome, http_status, error, fault, at, ms)
		VALUES ((SELECT MAX(id) FROM report_runs), ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cb.Operator, cb.Client, cb.Reference, cb.Outcome, cb.HTTPStatus, cb.Error, cb.Fault,
		cb.At.UTC().Format(timeLayout), cb.Millis)
	return err
}

func (rec *Recorder) addCall(ctx context.Context, c Call) error {
	_, err := rec.db.ExecContext(ctx, `INSERT INTO report_calls
		(run, operator, client, operation, method, path, reference, order_ref, refund_of, phone, amount,
		 currency, token, status, outcome, fault, at, ms)
		VALUES ((SELECT MAX(id) FROM report_runs), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Operator, c.Client, c.Operation, c.Method, c.Path, c.Reference, c.Order, c.RefundOf, c.Phone,
		c.Amount, c.Currency, c.Token, c.Status, c.Outcome, c.Fault, c.At.UTC().Format(timeLayout), c.Millis)
	return err
}

// Run is one stretch of recorded traffic.
type Run struct {
	ID      int64     `json:"id"`
	Started time.Time `json:"startedAt"`
}

// NewRun starts a run; what follows is recorded in it, and older runs stay readable.
func (rec *Recorder) NewRun(ctx context.Context) (Run, error) {
	now := rec.clock.Now().UTC()
	res, err := rec.db.ExecContext(ctx, "INSERT INTO report_runs (started_at) VALUES (?)", now.Format(timeLayout))
	if err != nil {
		return Run{}, err
	}
	id, err := res.LastInsertId()
	return Run{ID: id, Started: now}, err
}

// Runs lists the runs, newest first.
func (rec *Recorder) Runs(ctx context.Context) ([]Run, error) {
	rows, err := rec.db.QueryContext(ctx, "SELECT id, started_at FROM report_runs ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	runs := []Run{}
	for rows.Next() {
		var (
			r       Run
			started string
		)
		if err := rows.Scan(&r.ID, &started); err != nil {
			return nil, err
		}
		r.Started, _ = time.Parse(timeLayout, started)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

var ErrNoRun = errors.New("report: no such run")

// Latest is the run being recorded.
func (rec *Recorder) Latest(ctx context.Context) (int64, error) {
	var id int64
	err := rec.db.QueryRowContext(ctx, "SELECT MAX(id) FROM report_runs").Scan(&id)
	return id, err
}

func (rec *Recorder) checkRun(ctx context.Context, run int64) error {
	var found bool
	err := rec.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM report_runs WHERE id = ?)", run).Scan(&found)
	if err == nil && !found {
		err = ErrNoRun
	}
	return err
}

// Calls are a run's calls, oldest first.
func (rec *Recorder) Calls(ctx context.Context, run int64) ([]Call, error) {
	if err := rec.checkRun(ctx, run); err != nil {
		return nil, err
	}
	rows, err := rec.db.QueryContext(ctx, `SELECT id, run, operator, client, operation, method, path, reference,
		order_ref, refund_of, phone, amount, currency, token, status, outcome, fault, at, ms
		FROM report_calls WHERE run = ? ORDER BY id`, run)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	calls := []Call{}
	for rows.Next() {
		var (
			c  Call
			at string
		)
		if err := rows.Scan(&c.ID, &c.Run, &c.Operator, &c.Client, &c.Operation, &c.Method, &c.Path,
			&c.Reference, &c.Order, &c.RefundOf, &c.Phone, &c.Amount, &c.Currency, &c.Token, &c.Status,
			&c.Outcome, &c.Fault, &at, &c.Millis); err != nil {
			return nil, err
		}
		c.At, _ = time.Parse(timeLayout, at)
		calls = append(calls, c)
	}
	return calls, rows.Err()
}

// Callbacks are a run's callbacks, oldest first.
func (rec *Recorder) Callbacks(ctx context.Context, run int64) ([]Callback, error) {
	if err := rec.checkRun(ctx, run); err != nil {
		return nil, err
	}
	rows, err := rec.db.QueryContext(ctx, `SELECT id, run, operator, client, reference, outcome, http_status,
		error, fault, at, ms FROM report_callbacks WHERE run = ? ORDER BY id`, run)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cbs := []Callback{}
	for rows.Next() {
		var (
			cb Callback
			at string
		)
		if err := rows.Scan(&cb.ID, &cb.Run, &cb.Operator, &cb.Client, &cb.Reference, &cb.Outcome,
			&cb.HTTPStatus, &cb.Error, &cb.Fault, &at, &cb.Millis); err != nil {
			return nil, err
		}
		cb.At, _ = time.Parse(timeLayout, at)
		cbs = append(cbs, cb)
	}
	return cbs, rows.Err()
}

type readCloser struct {
	io.Reader
	io.Closer
}

// watched passes the answer on to the client and keeps a copy of its start.
type watched struct {
	http.ResponseWriter
	status int
	wrote  bool
	answer bytes.Buffer
}

func (w *watched) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *watched) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	if room := maxBody - w.answer.Len(); room > 0 {
		w.answer.Write(b[:min(len(b), room)])
	}
	return w.ResponseWriter.Write(b)
}

func (w *watched) Unwrap() http.ResponseWriter { return w.ResponseWriter }
