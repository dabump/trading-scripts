package engine

import (
	"context"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

// positionByID re-reads a position from the store, for asserting what a path
// persisted rather than what it returned.
func (h *harness) positionByID(t *testing.T, id int64) domain.Position {
	t.Helper()
	pos, err := h.store.PositionByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return pos
}

// A stop on a thin name fills in pieces. The share count on a "partially_filled"
// order is a snapshot of a sale still in progress, not an exit, and the daemon used
// to read it as one: it banked the piece, cleared the stop id and left the rest
// "held" — shares the broker went on to sell seconds later. The position was then
// open against a flat broker, and every tick tried to sell it.
//
// This is QTEX on 2026-10-05: a 1779-share stop reported 523 filled mid-fill, then
// filled all 1779. The daemon spent the next three hours sending sells for 1256
// shares that did not exist, 2862 of them, each refused with "cannot be sold short".
func TestPartiallyFilledProtectiveStopIsNotAnExit(t *testing.T) {
	h := newHarness(t)
	h.fastFills()
	pos := h.buyOne(t, "ABCD", 5.00)
	if pos.StopOrderID == "" {
		t.Fatal("precondition: the position should have a resting stop")
	}

	// The stop triggers, but the book only takes a third of it.
	h.fake.SetRestingFillLimit("ABCD", pos.SharesOpen/3)
	h.fake.SetPrice("ABCD", pos.StopPrice-0.01)

	res, err := h.eng.pollProtectiveStop(context.Background(), &pos)
	if err != nil {
		t.Fatal(err)
	}
	if res.FilledShares != 0 {
		t.Errorf("poll reported %d shares filled on an order still working; a partial fill "+
			"is not an outcome", res.FilledShares)
	}
	if pos.StopOrderID == "" {
		t.Error("the stop id was cleared while the order is still working: the engine would " +
			"take the floor from a live order and sell shares the broker is already selling")
	}

	after := h.positionByID(t, pos.ID)
	if !after.Open || after.SharesOpen != pos.Shares {
		t.Errorf("position = open:%v shares_open:%d, want the whole %d still held — the broker "+
			"owns the rest of that sale", after.Open, after.SharesOpen, pos.Shares)
	}
	if after.StopOrderID == "" {
		t.Error("the stored stop id was cleared while the order is still working")
	}
}

// Once the order finishes the whole sale, that is the exit — for every share,
// including the ones an earlier poll saw fill.
func TestPartiallyFilledProtectiveStopClosesOnceItCompletes(t *testing.T) {
	h := newHarness(t)
	h.fastFills()
	pos := h.buyOne(t, "ABCD", 5.00)

	h.fake.SetRestingFillLimit("ABCD", pos.SharesOpen/3)
	h.fake.SetPrice("ABCD", pos.StopPrice-0.01)
	if _, err := h.eng.pollProtectiveStop(context.Background(), &pos); err != nil {
		t.Fatal(err)
	}

	// Each further trigger takes another piece; the third completes the order.
	for range 3 {
		h.fake.SetPrice("ABCD", pos.StopPrice-0.01)
	}
	res, err := h.eng.pollProtectiveStop(context.Background(), &pos)
	if err != nil {
		t.Fatal(err)
	}
	if res.FilledShares != pos.Shares {
		t.Fatalf("poll reported %d of %d shares; want the completed order", res.FilledShares, pos.Shares)
	}
	if err := h.eng.closeOnProtectiveStop(pos, res); err != nil {
		t.Fatal(err)
	}

	after := h.positionByID(t, pos.ID)
	if after.Open {
		t.Error("position is still open after the stop sold every share")
	}
	if after.ExitReason != domain.ExitStopLoss {
		t.Errorf("exit reason = %q, want %q", after.ExitReason, domain.ExitStopLoss)
	}
}

// A stop cancelled part-way through really did leave shares behind, and that *is* an
// outcome: the order is terminal, so the fill is banked and the engine takes the
// floor for the remainder. This is the case the short-fill branch exists for, and
// the one it was wrongly reached for before.
func TestCancelledPartialProtectiveStopBanksTheFillAndHoldsTheRest(t *testing.T) {
	h := newHarness(t)
	h.fastFills()
	pos := h.buyOne(t, "ABCD", 5.00)
	sold := pos.SharesOpen / 3

	h.fake.SetRestingFillLimit("ABCD", sold)
	h.fake.SetPrice("ABCD", pos.StopPrice-0.01)

	// Taking the order off the book is what makes the partial final. releaseProtectiveStop
	// cancels, then reads: the read now says "canceled" with the piece that executed.
	res, err := h.eng.releaseProtectiveStop(context.Background(), &pos)
	if err != nil {
		t.Fatal(err)
	}
	if res.FilledShares != sold {
		t.Fatalf("release reported %d shares, want the %d that executed before the cancel",
			res.FilledShares, sold)
	}
	if err := h.eng.closeOnProtectiveStop(pos, res); err != nil {
		t.Fatal(err)
	}

	after := h.positionByID(t, pos.ID)
	if !after.Open {
		t.Fatal("position was closed; a short fill leaves the rest held")
	}
	if want := pos.Shares - sold; after.SharesOpen != want {
		t.Errorf("shares open = %d, want %d still held", after.SharesOpen, want)
	}
	if after.StopOrderID != "" {
		t.Errorf("stop id = %q, want it cleared so the engine holds the floor for the remainder",
			after.StopOrderID)
	}
}

// closeOnProtectiveStop records a sale as finished. Handing it a working order is a
// caller bug, and it says so rather than quietly banking a sale still in progress.
func TestCloseOnProtectiveStopRefusesAWorkingOrder(t *testing.T) {
	h := newHarness(t)
	h.fastFills()
	pos := h.buyOne(t, "ABCD", 5.00)

	h.fake.SetRestingFillLimit("ABCD", pos.SharesOpen/3)
	h.fake.SetPrice("ABCD", pos.StopPrice-0.01)

	working, err := h.fake.Order(context.Background(), pos.StopOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if working.Status != "partially_filled" {
		t.Fatalf("fake order status = %q, want partially_filled", working.Status)
	}
	if err := h.eng.closeOnProtectiveStop(pos, working); err == nil {
		t.Error("closeOnProtectiveStop accepted an order that is still working")
	}

	after := h.positionByID(t, pos.ID)
	if after.StopOrderID != pos.StopOrderID {
		t.Errorf("stop id = %q, want it untouched by the refusal", after.StopOrderID)
	}
}
