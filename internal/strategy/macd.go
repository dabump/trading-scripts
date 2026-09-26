package strategy

import "math"

// EMA returns an exponential moving average aligned with values: entries before
// the series has warmed up are NaN. The first defined entry is seeded with a
// simple average of the first period values, which is what charting platforms
// do — seeding with a single close instead makes the early values swing wildly
// and would produce crossovers that a chart never showed.
func EMA(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	for i := range out {
		out[i] = math.NaN()
	}
	if period < 1 || len(values) < period {
		return out
	}

	var sum float64
	for _, v := range values[:period] {
		sum += v
	}
	out[period-1] = sum / float64(period)

	k := 2.0 / (float64(period) + 1.0)
	for i := period; i < len(values); i++ {
		out[i] = (values[i]-out[i-1])*k + out[i-1]
	}
	return out
}

// MACD returns the MACD line and signal line aligned with closes, NaN-padded
// until each is defined.
func MACD(closes []float64, fast, slow, signal int) (macdLine, signalLine []float64) {
	macdLine = make([]float64, len(closes))
	signalLine = make([]float64, len(closes))
	for i := range macdLine {
		macdLine[i] = math.NaN()
		signalLine[i] = math.NaN()
	}
	if fast < 1 || slow < 1 || signal < 1 || fast >= slow || len(closes) < slow {
		return macdLine, signalLine
	}

	emaFast := EMA(closes, fast)
	emaSlow := EMA(closes, slow)
	for i := slow - 1; i < len(closes); i++ {
		macdLine[i] = emaFast[i] - emaSlow[i]
	}

	// The signal line is an EMA of the MACD line, so it has to be computed over
	// only the defined portion and then shifted back into place.
	defined := macdLine[slow-1:]
	sig := EMA(defined, signal)
	for i, v := range sig {
		signalLine[slow-1+i] = v
	}
	return macdLine, signalLine
}

// MACDBearishCross reports whether the most recent bar completed a bearish
// crossover: the MACD line was at or above its signal line on the previous bar
// and is below it now.
//
// It needs slow+signal bars before it can answer at all, which on 15-minute
// candles is 3h15m of session data with the configured 10/3 periods — later in
// the day than docs/strategy.md §4 estimates. Until then it reports false, so a
// position opened and closed earlier in the day never exits on this trigger.
func MACDBearishCross(closes []float64, fast, slow, signal int) bool {
	if len(closes) < slow+signal {
		return false
	}
	macdLine, signalLine := MACD(closes, fast, slow, signal)

	last := len(closes) - 1
	prev := last - 1
	for _, v := range []float64{macdLine[last], macdLine[prev], signalLine[last], signalLine[prev]} {
		if math.IsNaN(v) {
			return false
		}
	}
	return macdLine[prev] >= signalLine[prev] && macdLine[last] < signalLine[last]
}
