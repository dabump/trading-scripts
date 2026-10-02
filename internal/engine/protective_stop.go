package engine

import (
	"context"
	"fmt"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/store"
)

// A protective stop is left good-till-cancelled. A day order would expire at the
// close and leave the position bare overnight and through the next morning's gap —
// exactly the window the daemon is not watching, and the one that costs the most.
const protectiveStopTIF = "gtc"

// placeProtectiveStop leaves a sell stop order resting at the broker at the
// position's stop price, and records its id on the position.
//
// This is what makes a manual position's 1R stop real rather than advisory. The
// automated path evaluates its stop on the scan tick, which is good enough for a
// position the strategy sized, entered and watches; a position opened by hand is one
// an operator is holding deliberately, through the close and across sessions, and a
// stop that only exists inside a running process is not a stop for that. A resting
// order is enforced between ticks, while the daemon is restarting, and while it is
// not running at all.
//
// It is never sent as an extended-hours order. Alpaca accepts extended_hours only on
// a day limit order, and a stop does not trigger outside 09:30-16:00 in any case, so
// a stop placed pre-market is accepted and sits inert until the opening bell. That
// window is covered by the engine instead — see stopIsUnenforced.
func (e *Engine) placeProtectiveStop(ctx context.Context, sess scheduler.Session, pos domain.Position) (string, error) {
	if pos.SharesOpen <= 0 || pos.StopPrice <= 0 {
		return "", fmt.Errorf("protective stop for %s: %d shares at a stop of %.2f is not an order",
			pos.Symbol, pos.SharesOpen, pos.StopPrice)
	}

	clientOrderID := fmt.Sprintf("%s-%s-stop-%d", sess.Date, pos.Symbol, e.now().UnixNano())
	if err := e.store.RecordOrder(store.OrderRecord{
		ClientOrderID: clientOrderID, SessionDate: sess.Date, Symbol: pos.Symbol,
		Side: "sell", Shares: pos.SharesOpen, SubmittedAt: e.now(), Status: "submitted",
	}); err != nil {
		return "", err
	}

	res, err := e.trading.PlaceOrder(ctx, broker.OrderRequest{
		Symbol: pos.Symbol, Shares: pos.SharesOpen, Side: "sell",
		Type: "stop", StopPrice: pos.StopPrice,
		TimeInForce: protectiveStopTIF, ClientOrderID: clientOrderID,
	})
	if err != nil {
		if updateErr := e.store.UpdateOrderStatus(clientOrderID, "failed", ""); updateErr != nil {
			e.log.Error("failed to record protective stop failure", "err", updateErr)
		}
		return "", fmt.Errorf("place protective stop for %s: %w", pos.Symbol, err)
	}
	if err := e.store.UpdateOrderStatus(clientOrderID, res.Status, res.BrokerOrderID); err != nil {
		return "", err
	}
	if res.BrokerOrderID == "" {
		// Nothing can cancel an order with no id, and an uncancellable sell order
		// outliving its position would go short. Treat it as no stop at all so the
		// engine keeps the floor itself, and say so loudly.
		return "", fmt.Errorf("protective stop for %s was acknowledged without an order id, "+
			"so it could never be cancelled", pos.Symbol)
	}
	if err := e.store.SetStopOrderID(pos.ID, res.BrokerOrderID); err != nil {
		return "", err
	}
	return res.BrokerOrderID, nil
}

// pollProtectiveStop asks whether a position's resting stop has fired.
//
// The broker does not call back, so asking is the only way to learn. That costs one
// order lookup per manual position per tick, which is the same order of expense as
// the candle trail's bar request and is the price of the stop being enforced
// off-process.
//
// A terminal order that filled nothing — cancelled at the broker, rejected, expired —
// is reported as gone: the id is cleared so the engine stops asking and starts
// keeping the floor itself. It is cleared on the caller's copy too, so the takeover
// happens on this tick rather than the next one — the gap between the two is a
// position with no stop at all.
func (e *Engine) pollProtectiveStop(ctx context.Context, pos *domain.Position) (broker.OrderResult, error) {
	if pos.StopOrderID == "" {
		return broker.OrderResult{}, nil
	}
	res, err := e.trading.Order(ctx, pos.StopOrderID)
	if err != nil {
		return broker.OrderResult{}, fmt.Errorf("read protective stop for %s: %w", pos.Symbol, err)
	}
	if res.FilledShares > 0 {
		return res, nil
	}
	if res.Done() {
		if err := e.store.SetStopOrderID(pos.ID, ""); err != nil {
			return broker.OrderResult{}, err
		}
		pos.StopOrderID = ""
		e.log.Warn("protective stop is no longer working; the engine now holds the stop",
			"symbol", pos.Symbol, "status", res.Status)
		e.record(audit.Fault, pos.Symbol,
			fmt.Sprintf("the resting stop order for %s ended as %q without filling; the stop is now "+
				"evaluated on the tick instead", pos.Symbol, res.Status),
			map[string]any{
				"protective_stop": "gone",
				"order_status":    res.Status,
				"stop_price":      pos.StopPrice,
				"shares_open":     pos.SharesOpen,
			})
	}
	return broker.OrderResult{}, nil
}

// releaseProtectiveStop takes a position's resting stop off the book, and reports
// whether that stop had already filled.
//
// Every path that sells a manual position goes through this first, and the order of
// operations is the whole point: a resting sell order that outlives its holding sells
// shares that are no longer there, which is a short position rather than a flat one.
//
// Cancelling is not enough on its own, because the cancel can lose a race with the
// trigger. The order is looked up afterwards and that lookup is the authority: if it
// filled, the caller must record *that* exit rather than sell again. A cancel that
// fails on an order still working is returned as an error, and the caller must not
// sell — being late out of a position is recoverable, being short is not.
//
// The caller's copy has its id cleared along with the row, so nothing downstream can
// read a stale id and try to cancel the order a second time.
func (e *Engine) releaseProtectiveStop(ctx context.Context, pos *domain.Position) (broker.OrderResult, error) {
	if pos.StopOrderID == "" {
		return broker.OrderResult{}, nil
	}

	cancelErr := e.trading.CancelOrder(ctx, pos.StopOrderID)

	// The lookup decides, not the cancel: Alpaca refuses to cancel an order that has
	// already filled, and that refusal is exactly the case that must not be read as a
	// failure to get the order off the book.
	res, err := e.trading.Order(ctx, pos.StopOrderID)
	if err != nil {
		if cancelErr != nil {
			return broker.OrderResult{}, fmt.Errorf(
				"protective stop for %s could not be cancelled (%v) and could not be read: %w",
				pos.Symbol, cancelErr, err)
		}
		return broker.OrderResult{}, fmt.Errorf("read protective stop for %s after cancelling: %w",
			pos.Symbol, err)
	}
	if !res.Done() {
		return broker.OrderResult{}, fmt.Errorf(
			"protective stop for %s is still working as %q (cancel: %v), so selling now could go short",
			pos.Symbol, res.Status, cancelErr)
	}

	if err := e.store.SetStopOrderID(pos.ID, ""); err != nil {
		return broker.OrderResult{}, err
	}
	pos.StopOrderID = ""
	if res.FilledShares > 0 {
		return res, nil
	}
	e.log.Info("protective stop cancelled", "symbol", pos.Symbol, "status", res.Status)
	return broker.OrderResult{}, nil
}

// stopIsUnenforced reports whether the engine has to evaluate a manual position's
// stop itself, because the resting order is not covering it.
//
// Two cases, and neither is the normal one. There may be no order — placing it
// failed, or the broker ended it without a fill — and a manual position with no stop
// at all is the one outcome this must never produce, since the stop is what sized it.
// Or the clock may be before the opening bell, where a stop order is accepted and
// inert: it cannot trigger in the extended session, so between a pre-market open and
// 09:30 the floor is the engine's to hold.
//
// This is not a second stop running alongside the first. The two are mutually
// exclusive by construction, which is what keeps them from both selling the same
// shares — and the sell path cancels the resting order first in any case.
func (e *Engine) stopIsUnenforced(pos domain.Position, bounds scheduler.Boundaries) bool {
	if pos.StopOrderID == "" {
		return true
	}
	return scheduler.PhaseAt(e.now(), bounds) == domain.PhasePreMarket
}
