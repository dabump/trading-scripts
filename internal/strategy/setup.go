package strategy

// The entry setup: what a screened candidate has to print before it is bought.
//
// docs/strategy.md §3 describes this as a pullback-and-resumption, which is the
// mechanical reading of the pattern the strategy is built around. The shape is:
//
//	         ← pole: the session's high so far
//	        /|
//	       / |  ‾\__   ← flag: one to a few bars that stay below the pole
//	      /  |      \__
//	  ___/                ▲ trigger: a bar closes back above the pole high
//
// Two things fall out of that shape, and both matter more than the entry itself:
// the stop has an obvious home (just under the flag's low) and therefore the risk
// per share is known *before* the position is sized. Buying on the screen alone
// gives neither — there is no reference price, so the stop has to be an arbitrary
// percentage and the size has to be an arbitrary fraction of the account.

import (
	"fmt"
	"math"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// Setup is the outcome of looking for an entry in one symbol's bars.
type Setup struct {
	// Triggered is true when the pattern completed on the most recent bar.
	Triggered bool
	// Entry is the price the setup triggers at: the close of the trigger bar, which
	// is the last price known when the decision is made.
	Entry float64
	// Stop is the chart stop, just under the flag's low.
	Stop float64
	// RiskPerShare is Entry − Stop, which is what the position is sized from.
	RiskPerShare float64
	// StopDistancePct is the stop expressed as a percentage of entry, for logging
	// and for the distance limits.
	StopDistancePct float64
	// PoleHigh is the level the trigger bar closed above, and FlagBars is how long
	// the pullback lasted. Both are recorded so the audit trail can say what the
	// agent thought it saw.
	PoleHigh float64
	FlagBars int
	// Reason explains a refusal, so a near-miss is visible rather than silent.
	Reason string
}

// minBarsForSetup is how much session history the detector needs before it will
// call anything: enough for the EMA to mean something, plus the pole, the flag and
// the trigger bar.
func minBarsForSetup(cfg *config.Config) int {
	return cfg.Entry.EMAPeriod + cfg.Entry.MinPullbackBars + 2
}

// FindSetup looks for a completed pullback-and-resumption in bars, which must be
// this session's candles in chronological order, most recent last.
//
// The most recent bar is treated as just closed. Nothing here reads a bar after the
// trigger, so the decision uses only information that existed at the moment it would
// have been made.
func FindSetup(bars []domain.Bar, cfg *config.Config) Setup {
	need := minBarsForSetup(cfg)
	if len(bars) < need {
		return Setup{Reason: fmt.Sprintf("only %d bars so far, need %d", len(bars), need)}
	}

	n := len(bars)
	trigger := bars[n-1]
	if trigger.Close <= 0 {
		return Setup{Reason: "no valid close on the trigger bar"}
	}

	// Trend filters. The strategy only buys strength: price has to be holding above
	// its short EMA, and above the session VWAP, which is the line separating a name
	// being accumulated from one being distributed into.
	closes := make([]float64, n)
	for i, b := range bars {
		closes[i] = b.Close
	}
	ema := EMA(closes, cfg.Entry.EMAPeriod)
	if len(ema) == 0 {
		return Setup{Reason: "not enough data for the EMA"}
	}
	if trigger.Close < ema[len(ema)-1] {
		return Setup{Reason: fmt.Sprintf("close %.2f is below the %d-period EMA %.2f",
			trigger.Close, cfg.Entry.EMAPeriod, ema[len(ema)-1])}
	}
	if cfg.Entry.RequireAboveVWAP {
		vwap := VWAP(bars)
		if vwap > 0 && trigger.Close < vwap {
			return Setup{Reason: fmt.Sprintf("close %.2f is below VWAP %.2f", trigger.Close, vwap)}
		}
	}

	// The pole is the highest high before the flag, which for this pattern is the
	// session high so far. Taking the *last* such bar matters when the high is
	// matched twice: the flag is measured from the most recent one.
	poleIdx := 0
	for i := 1; i <= n-2; i++ {
		if bars[i].High >= bars[poleIdx].High {
			poleIdx = i
		}
	}
	pole := bars[poleIdx]

	// The flag is every bar between the pole and the trigger. By construction none
	// of them exceeded the pole high.
	flagBars := bars[poleIdx+1 : n-1]
	if len(flagBars) < cfg.Entry.MinPullbackBars {
		return Setup{Reason: fmt.Sprintf("pullback is %d bars, need at least %d",
			len(flagBars), cfg.Entry.MinPullbackBars)}
	}
	if len(flagBars) > cfg.Entry.MaxPullbackBars {
		return Setup{Reason: fmt.Sprintf(
			"pullback is %d bars, more than the %d allowed — the move has stalled rather than paused",
			len(flagBars), cfg.Entry.MaxPullbackBars)}
	}

	// The trigger: the bar closes back above the level the flag failed to clear.
	if trigger.Close <= pole.High {
		return Setup{Reason: fmt.Sprintf("close %.2f has not cleared the %.2f pullback high",
			trigger.Close, pole.High)}
	}

	// The stop lives just under the flag's low — the price that says the pullback was
	// not a pullback.
	low := math.Inf(1)
	for _, b := range flagBars {
		low = math.Min(low, b.Low)
	}
	if math.IsInf(low, 0) || low <= 0 {
		return Setup{Reason: "no valid low in the pullback"}
	}
	stop := low * (1 - cfg.Entry.StopBufferPct/100)

	// Distance limits. A stop too far away is refused outright rather than sized
	// around: that is part of the strategy, not a safety rail.
	dist := (trigger.Close - stop) / trigger.Close * 100
	if dist <= 0 {
		return Setup{Reason: "the pullback low is at or above the trigger close"}
	}
	if dist > cfg.Entry.MaxStopDistancePct {
		return Setup{Reason: fmt.Sprintf(
			"stop is %.2f%% away, beyond the %.2f%% limit — risk too wide to take",
			dist, cfg.Entry.MaxStopDistancePct)}
	}
	if dist < cfg.Entry.MinStopDistancePct {
		// Too close to be real: widen it rather than accept a stop inside the noise.
		stop = trigger.Close * (1 - cfg.Entry.MinStopDistancePct/100)
		dist = cfg.Entry.MinStopDistancePct
	}

	return Setup{
		Triggered:       true,
		Entry:           trigger.Close,
		Stop:            stop,
		RiskPerShare:    trigger.Close - stop,
		StopDistancePct: dist,
		PoleHigh:        pole.High,
		FlagBars:        len(flagBars),
	}
}

// EMA returns the exponential moving average of values, seeded with a simple
// average of the first `period` points so the first published value is not simply
// the first input. The result is aligned to the tail of values: the last element
// corresponds to the last input.
func EMA(values []float64, period int) []float64 {
	if period < 1 || len(values) < period {
		return nil
	}
	var seed float64
	for _, v := range values[:period] {
		seed += v
	}
	prev := seed / float64(period)

	out := make([]float64, 0, len(values)-period+1)
	out = append(out, prev)
	k := 2.0 / float64(period+1)
	for _, v := range values[period:] {
		prev = v*k + prev*(1-k)
		out = append(out, prev)
	}
	return out
}

// VWAP is the volume-weighted average price of the session so far, using each
// candle's typical price. It returns 0 when there is no volume to weight by, which
// callers treat as "no opinion" rather than as a price of zero.
func VWAP(bars []domain.Bar) float64 {
	var pv, vol float64
	for _, b := range bars {
		typical := (b.High + b.Low + b.Close) / 3
		pv += typical * b.Volume
		vol += b.Volume
	}
	if vol <= 0 {
		return 0
	}
	return pv / vol
}

// WarmupDuration is how long after the open the first setup can possibly appear,
// which is what the status page shows and what explains an empty early screen.
func WarmupDuration(cfg *config.Config) time.Duration {
	return time.Duration(minBarsForSetup(cfg)) * cfg.Entry.PatternInterval
}
