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

// enablePreMarket turns the section on for a harness, mirroring the shipped config:
// a 07:00 start, a slower cadence, and thresholds loose enough for the early tape.
func (h *harness) enablePreMarket() {
	h.cfg.PreMarket.Enabled = true
	h.cfg.PreMarket.Start = "07:00"
	h.cfg.PreMarket.ScanInterval = 5 * time.Minute
	h.cfg.PreMarket.MinDollarVolume = 100_000
	h.cfg.PreMarket.MinVolumeMultiple = 0.5
}

// allowPreMarketEntry additionally permits buying.
//
// The regular session deliberately stays on market orders here: pre-market sends a
// limit order regardless, and the tests below depend on that being true rather than
// on the config having been switched over.
func (h *harness) allowPreMarketEntry() {
	h.cfg.PreMarket.AllowEntry = true
	h.cfg.PreMarket.LimitSlipPct = 1
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
	h.enablePreMarket()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.tick()
	h.wantState(domain.StatePreMarket)

	evals := h.latestScreen()
	if len(evals) != 1 || !evals[0].Qualifies {
		t.Fatalf("pre-market screen = %+v, want one qualifying candidate", evals)
	}
	if evals[0].Outcome != "pre-market entry is disabled" {
		t.Errorf("Outcome = %q, want the page to say why it was not bought", evals[0].Outcome)
	}
	if got := len(h.tradeOrders()); got != 0 {
		t.Fatalf("placed %d orders with premarket.allow_entry off, want 0", got)
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
	h.enablePreMarket()
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
	h.enablePreMarket()
	h.allowPreMarketEntry()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.tick()
	h.wantState(domain.StatePreMarket)

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
	h.enablePreMarket()
	h.allowPreMarketEntry()
	h.setBearish()
	h.addPreMarketCandidate("EARLY", 5.00)

	h.at(7, 30)
	h.tick()
	h.wantState(domain.StatePreMarket)

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
	h.enablePreMarket()
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
	h.enablePreMarket()
	h.allowPreMarketEntry()
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
	h.enablePreMarket()
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
	h.enablePreMarket()
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
	h.enablePreMarket()
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
	h.enablePreMarket()
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
	h.enablePreMarket()
	h.allowPreMarketEntry()
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
	h.enablePreMarket()
	h.cfg.PreMarket.AllowEntry = true
	h.cfg.PreMarket.LimitSlipPct = 0 // unset
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
	h.enablePreMarket()
	h.allowPreMarketEntry()
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
