package risk

// Position sizing from a fixed dollar risk, which is the rule docs/risk.md now
// specifies.
//
// The share count is whatever makes the distance to the stop cost a fixed fraction
// of the account. This is the difference between "every trade risks 1% of the
// account" and "every trade commits 5% of the account": under the latter, a trade
// whose stop is 5% away loses ten times as much as one whose stop is 0.5% away, so
// the account's worst days are decided by which setups happened to be wide rather
// than by any decision anyone made.

import (
	"fmt"
	"math"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// shareEpsilon absorbs binary floating-point error before the share count is
// truncated.
//
// Risk per share is a difference of two prices, and a difference that is exact on
// paper usually is not in binary: 10.00 − 9.60 evaluates to 0.4000000000000004, so a
// $100 budget divides to 249.99… and floors to 249 rather than the 250 anyone reading
// the audit trail would compute. The tolerance is far smaller than one share, so it
// cannot invent one that the budget does not cover. Same reasoning as
// strategy.priceEpsilon.
const shareEpsilon = 1e-9

func floorShares(v float64) int {
	return int(math.Floor(v + shareEpsilon))
}

// SizeForRisk sizes a position from its entry and stop.
//
// Three limits apply in order: the risk budget sets the share count, the notional cap
// bounds one position's size regardless of how tight its stop is, and available cash
// bounds it again because portfolio value includes positions already held.
func SizeForRisk(acct domain.Account, entry, stop float64, cfg *config.Config) Sizing {
	if entry <= 0 {
		return Sizing{Reason: "no valid entry price"}
	}
	if stop <= 0 || stop >= entry {
		return Sizing{Reason: fmt.Sprintf("stop %.4f is not below the %.4f entry", stop, entry)}
	}
	if acct.PortfolioValue <= 0 {
		return Sizing{Reason: "portfolio value is zero"}
	}

	riskPerShare := entry - stop
	budget := acct.PortfolioValue * cfg.Risk.RiskPerTradePct / 100
	shares := floorShares(budget / riskPerShare)
	if shares < 1 {
		return Sizing{Reason: fmt.Sprintf(
			"a $%.2f risk budget buys no shares at $%.4f of risk each", budget, riskPerShare)}
	}

	// Cap the notional. A very tight stop would otherwise size to a position larger
	// than the account, turning a small risk-per-share into enormous exposure to a
	// gap that jumps the stop entirely.
	maxNotional := acct.PortfolioValue * cfg.Risk.MaxPositionPct / 100
	if cash := acct.Cash; cash < maxNotional {
		maxNotional = cash
	}
	if maxNotional <= 0 {
		return Sizing{Reason: "no cash available"}
	}
	if affordable := floorShares(maxNotional / entry); affordable < shares {
		shares = affordable
	}
	if shares < 1 {
		return Sizing{Reason: fmt.Sprintf(
			"position cap of $%.2f is below one share at $%.2f", maxNotional, entry)}
	}

	return Sizing{
		OK:         true,
		Shares:     shares,
		Dollars:    float64(shares) * entry,
		RiskDollar: float64(shares) * riskPerShare,
	}
}
