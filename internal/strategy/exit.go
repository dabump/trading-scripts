package strategy

import (
	"math"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// ExitDecision is the outcome of evaluating one open position. At most one action is
// due at a time, and the order EvaluateExit checks them in is the priority.
type ExitDecision struct {
	// Exit closes whatever is still held.
	Exit   bool
	Reason domain.ExitReason
	// Scale is a partial sale at the first profit target: the position stays open
	// with ScaleShares fewer shares.
	Scale       bool
	ScaleShares int
	// NewStop is non-zero when the stop should be moved, which happens when the
	// first target is banked and the runner's stop goes to breakeven.
	NewStop float64
}

// priceEpsilon absorbs binary floating-point error in threshold comparisons.
// Thresholds are products like entry*0.9 or stop levels derived from a percentage
// buffer, none of which are exactly representable: 4.20*0.9 evaluates to
// 3.7800000000000002, so a price of exactly 3.78 would otherwise read as above the
// stop and skip an exit the rules require. The tolerance is far below one hundredth
// of a cent, so it cannot mask a real move.
const priceEpsilon = 1e-9

// atOrBelow reports price <= threshold, treating an exact-threshold price as
// below it despite floating-point representation.
func atOrBelow(price, threshold float64) bool {
	return price <= threshold+priceEpsilon
}

// atOrAbove is its counterpart, used for profit targets.
func atOrAbove(price, threshold float64) bool {
	return price >= threshold-priceEpsilon
}

// ExitInput is everything needed to decide what to do with an open position.
type ExitInput struct {
	Position domain.Position
	Price    float64
	// EODReached is true once the clock passes the forced-exit mark.
	EODReached bool
}

// EvaluateExit applies the exit rules from docs/strategy.md §4 in priority order and
// reports the first action that is due.
//
// The order is: the forced end-of-day deadline, then the stop, then the first profit
// target. Risk before reward — docs/risk.md requires the stop to be a floor nothing
// can bypass, and on a bar where price traded through both the stop and the target
// the stop is the honest outcome to record.
//
// Two rules were removed from here on evidence and should not come back without
// their own:
//
//   - A MACD bearish crossover fired on 24.9% of trades for a mean of −0.08%, doing
//     no work while costing a bar request per position per tick.
//   - A fixed-percentage profit target arming a fixed-percentage trailing stop. Armed
//     at +15% and trailing 5% it could not mathematically exit above +9.25% and in
//     practice exited at +9.31%, while those same positions went on to average +53%.
//     Over one year and 1,697 trades it turned a per-trade mean of +0.18% into
//     −2.13%, a t-statistic of −9.76.
//
// The scale-out below is a different rule and not a re-run of that mistake: it sells
// a *fraction* at a multiple of the trade's own risk and leaves a runner, so the
// right tail stays open. The distinction is the whole point — this strategy's return
// lives in that tail, and a rule that closes all of it is taking the part that pays
// for every loss.
func EvaluateExit(in ExitInput, cfg *config.Config) ExitDecision {
	if in.EODReached {
		return ExitDecision{Exit: true, Reason: domain.ExitForcedEOD}
	}

	p := in.Position
	if p.EntryPrice <= 0 || in.Price <= 0 || p.SharesOpen <= 0 {
		return ExitDecision{}
	}

	// The working stop is the one the setup put on the chart. The percentage stop
	// behind it is a backstop for a gap straight through, and whichever is higher is
	// the one that protects the position.
	stop := p.StopPrice
	if backstop := p.EntryPrice * (1 - cfg.Risk.StopLossPct/100); backstop > stop {
		stop = backstop
	}
	if stop > 0 && atOrBelow(in.Price, stop) {
		return ExitDecision{Exit: true, Reason: domain.ExitStopLoss}
	}

	// The first target, once, at a multiple of this trade's own initial risk.
	if !p.TargetHit && p.InitialRisk > 0 {
		target := p.EntryPrice + p.InitialRisk*cfg.Exit.FirstTargetR
		if atOrAbove(in.Price, target) {
			shares := scaleShares(p.SharesOpen, cfg.Exit.FirstTargetFraction)
			if shares > 0 {
				d := ExitDecision{Scale: true, ScaleShares: shares}
				if cfg.Exit.BreakevenAfterTarget && p.EntryPrice > p.StopPrice {
					d.NewStop = p.EntryPrice
				}
				return d
			}
			// Too small to split — one share cannot be scaled out of. Let it run to
			// the stop or the bell rather than closing the whole thing at the target,
			// which is the truncation this strategy cannot afford.
		}
	}

	return ExitDecision{}
}

// EvaluateManualExit is the exit rule for a position opened from the status page,
// which is a much shorter rule than the one above: the forced end-of-day deadline,
// then the position's own stop. Nothing else.
//
// A manual position has no signal to read. The operator overrode the setup gate to
// open it, so the backstop, the candle trail and the scale-out — all of which are
// tuned to the pattern the gate looks for — have nothing to say about it, and the
// measured evidence behind them was gathered on trades the gate allowed. What does
// apply is the stop, because it is the stop that sized the position: the share count
// came from entry − stop, so letting price through it means losing more than the
// risk budget the trade was opened under.
//
// It lives here rather than in the engine so the rule has one home, the way
// EvaluateExit does. In the normal case the engine does not call it at all — a
// manual position's stop rests at the broker as a real order (see
// engine.placeProtectiveStop), and this is what covers the gaps that order cannot:
// the end of the day, which an order has no concept of, and the windows where the
// order does not exist or cannot yet trigger.
func EvaluateManualExit(in ExitInput) ExitDecision {
	if in.EODReached {
		return ExitDecision{Exit: true, Reason: domain.ExitForcedEOD}
	}
	p := in.Position
	if p.EntryPrice <= 0 || in.Price <= 0 || p.SharesOpen <= 0 {
		return ExitDecision{}
	}
	if p.StopPrice > 0 && atOrBelow(in.Price, p.StopPrice) {
		return ExitDecision{Exit: true, Reason: domain.ExitStopLoss}
	}
	return ExitDecision{}
}

// CandleTrailStop is the stop the candle trail (exit.candle_trail) puts under a
// position once last has completed: that candle's low, so the next candle to trade
// below it sells what is held. It returns 0 when the trail does not apply — switched
// off, waiting for the first target, or a candle that opened before the position was
// filled.
//
// That last case includes the candle the buy landed in. A live order fills part-way
// through a candle, so its low is usually a price printed *before* the fill, a
// fraction under the entry — and on a thin pre-market book it can be the fill price
// itself. Trailing to it put the stop at or within a cent of entry within the first
// minute, and on 2026-09-29 that stopped out 10 of 11 trailed positions, four at
// "breakeven" that was a loss after the spread. A candle counts only if the whole of
// it was held; the chart stop covers the entry candle.
//
// Callers only ever raise the stop to this, never lower it, which is what makes it a
// trail: a lower low after a higher one does not give ground back.
func CandleTrailStop(p domain.Position, last domain.Bar, cfg *config.Config) float64 {
	switch cfg.Exit.CandleTrail {
	case config.CandleTrailAlways:
	case config.CandleTrailAfterTarget:
		if !p.TargetHit {
			return 0
		}
	default:
		return 0
	}
	if last.Time.Before(p.EntryTime) || last.Low <= 0 {
		return 0
	}
	return last.Low
}

// LastCompletedBar returns the most recent bar that had closed by now, and false if
// none had. Bar feeds include the candle still forming; trailing on its low would be
// trailing on a price that can still move.
func LastCompletedBar(bars []domain.Bar, now time.Time, interval time.Duration) (domain.Bar, bool) {
	for i := len(bars) - 1; i >= 0; i-- {
		if !bars[i].Time.Add(interval).After(now) {
			return bars[i], true
		}
	}
	return domain.Bar{}, false
}

// scaleShares is how many shares the partial sale takes, rounded down but never to
// zero while at least one share can be left behind. A two-share position sells one
// and keeps one; a one-share position cannot scale at all.
func scaleShares(open int, fraction float64) int {
	if open < 2 || fraction <= 0 || fraction >= 1 {
		return 0
	}
	n := int(math.Floor(float64(open) * fraction))
	if n < 1 {
		n = 1
	}
	if n >= open {
		n = open - 1
	}
	return n
}
