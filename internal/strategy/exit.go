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

// ExitInput is everything needed to decide whether a position should close.
type ExitInput struct {
	Position domain.Position
	Price    float64
	// EODReached is true once the clock passes the forced-exit mark.
	EODReached bool
}

// EvaluateExit applies the two exit triggers from docs/strategy.md §4 in priority
// order and reports the first that fires.
//
// Order matters because it decides which reason gets recorded when both are true at
// once. The forced end-of-day exit comes first because it is a hard deadline. The
// stop-loss comes next: docs/risk.md requires it to be a floor that cannot be
// bypassed. There is nothing after it — the position runs until the bell.
//
// Two triggers have been removed on evidence, and neither should come back without
// its own:
//
//   - A MACD bearish crossover fired on 24.9% of trades for a mean of −0.08%, doing
//     no work while costing a bar request per position per tick.
//   - A profit target with a trailing stop was far worse than useless. Armed at +15%
//     and trailing 5%, it could not mathematically exit above +9.25% and in practice
//     exited at +9.31% — while those same positions went on to average +53%. It
//     capped the right tail at +9% and left the left tail at −10.7%, which at a 42%
//     win rate cannot be profitable. Over one year and 1,697 trades it turned a
//     per-trade mean of +0.18% into −2.13%, a t-statistic of −9.76. A 150-cell sweep
//     of stop × target × trail found no combination that made money.
//
// The lesson generalises: this strategy's return lives entirely in a thin right tail,
// so any rule that truncates gains is taking the part that pays for everything else.
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

	return ExitDecision{}
}
