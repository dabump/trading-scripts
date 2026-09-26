package store

import (
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

	if err := s.UpdateMark(id, 5.00, 5.00, true); err != nil {
		t.Fatal(err)
	}
	// A lower peak must never overwrite a higher one, and the armed flag must not
	// be un-latched, but the last mark does follow the price down.
	if err := s.UpdateMark(id, 4.50, 4.50, false); err != nil {
		t.Fatal(err)
	}
	open, _ = s.OpenPositions()
	if open[0].PeakPrice != 5.00 {
		t.Errorf("peak = %v, want 5.00 to be retained", open[0].PeakPrice)
	}
	if !open[0].TrailArmed {
		t.Error("trail must stay armed once latched")
	}
	if open[0].LastPrice != 4.50 {
		t.Errorf("last price = %v, want 4.50 to track the current mark", open[0].LastPrice)
	}

	exitAt := entry.Add(2 * time.Hour)
	if err := s.ClosePosition(id, 4.75, exitAt, domain.ExitTrailingStop); err != nil {
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
	if closed.Open || closed.ExitReason != domain.ExitTrailingStop {
		t.Errorf("got open=%v reason=%q, want closed via trailing stop", closed.Open, closed.ExitReason)
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
