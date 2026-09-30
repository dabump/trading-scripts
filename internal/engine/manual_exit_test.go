package engine

import (
	"context"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/audit"
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

// No exit rule touches a manual position: not the stop, not the gap backstop, not the
// target, and not the forced exit before the close. Only the Close button sells it.
func TestManualPositionIgnoresEveryExitRule(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	h.cfg.Exit.CandleTrail = "always"
	pos := h.openByHand(t, "HAND", 5.00)
	placed := len(h.fake.Placed())

	// Through the stop and the 10% backstop.
	h.fake.SetPrice("HAND", 4.00)
	h.at(10, 40)
	h.tick()
	// Far past the first target.
	h.fake.SetPrice("HAND", 7.00)
	h.at(10, 50)
	h.tick()
	// The forced exit, on two ticks of the window.
	h.at(15, 31)
	h.tick()
	h.at(15, 45)
	h.tick()

	if got := len(h.fake.Placed()); got != placed {
		t.Fatalf("placed %d orders after the open, want none: no rule may sell a manual position",
			got-placed)
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

	var held int
	for _, ev := range mem.Events() {
		if ev.Kind == audit.PositionHeld {
			held++
		}
	}
	if held != 1 {
		t.Errorf("POSITION_HELD recorded %d times, want once per session", held)
	}

	// The button still works, inside the EOD window too.
	closed, err := h.eng.ClosePosition(context.Background(), pos.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.ExitReason != domain.ExitManual || closed.Open {
		t.Errorf("closed = %+v, want a MANUAL close", closed)
	}
}

// The automated positions beside it are still managed as before.
func TestManualPositionDoesNotShieldAutomatedOnes(t *testing.T) {
	h := newHarness(t)
	h.addCandidate("AUTO", 5.00)
	h.openByHand(t, "HAND", 5.00)

	h.at(15, 31)
	h.tick()

	open := h.openPositions()
	if len(open) != 1 || open[0].Symbol != "HAND" {
		t.Fatalf("open = %+v, want only the manual position left after the forced exit", open)
	}
	var soldAuto bool
	for _, o := range h.fake.Placed() {
		soldAuto = soldAuto || (o.Symbol == "AUTO" && o.Side == "sell")
	}
	if !soldAuto {
		t.Error("the automated position was never bought and force-sold, so this proved nothing")
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
