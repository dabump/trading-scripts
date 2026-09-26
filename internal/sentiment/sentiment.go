// Package sentiment implements the first-hour gate from docs/strategy.md §1.
package sentiment

import (
	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// Classify turns one poll's percentage changes into a verdict.
//
// Only an overwhelmingly bearish reading halts trading: a neutral or unclear
// reading proceeds, which is the rule settled in docs/decisions.md. The average
// is taken across the configured symbols, and RequireAllNegative additionally
// demands that none of them is up — one index holding green is treated as the
// market not being uniformly bearish.
func Classify(pcts map[string]float64, cfg *config.Config) domain.Verdict {
	if len(pcts) == 0 {
		// No data is not evidence of a bearish tape, but it is also not something
		// to trade on. Callers treat PENDING as "keep polling".
		return domain.VerdictPending
	}

	var sum float64
	var n int
	allNegative := true
	for _, sym := range cfg.Sentiment.Symbols {
		v, ok := pcts[sym]
		if !ok {
			continue
		}
		sum += v
		n++
		if v >= 0 {
			allNegative = false
		}
	}
	if n == 0 {
		return domain.VerdictPending
	}

	avg := sum / float64(n)
	bearish := avg <= cfg.Sentiment.BearishAvgPct
	if cfg.Sentiment.RequireAllNegative && !allNegative {
		bearish = false
	}
	if bearish {
		return domain.VerdictBearish
	}
	return domain.VerdictProceed
}

// GateVerdict decides the session from the first hour's readings.
//
// docs/strategy.md §1 says the classification happens "at the end of the hour",
// so the most recent reading decides: the earlier polls exist to be recorded and
// displayed, not to be averaged into the decision. A tape that sold off at 09:40
// and recovered by 10:30 should not halt the day.
func GateVerdict(readings []domain.SentimentReading, cfg *config.Config) domain.Verdict {
	if len(readings) == 0 {
		return domain.VerdictPending
	}
	last := readings[len(readings)-1]
	return Classify(last.Percentages, cfg)
}

// PercentChange is the move from a previous close to the current price.
func PercentChange(current, prevClose float64) (float64, bool) {
	if prevClose <= 0 || current <= 0 {
		return 0, false
	}
	return (current - prevClose) / prevClose * 100, true
}
