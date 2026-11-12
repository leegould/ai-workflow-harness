package harness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no cgo)
)

var ErrNotFound = errors.New("ticket not found")

// Store keeps tickets and their events in SQLite. Each ticket is a JSON
// document plus the two columns the engine queries on, so adding a field
// needs no migration.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS tickets (
	id         TEXT PRIMARY KEY,
	seq        INTEGER NOT NULL,
	status     TEXT NOT NULL,
	not_before INTEGER NOT NULL,
	data       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	ticket  TEXT NOT NULL,
	at      INTEGER NOT NULL,
	step    TEXT NOT NULL,
	kind    TEXT NOT NULL,
	message TEXT NOT NULL,
	detail  TEXT NOT NULL,
	cost    REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS events_ticket ON events (ticket, id);
`

// migrate brings an older database up to date. Events gained a model column
// after the first tickets ran (old rows get ""), and a step was renamed.
func migrate(db *sql.DB) error {
	ctx := context.Background()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('events') WHERE name = 'model'`).Scan(&n); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if n == 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE events ADD COLUMN model TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('tickets') WHERE name = 'occurrences'`).Scan(&n); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if n == 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE tickets ADD COLUMN occurrences INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// The readiness step was renamed refinement. Each statement only matches
	// rows still using the old name, so running it again changes nothing.
	for _, q := range []string{
		`UPDATE events SET step = 'refinement' WHERE step = 'readiness'`,
		`UPDATE tickets SET data = replace(data, '"step":"readiness"', '"step":"refinement"') WHERE instr(data, '"step":"readiness"') > 0`,
		`UPDATE proposals SET data = replace(data, '"step":"readiness"', '"step":"refinement"') WHERE instr(data, '"step":"readiness"') > 0`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// Open opens (or creates) the store at path. ":memory:" works for tests.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	// One connection: SQLite allows one writer anyway, and it keeps :memory:
	// databases from vanishing between connections.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), schema+proposalSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}
	return nil
}

// Create adds a ticket with the next ID (T-1, T-2, ...).
func (s *Store) Create(ctx context.Context, t Ticket) (Ticket, error) {
	var seq int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM tickets`).Scan(&seq); err != nil {
		return Ticket{}, fmt.Errorf("next ticket id: %w", err)
	}
	t.ID = fmt.Sprintf("T-%d", seq)
	data, err := json.Marshal(t)
	if err != nil {
		return Ticket{}, fmt.Errorf("encode ticket: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tickets (id, seq, status, not_before, data) VALUES (?, ?, ?, ?, ?)`,
		t.ID, seq, t.Status, t.NotBefore.UnixMilli(), data); err != nil {
		return Ticket{}, fmt.Errorf("insert ticket: %w", err)
	}
	return t, nil
}

// CreateReported creates a ticket from a source's report, or adds the report
// to the open ticket for the same problem. It's one transaction, so two
// reports of the same problem arriving together can't both open a ticket.
func (s *Store) CreateReported(ctx context.Context, t Ticket, maxOpen int) (Ticket, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, "", fmt.Errorf("begin report: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	const open = `status NOT IN ('done', 'abandoned')`
	var data string
	err = tx.QueryRowContext(ctx, `SELECT data FROM tickets WHERE `+open+` AND json_extract(data, '$.externalID') = ? ORDER BY seq DESC LIMIT 1`, t.ExternalID).Scan(&data)
	switch {
	case err == nil:
		existing, err := decode(data)
		if err != nil {
			return Ticket{}, "", err
		}
		// Only the count changes, in its own column: a worker may be mid-step
		// on this ticket, and rewriting its document here would race its save.
		if err := tx.QueryRowContext(ctx, `UPDATE tickets SET occurrences = occurrences + 1 WHERE id = ? RETURNING occurrences`, existing.ID).Scan(&existing.Occurrences); err != nil {
			return Ticket{}, "", fmt.Errorf("count report: %w", err)
		}
		return existing, ReportDuplicate, commit(tx)
	case !errors.Is(err, sql.ErrNoRows):
		return Ticket{}, "", fmt.Errorf("find open report: %w", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets WHERE `+open+` AND json_extract(data, '$.source') = ?`, t.Source).Scan(&n); err != nil {
		return Ticket{}, "", fmt.Errorf("count open reports: %w", err)
	}
	if maxOpen > 0 && n >= maxOpen {
		return Ticket{}, ReportLimited, nil
	}
	// A problem whose last ticket was deployed has come back: say so, and
	// point the agents at the change that didn't hold.
	var prev, merged string
	err = tx.QueryRowContext(ctx, `SELECT id, COALESCE(json_extract(data, '$.merged'), '') FROM tickets WHERE status = 'done' AND json_extract(data, '$.externalID') = ? ORDER BY seq DESC LIMIT 1`, t.ExternalID).Scan(&prev, &merged)
	switch {
	case err == nil:
		t.RegressionOf = prev
		t.Description = fmt.Sprintf("This came back after %s was deployed (merge %s). Look at what %s changed first: its fix didn't hold.\n\n%s", prev, short(merged), prev, t.Description)
	case !errors.Is(err, sql.ErrNoRows):
		return Ticket{}, "", fmt.Errorf("find earlier fix: %w", err)
	}
	var seq int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM tickets`).Scan(&seq); err != nil {
		return Ticket{}, "", fmt.Errorf("next ticket id: %w", err)
	}
	t.ID = fmt.Sprintf("T-%d", seq)
	raw, err := json.Marshal(t)
	if err != nil {
		return Ticket{}, "", fmt.Errorf("encode ticket: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tickets (id, seq, status, not_before, data, occurrences) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, seq, t.Status, t.NotBefore.UnixMilli(), raw, t.Occurrences); err != nil {
		return Ticket{}, "", fmt.Errorf("insert ticket: %w", err)
	}
	return t, ReportCreated, commit(tx)
}

func commit(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (Ticket, error) {
	var data string
	var occurrences int
	err := s.db.QueryRowContext(ctx, `SELECT data, occurrences FROM tickets WHERE id = ?`, id).Scan(&data, &occurrences)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("get %s: %w", id, err)
	}
	t, err := decode(data)
	t.Occurrences = occurrences
	return t, err
}

// List returns all tickets, newest first.
func (s *Store) List(ctx context.Context) ([]Ticket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data, occurrences FROM tickets ORDER BY seq DESC`)
	if err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	defer rows.Close()
	var ts []Ticket
	for rows.Next() {
		var data string
		var occurrences int
		if err := rows.Scan(&data, &occurrences); err != nil {
			return nil, fmt.Errorf("list tickets: %w", err)
		}
		t, err := decode(data)
		if err != nil {
			return nil, err
		}
		t.Occurrences = occurrences
		ts = append(ts, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	return ts, nil
}

func (s *Store) Save(ctx context.Context, t Ticket) error {
	return save(ctx, s.db, t)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func save(ctx context.Context, db execer, t Ticket) error {
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode %s: %w", t.ID, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tickets SET status = ?, not_before = ?, data = ? WHERE id = ?`,
		t.Status, t.NotBefore.UnixMilli(), data, t.ID); err != nil {
		return fmt.Errorf("save %s: %w", t.ID, err)
	}
	return nil
}

// Claim takes the oldest queued ticket that's due and marks it running, so
// two workers never run the same ticket.
func (s *Store) Claim(ctx context.Context, now time.Time) (Ticket, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, false, fmt.Errorf("claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var data string
	err = tx.QueryRowContext(ctx, `SELECT data FROM tickets WHERE status = ? AND not_before <= ? ORDER BY seq LIMIT 1`,
		Queued, now.UnixMilli()).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, fmt.Errorf("claim: %w", err)
	}
	t, err := decode(data)
	if err != nil {
		return Ticket{}, false, err
	}
	t.Status = Running
	if err := save(ctx, tx, t); err != nil {
		return Ticket{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Ticket{}, false, fmt.Errorf("claim: %w", err)
	}
	return t, true, nil
}

// Recover requeues tickets left running by a crash or restart. Their current
// step runs again from the start, so steps must be safe to repeat.
func (s *Store) Recover(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM tickets WHERE status = ?`, Running)
	if err != nil {
		return 0, fmt.Errorf("recover: %w", err)
	}
	var stuck []Ticket
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("recover: %w", err)
		}
		t, err := decode(data)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		stuck = append(stuck, t)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("recover: %w", err)
	}
	for _, t := range stuck {
		t.Status = Queued
		if err := s.Save(ctx, t); err != nil {
			return 0, err
		}
	}
	// A retro cut off mid-run just runs again.
	if _, err := s.db.ExecContext(ctx, `UPDATE tickets SET data = json_set(data, '$.retro', ?) WHERE json_extract(data, '$.retro') = ?`,
		RetroPending, RetroRunning); err != nil {
		return 0, fmt.Errorf("recover retros: %w", err)
	}
	return len(stuck), nil
}

func (s *Store) AddEvent(ctx context.Context, e Event) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO events (ticket, at, step, kind, message, detail, cost, model) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Ticket, e.At.UnixMilli(), e.Step, e.Kind, e.Message, e.Detail, e.CostUSD, e.By); err != nil {
		return fmt.Errorf("add event: %w", err)
	}
	return nil
}

// Events returns a ticket's timeline, oldest first.
func (s *Store) Events(ctx context.Context, id string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, ticket, at, step, kind, message, detail, cost, model FROM events WHERE ticket = ? ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("events %s: %w", id, err)
	}
	defer rows.Close()
	var es []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &e.Ticket, &at, &e.Step, &e.Kind, &e.Message, &e.Detail, &e.CostUSD, &e.By); err != nil {
			return nil, fmt.Errorf("events %s: %w", id, err)
		}
		e.At = time.UnixMilli(at)
		es = append(es, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events %s: %w", id, err)
	}
	return es, nil
}

func decode(data string) (Ticket, error) {
	var t Ticket
	if err := json.Unmarshal([]byte(data), &t); err != nil {
		return Ticket{}, fmt.Errorf("decode ticket: %w", err)
	}
	return t, nil
}
