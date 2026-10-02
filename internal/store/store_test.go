package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Migrations must be idempotent: the daemon reopens the same database on every
// restart.
func TestMigrateTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open must not re-apply migrations: %v", err)
	}
	defer s2.Close()

	if _, err := s2.Session("2026-09-28"); err != nil {
		t.Errorf("store unusable after reopen: %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newStore(t)

	rec, err := s.Session("2026-09-28")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verdict != domain.VerdictPending {
		t.Errorf("new session verdict = %q, want PENDING", rec.Verdict)
	}
	if rec.Halted {
		t.Error("new session must not be halted")
	}

	if err := s.SetVerdict("2026-09-28", domain.VerdictBearish, "avg -1.4% across SPY/QQQ/IWM"); err != nil {
		t.Fatal(err)
	}
	rec, err = s.Session("2026-09-28")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Halted || rec.Verdict != domain.VerdictBearish {
		t.Errorf("got verdict=%q halted=%v, want bearish and halted", rec.Verdict, rec.Halted)
	}
	if rec.HaltReason == "" {
		t.Error("halt reason must be retained for the status page")
	}

	// A proceeding verdict must clear the halt rather than leaving a stale reason.
	if err := s.SetVerdict("2026-09-28", domain.VerdictProceed, "ignored"); err != nil {
		t.Fatal(err)
	}
	rec, _ = s.Session("2026-09-28")
	if rec.Halted || rec.HaltReason != "" {
		t.Errorf("proceed must clear halt state, got halted=%v reason=%q", rec.Halted, rec.HaltReason)
	}
}

func TestSentimentReadingsRoundTrip(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 9, 28, 13, 40, 0, 0, time.UTC)

	// Inserted out of order to prove the query sorts.
	for _, r := range []domain.SentimentReading{
		{SessionDate: "2026-09-28", TakenAt: base.Add(20 * time.Minute),
			Classification: domain.VerdictProceed, Percentages: map[string]float64{"SPY": 0.4}},
		{SessionDate: "2026-09-28", TakenAt: base,
			Classification: domain.VerdictBearish, Percentages: map[string]float64{"SPY": -1.2, "IWM": -1.9}},
	} {
		if err := s.AddSentimentReading(r); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.SentimentReadings("2026-09-28")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d readings, want 2", len(got))
	}
	if !got[0].TakenAt.Equal(base) {
		t.Errorf("readings not ordered oldest-first: got %v first", got[0].TakenAt)
	}
	if got[0].Percentages["IWM"] != -1.9 {
		t.Errorf("percentages lost in round trip: %v", got[0].Percentages)
	}
	if len(got[0].Percentages) != 2 {
		t.Errorf("got %d percentages, want 2", len(got[0].Percentages))
	}

	if other, err := s.SentimentReadings("2026-09-29"); err != nil || len(other) != 0 {
		t.Errorf("readings must be scoped per session: got %d, err %v", len(other), err)
	}
}

func TestPositionLifecycle(t *testing.T) {
	s := newStore(t)
	entry := time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)

	id, err := s.InsertPosition(domain.Position{
		SessionDate: "2026-09-28", Symbol: "ABCD", Shares: 200,
		EntryPrice: 4.20, EntryTime: entry,
	})
	if err != nil {
		t.Fatal(err)
	}

	open, err := s.OpenPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("got %d open positions, want 1", len(open))
	}
	// Peak must default to entry so an immediate drawdown is measured correctly,
	// and the mark is seeded too so the status page has a price before the first
	// trading-loop tick.
	if open[0].PeakPrice != 4.20 {
		t.Errorf("peak = %v, want it seeded to the entry price", open[0].PeakPrice)
	}
	if open[0].LastPrice != 4.20 {
		t.Errorf("last price = %v, want it seeded to the entry price", open[0].LastPrice)
	}
	if !open[0].EntryTime.Equal(entry) {
		t.Errorf("entry time = %v, want %v", open[0].EntryTime, entry)
	}

	if err := s.UpdateMark(id, 5.00, 5.00); err != nil {
		t.Fatal(err)
	}
	// A lower peak must never overwrite a higher one, but the last mark does follow
	// the price down.
	if err := s.UpdateMark(id, 4.50, 4.50); err != nil {
		t.Fatal(err)
	}
	open, _ = s.OpenPositions()
	if open[0].PeakPrice != 5.00 {
		t.Errorf("peak = %v, want 5.00 to be retained", open[0].PeakPrice)
	}
	if open[0].LastPrice != 4.50 {
		t.Errorf("last price = %v, want 4.50 to track the current mark", open[0].LastPrice)
	}

	exitAt := entry.Add(2 * time.Hour)
	if err := s.ClosePosition(id, 4.75, exitAt, domain.ExitStopLoss); err != nil {
		t.Fatal(err)
	}
	open, _ = s.OpenPositions()
	if len(open) != 0 {
		t.Errorf("got %d open positions after close, want 0", len(open))
	}

	all, err := s.SessionPositions("2026-09-28")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d session positions, want 1", len(all))
	}
	closed := all[0]
	if closed.Open || closed.ExitReason != domain.ExitStopLoss {
		t.Errorf("got open=%v reason=%q, want closed via the stop-loss", closed.Open, closed.ExitReason)
	}
	// 200 shares from 4.20 to 4.75 is $110 and +13.10%.
	if got := closed.RealizedDollars(); got < 109.99 || got > 110.01 {
		t.Errorf("realized dollars = %.2f, want 110.00", got)
	}
	if got := closed.RealizedPct(); got < 13.09 || got > 13.10 {
		t.Errorf("realized pct = %.2f, want ~13.10", got)
	}
}

// Double-buying a symbol would corrupt exposure accounting, so the schema must
// refuse it.
func TestDuplicateOpenPositionRejected(t *testing.T) {
	s := newStore(t)
	p := domain.Position{SessionDate: "2026-09-28", Symbol: "ABCD", Shares: 10,
		EntryPrice: 5, EntryTime: time.Now()}

	if _, err := s.InsertPosition(p); err != nil {
		t.Fatal(err)
	}
	_, err := s.InsertPosition(p)
	if !errors.Is(err, ErrDuplicateOpenPosition) {
		t.Fatalf("second insert error = %v, want ErrDuplicateOpenPosition", err)
	}

	// Once closed, the same symbol may be inserted again (re-entry is a policy
	// decision for the risk package, not a storage constraint).
	open, _ := s.OpenPositions()
	if err := s.ClosePosition(open[0].ID, 5, time.Now(), domain.ExitForcedEOD); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertPosition(p); err != nil {
		t.Errorf("insert after close failed: %v", err)
	}
}

func TestScreenSnapshotKeepsOnlyLatest(t *testing.T) {
	s := newStore(t)
	t0 := time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)

	first := []domain.Evaluation{{Symbol: "AAAA", Qualifies: false, FailReason: "fails: News catalyst"}}
	second := []domain.Evaluation{
		{Symbol: "BBBB", Qualifies: true, VolumeMultiple: 6.1,
			Criteria: []domain.Criterion{{Name: "Rel. volume", Pass: true, Display: "6.1x"}}},
	}

	if err := s.SaveScreenSnapshot("2026-09-28", t0, first); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveScreenSnapshot("2026-09-28", t0.Add(time.Minute), second); err != nil {
		t.Fatal(err)
	}

	got, at, err := s.LatestScreenSnapshot("2026-09-28")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Symbol != "BBBB" {
		t.Fatalf("got %+v, want only the newest snapshot", got)
	}
	if !at.Equal(t0.Add(time.Minute)) {
		t.Errorf("taken at = %v, want %v", at, t0.Add(time.Minute))
	}
	if len(got[0].Criteria) != 1 || got[0].Criteria[0].Display != "6.1x" {
		t.Errorf("criteria lost in round trip: %+v", got[0].Criteria)
	}

	// The same pass re-saved with a changed outcome replaces itself: the setup check
	// does this whenever a candidate's Action changes between screens.
	second[0].Outcome = "no setup: not yet"
	if err := s.SaveScreenSnapshot("2026-09-28", t0.Add(time.Minute), second); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM screen_snapshots`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("%d snapshot rows after re-saving the same pass, want 1", rows)
	}
	if got, _, _ := s.LatestScreenSnapshot("2026-09-28"); got[0].Outcome != "no setup: not yet" {
		t.Errorf("outcome = %q, want the re-saved one", got[0].Outcome)
	}

	// An absent snapshot is not an error; the page just has nothing to show yet.
	empty, _, err := s.LatestScreenSnapshot("2026-09-29")
	if err != nil || empty != nil {
		t.Errorf("missing snapshot: got %v, %v; want nil, nil", empty, err)
	}
}

func TestUnresolvedOrders(t *testing.T) {
	s := newStore(t)
	rec := OrderRecord{
		ClientOrderID: "cid-1", SessionDate: "2026-09-28", Symbol: "ABCD",
		Side: "buy", Shares: 100, SubmittedAt: time.Now().UTC(), Status: "submitted",
	}
	if err := s.RecordOrder(rec); err != nil {
		t.Fatal(err)
	}

	pending, err := s.UnresolvedOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Symbol != "ABCD" {
		t.Fatalf("got %+v, want the submitted order", pending)
	}

	if err := s.UpdateOrderStatus("cid-1", "filled", "broker-9"); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.UnresolvedOrders()
	if len(pending) != 0 {
		t.Errorf("got %d unresolved after confirmation, want 0", len(pending))
	}
}

func TestSymbolsTradedOn(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	for _, sym := range []string{"AAAA", "BBBB"} {
		id, err := s.InsertPosition(domain.Position{
			SessionDate: "2026-09-28", Symbol: sym, Shares: 1, EntryPrice: 10, EntryTime: now})
		if err != nil {
			t.Fatal(err)
		}
		// Closing must not remove it from the traded-today set, otherwise a
		// stopped-out name could be re-bought the same day.
		if err := s.ClosePosition(id, 9, now, domain.ExitStopLoss); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.SymbolsTradedOn("2026-09-28")
	if err != nil {
		t.Fatal(err)
	}
	if !got["AAAA"] || !got["BBBB"] || len(got) != 2 {
		t.Errorf("got %v, want AAAA and BBBB", got)
	}
	if other, _ := s.SymbolsTradedOn("2026-09-29"); len(other) != 0 {
		t.Errorf("got %v for a different date, want empty", other)
	}
}

// The deployment target is a single binary pointed at a database that already
// exists, so a migration has to work as an upgrade and not only on a fresh file.
// 003 drops a column, which SQLite refuses outright in some conditions — a fresh-DB
// test would pass while every real deployment failed to start.
// legacyOrdersTable is the orders table as 001 created it, for the tests that seed an
// old schema by hand: later migrations alter it, so a seed without it cannot migrate.
const legacyOrdersTable = `CREATE TABLE orders (
	client_order_id TEXT PRIMARY KEY,
	session_date    TEXT NOT NULL,
	symbol          TEXT NOT NULL,
	side            TEXT NOT NULL,
	shares          INTEGER NOT NULL,
	submitted_at    TEXT NOT NULL,
	status          TEXT NOT NULL,
	broker_order_id TEXT NOT NULL DEFAULT ''
)`

func TestMigrationUpgradesAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Build the schema as it stood before 003, with a row in it, and mark 001 and
	// 002 as already applied so Open has to run 003 and nothing else.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`CREATE TABLE positions (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			session_date TEXT NOT NULL,
			symbol       TEXT NOT NULL,
			shares       INTEGER NOT NULL,
			entry_price  REAL NOT NULL,
			entry_time   TEXT NOT NULL,
			peak_price   REAL NOT NULL,
			trail_armed  INTEGER NOT NULL DEFAULT 0,
			is_open      INTEGER NOT NULL DEFAULT 1,
			exit_price   REAL NOT NULL DEFAULT 0,
			exit_time    TEXT NOT NULL DEFAULT '',
			exit_reason  TEXT NOT NULL DEFAULT '',
			last_price   REAL NOT NULL DEFAULT 0
		)`,
		`CREATE UNIQUE INDEX positions_one_open_per_symbol ON positions (symbol) WHERE is_open = 1`,
		legacyOrdersTable,
		`INSERT INTO positions
			(session_date, symbol, shares, entry_price, entry_time, peak_price,
			 trail_armed, is_open, last_price)
		 VALUES ('2026-09-28', 'ABCD', 200, 4.20, '2026-09-28T10:35:00Z', 5.00, 1, 1, 4.80)`,
		`INSERT INTO schema_migrations (name, applied_at) VALUES ('001_init.sql', '2026-01-01T00:00:00Z')`,
		`INSERT INTO schema_migrations (name, applied_at) VALUES ('002_last_price.sql', '2026-01-01T00:00:00Z')`,
	}
	for _, q := range stmts {
		if _, err := legacy.Exec(q); err != nil {
			t.Fatalf("seed legacy schema: %v\n%s", err, q)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating an existing database failed: %v", err)
	}
	defer s.Close()

	// The position survives the column drop, with its other fields intact.
	open, err := s.OpenPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("got %d open positions, want the pre-existing one", len(open))
	}
	got := open[0]
	if got.Symbol != "ABCD" || got.Shares != 200 || got.EntryPrice != 4.20 ||
		got.PeakPrice != 5.00 || got.LastPrice != 4.80 {
		t.Errorf("position did not survive the migration intact: %+v", got)
	}

	// The column is actually gone, not merely unread.
	if _, err := s.db.Exec(`SELECT trail_armed FROM positions`); err == nil {
		t.Error("trail_armed is still present after 003")
	}

	// The one-open-position-per-symbol guarantee must survive too: SQLite rebuilds
	// the table for a column drop, and a lost partial index would silently allow
	// double positions.
	_, err = s.InsertPosition(domain.Position{
		SessionDate: "2026-09-28", Symbol: "ABCD", Shares: 1, EntryPrice: 4.20,
		EntryTime: time.Now(),
	})
	if !errors.Is(err, ErrDuplicateOpenPosition) {
		t.Errorf("duplicate insert error = %v, want ErrDuplicateOpenPosition — the partial index did not survive", err)
	}
}

// Scaling out is a partial sale: shares leave, their profit is banked, and the
// position stays open. The latch is what stops two ticks banking the same target.
func TestScaleOutBanksPartOfAPosition(t *testing.T) {
	s := newStore(t)
	entry := time.Date(2026, 9, 28, 10, 35, 0, 0, time.UTC)

	id, err := s.InsertPosition(domain.Position{
		SessionDate: "2026-09-28", Symbol: "ABCD", Shares: 200,
		EntryPrice: 4.00, EntryTime: entry, StopPrice: 3.90, InitialRisk: 0.10,
	})
	if err != nil {
		t.Fatal(err)
	}

	open, _ := s.OpenPositions()
	if open[0].SharesOpen != 200 || open[0].StopPrice != 3.90 || open[0].InitialRisk != 0.10 {
		t.Fatalf("insert did not seed the position for scaling: %+v", open[0])
	}

	// Sell half at 4.20 and lift the stop to breakeven.
	if err := s.ScaleOut(id, 100, 4.20, 4.00); err != nil {
		t.Fatal(err)
	}
	open, _ = s.OpenPositions()
	got := open[0]
	if got.SharesOpen != 100 {
		t.Errorf("shares open = %d, want 100", got.SharesOpen)
	}
	if got.Shares != 200 {
		t.Errorf("original size = %d, want it preserved at 200", got.Shares)
	}
	if want := 20.0; got.BankedDollars < want-0.01 || got.BankedDollars > want+0.01 {
		t.Errorf("banked = %v, want %v (100 x $0.20)", got.BankedDollars, want)
	}
	if got.StopPrice != 4.00 {
		t.Errorf("stop = %v, want it moved to 4.00", got.StopPrice)
	}
	if !got.TargetHit {
		t.Error("the target must be latched")
	}

	// A second attempt must change nothing, which is what protects against two
	// ticks racing on the same target.
	err = s.ScaleOut(id, 50, 4.30, 4.00)
	if !errors.Is(err, ErrScaleOutNotApplicable) {
		t.Errorf("second scale-out error = %v, want ErrScaleOutNotApplicable", err)
	}
	open, _ = s.OpenPositions()
	if open[0].SharesOpen != 100 || open[0].BankedDollars > 20.01 {
		t.Errorf("a repeat scale-out changed the position: %+v", open[0])
	}

	// Closing folds the final leg into what was already banked.
	exitAt := entry.Add(3 * time.Hour)
	if err := s.ClosePosition(id, 5.00, exitAt, domain.ExitForcedEOD); err != nil {
		t.Fatal(err)
	}
	all, _ := s.SessionPositions("2026-09-28")
	closed := all[0]
	if closed.SharesOpen != 0 {
		t.Errorf("shares open = %d after close, want 0", closed.SharesOpen)
	}
	// $20 banked at 4.20 plus 100 x $1.00 on the runner.
	if want := 120.0; closed.RealizedDollars() < want-0.01 || closed.RealizedDollars() > want+0.01 {
		t.Errorf("realized = %v, want %v", closed.RealizedDollars(), want)
	}
	// 15% on the $800 the position committed, not the 25% the final price move
	// alone would suggest.
	if got := closed.RealizedPct(); got < 14.99 || got > 15.01 {
		t.Errorf("realized pct = %v, want 15 (on committed capital)", got)
	}
}

// A stop may only ever move up. One that could move down would let a losing position
// keep giving ground.
func TestMoveStopNeverLowersIt(t *testing.T) {
	s := newStore(t)
	id, err := s.InsertPosition(domain.Position{
		SessionDate: "2026-09-28", Symbol: "ABCD", Shares: 100,
		EntryPrice: 4.00, EntryTime: time.Now(), StopPrice: 3.90,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MoveStop(id, 4.10); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveStop(id, 3.50); err != nil {
		t.Fatal(err)
	}
	open, _ := s.OpenPositions()
	if open[0].StopPrice != 4.10 {
		t.Errorf("stop = %v, want 4.10 retained", open[0].StopPrice)
	}
}

// Migration 004 has to backfill, not just add columns: a database written by the
// previous version has positions whose shares_open would otherwise be zero, which
// reads as "nothing held" and would silently stop managing a live position.
func TestMigration004BackfillsExistingPositions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre004.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`CREATE TABLE positions (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			session_date TEXT NOT NULL,
			symbol       TEXT NOT NULL,
			shares       INTEGER NOT NULL,
			entry_price  REAL NOT NULL,
			entry_time   TEXT NOT NULL,
			peak_price   REAL NOT NULL,
			is_open      INTEGER NOT NULL DEFAULT 1,
			exit_price   REAL NOT NULL DEFAULT 0,
			exit_time    TEXT NOT NULL DEFAULT '',
			exit_reason  TEXT NOT NULL DEFAULT '',
			last_price   REAL NOT NULL DEFAULT 0
		)`,
		`CREATE UNIQUE INDEX positions_one_open_per_symbol ON positions (symbol) WHERE is_open = 1`,
		legacyOrdersTable,
		`INSERT INTO positions (session_date, symbol, shares, entry_price, entry_time,
			peak_price, is_open, last_price)
		 VALUES ('2026-09-28', 'HELD', 300, 4.00, '2026-09-28T10:35:00Z', 4.50, 1, 4.40)`,
		`INSERT INTO positions (session_date, symbol, shares, entry_price, entry_time,
			peak_price, is_open, last_price, exit_price, exit_reason)
		 VALUES ('2026-09-28', 'DONE', 100, 2.00, '2026-09-28T10:35:00Z', 2.30, 0, 2.20, 2.20, 'STOP_LOSS')`,
	}
	for _, n := range []string{"001_init.sql", "002_last_price.sql", "003_drop_trail_armed.sql"} {
		stmts = append(stmts,
			`INSERT INTO schema_migrations (name, applied_at) VALUES ('`+n+`', '2026-01-01T00:00:00Z')`)
	}
	for _, q := range stmts {
		if _, err := legacy.Exec(q); err != nil {
			t.Fatalf("seed legacy schema: %v\n%s", err, q)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("migrating an existing database failed: %v", err)
	}
	defer st.Close()

	open, err := st.OpenPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("got %d open positions, want the pre-existing one", len(open))
	}
	// The whole position is still held: anything less and the trading loop would sell
	// the wrong number of shares.
	if open[0].SharesOpen != 300 {
		t.Errorf("shares open = %d, want the full 300 backfilled", open[0].SharesOpen)
	}
	if open[0].BankedDollars != 0 {
		t.Errorf("banked = %v, want 0 on a position that never scaled", open[0].BankedDollars)
	}
	// No chart stop, which EvaluateExit reads as the percentage backstop being the
	// only floor — the behaviour this row was opened under.
	if open[0].StopPrice != 0 {
		t.Errorf("stop = %v, want 0 so the backstop governs", open[0].StopPrice)
	}

	all, err := st.SessionPositions("2026-09-28")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if p.Symbol != "DONE" {
			continue
		}
		// 100 shares from 2.00 to 2.20 is $20, which the backfill has to compute
		// because banked_dollars is where realised P&L now lives.
		if p.RealizedDollars() < 19.99 || p.RealizedDollars() > 20.01 {
			t.Errorf("closed position realized = %v, want 20 backfilled",
				p.RealizedDollars())
		}
		if p.SharesOpen != 0 {
			t.Errorf("closed position shares open = %d, want 0", p.SharesOpen)
		}
	}
}

func TestRecordFillStoresWhatExecuted(t *testing.T) {
	s := newStore(t)
	if err := s.RecordOrder(OrderRecord{
		ClientOrderID: "cid-1", SessionDate: "2026-09-29", Symbol: "SANG",
		Side: "buy", Shares: 488, SubmittedAt: time.Now().UTC(), Status: "submitted",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFill("cid-1", "filled", "broker-1", 5.00, 488); err != nil {
		t.Fatal(err)
	}
	status, price, shares, err := s.OrderFill("cid-1")
	if err != nil {
		t.Fatal(err)
	}
	if status != "filled" || price != 5.00 || shares != 488 {
		t.Errorf("got %s %v x %d, want filled 5.00 x 488", status, price, shares)
	}
	if pending, _ := s.UnresolvedOrders(); len(pending) != 0 {
		t.Errorf("a filled order is still unresolved: %+v", pending)
	}
}

// A partly filled exit banks what sold and leaves the rest held, without latching
// the target the way a scale-out does.
func TestReduceSharesBanksAPartialExit(t *testing.T) {
	s := newStore(t)
	id, err := s.InsertPosition(domain.Position{
		SessionDate: "2026-09-29", Symbol: "ABCD", Shares: 200,
		EntryPrice: 4.00, EntryTime: time.Now().UTC(), StopPrice: 3.90, InitialRisk: 0.10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReduceShares(id, 150, 3.90); err != nil {
		t.Fatal(err)
	}
	open, _ := s.OpenPositions()
	got := open[0]
	if got.SharesOpen != 50 || got.Shares != 200 || got.TargetHit {
		t.Errorf("got %+v, want 50 of 200 still open and no target latched", got)
	}
	if want := -15.0; got.BankedDollars < want-0.01 || got.BankedDollars > want+0.01 {
		t.Errorf("banked = %v, want %v (150 x -$0.10)", got.BankedDollars, want)
	}
	if err := s.ReduceShares(id, 50, 3.90); err == nil {
		t.Error("reducing by everything held must be refused: that is a close")
	}
}

// A manual position is flagged in the store, and "closed today" finds it by when it
// closed rather than the session it was opened in — it may have been held overnight.
func TestManualPositionClosedInALaterSession(t *testing.T) {
	s := newStore(t)
	opened := time.Date(2026, 9, 29, 14, 0, 0, 0, time.UTC)
	id, err := s.InsertPosition(domain.Position{
		SessionDate: "2026-09-29", Symbol: "HAND", Shares: 100,
		EntryPrice: 5, EntryTime: opened, StopPrice: 4.8, InitialRisk: 0.2, Manual: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PositionByID(id); !got.Manual {
		t.Fatal("the manual flag did not survive a round trip")
	}
	closedAt := time.Date(2026, 9, 30, 15, 0, 0, 123456789, time.UTC)
	if err := s.ClosePosition(id, 5.5, closedAt, domain.ExitManual); err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC) // midnight ET
	got, err := s.PositionsClosedBetween(day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Symbol != "HAND" {
		t.Errorf("closed on 2026-09-30 = %+v, want HAND though it opened the day before", got)
	}
	if got, _ := s.PositionsClosedBetween(day.AddDate(0, 0, -1), day); len(got) != 0 {
		t.Errorf("closed on 2026-09-29 = %+v, want nothing", got)
	}
}

// ClosedPositionExits is what the page's date arrows are built from: it lists the
// exit instants of closed positions, newest first, and ignores open ones.
func TestClosedPositionExits(t *testing.T) {
	s := newStore(t)
	insert := func(symbol string, at time.Time, close bool) {
		id, err := s.InsertPosition(domain.Position{
			SessionDate: at.Format("2006-01-02"), Symbol: symbol, Shares: 10,
			EntryPrice: 5, EntryTime: at,
		})
		if err != nil {
			t.Fatal(err)
		}
		if close {
			if err := s.ClosePosition(id, 6, at.Add(time.Hour), domain.ExitStopLoss); err != nil {
				t.Fatal(err)
			}
		}
	}
	first := time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)
	second := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	insert("AAAA", first, true)
	insert("BBBB", second, true)
	insert("OPEN", second, false)

	exits, err := s.ClosedPositionExits()
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Time{second.Add(time.Hour), first.Add(time.Hour)}
	if len(exits) != len(want) {
		t.Fatalf("exits = %v, want the two closed positions newest first", exits)
	}
	for i, w := range want {
		if !exits[i].Equal(w) {
			t.Errorf("exits[%d] = %s, want %s", i, exits[i], w)
		}
	}
}

// The protective stop's order id survives a round trip, and can be cleared. It is in
// the row rather than in memory because the order outlives the process: a restart
// that forgot the id would leave a live sell order nothing would ever cancel.
func TestStopOrderIDRoundTrips(t *testing.T) {
	s := newStore(t)

	id, err := s.InsertPosition(domain.Position{
		SessionDate: "2026-10-02", Symbol: "HAND", Shares: 100,
		EntryPrice: 4.20, EntryTime: time.Now(), StopPrice: 3.78,
		InitialRisk: 0.42, Manual: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if p, err := s.PositionByID(id); err != nil {
		t.Fatal(err)
	} else if p.StopOrderID != "" {
		t.Errorf("stop order id = %q on a fresh row, want empty", p.StopOrderID)
	}

	if err := s.SetStopOrderID(id, "o-1"); err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].StopOrderID != "o-1" {
		t.Fatalf("open = %+v, want the stop order id read back", open)
	}

	if err := s.SetStopOrderID(id, ""); err != nil {
		t.Fatal(err)
	}
	if p, err := s.PositionByID(id); err != nil {
		t.Fatal(err)
	} else if p.StopOrderID != "" {
		t.Errorf("stop order id = %q after clearing, want empty", p.StopOrderID)
	}
}
