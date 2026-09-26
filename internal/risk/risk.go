// Package risk owns position sizing and the limits from docs/risk.md.
package risk

import (
	"fmt"
	"math"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// Sizing is the outcome of sizing one prospective position.
type Sizing struct {
	OK      bool
	Shares  int
	Dollars float64
	// Reason explains a refusal, so the log says why a qualifying candidate was
	// skipped rather than silently dropping it.
	Reason string
}

// Size allocates the configured share of the portfolio to one position.
//
// The allocation is clamped to available cash: the target is a percentage of
// total portfolio value, but portfolio value includes the market value of
// positions already held, so on a fully-deployed account the untouched
// percentage could still exceed the cash left to spend. Submitting an order the
// account cannot fund just earns a broker rejection mid-session.
func Size(acct domain.Account, price float64, cfg *config.Config) Sizing {
	if price <= 0 {
		return Sizing{Reason: "no valid price"}
	}
	if acct.PortfolioValue <= 0 {
		return Sizing{Reason: "portfolio value is zero"}
	}

	target := acct.PortfolioValue * cfg.Risk.PositionSizePct / 100
	spend := math.Min(target, acct.Cash)
	if spend <= 0 {
		return Sizing{Reason: "no cash available"}
	}

	shares := int(math.Floor(spend / price))
	if shares < 1 {
		return Sizing{Reason: fmt.Sprintf(
			"allocation $%.2f is below one share at $%.2f", spend, price)}
	}
	return Sizing{OK: true, Shares: shares, Dollars: float64(shares) * price}
}

// CanOpen reports whether another position fits under the concurrency cap.
func CanOpen(openPositions int, cfg *config.Config) bool {
	return openPositions < cfg.Risk.MaxConcurrentPositions
}

// FreeSlots is how many more positions may be opened right now.
func FreeSlots(openPositions int, cfg *config.Config) int {
	free := cfg.Risk.MaxConcurrentPositions - openPositions
	if free < 0 {
		return 0
	}
	return free
}

// AllowEntry gates a single symbol at entry time, applying the concurrency cap,
// the same-day re-entry rule, and the requirement that only one position per
// symbol is held at a time.
//
// The third return value marks a refusal as routine. A symbol already held or
// already traded today is refused on every scan for the rest of the session, so
// at a one-minute cadence logging those at info level would bury everything else
// under hundreds of identical lines.
func AllowEntry(symbol string, openSymbols map[string]bool, tradedToday map[string]bool, openCount int, cfg *config.Config) (allowed bool, reason string, routine bool) {
	if !CanOpen(openCount, cfg) {
		return false, fmt.Sprintf("position cap reached (%d)", cfg.Risk.MaxConcurrentPositions), false
	}
	if openSymbols[symbol] {
		return false, "already holding this symbol", true
	}
	if !cfg.Risk.AllowSameDayReentry && tradedToday[symbol] {
		return false, "already traded today and same-day re-entry is disabled", true
	}
	return true, "", false
}
