package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/store"
)

type harness struct {
	t     *testing.T
	eng   *Engine
	fake  *broker.Fake
	store *store.Store
	cfg   *config.Config
	now   time.Time
	date  string
	open  time.Time
	close time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{}
	cfg.MarketData = config.MarketData{Feed: "sip"}
	cfg.Screening = config.Screening{MinIntradayPct: 10,
		MinVolumeMultiple: 5, AvgVolumeLookbackDays: 20, MaxEnriched: 100,
		NewsLookback: 18 * time.Hour}
	cfg.Risk = config.Risk{PositionSizePct: 10, MaxConcurrentPositions: 5, StopLossPct: 10}
	cfg.Exit = config.Exit{ProfitTargetPct: 15, TrailingStopPct: 5, MACDFast: 5, MACDSlow: 10,
		MACDSignal: 3, MACDIntervalMins: 15, EODExitOffsetMins: 30}
	cfg.Timing = config.Timing{SentimentPollInterval: 10 * time.Minute, SentimentWindow: time.Hour,
		ScreenerScanInterval: time.Minute, PositionPollInterval: 15 * time.Second}
	cfg.Sentiment = config.Sentiment{Symbols: []string{"SPY", "QQQ", "IWM"},
		BearishAvgPct: -0.8, RequireAllNegative: true}
	cfg.Execution = config.Execution{OrderType: "market"}

	h := &harness{
		t: t, store: st, cfg: cfg,
		date:  "2026-09-28",
		open:  time.Date(2026, 9, 28, 9, 30, 0, 0, scheduler.ET),
		close: time.Date(2026, 9, 28, 16, 0, 0, 0, scheduler.ET),
	}
	h.now = h.open.Add(-time.Hour)

	h.fake = broker.NewFake(domain.Account{PortfolioValue: 100_000, Cash: 100_000, Equity: 100_000})
	h.fake.SetCalendar(broker.CalendarDay{Date: h.date, Open: h.open, Close: h.close})

	h.eng = New(Deps{
		Config: cfg, Store: st, Data: h.fake, Trading: h.fake,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return h.now },
	})
	return h
}

func (h *harness) at(hh, mm int) {
	h.now = time.Date(2026, 9, 28, hh, mm, 0, 0, scheduler.ET)
}

func (h *harness) tick() {
	h.t.Helper()
	h.eng.Tick(context.Background())
	if state, err := h.eng.State(); state == domain.StateError {
		h.t.Fatalf("engine entered ERROR at %s: %s", h.now.Format("15:04"), err)
	}
}

func (h *harness) wantState(want domain.AgentState) {
	h.t.Helper()
	got, errMsg := h.eng.State()
	if got != want {
		h.t.Fatalf("state at %s = %q, want %q (%s)", h.now.Format("15:04"), got, want, errMsg)
	}
}

// setBullish makes the broad market read clearly positive.
func (h *harness) setBullish() {
	for _, s := range []string{"SPY", "QQQ", "IWM"} {
		h.fake.SetSnapshot(s, 101, 100, 1_000_000)
	}
}

func (h *harness) setBearish() {
	for _, s := range []string{"SPY", "QQQ", "IWM"} {
		h.fake.SetSnapshot(s, 98, 100, 1_000_000)
	}
}

// addCandidate registers a symbol that clears all four screening criteria.
func (h *harness) addCandidate(symbol string, price float64) {
	h.fake.SetSnapshot(symbol, price, price/1.14, 6_000_000) // +14%
	h.fake.SetAverageVolume(symbol, 1_000_000)               // 6x
	h.fake.SetNews(symbol, 2)
}

func (h *harness) openPositions() []domain.Position {
	h.t.Helper()
	pos, err := h.store.OpenPositions()
	if err != nil {
		h.t.Fatal(err)
	}
	return pos
}

// A full bullish day: gate passes, a candidate is bought, it runs up, the
// trailing stop takes it out, and the day ends flat.
func TestFullBullishDay(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	// Before the open nothing happens.
	h.at(9, 0)
	h.tick()
	h.wantState(domain.StateMarketClosed)
	if got := len(h.fake.Placed()); got != 0 {
		t.Fatalf("placed %d orders before the open, want 0", got)
	}

	// First hour: sentiment polls only, never a trade.
	for _, m := range []int{30, 40, 50} {
		h.at(9, m)
		h.tick()
		h.wantState(domain.StateSentimentCheck)
	}
	h.at(10, 20)
	h.tick()

	readings, err := h.store.SentimentReadings(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 4 {
		t.Errorf("got %d sentiment readings across the first hour, want 4", len(readings))
	}
	if got := len(h.fake.Placed()); got != 0 {
		t.Fatalf("placed %d orders during the first hour, want 0 (docs/strategy.md §1)", got)
	}
	if len(h.openPositions()) != 0 {
		t.Fatal("no positions may be open during the first hour")
	}

	// Gate opens: the candidate is bought.
	h.at(10, 31)
	h.tick()
	h.wantState(domain.StateScreening)

	rec, err := h.store.Session(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verdict != domain.VerdictProceed || rec.Halted {
		t.Fatalf("gate = %q halted=%v, want PROCEED", rec.Verdict, rec.Halted)
	}

	pos := h.openPositions()
	if len(pos) != 1 {
		t.Fatalf("got %d positions after the gate opened, want 1", len(pos))
	}
	// 10% of a $100k portfolio at $5.00 is 2000 shares.
	if pos[0].Symbol != "ABCD" || pos[0].Shares != 2000 {
		t.Errorf("position = %s x%d, want ABCD x2000", pos[0].Symbol, pos[0].Shares)
	}

	// Runs up past the +15% profit target, arming the trailing stop.
	h.at(11, 0)
	h.fake.SetPrice("ABCD", 6.00) // +20%
	h.tick()
	pos = h.openPositions()
	if len(pos) != 1 {
		t.Fatalf("position closed too early: %d open", len(pos))
	}
	if pos[0].PeakPrice != 6.00 {
		t.Errorf("peak = %v, want 6.00", pos[0].PeakPrice)
	}
	if !pos[0].TrailArmed {
		t.Error("trailing stop must arm once the profit target is reached")
	}

	// Falls 5% off the peak: the trailing stop fires.
	h.at(11, 30)
	h.fake.SetPrice("ABCD", 5.70)
	h.tick()
	if got := len(h.openPositions()); got != 0 {
		t.Fatalf("got %d open positions, want 0 after the trailing stop", got)
	}

	all, err := h.store.SessionPositions(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d session positions, want 1", len(all))
	}
	if all[0].ExitReason != domain.ExitTrailingStop {
		t.Errorf("exit reason = %q, want TRAILING_STOP", all[0].ExitReason)
	}
	// 2000 shares from $5.00 to $5.70 is +$1400.
	if got := all[0].RealizedDollars(); got < 1399.9 || got > 1400.1 {
		t.Errorf("realized = $%.2f, want $1400", got)
	}

	// Same-day re-entry is disabled by default, so it must not be re-bought.
	h.at(12, 0)
	h.tick()
	if got := len(h.openPositions()); got != 0 {
		t.Errorf("re-entered a symbol already traded today: %d open", got)
	}

	// After the close the agent is idle again.
	h.at(16, 30)
	h.tick()
	h.wantState(domain.StateMarketClosed)
}

// An overwhelmingly bearish first hour must halt the day with no entries.
func TestBearishGateHaltsTheDay(t *testing.T) {
	h := newHarness(t)
	h.setBearish()
	h.addCandidate("ABCD", 5.00)

	for _, m := range []int{30, 45} {
		h.at(9, m)
		h.tick()
	}
	h.at(10, 25)
	h.tick()

	h.at(10, 35)
	h.tick()
	h.wantState(domain.StateHaltedBearish)

	rec, _ := h.store.Session(h.date)
	if rec.Verdict != domain.VerdictBearish || !rec.Halted {
		t.Fatalf("gate = %q halted=%v, want bearish halt", rec.Verdict, rec.Halted)
	}
	if rec.HaltReason == "" {
		t.Error("halt reason must be recorded for the status page")
	}

	// Screening must not run for the rest of the day, even hours later.
	for _, hh := range []int{11, 13, 15} {
		h.at(hh, 0)
		h.tick()
		h.wantState(domain.StateHaltedBearish)
	}
	if got := len(h.fake.Placed()); got != 0 {
		t.Errorf("placed %d orders on a halted day, want 0", got)
	}
}

// A recovery during the first hour must not halt the day: the gate reads the
// final poll, not the worst one.
func TestGateUsesFinalReading(t *testing.T) {
	h := newHarness(t)

	h.setBearish()
	h.at(9, 30)
	h.tick()

	h.setBullish()
	h.at(10, 25)
	h.tick()

	h.at(10, 35)
	h.tick()
	h.wantState(domain.StateScreening)
}

// Starting the daemon after the first hour leaves the gate with no data. Trading
// without a sentiment check would skip a documented safety step, so the day must
// be sat out.
func TestStartedAfterFirstHourHaltsForSafety(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	h.at(13, 0)
	h.tick()
	h.wantState(domain.StateHaltedBearish)
	if got := len(h.fake.Placed()); got != 0 {
		t.Errorf("placed %d orders without a sentiment check, want 0", got)
	}
}

// Only five positions may be held at once, and the strongest relative volume
// wins the available slots.
func TestExposureCapAndRanking(t *testing.T) {
	h := newHarness(t)
	h.setBullish()

	// Seven candidates with increasing relative volume.
	for i, sym := range []string{"AAAA", "BBBB", "CCCC", "DDDD", "EEEE", "FFFF", "GGGG"} {
		h.addCandidate(sym, 5.00)
		h.fake.SetAverageVolume(sym, 6_000_000/float64(6+i)) // rel vol grows with i
	}

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	pos := h.openPositions()
	if len(pos) != 5 {
		t.Fatalf("got %d positions, want exactly 5 (the documented cap)", len(pos))
	}
	held := map[string]bool{}
	for _, p := range pos {
		held[p.Symbol] = true
	}
	// GGGG has the highest relative volume and must be taken; AAAA the lowest and
	// must be left out.
	if !held["GGGG"] {
		t.Error("highest relative-volume candidate was not taken")
	}
	if held["AAAA"] {
		t.Error("lowest relative-volume candidate took a slot from a stronger one")
	}
}

func TestStopLossExit(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()
	if len(h.openPositions()) != 1 {
		t.Fatal("expected a position to be open")
	}

	h.at(11, 0)
	h.fake.SetPrice("ABCD", 4.50) // exactly -10%
	h.tick()

	all, _ := h.store.SessionPositions(h.date)
	if len(all) != 1 || all[0].Open {
		t.Fatalf("position should be closed: %+v", all)
	}
	if all[0].ExitReason != domain.ExitStopLoss {
		t.Errorf("exit reason = %q, want STOP_LOSS", all[0].ExitReason)
	}
}

// A MACD bearish crossover closes a position that no other rule would touch.
func TestMACDExit(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()
	if len(h.openPositions()) != 1 {
		t.Fatal("expected a position to be open")
	}

	// 13 rising bars (the warm-up) then one sharp down bar: the cross lands on the
	// most recent candle. The price itself stays well inside the stop-loss and
	// below the profit target, so only the MACD rule can fire.
	closes := make([]float64, 13)
	for i := range closes {
		closes[i] = 5.00 + float64(i)*0.02
	}
	closes = append(closes, 4.95)
	h.fake.SetBars("ABCD", closes, h.open, 15)

	h.at(14, 0)
	h.fake.SetPrice("ABCD", 4.95) // -1%: no stop, no target
	h.tick()

	all, _ := h.store.SessionPositions(h.date)
	if len(all) != 1 || all[0].Open {
		t.Fatalf("position should be closed by the MACD rule: %+v", all)
	}
	if all[0].ExitReason != domain.ExitMACDCrossover {
		t.Errorf("exit reason = %q, want MACD_BEARISH_CROSS", all[0].ExitReason)
	}
}

// Everything must be flat before the close, whatever the P&L.
func TestForcedEODExit(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()
	if len(h.openPositions()) != 1 {
		t.Fatal("expected a position to be open")
	}

	// Sitting at a small gain: no other rule would close it.
	h.at(15, 0)
	h.fake.SetPrice("ABCD", 5.20)
	h.tick()
	if len(h.openPositions()) != 1 {
		t.Fatal("position closed before the EOD window")
	}

	h.at(15, 30)
	h.tick()
	h.wantState(domain.StateEODWindow)
	if got := len(h.openPositions()); got != 0 {
		t.Fatalf("got %d open positions in the EOD window, want 0", got)
	}

	all, _ := h.store.SessionPositions(h.date)
	if all[0].ExitReason != domain.ExitForcedEOD {
		t.Errorf("exit reason = %q, want FORCED_EOD", all[0].ExitReason)
	}
}

// Nothing should happen on a holiday, and the calendar must not be re-fetched
// endlessly.
func TestClosedDayDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.fake.SetCalendar(broker.CalendarDay{}) // exchange reports no session
	h.setBullish()

	h.at(11, 0)
	h.tick()
	h.wantState(domain.StateMarketClosed)
	if got := len(h.fake.Placed()); got != 0 {
		t.Errorf("placed %d orders on a closed day, want 0", got)
	}
	if _, known := h.eng.Session(); known {
		t.Error("a closed day must not report a tradable session")
	}
}

// Reconciliation adopts a broker position the store never recorded, which is what
// a crash between order submission and confirmation leaves behind.
func TestReconcileAdoptsUnknownBrokerPosition(t *testing.T) {
	h := newHarness(t)
	h.at(11, 0)
	h.fake.SeedPosition(broker.BrokerPosition{
		Symbol: "ORPH", Shares: 500, AvgEntry: 3.00, CurrentPrice: 3.20})
	h.fake.SetSnapshot("ORPH", 3.20, 3.00, 500_000)

	if err := h.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	pos := h.openPositions()
	if len(pos) != 1 {
		t.Fatalf("got %d positions, want the orphan adopted", len(pos))
	}
	if pos[0].Symbol != "ORPH" || pos[0].Shares != 500 || pos[0].EntryPrice != 3.00 {
		t.Errorf("adopted position = %+v, want ORPH x500 @ 3.00", pos[0])
	}
}

// The mirror case: the store thinks a position is open but the broker does not
// hold it. It must be closed as reconciled, not attributed to a strategy rule.
func TestReconcileClosesVanishedPosition(t *testing.T) {
	h := newHarness(t)
	h.at(11, 0)
	if _, err := h.store.InsertPosition(domain.Position{
		SessionDate: h.date, Symbol: "GONE", Shares: 100,
		EntryPrice: 2.00, EntryTime: h.now, PeakPrice: 2.00,
	}); err != nil {
		t.Fatal(err)
	}
	h.fake.SetSnapshot("GONE", 2.50, 2.00, 100_000)

	if err := h.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := len(h.openPositions()); got != 0 {
		t.Fatalf("got %d open positions, want 0", got)
	}
	all, _ := h.store.SessionPositions(h.date)
	if all[0].ExitReason != domain.ExitReconciled {
		t.Errorf("exit reason = %q, want RECONCILED rather than a strategy exit", all[0].ExitReason)
	}
	// Closed at the last known market price, not the entry price.
	if all[0].ExitPrice != 2.50 {
		t.Errorf("exit price = %v, want 2.50", all[0].ExitPrice)
	}
}

func TestReconcileResolvesUnconfirmedOrders(t *testing.T) {
	h := newHarness(t)
	h.at(11, 0)
	if err := h.store.RecordOrder(store.OrderRecord{
		ClientOrderID: "cid-x", SessionDate: h.date, Symbol: "ABCD",
		Side: "buy", Shares: 10, SubmittedAt: h.now, Status: "submitted",
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	unresolved, err := h.store.UnresolvedOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Errorf("got %d unresolved orders after reconciliation, want 0", len(unresolved))
	}
}

// A data-source failure must surface as ERROR on the page rather than being
// swallowed or crashing the daemon.
func TestBrokerFailureSurfacesAsError(t *testing.T) {
	h := newHarness(t)
	h.fake.SetError(errors.New("upstream 503"))

	h.at(9, 40)
	h.eng.Tick(context.Background())

	state, msg := h.eng.State()
	if state != domain.StateError {
		t.Fatalf("state = %q, want ERROR", state)
	}
	if msg == "" {
		t.Error("the error message must be retained for display")
	}
}

// Restarting mid-first-hour must not re-poll immediately or lose the schedule:
// the cadence is derived from stored readings, not in-memory state.
func TestSentimentCadenceSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.setBullish()

	h.at(9, 30)
	h.tick()

	// A "restart": a brand-new engine over the same store.
	h.eng = New(Deps{
		Config: h.cfg, Store: h.store, Data: h.fake, Trading: h.fake,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return h.now },
	})

	h.at(9, 35) // only 5 minutes later: not yet due
	h.tick()
	readings, _ := h.store.SentimentReadings(h.date)
	if len(readings) != 1 {
		t.Fatalf("got %d readings, want 1: the 10-minute cadence must survive a restart", len(readings))
	}

	h.at(9, 41)
	h.tick()
	readings, _ = h.store.SentimentReadings(h.date)
	if len(readings) != 2 {
		t.Fatalf("got %d readings, want 2 once the interval elapsed", len(readings))
	}
}
