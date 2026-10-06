package strategy

// The entry setup: what a screened candidate has to print before it is bought.
//
// It is a micro pullback. A stock surging to a new high of day pauses for one or two
// candles, and is bought when a candle closes back above the last pause candle's
// high (docs/strategy.md §3):
//
//	            ┃ ← top of the surge: the high of day
//	          ┃ ┃ ╻   ← pause: 1-2 candles that close red or undercut the previous low
//	        ┃   ╹ ┃ ← trigger: closes above the last pause candle's high
//	      ┃
//	  surge_bars candles, up at least min_surge_pct
//
// A candle that ticks a marginal new high and then closes red still counts as a
// pause. The flag detector this replaced read every such candle as a new pole, so on
// a vertical mover it reported a 0-bar pullback for as long as the move was clean.
// What does *not* count is a candle that simply failed to extend: see isPauseCandle.
//
// Two things fall out of that shape, and both matter more than the entry itself:
// the stop has an obvious home (just under the pause's low) and therefore the risk
// per share is known *before* the position is sized. Buying on the screen alone
// gives neither — there is no reference price, so the stop has to be an arbitrary
// percentage and the size has to be an arbitrary fraction of the account.
//
// The daemon scans closed candles, so it enters at the trigger candle's close rather
// than the instant price trades through the level. ArmMicroPullback reads the same
// pattern a candle earlier and gives the buy-stop price the discretionary version
// would use; the daemon places no stop orders, so only cmd/backtest calls it.

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
	// Stop is the chart stop, just under the pause's low.
	Stop float64
	// RiskPerShare is Entry − Stop, which is what the position is sized from.
	RiskPerShare float64
	// StopDistancePct is the stop expressed as a percentage of entry, for logging
	// and for the distance limits.
	StopDistancePct float64
	// PauseHigh is the level the trigger bar closed above (the last pause candle's
	// high), and PauseBars is how long the pause lasted. Both are recorded so the
	// audit trail can say what the agent thought it saw.
	PauseHigh float64
	PauseBars int
	// BuyStop is set only by ArmMicroPullback: the price a buy-stop order would
	// enter at on the next candle. Entry equals it.
	BuyStop float64
	// Reason explains a refusal, so a near-miss is visible rather than silent.
	Reason string
}

// minBarsForSetup is how much session history the detector needs before it will
// call anything: the surge, at least one pause candle and the trigger — or the EMA's
// own seed, whichever is longer.
func minBarsForSetup(cfg *config.Config) int {
	return max(cfg.Entry.EMAPeriod, cfg.Entry.SurgeBars+2)
}

// The MACD's periods are the conventional ones and deliberately not configurable:
// the filter is only worth measuring in the form the discretionary approach uses.
const (
	macdFast   = 12
	macdSlow   = 26
	macdSignal = 9
)

// microShape is one pause found in the chart.
type microShape struct {
	// Bars is how many candles the pause lasted.
	Bars int
	// Trigger is the last pause candle's high: the level the entry has to clear.
	Trigger float64
	// Low is the pause's lowest low, which is where the stop goes.
	Low float64
}

// FindSetup looks for a completed micro pullback in bars, which must be this
// session's candles in chronological order, most recent last.
//
// The most recent bar is treated as just closed and as the trigger candle: the pause
// ended on the bar before it, and it must have closed above that bar's high. Nothing
// here reads a bar after the trigger, so the decision uses only information that
// existed at the moment it would have been made.
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
	if reason := microFilters(bars, cfg); reason != "" {
		return Setup{Reason: reason}
	}
	shape, reason := microPause(bars[:n-1], cfg)
	if reason != "" {
		return Setup{Reason: reason}
	}
	if trigger.Close <= shape.Trigger {
		return Setup{Reason: fmt.Sprintf("close %.2f has not cleared the %.2f pullback candle's high",
			trigger.Close, shape.Trigger)}
	}
	setup := stopBelow(trigger.Close, shape.Low, cfg)
	setup.PauseHigh = shape.Trigger
	setup.PauseBars = shape.Bars
	return setup
}

// ArmMicroPullback reads the micro pullback with the pause still in progress: the
// most recent bar is the last pause candle, and a buy-stop one tick above its high
// would enter on the next candle that trades there. Triggered means armed; Entry and
// BuyStop are that price, and the stop and distance limits are measured from it.
func ArmMicroPullback(bars []domain.Bar, cfg *config.Config) Setup {
	// One fewer bar than the close-triggered reading, because there is no trigger
	// candle yet.
	need := minBarsForSetup(cfg) - 1
	if len(bars) < need {
		return Setup{Reason: fmt.Sprintf("only %d bars so far, need %d", len(bars), need)}
	}
	if reason := microFilters(bars, cfg); reason != "" {
		return Setup{Reason: reason}
	}
	shape, reason := microPause(bars, cfg)
	if reason != "" {
		return Setup{Reason: reason}
	}
	setup := stopBelow(shape.Trigger, shape.Low, cfg)
	setup.PauseHigh = shape.Trigger
	setup.PauseBars = shape.Bars
	if setup.Triggered {
		setup.BuyStop = shape.Trigger
	}
	return setup
}

// microFilters applies the shared trend filters and the optional MACD filter to the
// most recent bar.
func microFilters(bars []domain.Bar, cfg *config.Config) string {
	if reason := trendFilters(bars, cfg); reason != "" {
		return reason
	}
	if cfg.Entry.RequireMACD {
		closes := make([]float64, len(bars))
		for i, b := range bars {
			closes[i] = b.Close
		}
		if line, signal, ok := MACD(closes); ok && line <= signal {
			return fmt.Sprintf("MACD %.4f is not above its signal line %.4f", line, signal)
		}
	}
	return ""
}

// microPause finds the pause ending on the last of bars, and checks the surge before
// it. It returns why there is no micro pullback there, or "".
func microPause(bars []domain.Bar, cfg *config.Config) (microShape, string) {
	e := cfg.Entry
	end := len(bars) - 1
	if end < 1 {
		return microShape{}, "not enough bars for a pullback"
	}
	if !isPauseCandle(bars, end) {
		return microShape{}, "no pullback: the last candle closed green without undercutting the previous low"
	}

	// Walk back over the whole run of pause candles, not just the allowed number, so
	// an over-long pause is reported as one rather than mistaken for a surge.
	start := end
	for start > 1 && isPauseCandle(bars, start-1) {
		start--
	}
	count := end - start + 1
	if count > e.MaxPullbackBars {
		return microShape{}, fmt.Sprintf(
			"pullback is %d bars, more than the %d a micro pullback allows", count, e.MaxPullbackBars)
	}

	// The surge is the SurgeBars candles ending at the one before the pause.
	top := start - 1
	first := top - e.SurgeBars + 1
	if first < 0 {
		return microShape{}, fmt.Sprintf("only %d bars before the pullback, need %d for the surge",
			top+1, e.SurgeBars)
	}
	surgeLow, surgeVol := math.Inf(1), 0.0
	for _, b := range bars[first : top+1] {
		surgeLow = math.Min(surgeLow, b.Low)
		surgeVol += b.Volume
	}
	// A pause candle can tick a marginal new high before closing red, so the surge's
	// top is the highest high across the surge and the pause together.
	surgeHigh := bars[top].High
	pauseLow, pauseVol := math.Inf(1), 0.0
	for _, b := range bars[start : end+1] {
		surgeHigh = math.Max(surgeHigh, b.High)
		pauseLow = math.Min(pauseLow, b.Low)
		pauseVol += b.Volume
	}
	if surgeLow <= 0 || surgeHigh <= surgeLow {
		return microShape{}, "no surge before the pullback"
	}

	// This is a pause at the high of day, not a bounce somewhere below it.
	for _, b := range bars[:first] {
		if b.High > surgeHigh+priceEpsilon {
			return microShape{}, fmt.Sprintf("surge high %.2f is below the %.2f high of day",
				surgeHigh, b.High)
		}
	}

	if surge := (surgeHigh - surgeLow) / surgeLow * 100; surge < e.MinSurgePct-priceEpsilon {
		return microShape{}, fmt.Sprintf("surge is %.2f%% over %d bars, need %.2f%%",
			surge, e.SurgeBars, e.MinSurgePct)
	}
	if retrace := (surgeHigh - pauseLow) / (surgeHigh - surgeLow) * 100; retrace > e.MaxRetracePct+priceEpsilon {
		return microShape{}, fmt.Sprintf("pullback gave back %.0f%% of the surge, more than %.0f%%",
			retrace, e.MaxRetracePct)
	}
	if e.RequireVolumeDecline &&
		pauseVol/float64(count) >= surgeVol/float64(e.SurgeBars) {
		return microShape{}, "pullback volume is not lighter than the surge's"
	}
	return microShape{Bars: count, Trigger: bars[end].High, Low: pauseLow}, ""
}

// isPauseCandle reports whether bars[i] is part of a pullback: it gave something
// back, either by closing red or by trading below the previous candle's low.
//
// It used to accept any candle that merely failed to make a higher high, with no
// magnitude attached. On a vertical mover that admits pure continuation: AIFA on
// 2026-10-06 was bought at 10:09 ET on a "pause" whose only qualification was a high
// $0.0006 below the previous candle's, while it closed green on a higher low and a
// higher close. The last red close had been nine candles earlier at 10:00, so the
// entry was 7.9% into an unbroken run -- the extension-buying the micro pullback
// exists to replace -- and the stop went under a continuation candle's low, a level
// nothing had defended. It was stopped out eleven seconds after the fill. A
// tolerance on the higher-high test would not have helped: the gap was real, just
// meaningless. What the pattern needs is evidence that price actually retraced.
func isPauseCandle(bars []domain.Bar, i int) bool {
	b := bars[i]
	return b.Close < b.Open || b.Low < bars[i-1].Low
}

// MACD returns the latest MACD line (12-EMA minus 26-EMA of closes) and its 9-period
// signal line, and false when there are too few closes to compute both.
func MACD(closes []float64) (line, signal float64, ok bool) {
	fast := EMA(closes, macdFast)
	slow := EMA(closes, macdSlow)
	if len(slow) == 0 {
		return 0, 0, false
	}
	// Both are aligned to the tail of closes, so the fast series is the longer one
	// and its last len(slow) values line up with slow.
	offset := len(fast) - len(slow)
	diff := make([]float64, len(slow))
	for i := range slow {
		diff[i] = fast[offset+i] - slow[i]
	}
	sig := EMA(diff, macdSignal)
	if len(sig) == 0 {
		return 0, 0, false
	}
	return diff[len(diff)-1], sig[len(sig)-1], true
}

// trendFilters returns why the most recent bar fails the trend filters, or "" when
// it passes. The strategy only buys strength: price has to be holding above its
// short EMA, and above the session VWAP, which is the line separating a name being
// accumulated from one being distributed into.
func trendFilters(bars []domain.Bar, cfg *config.Config) string {
	last := bars[len(bars)-1]
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	ema := EMA(closes, cfg.Entry.EMAPeriod)
	if len(ema) == 0 {
		return "not enough data for the EMA"
	}
	if last.Close < ema[len(ema)-1] {
		return fmt.Sprintf("close %.2f is below the %d-period EMA %.2f",
			last.Close, cfg.Entry.EMAPeriod, ema[len(ema)-1])
	}
	if cfg.Entry.RequireAboveVWAP {
		vwap := VWAP(bars)
		if vwap > 0 && last.Close < vwap {
			return fmt.Sprintf("close %.2f is below VWAP %.2f", last.Close, vwap)
		}
	}
	return ""
}

// stopBelow places the stop just under low and applies the distance limits, giving a
// triggered Setup entered at entry, or a refusal.
func stopBelow(entry, low float64, cfg *config.Config) Setup {
	if math.IsInf(low, 0) || low <= 0 {
		return Setup{Reason: "no valid low in the pause"}
	}
	stop := low * (1 - cfg.Entry.StopBufferPct/100)

	// Distance limits. A stop too far away is refused outright rather than sized
	// around: that is part of the strategy, not a safety rail.
	dist := (entry - stop) / entry * 100
	if dist <= 0 {
		return Setup{Reason: "the pause low is at or above the entry"}
	}
	if dist > cfg.Entry.MaxStopDistancePct {
		return Setup{Reason: fmt.Sprintf(
			"stop is %.2f%% away, beyond the %.2f%% limit — risk too wide to take",
			dist, cfg.Entry.MaxStopDistancePct)}
	}
	if dist < cfg.Entry.MinStopDistancePct {
		// Too close to be real: widen it rather than accept a stop inside the noise.
		stop = entry * (1 - cfg.Entry.MinStopDistancePct/100)
		dist = cfg.Entry.MinStopDistancePct
	}

	return Setup{
		Triggered:       true,
		Entry:           entry,
		Stop:            stop,
		RiskPerShare:    entry - stop,
		StopDistancePct: dist,
	}
}

// CheckEntryPrice judges a triggered setup against the price an order can actually
// get now, and returns why the trade is off, or "" when it may go ahead at price.
//
// The setup is read on a closed candle, so its Entry is history by the time an order
// can be sent. On 2026-10-02 all three automated entries filled 2-8% away from the
// trigger close they were sized on: one 5.7% above it, carrying 2.8x the risk budget,
// and two after price had already fallen back through the pause — one of them below
// its own stop, which sold it four seconds later. The setup's levels still stand; it
// is the price that has to be re-read, and the stop distance with it.
func CheckEntryPrice(s Setup, price float64, cfg *config.Config) string {
	switch {
	case price <= 0:
		return "no live price"
	case price <= s.Stop+priceEpsilon:
		return fmt.Sprintf("price %.2f is already at or below the %.2f stop", price, s.Stop)
	case s.PauseHigh > 0 && price < s.PauseHigh-priceEpsilon:
		return fmt.Sprintf("price %.2f is back under the %.2f pause high: the breakout failed",
			price, s.PauseHigh)
	}
	if drift := (price - s.Entry) / s.Entry * 100; drift > cfg.Entry.MaxEntryDriftPct+priceEpsilon {
		return fmt.Sprintf("price %.2f has run %.2f%% past the %.2f trigger, beyond the %.2f%% limit",
			price, drift, s.Entry, cfg.Entry.MaxEntryDriftPct)
	}
	dist := (price - s.Stop) / price * 100
	if dist > cfg.Entry.MaxStopDistancePct+priceEpsilon {
		return fmt.Sprintf("stop is %.2f%% away at %.2f, beyond the %.2f%% limit",
			dist, price, cfg.Entry.MaxStopDistancePct)
	}
	// Not widened, unlike a tight stop on the chart: here the stop is close because
	// price has fallen back towards it since the trigger, which is the breakout
	// failing, not noise in a valid one.
	if dist < cfg.Entry.MinStopDistancePct-priceEpsilon {
		return fmt.Sprintf("stop is only %.2f%% away at %.2f, inside the %.2f%% minimum",
			dist, price, cfg.Entry.MinStopDistancePct)
	}
	return ""
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
