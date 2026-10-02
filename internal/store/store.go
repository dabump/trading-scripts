// Package store is the SQLite persistence layer. The daemon must survive a
// restart mid-session, so anything the trading loop would otherwise hold only in
// memory lives here.
package store

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the build CGO-free

	"github.com/martincoetzee/trading-agent/internal/domain"
)

//go:embed all:migrations
var migrationsFS embed.FS

// ErrDuplicateOpenPosition is returned when a second open position in the same
// symbol is attempted.
var ErrDuplicateOpenPosition = errors.New("a position in this symbol is already open")

// ErrScaleOutNotApplicable means the partial sale changed nothing: the position was
// already closed, had already banked its target, or has too few shares left to split.
// Treated as benign by callers — it is what a duplicate attempt looks like.
var ErrScaleOutNotApplicable = errors.New("position cannot be scaled out of")

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	// _txlock=immediate avoids SQLITE_BUSY surprises when the web handler reads
	// while the trading loop writes.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// A single writer keeps WAL contention predictable; reads still go through the
	// same pool but are short.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).
			Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if count > 0 {
			continue
		}

		body, err := migrationsFS.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			name, nowUTC()); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// SessionRecord is the per-day state the gate and kill switch depend on.
type SessionRecord struct {
	Date       string
	Verdict    domain.Verdict
	Halted     bool
	HaltReason string
}

// Session returns the record for a date, creating a pending one if absent.
func (s *Store) Session(date string) (SessionRecord, error) {
	if _, err := s.db.Exec(
		`INSERT INTO sessions (session_date, verdict, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(session_date) DO NOTHING`,
		date, string(domain.VerdictPending), nowUTC()); err != nil {
		return SessionRecord{}, fmt.Errorf("ensure session: %w", err)
	}

	var rec SessionRecord
	var halted int
	err := s.db.QueryRow(
		`SELECT session_date, verdict, halted, halt_reason FROM sessions WHERE session_date = ?`, date).
		Scan(&rec.Date, &rec.Verdict, &halted, &rec.HaltReason)
	if err != nil {
		return SessionRecord{}, fmt.Errorf("load session: %w", err)
	}
	rec.Halted = halted == 1
	return rec, nil
}

// SetVerdict records the gate outcome, halting the session when bearish.
func (s *Store) SetVerdict(date string, v domain.Verdict, haltReason string) error {
	halted := 0
	if v == domain.VerdictBearish {
		halted = 1
	} else {
		haltReason = ""
	}
	_, err := s.db.Exec(
		`UPDATE sessions SET verdict = ?, halted = ?, halt_reason = ?, updated_at = ?
		 WHERE session_date = ?`,
		string(v), halted, haltReason, nowUTC(), date)
	if err != nil {
		return fmt.Errorf("set verdict: %w", err)
	}
	return nil
}

// AddSentimentReading appends one first-hour poll.
func (s *Store) AddSentimentReading(r domain.SentimentReading) error {
	payload, err := json.Marshal(r.Percentages)
	if err != nil {
		return fmt.Errorf("encode percentages: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO sentiment_readings (session_date, taken_at, classification, percentages)
		 VALUES (?, ?, ?, ?)`,
		r.SessionDate, formatTime(r.TakenAt), string(r.Classification), string(payload))
	if err != nil {
		return fmt.Errorf("insert sentiment reading: %w", err)
	}
	return nil
}

// SentimentReadings returns a session's readings, oldest first.
func (s *Store) SentimentReadings(date string) ([]domain.SentimentReading, error) {
	rows, err := s.db.Query(
		`SELECT session_date, taken_at, classification, percentages
		 FROM sentiment_readings WHERE session_date = ? ORDER BY taken_at`, date)
	if err != nil {
		return nil, fmt.Errorf("query sentiment readings: %w", err)
	}
	defer rows.Close()

	var out []domain.SentimentReading
	for rows.Next() {
		var r domain.SentimentReading
		var takenAt, payload string
		if err := rows.Scan(&r.SessionDate, &takenAt, &r.Classification, &payload); err != nil {
			return nil, err
		}
		r.TakenAt = parseTime(takenAt)
		if err := json.Unmarshal([]byte(payload), &r.Percentages); err != nil {
			return nil, fmt.Errorf("decode percentages: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertPosition records a newly opened position.
func (s *Store) InsertPosition(p domain.Position) (int64, error) {
	peak := p.PeakPrice
	if peak < p.EntryPrice {
		peak = p.EntryPrice
	}
	// last_price is seeded to the entry price so the status page shows a sensible
	// mark for a position opened between two trading-loop ticks.
	res, err := s.db.Exec(
		`INSERT INTO positions
		 (session_date, symbol, shares, shares_open, entry_price, entry_time, peak_price,
		  last_price, stop_price, initial_risk, manual, is_open)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		p.SessionDate, p.Symbol, p.Shares, p.Shares, p.EntryPrice, formatTime(p.EntryTime),
		peak, p.EntryPrice, p.StopPrice, p.InitialRisk, boolInt(p.Manual))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("%w: %s", ErrDuplicateOpenPosition, p.Symbol)
		}
		return 0, fmt.Errorf("insert position: %w", err)
	}
	return res.LastInsertId()
}

// UpdateMark records the latest observed price and raises the high-water mark. The
// peak only ever moves up, so a stale or out-of-order tick cannot undo it.
func (s *Store) UpdateMark(id int64, lastPrice, peak float64) error {
	_, err := s.db.Exec(
		`UPDATE positions
		 SET last_price = ?, peak_price = MAX(peak_price, ?)
		 WHERE id = ?`, lastPrice, peak, id)
	if err != nil {
		return fmt.Errorf("update mark: %w", err)
	}
	return nil
}

// ScaleOut records a partial sale: shares leave the position, their profit or loss
// is banked, and the position stays open.
//
// The share count is decremented rather than set, and `target_hit` is latched here
// rather than by the caller, so two ticks racing on the same target cannot bank the
// same shares twice. The WHERE clause is what enforces it: the second update matches
// nothing once the first has latched the flag.
func (s *Store) ScaleOut(id int64, shares int, price float64, newStop float64) error {
	if shares < 1 {
		return fmt.Errorf("scale out: %d shares is not a partial sale", shares)
	}
	res, err := s.db.Exec(
		`UPDATE positions
		 SET shares_open    = shares_open - ?,
		     banked_dollars = banked_dollars + (? - entry_price) * ?,
		     stop_price     = MAX(stop_price, ?),
		     target_hit     = 1,
		     last_price     = ?
		 WHERE id = ? AND is_open = 1 AND target_hit = 0 AND shares_open > ?`,
		shares, price, shares, newStop, price, id, shares)
	if err != nil {
		return fmt.Errorf("scale out: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: position %d", ErrScaleOutNotApplicable, id)
	}
	return nil
}

// ClosePosition marks a position closed with its realized outcome, folding the final
// leg into whatever earlier partial sales already banked.
func (s *Store) ClosePosition(id int64, exitPrice float64, exitTime time.Time, reason domain.ExitReason) error {
	_, err := s.db.Exec(
		`UPDATE positions
		 SET is_open        = 0,
		     banked_dollars = banked_dollars + (? - entry_price) * shares_open,
		     shares_open    = 0,
		     exit_price     = ?,
		     exit_time      = ?,
		     exit_reason    = ?
		 WHERE id = ? AND is_open = 1`,
		exitPrice, exitPrice, formatTime(exitTime), string(reason), id)
	if err != nil {
		return fmt.Errorf("close position: %w", err)
	}
	return nil
}

// ReduceShares records an exit that only partly filled: the shares that sold leave
// the position with their profit or loss banked, and the rest stays open for the
// exit rule to sell on the next tick. Unlike ScaleOut it latches nothing, because no
// target was reached.
func (s *Store) ReduceShares(id int64, shares int, price float64) error {
	res, err := s.db.Exec(
		`UPDATE positions
		 SET shares_open    = shares_open - ?,
		     banked_dollars = banked_dollars + (? - entry_price) * ?,
		     last_price     = ?
		 WHERE id = ? AND is_open = 1 AND shares_open > ?`,
		shares, price, shares, price, id, shares)
	if err != nil {
		return fmt.Errorf("reduce shares: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("reduce shares: position %d is not open with more than %d shares", id, shares)
	}
	return nil
}

// MoveStop raises a position's working stop. It never lowers one: a stop that could
// move down would let a losing position keep giving ground.
func (s *Store) MoveStop(id int64, stop float64) error {
	_, err := s.db.Exec(
		`UPDATE positions SET stop_price = MAX(stop_price, ?) WHERE id = ? AND is_open = 1`,
		stop, id)
	if err != nil {
		return fmt.Errorf("move stop: %w", err)
	}
	return nil
}

// SetStopOrderID records the broker order id of the protective stop resting against
// a position, or clears it with an empty string once that order is cancelled or
// filled.
//
// It is stored rather than held in memory because the order outlives the process: a
// restart that forgot the id would leave a live sell order at the broker that
// nothing would ever cancel, and the next exit would sell the position twice.
func (s *Store) SetStopOrderID(id int64, orderID string) error {
	_, err := s.db.Exec(`UPDATE positions SET stop_order_id = ? WHERE id = ?`, orderID, id)
	if err != nil {
		return fmt.Errorf("set stop order id: %w", err)
	}
	return nil
}

func (s *Store) scanPositions(rows *sql.Rows) ([]domain.Position, error) {
	defer rows.Close()
	var out []domain.Position
	for rows.Next() {
		var p domain.Position
		var entryTime, exitTime string
		var isOpen, targetHit, manual int
		if err := rows.Scan(&p.ID, &p.SessionDate, &p.Symbol, &p.Shares, &p.SharesOpen,
			&p.EntryPrice, &entryTime, &p.PeakPrice, &p.LastPrice, &p.StopPrice,
			&p.InitialRisk, &targetHit, &p.BankedDollars, &isOpen, &p.ExitPrice,
			&exitTime, &p.ExitReason, &manual, &p.StopOrderID); err != nil {
			return nil, err
		}
		p.EntryTime = parseTime(entryTime)
		p.ExitTime = parseTime(exitTime)
		p.TargetHit = targetHit == 1
		p.Open = isOpen == 1
		p.Manual = manual == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

const positionColumns = `id, session_date, symbol, shares, shares_open, entry_price,
	entry_time, peak_price, last_price, stop_price, initial_risk, target_hit,
	banked_dollars, is_open, exit_price, exit_time, exit_reason, manual, stop_order_id`

// OpenPositions returns every currently-held position, regardless of session.
func (s *Store) OpenPositions() ([]domain.Position, error) {
	rows, err := s.db.Query(`SELECT ` + positionColumns + ` FROM positions WHERE is_open = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query open positions: %w", err)
	}
	return s.scanPositions(rows)
}

// ErrPositionNotFound means no position carries that id.
var ErrPositionNotFound = errors.New("position not found")

// PositionByID returns one position, open or closed.
//
// The manual close needs it: the page hands back an id, and acting on it means
// reading the current row rather than trusting what the browser last rendered, which
// may be up to a poll interval old and may describe a position the trading loop has
// since exited.
func (s *Store) PositionByID(id int64) (domain.Position, error) {
	rows, err := s.db.Query(`SELECT `+positionColumns+` FROM positions WHERE id = ?`, id)
	if err != nil {
		return domain.Position{}, fmt.Errorf("query position %d: %w", id, err)
	}
	positions, err := s.scanPositions(rows)
	if err != nil {
		return domain.Position{}, err
	}
	if len(positions) == 0 {
		return domain.Position{}, fmt.Errorf("%w: %d", ErrPositionNotFound, id)
	}
	return positions[0], nil
}

// PositionsClosedBetween returns the positions closed at or after from and before to,
// whatever session they were opened in. A manual position can be held overnight, so
// "closed today" is no longer the same set as "opened today and closed".
func (s *Store) PositionsClosedBetween(from, to time.Time) ([]domain.Position, error) {
	// exit_time is RFC 3339 with trimmed nanoseconds, which does not sort exactly as
	// text within one second; the SQL bound is a coarse prefilter and the exact
	// comparison is done on parsed times.
	rows, err := s.db.Query(`SELECT `+positionColumns+
		` FROM positions WHERE is_open = 0 AND exit_time >= ? ORDER BY id`,
		formatTime(from.Add(-time.Second)))
	if err != nil {
		return nil, fmt.Errorf("query closed positions: %w", err)
	}
	all, err := s.scanPositions(rows)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, p := range all {
		if !p.ExitTime.Before(from) && p.ExitTime.Before(to) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ClosedPositionExits returns the exit instant of every closed position, newest
// first. The page's date arrows need to know which days have something to show,
// and that is a calendar question: the dates are grouped by ET session day in the
// web layer, which owns the exchange timezone. Doing it in SQL would group by the
// stored UTC day, and an after-hours or manual close at 20:00 ET falls on the next
// UTC day — filed under a date the operator never traded.
func (s *Store) ClosedPositionExits() ([]time.Time, error) {
	rows, err := s.db.Query(
		`SELECT exit_time FROM positions WHERE is_open = 0 AND exit_time != ''`)
	if err != nil {
		return nil, fmt.Errorf("query closed position exits: %w", err)
	}
	defer rows.Close()

	var out []time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan closed position exit: %w", err)
		}
		if t := parseTime(raw); !t.IsZero() {
			out = append(out, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate closed position exits: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].After(out[j]) })
	return out, nil
}

// SessionPositions returns all positions opened on a date, open or closed.
func (s *Store) SessionPositions(date string) ([]domain.Position, error) {
	rows, err := s.db.Query(`SELECT `+positionColumns+
		` FROM positions WHERE session_date = ? ORDER BY id`, date)
	if err != nil {
		return nil, fmt.Errorf("query session positions: %w", err)
	}
	return s.scanPositions(rows)
}

// SaveScreenSnapshot stores the latest screening pass for the web page.
func (s *Store) SaveScreenSnapshot(date string, takenAt time.Time, evals []domain.Evaluation) error {
	payload, err := json.Marshal(evals)
	if err != nil {
		return fmt.Errorf("encode evaluations: %w", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO screen_snapshots (session_date, taken_at, payload) VALUES (?, ?, ?)`,
		date, formatTime(takenAt), string(payload)); err != nil {
		return fmt.Errorf("insert screen snapshot: %w", err)
	}
	// Only the newest pass is displayed, and a 1-minute scan cadence would
	// otherwise accumulate ~390 rows a day for no reader.
	_, err = s.db.Exec(
		`DELETE FROM screen_snapshots WHERE session_date = ? AND taken_at < ?`,
		date, formatTime(takenAt))
	if err != nil {
		return fmt.Errorf("prune screen snapshots: %w", err)
	}
	return nil
}

// LatestScreenSnapshot returns the most recent screening pass for a date.
func (s *Store) LatestScreenSnapshot(date string) ([]domain.Evaluation, time.Time, error) {
	var payload, takenAt string
	err := s.db.QueryRow(
		`SELECT payload, taken_at FROM screen_snapshots WHERE session_date = ?
		 ORDER BY taken_at DESC LIMIT 1`, date).Scan(&payload, &takenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("query screen snapshot: %w", err)
	}
	var evals []domain.Evaluation
	if err := json.Unmarshal([]byte(payload), &evals); err != nil {
		return nil, time.Time{}, fmt.Errorf("decode evaluations: %w", err)
	}
	return evals, parseTime(takenAt), nil
}

// OrderRecord tracks a submitted order so a crash mid-submission is recoverable.
type OrderRecord struct {
	ClientOrderID string
	SessionDate   string
	Symbol        string
	Side          string
	Shares        int
	SubmittedAt   time.Time
	Status        string
	BrokerOrderID string
}

// RecordOrder writes an order's intent before it is sent to the broker.
func (s *Store) RecordOrder(r OrderRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO orders
		 (client_order_id, session_date, symbol, side, shares, submitted_at, status, broker_order_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ClientOrderID, r.SessionDate, r.Symbol, r.Side, r.Shares,
		formatTime(r.SubmittedAt), r.Status, r.BrokerOrderID)
	if err != nil {
		return fmt.Errorf("record order: %w", err)
	}
	return nil
}

// UpdateOrderStatus records what the broker said about an order.
func (s *Store) UpdateOrderStatus(clientOrderID, status, brokerOrderID string) error {
	_, err := s.db.Exec(
		`UPDATE orders SET status = ?, broker_order_id = ? WHERE client_order_id = ?`,
		status, brokerOrderID, clientOrderID)
	if err != nil {
		return fmt.Errorf("update order status: %w", err)
	}
	return nil
}

// RecordFill stores the outcome the broker reported for an order: its final status
// and what actually executed.
func (s *Store) RecordFill(clientOrderID, status, brokerOrderID string, price float64, shares int) error {
	_, err := s.db.Exec(
		`UPDATE orders SET status = ?, broker_order_id = ?, filled_price = ?, filled_shares = ?
		 WHERE client_order_id = ?`,
		status, brokerOrderID, price, shares, clientOrderID)
	if err != nil {
		return fmt.Errorf("record fill: %w", err)
	}
	return nil
}

// OrderFill returns what RecordFill stored for an order.
func (s *Store) OrderFill(clientOrderID string) (status string, price float64, shares int, err error) {
	err = s.db.QueryRow(
		`SELECT status, filled_price, filled_shares FROM orders WHERE client_order_id = ?`,
		clientOrderID).Scan(&status, &price, &shares)
	if err != nil {
		return "", 0, 0, fmt.Errorf("order fill: %w", err)
	}
	return status, price, shares, nil
}

// UnresolvedOrders returns orders that were recorded but never confirmed, which
// is what restart reconciliation has to investigate: 'submitted' never reached the
// broker's acknowledgement, and 'unconfirmed' was acknowledged but its fill was never
// learned.
func (s *Store) UnresolvedOrders() ([]OrderRecord, error) {
	rows, err := s.db.Query(
		`SELECT client_order_id, session_date, symbol, side, shares, submitted_at, status, broker_order_id
		 FROM orders WHERE status IN ('submitted', 'unconfirmed') ORDER BY submitted_at`)
	if err != nil {
		return nil, fmt.Errorf("query unresolved orders: %w", err)
	}
	defer rows.Close()

	var out []OrderRecord
	for rows.Next() {
		var r OrderRecord
		var submittedAt string
		if err := rows.Scan(&r.ClientOrderID, &r.SessionDate, &r.Symbol, &r.Side,
			&r.Shares, &submittedAt, &r.Status, &r.BrokerOrderID); err != nil {
			return nil, err
		}
		r.SubmittedAt = parseTime(submittedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SymbolsTradedOn lists every symbol with a position opened on a date, used to
// enforce the same-day re-entry rule across restarts.
func (s *Store) SymbolsTradedOn(date string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT DISTINCT symbol FROM positions WHERE session_date = ?`, date)
	if err != nil {
		return nil, fmt.Errorf("query traded symbols: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			return nil, err
		}
		out[sym] = true
	}
	return out, rows.Err()
}
