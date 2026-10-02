package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/broker"
)

// fastFills shortens the fill wait so the tests that leave an order working do not
// sit through the production ten seconds.
func (h *harness) fastFills() {
	h.eng.fillWait = 20 * time.Millisecond
	h.eng.fillPoll = time.Millisecond
}

// A position is what the broker executed, not what the daemon read. On 2026-09-29
// every SANG buy quoted 4.99 and filled at 5.00, which turned four recorded
// breakevens into four real losses.
func TestEntryRecordsTheFillNotTheQuote(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	h.fake.SetFillPrice("ABCD", 5.02)
	pos := h.buyOne(t, "ABCD", 5.00)

	if pos.EntryPrice != 5.02 {
		t.Errorf("entry = %v, want the 5.02 fill rather than the 5.00 quote", pos.EntryPrice)
	}
	// The risk the target is a multiple of is the risk actually taken on.
	if want := 5.02 - pos.StopPrice; pos.InitialRisk < want-1e-9 || pos.InitialRisk > want+1e-9 {
		t.Errorf("initial risk = %v, want %v measured from the fill", pos.InitialRisk, want)
	}

	var opened *audit.Event
	for _, ev := range mem.Events() {
		if ev.Kind == audit.PositionOpened {
			opened = &ev
		}
	}
	if opened == nil {
		t.Fatal("no POSITION_OPENED event")
	}
	if opened.Detail["price"] != 5.02 || opened.Detail["quoted_price"] != 5.00 {
		t.Errorf("audit price = %v, quoted = %v; want 5.02 and 5.00",
			opened.Detail["price"], opened.Detail["quoted_price"])
	}
	if opened.Detail["fill_confirmed"] != true {
		t.Error("a fill the broker reported must be recorded as confirmed")
	}
}

// A buy that fills nothing within the wait is cancelled and is not a position.
func TestUnfilledEntryIsCancelledNotRecorded(t *testing.T) {
	h := newHarness(t)
	h.fastFills()
	h.fake.SetFillLimit("ABCD", 0)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)
	h.at(9, 30)
	h.tick()
	h.at(10, 31)
	h.tick()

	if n := len(h.openPositions()); n != 0 {
		t.Fatalf("open positions = %d, want 0: nothing was bought", n)
	}
	placed := h.fake.Placed()
	if len(placed) != 1 {
		t.Fatalf("placed %d orders, want 1", len(placed))
	}
	got, err := h.fake.Order(context.Background(), "fake-"+placed[0].ClientOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "canceled" {
		t.Errorf("order status = %q, want the working order cancelled", got.Status)
	}
	status, _, shares, err := h.store.OrderFill(placed[0].ClientOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if status != "canceled" || shares != 0 {
		t.Errorf("stored order = %s x %d, want canceled x 0", status, shares)
	}
}

// A buy that fills part of its size holds what filled.
func TestPartialEntryHoldsWhatFilled(t *testing.T) {
	h := newHarness(t)
	h.fastFills()
	h.fake.SetFillLimit("ABCD", 100)
	pos := h.buyOne(t, "ABCD", 5.00)

	if pos.Shares != 100 || pos.SharesOpen != 100 {
		t.Errorf("position = %d shares (%d open), want the 100 that filled", pos.Shares, pos.SharesOpen)
	}
}

// An exit that sells short banks what sold and keeps the rest, which the stop — still
// breached — sells on the next tick. Closing the whole row would leave the store flat
// while the broker still held shares.
//
// Only reachable when the tick sells, which with a resting stop it does not do for a
// stop-loss — so the broker refuses stop orders here, and the engine holds the stop.
func TestShortExitKeepsTheRemainderOpen(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	h.fastFills()
	h.fake.SetStopOrderError(errors.New("stop orders refused"))
	pos := h.buyOne(t, "ABCD", 5.00)

	h.fake.SetFillLimit("ABCD", 1000)
	h.fake.SetPrice("ABCD", pos.StopPrice-0.05)
	h.at(10, 32)
	h.tick()

	open := h.openPositions()
	if len(open) != 1 {
		t.Fatalf("open positions = %d, want the unsold remainder still open", len(open))
	}
	if want := pos.Shares - 1000; open[0].SharesOpen != want {
		t.Errorf("shares open = %d, want %d", open[0].SharesOpen, want)
	}
	if open[0].BankedDollars >= 0 {
		t.Errorf("banked = %v, want the loss on the 1000 sold", open[0].BankedDollars)
	}
	var short bool
	for _, ev := range mem.Events() {
		short = short || ev.Kind == audit.OrderNotFilled
	}
	if !short {
		t.Error("a short exit must be audited")
	}

	// The book fills again: the next tick sells the rest and closes the position.
	h.fake.SetFillLimit("ABCD", 1_000_000)
	h.at(10, 33)
	h.tick()
	if n := len(h.openPositions()); n != 0 {
		t.Fatalf("open positions = %d after the book filled, want 0", n)
	}
}

// A manual close that sells nothing says so rather than reporting a closed position.
func TestManualCloseThatDoesNotFillIsRefused(t *testing.T) {
	h := newHarness(t)
	h.fastFills()
	pos := h.buyOne(t, "ABCD", 5.00)

	h.fake.SetFillLimit("ABCD", 0)
	_, err := h.eng.ClosePosition(context.Background(), pos.ID)
	if !errors.Is(err, ErrNotFilled) {
		t.Fatalf("err = %v, want ErrNotFilled", err)
	}
	if n := len(h.openPositions()); n != 1 {
		t.Errorf("open positions = %d, want the position still held", n)
	}
}

// lostOrders acknowledges orders but can never say how they ended, as when the
// broker's order endpoint is unreachable.
type lostOrders struct{ *broker.Fake }

func (lostOrders) Order(context.Context, string) (broker.OrderResult, error) {
	return broker.OrderResult{}, errors.New("order lookup unavailable")
}

func (lostOrders) CancelOrder(context.Context, string) error {
	return errors.New("cancel unavailable")
}

// When the outcome cannot be learned, the order is assumed to have filled as sent —
// the behaviour from before fills were read — and left for Reconcile.
func TestUnconfirmedFillFallsBackToTheQuote(t *testing.T) {
	h := newHarness(t)
	h.eng = New(Deps{
		Config: h.cfg, Store: h.store, Data: h.fake, Trading: lostOrders{h.fake},
		Logger: discardLogger(), Now: func() time.Time { return h.now },
	})
	h.fastFills()
	h.fake.SetFillLimit("ABCD", 0)
	pos := h.buyOne(t, "ABCD", 5.00)

	if pos.EntryPrice != 5.00 {
		t.Errorf("entry = %v, want the 5.00 quote when no fill was learned", pos.EntryPrice)
	}
	pending, err := h.store.UnresolvedOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Status != "unconfirmed" {
		t.Errorf("unresolved = %+v, want the one unconfirmed order for Reconcile", pending)
	}
}
