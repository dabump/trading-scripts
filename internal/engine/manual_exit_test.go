package engine

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// openByHand opens symbol from the page mid-session and returns the stored position.
func (h *harness) openByHand(t *testing.T, symbol string, price float64) domain.Position {
	t.Helper()
	h.addScreenedOnly(symbol, price)
	h.tradingSession(t)
	res, err := h.eng.OpenPosition(context.Background(), symbol)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Position.Manual {
		t.Fatal("a position opened from the page must be marked manual")
	}
	return res.Position
}

// restingStopFor returns the stop order the open left at the broker: either one sent
// on its own, or the leg attached to the buy, described as the sell stop it becomes.
func (h *harness) restingStopFor(t *testing.T, symbol string) broker.OrderRequest {
	t.Helper()
	var found []broker.OrderRequest
	for _, o := range h.fake.Placed() {
		switch {
		case o.Symbol != symbol:
		case o.Type == "stop":
			found = append(found, o)
		case o.StopLoss > 0:
			found = append(found, broker.OrderRequest{
				Symbol: o.Symbol, Shares: o.Shares, Side: "sell", Type: "stop",
				StopPrice: o.StopLoss, TimeInForce: o.TimeInForce, ExtendedHours: o.ExtendedHours,
			})
		}
	}
	if len(found) != 1 {
		t.Fatalf("placed %d stop orders for %s, want exactly 1", len(found), symbol)
	}
	return found[0]
}

// The stop that sized a manual position is left with the broker as a real order, at
// one R. That is what makes it a stop rather than a note: it is enforced between
// ticks and while this process is not running.
func TestManualOpenLeavesAOneRStopRestingAtTheBroker(t *testing.T) {
	h := newHarness(t)
	pos := h.openByHand(t, "HAND", 5.00)

	order := h.restingStopFor(t, "HAND")
	if order.Side != "sell" || order.Shares != pos.Shares {
		t.Errorf("stop order = %+v, want a sell of all %d shares", order, pos.Shares)
	}
	if order.StopPrice != pos.StopPrice {
		t.Errorf("stop order trigger = %v, want the position's stop %v", order.StopPrice, pos.StopPrice)
	}
	if order.TimeInForce != "gtc" {
		t.Errorf("time in force = %q, want gtc: a day order expires at the close and leaves "+
			"the position bare overnight", order.TimeInForce)
	}
	if order.ExtendedHours {
		t.Error("a stop order must never be routed extended-hours: Alpaca accepts that only on a day limit")
	}
	// 1R is the distance the share count was derived from, so the order has to sit
	// exactly there — anywhere else and the loss is not the risk that was budgeted.
	if got := pos.EntryPrice - order.StopPrice; math.Abs(got-pos.InitialRisk) > 1e-9 {
		t.Errorf("stop is %v below entry, want 1R = %v", got, pos.InitialRisk)
	}
	if h.fake.RestingStops() != 1 {
		t.Errorf("resting stops = %d, want the order still working", h.fake.RestingStops())
	}
}

// Price through the stop: the broker's resting order does the selling, and the agent
// records the exit it finds rather than sending a sell of its own.
func TestManualPositionIsSoldByItsRestingStop(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	pos := h.openByHand(t, "HAND", 5.00)
	sells := sellOrders(h, "HAND")

	h.fake.SetPrice("HAND", pos.StopPrice-0.10) // triggers the resting order
	h.at(10, 40)
	h.tick()

	if got := sellOrders(h, "HAND"); got != sells {
		t.Errorf("the agent sent %d sell orders of its own, want none: the resting stop sold it",
			got-sells)
	}
	if open := h.openPositions(); len(open) != 0 {
		t.Fatalf("open = %+v, want flat: the stop filled", open)
	}
	closed, err := h.store.PositionByID(pos.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.ExitReason != domain.ExitStopLoss {
		t.Errorf("exit reason = %q, want %q", closed.ExitReason, domain.ExitStopLoss)
	}
	if closed.ExitPrice != pos.StopPrice {
		t.Errorf("exit price = %v, want the broker's fill at %v", closed.ExitPrice, pos.StopPrice)
	}
	if closed.StopOrderID != "" {
		t.Error("the filled order's id must be cleared, or the next exit tries to cancel it")
	}
	var found bool
	for _, ev := range mem.Events() {
		if ev.Kind == audit.PositionClosed && ev.Detail["protective_stop"] == "filled" {
			found = true
		}
	}
	if !found {
		t.Error("the trail must say the resting order was what sold it")
	}
}

// The signal rules still do not apply: no scale-out at the target, and the candle
// trail does not move the stop out from under a position the operator is holding.
func TestManualPositionIgnoresTheSignalExitRules(t *testing.T) {
	h := newHarness(t)
	h.cfg.Exit.CandleTrail = "always"
	pos := h.openByHand(t, "HAND", 5.00)
	sells := sellOrders(h, "HAND")

	// Far past the first target, which would scale an automated position out.
	h.fake.SetPrice("HAND", 7.00)
	h.at(10, 50)
	h.tick()

	if got := sellOrders(h, "HAND"); got != sells {
		t.Fatalf("placed %d sells, want none: no target or trail may touch a manual position",
			got-sells)
	}
	open := h.openPositions()
	if len(open) != 1 || open[0].SharesOpen != pos.Shares || open[0].TargetHit {
		t.Fatalf("open = %+v, want the whole manual position still held", open)
	}
	if open[0].StopPrice != pos.StopPrice {
		t.Errorf("stop moved from %v to %v: the candle trail must not touch it",
			pos.StopPrice, open[0].StopPrice)
	}
	if open[0].LastPrice != 7.00 || open[0].PeakPrice != 7.00 {
		t.Errorf("mark = %v peak = %v, want 7.00: the page still needs a live price",
			open[0].LastPrice, open[0].PeakPrice)
	}
}

// The forced exit does apply, and it takes the resting stop off the book before
// selling — an order left working against a holding that is gone sells short.
func TestManualPositionIsForceSoldAtTheBell(t *testing.T) {
	h := newHarness(t)
	pos := h.openByHand(t, "HAND", 5.00)

	h.at(15, 31)
	h.tick()

	if open := h.openPositions(); len(open) != 0 {
		t.Fatalf("open = %+v, want flat after the forced exit", open)
	}
	closed, err := h.store.PositionByID(pos.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.ExitReason != domain.ExitForcedEOD {
		t.Errorf("exit reason = %q, want %q", closed.ExitReason, domain.ExitForcedEOD)
	}
	if h.fake.RestingStops() != 0 {
		t.Error("the protective stop is still working after the position was sold; " +
			"the next time price touched it the account would go short")
	}
}

// The Close button cancels the resting stop first, for the same reason.
func TestManualCloseCancelsTheRestingStop(t *testing.T) {
	h := newHarness(t)
	pos := h.openByHand(t, "HAND", 5.00)

	closed, err := h.eng.ClosePosition(context.Background(), pos.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.ExitReason != domain.ExitManual || closed.Open {
		t.Errorf("closed = %+v, want a MANUAL close", closed)
	}
	if h.fake.RestingStops() != 0 {
		t.Error("the protective stop outlived the position it protected")
	}
	if stored, err := h.store.PositionByID(pos.ID); err == nil && stored.StopOrderID != "" {
		t.Error("the cancelled order's id must be cleared from the row")
	}
}

// sellOrders counts the sell orders the agent itself sent for a symbol, which is
// every sell other than the resting stop.
func sellOrders(h *harness, symbol string) int {
	var n int
	for _, o := range h.fake.Placed() {
		if o.Symbol == symbol && o.Side == "sell" && o.Type != "stop" {
			n++
		}
	}
	return n
}

// A manual position beside an automated one does not shield it, and the forced exit
// now flattens both — each recorded under its own reason, so nothing later reads a
// hand-held position as a rule firing or the other way round.
func TestTheForcedExitFlattensBothKindsOfPosition(t *testing.T) {
	h := newHarness(t)
	h.addCandidate("AUTO", 5.00)
	hand := h.openByHand(t, "HAND", 5.00)

	h.at(15, 31)
	h.tick()

	if open := h.openPositions(); len(open) != 0 {
		t.Fatalf("open = %+v, want flat after the forced exit", open)
	}
	var soldAuto bool
	for _, o := range h.fake.Placed() {
		soldAuto = soldAuto || (o.Symbol == "AUTO" && o.Side == "sell" && o.Type != "stop")
	}
	if !soldAuto {
		t.Error("the automated position was never bought and force-sold, so this proved nothing")
	}
	closed, err := h.store.PositionByID(hand.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.ExitReason != domain.ExitForcedEOD || !closed.Manual {
		t.Errorf("manual position closed as %+v, want a manual row exited at EOD", closed)
	}
}

// A symbol that failed the screen can still be opened by hand. The open records that
// it overrode the screen, and why the screen failed it.
func TestOpenPositionOnAFailingScreenRow(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	h.fake.SetSnapshot("NONEWS", 5.00, 5.00/1.14, 6_000_000) // moves and trades, no story
	h.fake.SetAverageVolume("NONEWS", 1_000_000)
	h.tradingSession(t)

	res, err := h.eng.OpenPosition(context.Background(), "NONEWS")
	if err != nil {
		t.Fatal(err)
	}
	if res.Qualified || res.ScreenReason == "" || res.ScreenReason == "not on the latest screen" {
		t.Fatalf("qualified = %v reason = %q, want the screen's own failure reason",
			res.Qualified, res.ScreenReason)
	}
	if !res.Position.Manual {
		t.Error("it is a manual position like any other")
	}
	var opened *audit.Event
	for _, ev := range mem.Events() {
		if ev.Kind == audit.PositionOpened && ev.Symbol == "NONEWS" {
			opened = &ev
		}
	}
	if opened == nil || opened.Detail["screen_qualified"] != false ||
		opened.Detail["screen_reason"] != res.ScreenReason {
		t.Errorf("audit = %+v, want screen_qualified false with the reason", opened)
	}
}

// The case the fallback exists for: the buy filled and the stop order was refused.
// The position keeps its stop — the engine evaluates it on the tick — because a
// manual position with no stop at all is the one outcome this must never produce.
func TestARefusedStopOrderLeavesTheEngineHoldingTheStop(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	h.fake.SetStopOrderError(errors.New("stop orders are not permitted on this account"))
	h.addScreenedOnly("HAND", 5.00)
	h.tradingSession(t)

	res, err := h.eng.OpenPosition(context.Background(), "HAND")
	if err != nil {
		t.Fatalf("the buy must still stand: selling straight back is a certain loss to avoid "+
			"an uncertain one (%v)", err)
	}
	if res.StopOrderPlaced || res.StopOrderNote == "" {
		t.Errorf("placed = %v note = %q, want the operator told it is not resting",
			res.StopOrderPlaced, res.StopOrderNote)
	}
	if res.Stop <= 0 || res.Position.StopOrderID != "" {
		t.Errorf("position = %+v, want a stop price and no order id", res.Position)
	}
	var faulted bool
	for _, ev := range mem.Events() {
		if ev.Kind == audit.Fault && ev.Detail["protective_stop"] == "not placed" {
			faulted = true
		}
	}
	if !faulted {
		t.Error("a position left without a resting stop must be in the trail")
	}

	// And the stop still works, one tick late.
	h.fake.SetPrice("HAND", res.Stop-0.05)
	h.at(10, 40)
	h.tick()

	if open := h.openPositions(); len(open) != 0 {
		t.Fatalf("open = %+v, want the engine to have sold it at its stop", open)
	}
	closed, err := h.store.PositionByID(res.Position.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.ExitReason != domain.ExitStopLoss {
		t.Errorf("exit reason = %q, want %q", closed.ExitReason, domain.ExitStopLoss)
	}
}

// Before the bell the order is placed but inert — Alpaca will not trigger a stop in
// the extended session — so the engine keeps the floor until the open. The order is
// still sent, and not as an extended-hours order, so it is live at 09:30 without
// anything having to remember to place it.
func TestAPreMarketManualOpenIsCoveredUntilTheBell(t *testing.T) {
	h := newHarness(t)
	h.enableExtended()
	h.allowExtendedEntry()
	h.addScreenedOnly("HAND", 5.00)
	h.at(8, 0)
	h.tick()

	res, err := h.eng.OpenPosition(context.Background(), "HAND")
	if err != nil {
		t.Fatal(err)
	}
	order := h.restingStopFor(t, "HAND")
	if order.ExtendedHours {
		t.Error("the stop order must not be routed extended-hours")
	}
	if !res.StopOrderPlaced || res.StopOrderNote == "" {
		t.Errorf("placed = %v note = %q, want it resting with the pre-market caveat said out loud",
			res.StopOrderPlaced, res.StopOrderNote)
	}

	// Pre-market, through the stop: the resting order cannot trigger, so the engine does.
	h.fake.SetPrice("HAND", res.Stop-0.05)
	h.at(8, 30)
	h.tick()

	if open := h.openPositions(); len(open) != 0 {
		t.Fatalf("open = %+v, want sold: a stop order cannot trigger before 09:30, so the "+
			"engine has to hold this stop", open)
	}
	if h.fake.RestingStops() != 0 {
		t.Error("the inert stop order was left on the book after the position was sold")
	}
}
