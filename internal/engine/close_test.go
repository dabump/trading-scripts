package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// buyOne drives the harness through a normal entry and returns the open position.
func (h *harness) buyOne(t *testing.T, symbol string, price float64) domain.Position {
	t.Helper()
	h.setBullish()
	h.addCandidate(symbol, price)
	h.at(9, 30)
	h.tick()
	h.at(10, 31)
	h.tick()
	open := h.openPositions()
	if len(open) != 1 {
		t.Fatalf("open positions = %d, want 1", len(open))
	}
	return open[0]
}

// The whole point of the button: the remaining shares are sold and the position is
// closed with its P&L, mid-session, without waiting for a rule.
func TestClosePositionSellsAndRecords(t *testing.T) {
	h := newHarness(t)
	pos := h.buyOne(t, "ABCD", 5.00)
	before := len(h.fake.Placed())

	h.fake.SetPrice("ABCD", 5.50)
	closed, err := h.eng.ClosePosition(context.Background(), pos.ID)
	if err != nil {
		t.Fatal(err)
	}

	placed := h.fake.Placed()
	if len(placed) != before+1 {
		t.Fatalf("placed %d orders, want one more than %d", len(placed), before)
	}
	sell := placed[len(placed)-1]
	if sell.Side != "sell" || sell.Symbol != "ABCD" || sell.Shares != pos.SharesOpen {
		t.Errorf("order = %+v, want a sell of all %d shares", sell, pos.SharesOpen)
	}
	if len(h.openPositions()) != 0 {
		t.Error("the position is still open")
	}
	// MANUAL, not a strategy reason — the end-of-day summary and anything measuring
	// the rules later must not read this as one having fired.
	if closed.ExitReason != domain.ExitManual {
		t.Errorf("exit reason = %q, want MANUAL", closed.ExitReason)
	}
	if want := (5.50 - 5.00) * float64(pos.Shares); closed.RealizedDollars() != want {
		t.Errorf("realised = %.2f, want %.2f", closed.RealizedDollars(), want)
	}
}

// It goes in the trail like every other decision, and says it was a person.
func TestClosePositionIsAudited(t *testing.T) {
	h := newHarness(t)
	rec := &audit.Memory{}
	h.eng.audit = rec
	pos := h.buyOne(t, "ABCD", 5.00)

	if _, err := h.eng.ClosePosition(context.Background(), pos.ID); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, ev := range rec.Events() {
		if ev.Kind == audit.PositionClosed && ev.Detail["reason"] == string(domain.ExitManual) {
			found = true
			if ev.Symbol != "ABCD" {
				t.Errorf("event symbol = %q", ev.Symbol)
			}
		}
	}
	if !found {
		t.Error("a manual close must be recorded in the audit trail")
	}
}

// A second press, or a press on a position the loop has already exited, must not
// send a second sell order.
func TestClosePositionRefusesWhenAlreadyClosed(t *testing.T) {
	h := newHarness(t)
	pos := h.buyOne(t, "ABCD", 5.00)

	if _, err := h.eng.ClosePosition(context.Background(), pos.ID); err != nil {
		t.Fatal(err)
	}
	after := len(h.fake.Placed())

	_, err := h.eng.ClosePosition(context.Background(), pos.ID)
	if !errors.Is(err, ErrPositionNotOpen) {
		t.Errorf("second close returned %v, want ErrPositionNotOpen", err)
	}
	if len(h.fake.Placed()) != after {
		t.Error("a second press must not place a second sell order")
	}
}

// Closing into a shut exchange would leave the store saying flat while the broker
// still held the position, so it is refused rather than half-done.
func TestClosePositionRefusedWhenExchangeIsShut(t *testing.T) {
	h := newHarness(t)
	pos := h.buyOne(t, "ABCD", 5.00)
	before := len(h.fake.Placed())

	h.at(18, 0) // after the close
	h.tick()

	_, err := h.eng.ClosePosition(context.Background(), pos.ID)
	if !errors.Is(err, ErrExchangeClosed) {
		t.Errorf("close after hours returned %v, want ErrExchangeClosed", err)
	}
	if len(h.fake.Placed()) != before {
		t.Error("no order may be placed with the exchange shut")
	}
	if len(h.openPositions()) != 1 {
		t.Error("the position must stay open when the close is refused")
	}
}

// Pre-market the sale has to be routed to the extended-hours book like any other.
func TestClosePositionPreMarketUsesExtendedHours(t *testing.T) {
	h := newHarness(t)
	h.enablePreMarket()
	h.allowPreMarketEntry()
	h.setBullish()
	h.addPreMarketCandidate("EARLY", 5.00)
	h.at(7, 30)
	h.tick()

	open := h.openPositions()
	if len(open) != 1 {
		t.Fatalf("open positions = %d, want 1", len(open))
	}
	if _, err := h.eng.ClosePosition(context.Background(), open[0].ID); err != nil {
		t.Fatal(err)
	}

	placed := h.fake.Placed()
	sell := placed[len(placed)-1]
	if sell.Side != "sell" || !sell.ExtendedHours || sell.Type != "limit" {
		t.Errorf("pre-market close = %+v, want an extended-hours limit sell", sell)
	}
}

// An unknown id is an error, not a silent no-op.
func TestClosePositionUnknownID(t *testing.T) {
	h := newHarness(t)
	if _, err := h.eng.ClosePosition(context.Background(), 4242); err == nil {
		t.Error("closing a position that does not exist must fail")
	}
}
