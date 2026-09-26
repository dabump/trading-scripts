package strategy

import (
	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// ExitDecision is the outcome of evaluating one open position.
type ExitDecision struct {
	Exit   bool
	Reason domain.ExitReason
}

// priceEpsilon absorbs binary floating-point error in threshold comparisons.
// Thresholds are products like peak*0.95, which is not exactly representable:
// 6.00*0.95 evaluates to 5.699999999999999, so a price of exactly 5.70 would
// otherwise read as above a 5% drawdown and skip an exit the rules require. The
// tolerance is far below one hundredth of a cent, so it cannot mask a real move.
const priceEpsilon = 1e-9

// atOrBelow reports price <= threshold, treating an exact-threshold price as
// below it despite floating-point representation.
func atOrBelow(price, threshold float64) bool {
	return price <= threshold+priceEpsilon
}

// atOrAbove is atOrBelow's counterpart, used for the profit target.
func atOrAbove(price, threshold float64) bool {
	return price >= threshold-priceEpsilon
}

// ExitInput is everything needed to decide whether a position should close.
type ExitInput struct {
	Position domain.Position
	Price    float64
	// Closes are the session's candle closes at the configured MACD interval,
	// oldest first. Fewer than the warm-up length simply means the MACD trigger
	// cannot fire yet.
	Closes []float64
	// EODReached is true once the clock passes the forced-exit mark.
	EODReached bool
}

// EvaluateExit applies the four exit triggers from docs/strategy.md §4 in
// priority order and reports the first that fires.
//
// Order matters because it decides which reason gets recorded when several are
// true at once. The forced end-of-day exit comes first because it is a hard
// deadline. The stop-loss comes next: docs/risk.md requires it to take
// precedence over the momentum-based signals so that a gap or bug in those
// cannot turn into an unbounded loss.
func EvaluateExit(in ExitInput, cfg *config.Config) ExitDecision {
	if in.EODReached {
		return ExitDecision{Exit: true, Reason: domain.ExitForcedEOD}
	}

	entry := in.Position.EntryPrice
	if entry <= 0 || in.Price <= 0 {
		return ExitDecision{}
	}

	if atOrBelow(in.Price, entry*(1-cfg.Risk.StopLossPct/100)) {
		return ExitDecision{Exit: true, Reason: domain.ExitStopLoss}
	}

	// The trailing stop only arms once the profit target has been reached; before
	// that the hard stop-loss is the only floor. This is how docs/strategy.md §4
	// words it ("take profit once up some %, then trail a stop below the peak").
	peak := in.Position.PeakPrice
	if peak < entry {
		peak = entry
	}
	armed := in.Position.TrailArmed || atOrAbove(peak, entry*(1+cfg.Exit.ProfitTargetPct/100))
	if armed && atOrBelow(in.Price, peak*(1-cfg.Exit.TrailingStopPct/100)) {
		return ExitDecision{Exit: true, Reason: domain.ExitTrailingStop}
	}

	if MACDBearishCross(in.Closes, cfg.Exit.MACDFast, cfg.Exit.MACDSlow, cfg.Exit.MACDSignal) {
		return ExitDecision{Exit: true, Reason: domain.ExitMACDCrossover}
	}

	return ExitDecision{}
}

// TrailArmed reports whether a position has reached its profit target, which
// latches the trailing stop on for the rest of the hold.
func TrailArmed(p domain.Position, cfg *config.Config) bool {
	if p.TrailArmed {
		return true
	}
	peak := p.PeakPrice
	if peak < p.EntryPrice {
		peak = p.EntryPrice
	}
	return p.EntryPrice > 0 && atOrAbove(peak, p.EntryPrice*(1+cfg.Exit.ProfitTargetPct/100))
}
