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
		NewsLookback: 18 * time.Hour, MinPrice: 1, MaxPrice: 20,
		MinDollarVolume: 1_000_000}
	cfg.Risk = config.Risk{RiskPerTradePct: 1, MaxPositionPct: 33,
		MaxConcurrentPositions: 3, StopLossPct: 10}
	cfg.Exit = config.Exit{FirstTargetR: 2, FirstTargetFraction: 0.5,
		BreakevenAfterTarget: true, EODExitOffsetMins: 30}
	cfg.Entry = config.Entry{PatternInterval: time.Minute, EMAPeriod: 9,
		MinPullbackBars: 1, MaxPullbackBars: 5, StopBufferPct: 0.1,
		MinStopDistancePct: 0.5, MaxStopDistancePct: 4}
	cfg.Timing = config.Timing{SentimentPollInterval: 10 * time.Minute,
		SentimentWindow: time.Hour, EntryWindow: 4 * time.Hour,
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

// addCandidate registers a symbol that clears every screening criterion and also
// prints a valid setup, so it is actually buyable.
//
// Screening alone is no longer enough to produce an entry: the chart has to show a
// pullback and resumption, which is what defines the stop and therefore the size.
func (h *harness) addCandidate(symbol string, price float64) {
	h.fake.SetSnapshot(symbol, price, price/1.14, 6_000_000) // +14%
	h.fake.SetAverageVolume(symbol, 1_000_000)               // 6x
	h.fake.SetNews(symbol, 2)
	h.fake.SetSetupBars(symbol, price, h.open, h.cfg.Entry.PatternInterval)
}

// addScreenedOnly registers a symbol that passes the screen but never sets up, for
// asserting that the setup gate is what stops it being bought.
func (h *harness) addScreenedOnly(symbol string, price float64) {
	h.fake.SetSnapshot(symbol, price, price/1.14, 6_000_000)
	h.fake.SetAverageVolume(symbol, 1_000_000)
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
	// Sized from the stop, then capped: 1% of $100k is $1,000 of risk, and the
	// setup's stop is about $0.146 below the $5.00 entry, which asks for ~6,800
	// shares — more than the 33% notional cap allows, so the cap decides at 6,600.
	if pos[0].Symbol != "ABCD" {
		t.Fatalf("position = %s, want ABCD", pos[0].Symbol)
	}
	if got := float64(pos[0].Shares) * pos[0].EntryPrice; got > 33_000.01 {
		t.Errorf("notional $%.2f exceeds the 33%% position cap", got)
	}
	if pos[0].StopPrice <= 0 || pos[0].StopPrice >= pos[0].EntryPrice {
		t.Errorf("stop = %v, want it below the %v entry — the setup has to define it",
			pos[0].StopPrice, pos[0].EntryPrice)
	}
	if pos[0].InitialRisk <= 0 {
		t.Error("initial risk must be recorded, or profit targets have no unit")
	}
	entryShares := pos[0].Shares
	entryStop := pos[0].StopPrice

	// Runs well past the first target. Half is banked and the stop moves to
	// breakeven; the rest keeps running, which is the whole point of scaling rather
	// than taking a fixed profit.
	h.at(11, 0)
	h.fake.SetPrice("ABCD", 6.00) // far beyond 2R
	h.tick()
	pos = h.openPositions()
	if len(pos) != 1 {
		t.Fatalf("scaling out must not close the position: %d open", len(pos))
	}
	if pos[0].Shares != entryShares {
		t.Errorf("original size = %d, want it preserved at %d", pos[0].Shares, entryShares)
	}
	if pos[0].SharesOpen != entryShares/2 {
		t.Errorf("shares open = %d, want half of %d", pos[0].SharesOpen, entryShares)
	}
	if !pos[0].TargetHit {
		t.Error("the target must latch so it cannot be banked twice")
	}
	// The balance the status page shows tracks the account: the buy consumed cash, and
	// the next tick's refresh picked that up.
	if snap := h.eng.Account(); !snap.Known || snap.Account.Cash >= 100_000 {
		t.Errorf("published balance = %+v, want the cash the buy consumed", snap)
	}
	if pos[0].BankedDollars <= 0 {
		t.Errorf("banked = %v, want the scale-out profit recorded", pos[0].BankedDollars)
	}
	if pos[0].StopPrice != pos[0].EntryPrice {
		t.Errorf("stop = %v, want it moved to the %v entry after the target",
			pos[0].StopPrice, pos[0].EntryPrice)
	}
	if pos[0].StopPrice <= entryStop {
		t.Error("the stop must have moved up, not down")
	}
	if pos[0].PeakPrice != 6.00 {
		t.Errorf("peak = %v, want 6.00", pos[0].PeakPrice)
	}

	// A second tick at the same price must not bank the target again.
	h.at(11, 15)
	h.tick()
	if got := h.openPositions()[0].SharesOpen; got != entryShares/2 {
		t.Errorf("shares open = %d, want the target to pay only once", got)
	}

	h.at(11, 30)
	h.fake.SetPrice("ABCD", 5.70) // well above breakeven: the runner stays
	h.tick()
	if got := len(h.openPositions()); got != 1 {
		t.Fatalf("the runner must survive a pullback that holds above breakeven: %d open", got)
	}

	// Letting it run is the point of removing those rules: the rest of the move is
	// what pays for the losing trades.
	h.at(14, 0)
	h.fake.SetPrice("ABCD", 7.00) // +40%
	h.tick()
	if got := len(h.openPositions()); got != 1 {
		t.Fatalf("position closed before the bell: %d open", got)
	}
	if pos = h.openPositions(); pos[0].PeakPrice != 7.00 {
		t.Errorf("peak = %v, want it to follow the price up to 7.00", pos[0].PeakPrice)
	}

	// The forced end-of-day exit is what finally closes it.
	h.at(15, 30)
	h.tick()
	if got := len(h.openPositions()); got != 0 {
		t.Fatalf("got %d open positions after the EOD exit, want 0", got)
	}

	all, err := h.store.SessionPositions(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d session positions, want 1", len(all))
	}
	if all[0].ExitReason != domain.ExitForcedEOD {
		t.Errorf("exit reason = %q, want FORCED_EOD", all[0].ExitReason)
	}
	// Half was banked near 2R and half rode to $7.00, so the realised total is the
	// sum of two fills rather than one price move.
	half := float64(entryShares / 2)
	wantRunner := half * (7.00 - all[0].EntryPrice)
	if got := all[0].RealizedDollars(); got <= wantRunner {
		t.Errorf("realized $%.2f, want more than the $%.2f runner alone — the banked half is missing",
			got, wantRunner)
	}
	if all[0].SharesOpen != 0 {
		t.Errorf("shares open = %d after close, want 0", all[0].SharesOpen)
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
	if len(pos) != h.cfg.Risk.MaxConcurrentPositions {
		t.Fatalf("got %d positions, want exactly %d (the documented cap)",
			len(pos), h.cfg.Risk.MaxConcurrentPositions)
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

	// Same-day re-entry is disabled by default, so a name that just stopped out
	// must not be bought back even though it still clears every criterion. This is
	// checked here rather than on the winning path because the winner now stays
	// open until the bell.
	h.at(12, 0)
	h.fake.SetPrice("ABCD", 5.00)
	h.tick()
	if got := len(h.openPositions()); got != 0 {
		t.Errorf("re-entered a symbol already traded today: %d open", got)
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

// The status page cannot call the broker, so the balance it shows is whatever the
// tick published — including on a day the exchange never opens, which is when
// nothing else in the tick talks to the broker at all.
func TestTickPublishesAccountBalance(t *testing.T) {
	h := newHarness(t)

	if snap := h.eng.Account(); snap.Known {
		t.Error("no balance should be known before the first tick")
	}

	h.fake.SetCalendar(broker.CalendarDay{}) // no session today
	h.at(11, 0)
	h.tick()

	snap := h.eng.Account()
	if !snap.Known {
		t.Fatal("the tick should publish a balance even with the exchange closed")
	}
	if snap.Account.Cash != 100_000 || snap.Account.Equity != 100_000 {
		t.Errorf("published balance = %+v, want the broker's $100k", snap.Account)
	}
	if !snap.At.Equal(h.now) {
		t.Errorf("reading timestamped %s, want %s", snap.At, h.now)
	}

	// A failing account endpoint keeps the last reading rather than blanking it, and
	// does not fault the agent: the balance is display-only.
	h.fake.SetError(errors.New("account 503"))
	h.at(11, 1)
	h.eng.Tick(context.Background())
	if again := h.eng.Account(); !again.Known || !again.At.Equal(snap.At) {
		t.Errorf("failed refresh should leave the previous reading untouched, got %+v", again)
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
