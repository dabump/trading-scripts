package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// errUpstream stands in for a market-data outage.
var errUpstream = errors.New("upstream unavailable")

// enableExtended turns the section on for a harness, mirroring the shipped config:
// a 07:00 start, a 20:00 end, a slower cadence, and thresholds loose enough for the
// thin tape at either end of the day.
func (h *harness) enableExtended() {
	h.cfg.Extended.Enabled = true
	h.cfg.Extended.Start = "07:00"
	h.cfg.Extended.End = "20:00"
	h.cfg.Extended.ScanInterval = 5 * time.Minute
	h.cfg.Extended.MinDollarVolume = 100_000
	h.cfg.Extended.MinVolumeMultiple = 0.5
}

// allowExtendedEntry additionally permits buying.
//
// The regular session deliberately stays on market orders here: an extended-hours
// order is a limit order regardless, and the tests below depend on that being true
// rather than on the config having been switched over.
func (h *harness) allowExtendedEntry() {
	h.cfg.Extended.AllowEntry = true
	h.cfg.Extended.LimitSlipPct = 1
}

// latestScreen is the screening table the page would show right now.
func (h *harness) latestScreen() []domain.Evaluation {
	h.t.Helper()
	evals, _, err := h.store.LatestScreenSnapshot(h.date)
	if err != nil {
		h.t.Fatal(err)
	}
	return evals
}

// lastScreenAt is when the most recent screening pass ran, in exchange time.
func (h *harness) lastScreenAt() string {
	h.t.Helper()
	_, at, err := h.store.LatestScreenSnapshot(h.date)
	if err != nil {
		h.t.Fatal(err)
	}
	return at.In(scheduler.ET).Format("15:04")
}

// tradeOrders is every order placed except the protective stops, which follow every
// entry and are tested on their own.
func (h *harness) tradeOrders() []broker.OrderRequest {
	var out []broker.OrderRequest
	for _, o := range h.fake.Placed() {
		if o.Type != "stop" {
			out = append(out, o)
		}
	}
	return out
}

// addPreMarketCandidate registers a symbol whose numbers are a pre-market mover:
// a big move on volume that is a real fraction of a normal day, but nowhere near the
// regular session's 5x or its $1,000,000 floor.
//
// The snapshot carries no volume, which is not a simplification — it is what Alpaca
// actually returns before the bell, because no daily bar for today exists yet. The
// session's volume comes from SessionVolumes instead.
func (h *harness) addPreMarketCandidate(symbol string, price float64) {
	h.fake.SetSnapshot(symbol, price, price/1.14, 0) // +14%, no daily bar yet
	h.fake.SetSessionVolume(symbol, 800_000)         // 0.8x the daily average
	h.fake.SetAverageVolume(symbol, 1_000_000)
	h.fake.SetNews(symbol, 2)
	// Sixteen candles ending at 07:30, so the setup has closed by the tests' 07:30 tick:
	// a candle still forming is never read as a trigger.
	h.fake.SetSetupBars(symbol, price, h.open.Add(-2*time.Hour-16*h.cfg.Entry.PatternInterval),
		h.cfg.Entry.PatternInterval)
}

// With the section off, the hours before the bell are exactly what they always were:
// idle. This is the switch's whole promise — turning it off costs nothing and calls
// nothing.
func TestPreMarketDisabledLeavesTheMorningIdle(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(8, 0)
	h.tick()
	h.wantState(domain.StateMarketClosed)

	if got := len(h.latestScreen()); got != 0 {
		t.Errorf("screened %d symbols before the open with pre-market off, want 0", got)
	}
	if got := len(h.tradeOrders()); got != 0 {
		t.Errorf("placed %d orders, want 0", got)
	}
}

// Enabled but not permitted to trade: the agent scans, the page fills, and every
// qualifying candidate carries the reason it was not bought. An empty table would
// read as a broken scanner, which is the same reasoning as the closed entry window.
func TestPreMarketScreensWithoutBuying(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.tick()
	h.wantState(domain.StateExtendedMarket)

	evals := h.latestScreen()
	if len(evals) != 1 || !evals[0].Qualifies {
		t.Fatalf("pre-market screen = %+v, want one qualifying candidate", evals)
	}
	if evals[0].Outcome != "extended-hours entry is disabled" {
		t.Errorf("Outcome = %q, want the page to say why it was not bought", evals[0].Outcome)
	}
	if got := len(h.tradeOrders()); got != 0 {
		t.Fatalf("placed %d orders with extended.allow_entry off, want 0", got)
	}
	if len(h.openPositions()) != 0 {
		t.Fatal("no position may be opened with pre-market entry disabled")
	}
}

// The regular session's thresholds are regular-session numbers. Applying them to the
// early tape rejects everything, so the pre-market pass has to use its own — and the
// same candidate must then be turned away at 10:31, when the regular ones apply.
func TestPreMarketUsesItsOwnThresholds(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.tick()
	if evals := h.latestScreen(); len(evals) != 1 || !evals[0].Qualifies {
		t.Fatalf("pre-market screen = %+v, want the 0.8x candidate to qualify", evals)
	}

	// The gate needs a reading before the regular session will screen at all.
	h.at(9, 30)
	h.tick()

	// Once the bell has rung the daily bar exists, so the same 800,000 shares now
	// arrive on the snapshot the way the regular session reads them.
	h.fake.SetSnapshot("EARLY", 5.00, 5.00/1.14, 800_000)

	// Same volume, regular session: $4,000,000 of turnover clears the liquidity
	// floor, but 0.8x is nowhere near 5x and the volume criterion fails.
	h.at(10, 31)
	h.tick()
	evals := h.latestScreen()
	if len(evals) != 1 {
		t.Fatalf("regular screen = %+v, want the same symbol evaluated", evals)
	}
	if evals[0].Qualifies {
		t.Error("0.8x relative volume must not qualify once the regular thresholds apply")
	}
}

// Pre-market entry buys through the extended-hours book. The order has to be a limit
// order flagged for extended hours or the broker rejects it outright.
func TestPreMarketEntryUsesExtendedHoursOrders(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.tick()
	h.wantState(domain.StateExtendedMarket)

	placed := h.tradeOrders()
	if len(placed) != 1 {
		t.Fatalf("placed %d orders, want 1: %+v", len(placed), placed)
	}
	if !placed[0].ExtendedHours {
		t.Error("a pre-market order must be routed to the extended-hours book")
	}
	if placed[0].Type != "limit" {
		t.Errorf("order type = %q, want limit — the extended session accepts nothing else",
			placed[0].Type)
	}
	// Priced off the pre-market allowance, above the reference price so a buy is
	// marketable rather than resting behind the spread.
	if want := 5.00 * 1.01; placed[0].LimitPrice < want-0.001 || placed[0].LimitPrice > want+0.001 {
		t.Errorf("limit price = %.4f, want ~%.4f from premarket.limit_slip_pct", placed[0].LimitPrice, want)
	}
	if len(h.openPositions()) != 1 {
		t.Fatalf("open positions = %d, want 1", len(h.openPositions()))
	}
}

// The first-hour gate cannot have run yet, so pre-market entry stands a live
// sentiment read in its place. A bearish tape stops the buying without stopping the
// screening — the same shape as the session-long halt.
func TestPreMarketEntryIsGatedOnLiveSentiment(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	h.setBearish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.tick()
	h.wantState(domain.StateExtendedMarket)

	if got := len(h.tradeOrders()); got != 0 {
		t.Fatalf("placed %d orders on a bearish pre-market tape, want 0", got)
	}
	evals := h.latestScreen()
	if len(evals) != 1 || !evals[0].Qualifies {
		t.Fatalf("screening must continue through a bearish read: %+v", evals)
	}
	if evals[0].Outcome != "pre-market sentiment is overwhelmingly bearish" {
		t.Errorf("Outcome = %q, want the bearish read named", evals[0].Outcome)
	}

	// That read must not have been persisted: the session's verdict belongs to the
	// first hour, and a 07:30 sample cannot be allowed to settle the day.
	readings, err := h.store.SentimentReadings(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 0 {
		t.Errorf("stored %d pre-market readings, want 0 — resolveGate judges the first hour",
			len(readings))
	}
}

// Pre-market scans on its own cadence. Sharing timing.screener_scan_interval would
// mean either flooding the early session with ~130-request passes or starving the
// regular one.
func TestPreMarketScansOnItsOwnCadence(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.tick()
	first := h.fake.AssetCalls()

	// A minute later is due under the regular cadence but not the pre-market one.
	h.at(7, 31)
	h.tick()
	if got := h.lastScreenAt(); got != "07:30" {
		t.Errorf("latest screen at %s, want it still 07:30 under a 5m pre-market cadence", got)
	}

	h.at(7, 36)
	h.tick()
	if got := h.lastScreenAt(); got != "07:36" {
		t.Errorf("latest screen at %s, want 07:36 once the interval has elapsed", got)
	}
	if h.fake.AssetCalls() != first {
		t.Error("the universe must still be fetched once per session, not once per pass")
	}
}

// A position opened before the bell has to be exitable before the bell. Waiting for
// 09:30 to notice a stop would hold a loser for two hours, and the exit order needs
// the same extended-hours routing the entry did.
func TestPreMarketPositionCanStopOutBeforeTheBell(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)
	// Alpaca accepts the protective stop before the bell but will not fire it, so
	// the engine is what has to sell here.
	h.fake.SetStopsInert(true)

	h.at(7, 30)
	h.tick()
	pos := h.openPositions()
	if len(pos) != 1 {
		t.Fatalf("open positions = %d, want 1", len(pos))
	}

	// Through the chart stop, still pre-market.
	h.fake.SetPrice("EARLY", pos[0].StopPrice*0.98)
	h.at(8, 0)
	h.tick()

	if len(h.openPositions()) != 0 {
		t.Fatal("the stop must fire during pre-market, not wait for the open")
	}
	placed := h.tradeOrders()
	last := placed[len(placed)-1]
	if last.Side != "sell" || !last.ExtendedHours {
		t.Errorf("exit order = %+v, want a sell routed to the extended-hours book", last)
	}
}

// The regression this exists for: Alpaca's snapshot carries no volume before the
// opening bell — no daily bar for today has been created yet — so every pre-market
// symbol reads as zero shares traded. Taking that zero at face value failed every
// candidate on the dollar-volume floor and the relative-volume criterion, and the
// pre-market screen returned nothing however its thresholds were set.
func TestPreMarketVolumeComesFromTheSessionNotTheSnapshot(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.tick()

	evals := h.latestScreen()
	if len(evals) == 0 {
		t.Fatal("no candidates: the pre-market screen fell back to the snapshot's zero volume")
	}
	if !evals[0].Qualifies {
		t.Fatalf("candidate did not qualify: %s", evals[0].FailReason)
	}
	// The relative-volume cell has to show the session's real figure. "0.0x" would
	// mean the zero was used and the row only passed because the threshold was low.
	var volume string
	for _, c := range evals[0].Criteria {
		if c.Name == "Rel. volume" {
			volume = c.Display
		}
	}
	if volume != "0.8x" {
		t.Errorf("relative volume = %q, want 0.8x from the session's 800,000 shares", volume)
	}
}

// A symbol that has not printed at all before the bell is absent from the volume
// response, and must be rejected rather than treated as having traded.
func TestPreMarketSymbolWithNoPrintsIsRejected(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.setBullish()
	h.addPreMarketCandidate("QUIET", 5.00)
	h.fake.SetSessionVolume("QUIET", 0)

	h.at(7, 30)
	h.tick()

	if evals := h.latestScreen(); len(evals) != 0 {
		t.Errorf("screened %+v, want a symbol with no pre-market prints dropped by the liquidity floor", evals)
	}
}

// The volume lookup runs on every name that cleared the price move, which is the one
// place in the scan a per-symbol call cannot be afforded. It must be batched.
func TestPreMarketVolumeLookupIsBatched(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.setBullish()
	for _, sym := range []string{"AAA", "BBB", "CCC", "DDD", "EEE"} {
		h.addPreMarketCandidate(sym, 5.00)
	}

	h.at(7, 30)
	h.tick()

	if got := h.fake.SessionVolumeCalls(); got != 1 {
		t.Errorf("made %d volume requests for 5 movers, want 1 batched request", got)
	}
}

// A failed volume lookup is not specific to one candidate, so it faults the agent
// rather than quietly producing an empty screen that looks like a quiet morning.
func TestPreMarketVolumeFailureFaults(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.fake.SetError(errUpstream)
	h.eng.Tick(context.Background())

	if state, _ := h.eng.State(); state != domain.StateError {
		t.Errorf("state = %s, want ERROR when the pre-market volume lookup fails", state)
	}
}

// Enabling pre-market entry must not change how the regular session trades. The
// coupling used to be enforced by config validation, which meant switching the whole
// day to limit orders as the price of buying before the bell — an unrelated change
// that the regular session's order type is still an open question about.
func TestPreMarketLimitOrdersDoNotChangeTheRegularSession(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	h.cfg.Execution.OrderType = "market" // the shipped default, left alone
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)
	h.addCandidate("LATER", 4.00)

	h.at(7, 30)
	h.tick()
	pre := h.tradeOrders()
	if len(pre) != 1 || pre[0].Type != "limit" || !pre[0].ExtendedHours {
		t.Fatalf("pre-market order = %+v, want an extended-hours limit order", pre)
	}

	// Now the regular session, with the same config.
	h.at(9, 30)
	h.tick()
	h.at(10, 31)
	h.tick()

	var regular *broker.OrderRequest
	for i, o := range h.tradeOrders() {
		if o.Symbol == "LATER" {
			regular = &h.tradeOrders()[i]
		}
	}
	if regular == nil {
		t.Fatal("the regular-session candidate was never bought")
	}
	if regular.Type != "market" {
		t.Errorf("regular-session order type = %q, want market — execution.order_type still governs after the bell",
			regular.Type)
	}
	if regular.ExtendedHours {
		t.Error("a regular-session order must not be flagged for extended hours")
	}
}

// The pre-market allowance is optional: unset, it falls back to the regular one, so
// an operator enabling pre-market entry has one fewer thing to remember.
func TestPreMarketLimitSlipFallsBackToTheRegularAllowance(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.cfg.Extended.AllowEntry = true
	h.cfg.Extended.LimitSlipPct = 0 // unset
	h.cfg.Execution.LimitSlipPct = 0.5
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.tick()

	placed := h.tradeOrders()
	if len(placed) != 1 {
		t.Fatalf("placed %d orders, want 1", len(placed))
	}
	if want := 5.00 * 1.005; placed[0].LimitPrice < want-0.001 || placed[0].LimitPrice > want+0.001 {
		t.Errorf("limit price = %.4f, want ~%.4f from execution.limit_slip_pct",
			placed[0].LimitPrice, want)
	}
}

// A pre-market exit is a limit order too, and it has to sit *below* the reference
// price or it never fills. Getting the sign wrong on an exit is how a stop goes
// unfilled while the position keeps falling.
func TestPreMarketExitPricesBelowTheMark(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)
	// Alpaca accepts the protective stop before the bell but will not fire it, so
	// the engine is what has to sell here.
	h.fake.SetStopsInert(true)

	h.at(7, 30)
	h.tick()
	pos := h.openPositions()
	if len(pos) != 1 {
		t.Fatalf("open positions = %d, want 1", len(pos))
	}

	mark := pos[0].StopPrice * 0.98
	h.fake.SetPrice("EARLY", mark)
	h.at(8, 0)
	h.tick()

	placed := h.tradeOrders()
	sell := placed[len(placed)-1]
	if sell.Side != "sell" || sell.Type != "limit" || !sell.ExtendedHours {
		t.Fatalf("exit order = %+v, want an extended-hours limit sell", sell)
	}
	if want := mark * 0.99; sell.LimitPrice < want-0.001 || sell.LimitPrice > want+0.001 {
		t.Errorf("exit limit price = %.4f, want ~%.4f — a sell limit must sit below the mark to fill",
			sell.LimitPrice, want)
	}
}

// addExtendedCandidate registers a symbol whose numbers are an extended-hours mover:
// a big move on volume that is a real fraction of a normal day, but nowhere near the
// regular session's 5x or its $1,000,000 floor. The chart is seeded so the setup has
// closed by chartEnd — a candle still forming is never read as a trigger.
//
// The snapshot's volume is deliberately wrong in both directions, because that is what
// Alpaca actually returns: zero before the bell, where no daily bar for today exists,
// and the whole regular session after the close. The extended session's own volume
// comes from SessionVolumes instead.
func (h *harness) addExtendedCandidate(symbol string, price float64, snapVolume float64, chartEnd time.Time) {
	h.fake.SetSnapshot(symbol, price, price/1.14, snapVolume) // +14%
	h.fake.SetSessionVolume(symbol, 800_000)                  // 0.8x the daily average
	h.fake.SetAverageVolume(symbol, 1_000_000)
	h.fake.SetNews(symbol, 2)
	h.fake.SetSetupBars(symbol, price,
		chartEnd.Add(-16*h.cfg.Entry.PatternInterval), h.cfg.Entry.PatternInterval)
}

// passTheGate takes a real first-hour sentiment reading on a bullish tape, so the
// day's verdict is PROCEED rather than the halt an unjudged gate produces. Post-market
// entry defers to that verdict, so a post-market test that skipped this would be
// testing the kill switch instead.
//
// Candidates have to be registered before this runs: the tradable universe is fetched
// once per session date, so a symbol added after the regular session's first screen is
// not in the list post-market either.
func (h *harness) passTheGate() {
	h.t.Helper()
	h.setBullish()
	h.at(10, 0)
	h.tick()
	h.at(11, 0)
	h.tick()
}

// The close no longer ends the agent's day. The post-market session is covered by the
// same state, the same thresholds and the same entry machinery as pre-market.
func TestPostMarketScreensOnTheExtendedThresholds(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	// A daily bar now exists and carries the whole regular session, which is exactly
	// the number that must not be used: at $5 it is $4,000,000 of turnover and would
	// clear the extended floor for any name at all.
	h.addExtendedCandidate("LATE", 5.00, 800_000, time.Date(2026, 9, 28, 17, 30, 0, 0, scheduler.ET))
	h.passTheGate()

	h.at(17, 30)
	h.tick()
	h.wantState(domain.StateExtendedMarket)

	evals := h.latestScreen()
	if len(evals) != 1 || !evals[0].Qualifies {
		t.Fatalf("post-market screen = %+v, want one qualifying candidate", evals)
	}
	// 0.8x against the extended 0.5x threshold. On the regular 5x it would fail, which
	// is what makes this an extended-hours pass rather than the regular one running
	// late.
	if evals[0].VolumeMultiple > 1 {
		t.Errorf("relative volume = %.2fx, want the session's own volume (0.8x), not the day's",
			evals[0].VolumeMultiple)
	}
}

// With entry permitted the post-market session buys, and routes the order to the
// extended-hours book: Alpaca accepts nothing but a day limit order there, so a market
// order would simply be rejected.
func TestPostMarketEntryUsesExtendedHoursOrders(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	h.addExtendedCandidate("LATE", 5.00, 800_000, time.Date(2026, 9, 28, 17, 30, 0, 0, scheduler.ET))
	h.passTheGate()

	h.at(17, 30)
	h.tick()

	orders := h.tradeOrders()
	if len(orders) != 1 {
		t.Fatalf("placed %d orders after the close, want 1: %+v", len(orders), orders)
	}
	o := orders[0]
	if !o.ExtendedHours {
		t.Error("a post-market order must be routed to the extended-hours book")
	}
	if o.Type != "limit" {
		t.Errorf("order type = %q, want limit: the extended session takes nothing else "+
			"whatever execution.order_type says", o.Type)
	}
	if len(h.openPositions()) != 1 {
		t.Fatalf("open positions = %d, want 1", len(h.openPositions()))
	}
}

// The gate's authority carries into post-market. Before the bell there are no readings
// to defer to and a live read stands in; after the close the day has already been
// judged, so a halted day stays halted rather than getting a second opinion from a
// differently-sourced read.
func TestPostMarketRespectsTheSessionHalt(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	// A bearish first hour, judged into a halt by the regular session.
	h.setBearish()
	h.at(10, 0)
	h.tick()
	h.at(11, 0)
	h.tick()
	h.wantState(domain.StateHaltedBearish)

	h.addExtendedCandidate("LATE", 5.00, 800_000, time.Date(2026, 9, 28, 17, 30, 0, 0, scheduler.ET))
	h.at(17, 30)
	h.tick()
	h.wantState(domain.StateExtendedMarket)

	if got := len(h.tradeOrders()); got != 0 {
		t.Fatalf("placed %d orders after the close on a halted day, want 0", got)
	}
	evals := h.latestScreen()
	if len(evals) != 1 || evals[0].Outcome != "halted for the session" {
		t.Errorf("post-market outcome = %+v, want the page to say the day is halted", evals)
	}
}

// The book has to be flat at the end of the day the agent traded, and post-market is
// part of that day. A position opened at 17:30 is force-sold before the extended
// close, not carried overnight on its resting stop alone.
func TestPostMarketPositionIsFlattenedBeforeTheExtendedClose(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	h.addExtendedCandidate("LATE", 5.00, 800_000, time.Date(2026, 9, 28, 17, 30, 0, 0, scheduler.ET))
	h.passTheGate()

	h.at(17, 30)
	h.tick()
	if len(h.openPositions()) != 1 {
		t.Fatalf("open positions after the post-market entry = %d, want 1", len(h.openPositions()))
	}

	// 20:00 minus the harness's 30-minute forced-exit offset.
	h.at(19, 40)
	h.tick()
	h.wantState(domain.StateEODWindow)
	if got := len(h.openPositions()); got != 0 {
		t.Fatalf("open positions in the post-market EOD window = %d, want 0", got)
	}

	all, err := h.store.SessionPositions(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ExitReason != domain.ExitForcedEOD {
		t.Fatalf("session positions = %+v, want one FORCED_EOD exit", all)
	}
	// The sell is routed to the extended book like the session it is flattening.
	sells := 0
	for _, o := range h.tradeOrders() {
		if o.Side == "sell" {
			sells++
			if !o.ExtendedHours || o.Type != "limit" {
				t.Errorf("post-market forced exit = %+v, want an extended-hours limit order", o)
			}
		}
	}
	if sells != 1 {
		t.Fatalf("sell orders = %d, want 1", sells)
	}
}

// Past the configured end the day is over. Without this the agent would keep scanning
// a tape with no prints on it until midnight.
func TestExtendedCoverageStopsAtTheConfiguredEnd(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.addExtendedCandidate("LATE", 5.00, 800_000, time.Date(2026, 9, 28, 20, 30, 0, 0, scheduler.ET))
	h.passTheGate()

	h.at(20, 30)
	h.tick()
	h.wantState(domain.StateMarketClosed)
}

// The two extended halves are hours apart, and the watchlist between them is not
// interchangeable. On a halted day the regular session publishes none at all, so the
// morning's would otherwise still be the newest one at 17:00 — and post-market would
// buy on a screen taken before the bell, against prices from another session.
func TestPostMarketIgnoresThePreMarketWatchlist(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	h.setBearish() // so the regular session halts and never screens
	h.addExtendedCandidate("EARLY", 5.00, 0, h.open.Add(-30*time.Minute))

	h.at(8, 0)
	h.tick()
	if len(h.latestScreen()) != 1 {
		t.Fatalf("pre-market screen = %+v, want one candidate to go stale", h.latestScreen())
	}

	h.at(10, 0)
	h.tick()
	h.at(11, 0)
	h.tick()
	h.wantState(domain.StateHaltedBearish)

	// Post-market, before its own screen has had a chance to run: the stale pre-market
	// watchlist must not be acted on. claimScan is satisfied here, so the only thing
	// standing between the morning's candidates and an order is the window check.
	h.at(16, 30)
	h.eng.watch.pass.blocked = "" // the pre-market pass had its own reason; remove it
	sess := scheduler.Session{Date: h.date, Open: h.open, Close: h.close}
	if err := h.eng.checkSetups(context.Background(), sess,
		scheduler.Bounds(sess, h.cfg), windowPostBell); err != nil {
		t.Fatal(err)
	}
	if got := len(h.tradeOrders()); got != 0 {
		t.Fatalf("placed %d orders from the pre-market watchlist after the close, want 0", got)
	}
}

// Post-market gets the same quiet time before its forced exit that the regular session
// gets, and says so on the page rather than going quiet: an empty table reads as a
// broken scanner, which is the whole reason screening outlives entry.
func TestPostMarketEntryWindowClosesBeforeItsForcedExit(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	h.cfg.Timing.EntryCutoffBuffer = time.Hour // last post-market entry at 18:30
	h.addExtendedCandidate("LATE", 5.00, 800_000, time.Date(2026, 9, 28, 19, 0, 0, 0, scheduler.ET))
	h.passTheGate()

	h.at(19, 0)
	h.tick()
	h.wantState(domain.StateExtendedMarket)

	evals := h.latestScreen()
	if len(evals) != 1 || !evals[0].Qualifies {
		t.Fatalf("post-market screen = %+v, want the candidate still shown", evals)
	}
	if evals[0].Outcome != "entry window closed" {
		t.Errorf("Outcome = %q, want the page to say the entry window has closed", evals[0].Outcome)
	}
	if got := len(h.tradeOrders()); got != 0 {
		t.Fatalf("placed %d orders past the post-market entry cutoff, want 0", got)
	}
}
