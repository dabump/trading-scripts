package engine

import (
	"context"
	"fmt"
	"time"

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

// protectEntry leaves a freshly bought position's stop resting at the broker, and
// reports whether it did, with a note when it did not or when the order is not live.
//
// The usual case is that the stop came attached to the buy (submit's stopLoss) and
// only has to be adopted. A short fill gets a fresh stop for exactly what was bought
// instead, because a leg sized for the whole order would sell shares that were never
// held. With no leg — the extended session, or the broker refused the attachment —
// the stop is placed separately, as soon after the fill as the daemon can.
//
// A failure is reported and faulted but does not unwind the buy: selling straight back
// across the spread is a certain loss to avoid an uncertain one, and with no order
// working the engine evaluates the stop on the tick (stopIsUnenforced).
func (e *Engine) protectEntry(ctx context.Context, sess scheduler.Session, pos domain.Position,
	f fill, ordered int, extendedHours bool) (bool, string) {
	if f.StopLegID != "" {
		pos.StopOrderID = f.StopLegID
		if err := e.store.SetStopOrderID(pos.ID, f.StopLegID); err != nil {
			e.log.Error("could not record the attached stop", "symbol", pos.Symbol, "err", err)
		}
		if f.Shares >= ordered {
			return true, ""
		}
		if _, err := e.releaseProtectiveStop(ctx, &pos); err != nil {
			// Still recorded on the row, so every sell cancels it first.
			e.faultStop(pos, "the stop attached to a short-filled buy could not be resized", err)
			return true, "attached to the buy, but sized for more than was bought and could not be resized"
		}
	}

	id, err := e.placeProtectiveStop(ctx, sess, pos)
	if err != nil {
		e.faultStop(pos, "the resting stop order could not be placed; its stop is evaluated on the tick instead", err)
		return false, "it could not be placed: " + err.Error()
	}
	pos.StopOrderID = id
	if extendedHours {
		// Accepted, but a stop cannot trigger in the extended session, so it is inert
		// until the bell. The engine covers that window.
		return true, "resting at the broker, but a stop cannot trigger before 09:30 — " +
			"until the open the agent holds this stop on the scan tick"
	}
	if f.StopAttachErr != "" {
		return true, "placed just after the buy: the broker refused it attached (" + f.StopAttachErr + ")"
	}
	return true, ""
}

// faultStop records that a position is not protected the way it should be.
func (e *Engine) faultStop(pos domain.Position, what string, err error) {
	e.log.Error("protective stop: "+what, "symbol", pos.Symbol, "stop", pos.StopPrice, "err", err)
	e.record(audit.Fault, pos.Symbol, fmt.Sprintf("%s: %s", pos.Symbol, what),
		map[string]any{
			"protective_stop": "not placed",
			"stop_price":      pos.StopPrice,
			"shares":          pos.SharesOpen,
			"err":             err.Error(),
		})
}

// pollProtectiveStop asks whether a position's resting stop has fired.
//
// The broker does not call back, so asking is the only way to learn. Each call is
// one order lookup; checkProtectiveStop decides when one is worth making.
//
// Only a *terminal* order is an outcome. A stop filling in pieces reports
// "partially_filled" with a share count that is a snapshot of a sale still in
// progress, not the exit: the broker owns the remaining shares and goes on selling
// them. Reading that snapshot as the whole exit banks shares that are about to sell
// anyway and — because recording it clears the id — hands the floor to the engine
// while the order is still live, the one state the two are meant never to share. The
// engine then tries to sell shares the broker no longer holds, every tick, forever.
// That is what happened to QTEX on 2026-10-05. Keep asking instead; the order
// becomes "filled", or "canceled" with a partial, and both of those are answers.
// releaseProtectiveStop reads the same status the same way.
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
	if !res.Done() {
		return broker.OrderResult{}, nil
	}
	if res.FilledShares > 0 {
		return res, nil
	}
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

// protectiveStopPoll is how often a resting stop is looked up while price is above it.
// The broker enforces the stop either way; the lookup is only how the daemon learns
// that it fired, and every sell looks the order up before selling regardless. At
// or under the stop it is asked on every tick.
const protectiveStopPoll = 15 * time.Second

// checkProtectiveStop asks whether p's resting stop has fired, when that is worth a
// request: on every tick once price is at or under the stop, otherwise every
// protectiveStopPoll. One lookup per position per 2-second tick would be 30 requests a
// minute each, against a 200/min budget the scan already spends most of.
func (e *Engine) checkProtectiveStop(ctx context.Context, p *domain.Position, price float64) (broker.OrderResult, error) {
	if p.StopOrderID == "" {
		return broker.OrderResult{}, nil
	}
	now := e.now()
	near := price > 0 && price <= p.StopPrice*(1+protectiveStopNear)
	if !near && now.Sub(e.stopPolled[p.ID]) < protectiveStopPoll {
		return broker.OrderResult{}, nil
	}
	if e.stopPolled == nil {
		e.stopPolled = map[int64]time.Time{}
	}
	e.stopPolled[p.ID] = now
	return e.pollProtectiveStop(ctx, p)
}

// protectiveStopNear widens "at the stop" a little for checkProtectiveStop: the mark
// is the last trade, and a stop that printed between two marks may already have
// filled while the next mark sits a cent above it.
const protectiveStopNear = 0.005

// moveProtectiveStop re-places p's resting stop at p.StopPrice, after the candle trail
// has raised it. Cancel, confirm, place: a replacement that lost the race to the old
// order filling records that fill instead.
//
// Any failure leaves the position safe rather than unprotected. If the old order could
// not be taken off it still rests at the lower stop, and the engine evaluates the new
// one on the tick; if the new one could not be placed, the tick holds it alone. A raised
// stop that price is already under is not placed at all — the broker would refuse it —
// and the tick sells the position instead.
func (e *Engine) moveProtectiveStop(ctx context.Context, sess scheduler.Session, p *domain.Position, price float64) error {
	if p.StopOrderID == "" {
		return nil
	}
	res, err := e.releaseProtectiveStop(ctx, p)
	if err != nil {
		e.log.Warn("protective stop not moved; it still rests at the old stop",
			"symbol", p.Symbol, "stop", p.StopPrice, "err", err)
		return nil
	}
	if res.FilledShares > 0 {
		return e.closeOnProtectiveStop(*p, res)
	}
	return e.restoreProtectiveStop(ctx, sess, p.ID, price)
}

// restoreProtectiveStop places a resting stop for what is held of the position now,
// at its stored stop — after a scale-out or a move took the old one off.
func (e *Engine) restoreProtectiveStop(ctx context.Context, sess scheduler.Session, id int64, price float64) error {
	pos, err := e.store.PositionByID(id)
	if err != nil {
		return err
	}
	if !pos.Open || pos.SharesOpen <= 0 || pos.StopOrderID != "" {
		return nil
	}
	if price > 0 && price <= pos.StopPrice {
		return nil
	}
	if _, err := e.placeProtectiveStop(ctx, sess, pos); err != nil {
		e.faultStop(pos, "the resting stop order could not be re-placed; its stop is evaluated on the tick instead", err)
	}
	return nil
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
