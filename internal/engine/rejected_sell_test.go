package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// refusesSells is a broker that refuses every sell, the way Alpaca refuses one for
// shares the account does not hold. holds is what it reports owning, so a test can
// put the store and the broker at odds the way a mis-read fill does.
type refusesSells struct {
	*broker.Fake
	holds []broker.BrokerPosition
	sells int
}

func (r *refusesSells) PlaceOrder(ctx context.Context, req broker.OrderRequest) (broker.OrderResult, error) {
	if req.Side == "sell" {
		r.sells++
		return broker.OrderResult{}, errors.New(
			`422 Unprocessable Entity: {"code":42210000,"message":"asset \"ABCD\" cannot be sold short"}`)
	}
	return r.Fake.PlaceOrder(ctx, req)
}

func (r *refusesSells) Positions(context.Context) ([]broker.BrokerPosition, error) {
	return r.holds, nil
}

// refuseSellsAfterEntry swaps the broker for one that refuses sells, once a position
// is already open and protected. Doing it before would refuse the stop order too.
func (h *harness) refuseSellsAfterEntry(mem audit.Recorder, holds ...broker.BrokerPosition) *refusesSells {
	br := &refusesSells{Fake: h.fake, holds: holds}
	h.eng = New(Deps{
		Config: h.cfg, Store: h.store, Data: h.fake, Trading: br,
		Logger: discardLogger(), Audit: mem,
		Now: func() time.Time { return h.now },
	})
	h.eng.fillWait = 20 * time.Millisecond
	h.eng.fillPoll = time.Millisecond
	return br
}

// A sell the broker refuses because it holds nothing is the store being wrong, and
// no number of retries changes that. The store is corrected against the broker and
// the position closes, rather than the same order going out every 2-second tick —
// QTEX sent 2,862 of them over three hours on 2026-10-05.
func TestRejectedSellCorrectsTheStoreAgainstTheBroker(t *testing.T) {
	h := newHarness(t)
	h.fastFills()
	pos := h.buyOne(t, "ABCD", 5.00)

	var mem audit.Memory
	br := h.refuseSellsAfterEntry(&mem) // the broker holds nothing

	// Hold the resting order inert so the exit goes through the engine's own sell
	// rather than the broker filling the stop itself — the sell is what is under test.
	h.fake.SetStopsInert(true)
	h.at(15, 45)
	h.fake.SetPrice("ABCD", pos.StopPrice-0.05)
	h.eng.Tick(context.Background())

	if br.sells != 1 {
		t.Errorf("sell orders = %d, want the one refused attempt", br.sells)
	}
	after := h.positionByID(t, pos.ID)
	if after.Open {
		t.Error("position is still open against a broker that holds nothing")
	}
	if after.ExitReason != domain.ExitReconciled {
		t.Errorf("exit reason = %q, want %q: there was no fill to record",
			after.ExitReason, domain.ExitReconciled)
	}
	if _, ok := findEvent(mem.Events(), audit.Reconciled, "ABCD"); !ok {
		t.Error("no RECONCILED event; writing off shares must leave a trail")
	}

	// Closed, so nothing is left to retry.
	h.eng.Tick(context.Background())
	if br.sells != 1 {
		t.Errorf("sell orders = %d after a second tick, want no retry", br.sells)
	}
}

// A refusal with the holding intact is not the store's fault — a halted symbol, a
// wash-trade block. A retry does not resolve that either, so it is latched and sent
// once rather than every tick.
func TestRejectedSellWithTheHoldingIntactIsNotRetried(t *testing.T) {
	h := newHarness(t)
	h.fastFills()
	pos := h.buyOne(t, "ABCD", 5.00)

	br := h.refuseSellsAfterEntry(nil, broker.BrokerPosition{
		Symbol: "ABCD", Shares: pos.SharesOpen, AvgEntry: pos.EntryPrice,
	})

	h.fake.SetStopsInert(true)
	h.at(15, 45)
	h.fake.SetPrice("ABCD", pos.StopPrice-0.05)
	for range 5 {
		h.eng.Tick(context.Background())
	}

	if br.sells != 1 {
		t.Errorf("sell orders = %d over five ticks, want one: a standing refusal is "+
			"not something a retry resolves", br.sells)
	}
	// The shares are really there, so the position stays open and stays reported.
	if after := h.positionByID(t, pos.ID); !after.Open {
		t.Error("position was closed; the broker holds the shares and the refusal was something else")
	}
	// Not re-sending the order must not make the problem look resolved: a position
	// the rules wanted sold is still held, and the page says so until it is.
	if state, msg := h.eng.State(); state != domain.StateError {
		t.Errorf("state = %q (%s), want ERROR while an exit the rules called for cannot be placed",
			state, msg)
	}
}

// Two ops failing in turn used to defeat the fault de-duplication entirely: a single
// slot meant each overwrote the other's message, so neither ever looked like a
// repeat. On 2026-10-05 "manage positions" and "force exit" alternated on the same
// stuck QTEX sell and wrote 246 copies of one fault.
func TestAlternatingOpsDoNotDefeatFaultDeduplication(t *testing.T) {
	h := newHarness(t)
	var mem audit.Memory
	h.eng = New(Deps{
		Config: h.cfg, Store: h.store, Data: h.fake, Trading: h.fake,
		Logger: discardLogger(), Audit: &mem,
		Now: func() time.Time { return h.now },
	})

	stuck := errors.New(`asset "ABCD" cannot be sold short`)
	for range 10 {
		h.eng.fail("manage positions", stuck)
		h.eng.fail("force exit", stuck)
	}

	faults := 0
	for _, ev := range mem.Events() {
		if ev.Kind == audit.Fault {
			faults++
		}
	}
	if faults != 2 {
		t.Errorf("faults = %d, want 2 — one per op, however many times each repeats", faults)
	}
}
