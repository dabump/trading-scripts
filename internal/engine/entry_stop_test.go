package engine

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// The stop goes to the broker as part of the buy, so it is resting from the instant
// the buy fills — not a tick later, and not only while this process is running.
func TestAutomatedEntryCarriesItsStop(t *testing.T) {
	h := newHarness(t)
	pos := h.buyOne(t, "ABCD", 5.00)

	buy := h.fake.Placed()[0]
	if buy.Side != "buy" || buy.StopLoss != pos.StopPrice {
		t.Errorf("buy = %+v, want it to carry the %v chart stop", buy, pos.StopPrice)
	}
	if buy.TimeInForce != "gtc" {
		t.Errorf("time in force = %q, want gtc so the stop leg outlives the day", buy.TimeInForce)
	}
	if len(h.fake.Placed()) != 1 {
		t.Errorf("placed %d orders, want the one buy with its stop attached", len(h.fake.Placed()))
	}
	if pos.StopOrderID == "" {
		t.Error("the position must record the stop order protecting it")
	}
	if shares, stop, ok := h.fake.RestingStop("ABCD"); !ok || shares != pos.Shares || stop != pos.StopPrice {
		t.Errorf("resting stop = %d @ %v (ok %v), want %d @ %v", shares, stop, ok, pos.Shares, pos.StopPrice)
	}
}

// The broker's stop does the selling, and the agent records the fill it finds rather
// than selling again.
func TestAutomatedPositionIsSoldByItsRestingStop(t *testing.T) {
	h := newHarness(t)
	pos := h.buyOne(t, "ABCD", 5.00)

	h.fake.SetPrice("ABCD", pos.StopPrice-0.03) // the resting stop fires at its trigger
	h.at(10, 32)
	h.tick()

	all, _ := h.store.SessionPositions(h.date)
	if len(all) != 1 || all[0].Open {
		t.Fatalf("position should be closed by its stop: %+v", all)
	}
	if all[0].ExitReason != domain.ExitStopLoss || all[0].ExitPrice != pos.StopPrice {
		t.Errorf("exit = %s at %v, want STOP_LOSS at the %v stop's fill", all[0].ExitReason,
			all[0].ExitPrice, pos.StopPrice)
	}
	for _, o := range h.fake.Placed()[1:] {
		t.Errorf("placed %+v after the buy; the resting stop had already sold it", o)
	}
}

// If the broker will not take the stop attached to the buy, the buy goes alone and
// the stop follows straight after the fill.
func TestRefusedAttachedStopIsPlacedSeparately(t *testing.T) {
	h := newHarness(t)
	h.fake.SetAttachedStopError(errors.New("order_class not supported"))
	pos := h.buyOne(t, "ABCD", 5.00)

	if pos.StopOrderID == "" {
		t.Fatal("the position must still get a resting stop")
	}
	if shares, stop, ok := h.fake.RestingStop("ABCD"); !ok || shares != pos.Shares || stop != pos.StopPrice {
		t.Errorf("resting stop = %d @ %v (ok %v), want %d @ %v", shares, stop, ok, pos.Shares, pos.StopPrice)
	}
}

// A buy that fills short gets a stop for what it bought, not for what it asked for: a
// stop sized for the whole order would sell shares that were never held.
func TestShortFilledEntryGetsAStopForWhatFilled(t *testing.T) {
	h := newHarness(t)
	h.fastFills()
	h.fake.SetFillLimit("ABCD", 100)
	pos := h.buyOne(t, "ABCD", 5.00)

	if shares, _, ok := h.fake.RestingStop("ABCD"); !ok || shares != 100 {
		t.Errorf("resting stop for %d shares (ok %v), want exactly one, for the 100 bought", shares, ok)
	}
	if pos.StopOrderID == "" {
		t.Error("the position must record its resting stop")
	}
}

// The first target sells half. The resting stop comes off first — it covers every
// share — and goes back on for the runner, at breakeven.
func TestScaleOutReplacesTheStopForTheRunner(t *testing.T) {
	h := newHarness(t)
	pos := h.buyOne(t, "ABCD", 5.00)

	h.fake.SetPrice("ABCD", pos.EntryPrice+2*pos.InitialRisk+0.01)
	h.at(10, 32)
	h.tick()

	open := h.openPositions()
	if len(open) != 1 || !open[0].TargetHit {
		t.Fatalf("want the runner still open after the target: %+v", open)
	}
	shares, stop, ok := h.fake.RestingStop("ABCD")
	if !ok || shares != open[0].SharesOpen {
		t.Fatalf("resting stop for %d shares (ok %v), want one for the %d still held",
			shares, ok, open[0].SharesOpen)
	}
	if stop != pos.EntryPrice {
		t.Errorf("runner's stop = %v, want breakeven at %v", stop, pos.EntryPrice)
	}
}

// When the candle trail raises the stop, the order at the broker moves with it.
func TestCandleTrailMovesTheRestingStop(t *testing.T) {
	h := openForTrail(t, config.CandleTrailAlways)

	h.at(10, 37)
	h.fake.SetPrice("ABCD", 5.10)
	h.tick()

	pos := h.openPositions()
	if len(pos) != 1 {
		t.Fatal("position should still be open above the 10:36 low")
	}
	shares, stop, ok := h.fake.RestingStop("ABCD")
	if !ok || shares != pos[0].SharesOpen || math.Abs(stop-5.02) > 1e-9 {
		t.Errorf("resting stop = %d @ %v (ok %v), want %d @ the 5.02 trailed low",
			shares, stop, ok, pos[0].SharesOpen)
	}

	// And it is the broker that sells on the new low.
	h.now = h.now.Add(30 * time.Second)
	h.fake.SetPrice("ABCD", 5.01)
	h.tick()
	all, _ := h.store.SessionPositions(h.date)
	if all[0].Open || all[0].ExitPrice != 5.02 {
		t.Errorf("position = %+v, want sold by the resting stop at 5.02", all[0])
	}
}
