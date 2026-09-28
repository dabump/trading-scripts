// Package risk owns position sizing and the limits from docs/risk.md.
package risk

import (
	"fmt"

	"github.com/martincoetzee/trading-agent/internal/config"
)

// Sizing is the outcome of sizing one prospective position.
type Sizing struct {
	OK      bool
	Shares  int
	Dollars float64
	// RiskDollar is what the position loses if the stop fills at its price. It is
	// recorded so the audit trail can show that risk really was held constant across
	// trades of very different sizes.
	RiskDollar float64
	// Reason explains a refusal, so the log says why a qualifying candidate was
	// skipped rather than silently dropping it.
	Reason string
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
