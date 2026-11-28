package phone

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/migrate"
)

const schema = `
CREATE TABLE phones (
    id INTEGER PRIMARY KEY AUTOINCREMENT
);
CREATE TABLE phone_sims (
    phone_id INTEGER NOT NULL REFERENCES phones (id) ON DELETE CASCADE,
    slot     INTEGER NOT NULL CHECK (slot IN (1, 2)),
    operator TEXT    NOT NULL,
    number   TEXT    NOT NULL,
    PRIMARY KEY (phone_id, slot),
    UNIQUE (operator, number)
);
CREATE TABLE sms (
    id       INTEGER PRIMARY KEY,
    operator TEXT    NOT NULL,
    number   TEXT    NOT NULL,
    sender   TEXT    NOT NULL,
    body     TEXT    NOT NULL,
    at       TEXT    NOT NULL,
    read     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX sms_number ON sms (operator, number, id);
`

var (
	errFull          = errors.New("phone: that phone already holds two SIMs")
	errUnknownNumber = errors.New("phone: no customer has that number")
	errAmbiguous     = errors.New("phone: customers at both operators have that number")
)

// SIM is a customer's number at one operator, in slot 1 or 2 of a phone.
type SIM struct {
	Slot     int           `json:"slot"`
	Operator bank.Operator `json:"operator"`
	Number   string        `json:"number"`
}

type message struct {
	ID       int64
	Operator bank.Operator
	Number   string
	Sender   string
	Body     string
	At       time.Time
	Read     bool
}

type store struct {
	db    *sql.DB
	books *bank.Bank
}

func newStore(ctx context.Context, books *bank.Bank) (*store, error) {
	if err := migrate.Run(ctx, books.DB(), "phone", []string{schema}); err != nil {
		return nil, err
	}
	return &store{db: books.DB(), books: books}, nil
}

// phoneOf finds the phone holding the SIM, or gives the SIM a phone of its own.
func (s *store) phoneOf(ctx context.Context, op bank.Operator, number string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := phoneIn(ctx, tx, op, number)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// phoneIn is phoneOf inside a database transaction, so two visits at once give one phone.
func phoneIn(ctx context.Context, tx *sql.Tx, op bank.Operator, number string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT phone_id FROM phone_sims WHERE operator = ? AND number = ?",
		op, number).Scan(&id)
	if !errors.Is(err, sql.ErrNoRows) {
		return id, err
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO phones DEFAULT VALUES")
	if err != nil {
		return 0, err
	}
	if id, err = res.LastInsertId(); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO phone_sims (phone_id, slot, operator, number) VALUES (?, 1, ?, ?)",
		id, op, number)
	return id, err
}

// host finds the phone a SIM joining the number's device goes into: the phone holding that
// number, given one if it has none yet. It fails when no customer has the number or when
// customers at both operators do.
func host(ctx context.Context, tx *sql.Tx, number string) (int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT DISTINCT operator FROM accounts WHERE kind = 'customer' AND name = ?", number)
	if err != nil {
		return 0, err
	}
	var ops []bank.Operator
	for rows.Next() {
		var op bank.Operator
		if err := rows.Scan(&op); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ops = append(ops, op)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	switch {
	case len(ops) == 0:
		return 0, errUnknownNumber
	case len(ops) > 1:
		return 0, errAmbiguous
	}
	return phoneIn(ctx, tx, ops[0], number)
}

// join puts the SIM in the free slot of the phone holding the other number, taking it out of
// a phone of its own; a SIM already in that phone stays where it is. It fails when that phone
// already holds two SIMs.
func join(ctx context.Context, tx *sql.Tx, sim SIM, number string) error {
	id, err := host(ctx, tx, number)
	if err != nil {
		return err
	}
	var current int64
	err = tx.QueryRowContext(ctx, "SELECT phone_id FROM phone_sims WHERE operator = ? AND number = ?",
		sim.Operator, sim.Number).Scan(&current)
	switch {
	case err == nil && current == id:
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var n, used int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*), MAX(slot) FROM phone_sims WHERE phone_id = ?", id).
		Scan(&n, &used); err != nil {
		return err
	}
	if n >= 2 {
		return errFull
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM phone_sims WHERE operator = ? AND number = ?",
		sim.Operator, sim.Number); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM phones WHERE id NOT IN (SELECT phone_id FROM phone_sims)"); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO phone_sims (phone_id, slot, operator, number) VALUES (?, ?, ?, ?)",
		id, 3-used, sim.Operator, sim.Number)
	return err
}

func (s *store) sims(ctx context.Context, phone int64) ([]SIM, error) {
	return scanSIMs(s.db.QueryContext(ctx,
		"SELECT slot, operator, number FROM phone_sims WHERE phone_id = ? ORDER BY slot", phone))
}

type phoneSIMs struct {
	ID   int64
	SIMs []SIM
}

func (s *store) phones(ctx context.Context) ([]phoneSIMs, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT phone_id, slot, operator, number FROM phone_sims ORDER BY phone_id, slot")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var list []phoneSIMs
	for rows.Next() {
		var id int64
		var sim SIM
		if err := rows.Scan(&id, &sim.Slot, &sim.Operator, &sim.Number); err != nil {
			return nil, err
		}
		if len(list) == 0 || list[len(list)-1].ID != id {
			list = append(list, phoneSIMs{ID: id})
		}
		list[len(list)-1].SIMs = append(list[len(list)-1].SIMs, sim)
	}
	return list, rows.Err()
}

func scanSIMs(rows *sql.Rows, err error) ([]SIM, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var sims []SIM
	for rows.Next() {
		var sim SIM
		if err := rows.Scan(&sim.Slot, &sim.Operator, &sim.Number); err != nil {
			return nil, err
		}
		sims = append(sims, sim)
	}
	return sims, rows.Err()
}

func (s *store) addSMS(ctx context.Context, m message) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO sms (operator, number, sender, body, at) VALUES (?, ?, ?, ?, ?)",
		m.Operator, m.Number, m.Sender, m.Body, m.At.UTC().Format(bank.TimeLayout))
	return err
}

// inbox is the SIM's messages, newest first.
func (s *store) inbox(ctx context.Context, op bank.Operator, number string, limit int) ([]message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, operator, number, sender, body, at, read FROM sms
		WHERE operator = ? AND number = ? ORDER BY id DESC LIMIT ?`, op, number, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var list []message
	for rows.Next() {
		var m message
		var at string
		if err := rows.Scan(&m.ID, &m.Operator, &m.Number, &m.Sender, &m.Body, &at, &m.Read); err != nil {
			return nil, err
		}
		if m.At, err = time.Parse(bank.TimeLayout, at); err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

func (s *store) markRead(ctx context.Context, op bank.Operator, number string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sms SET read = 1 WHERE operator = ? AND number = ? AND read = 0", op, number)
	return err
}
