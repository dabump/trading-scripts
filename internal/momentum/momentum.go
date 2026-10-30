// Package momentum reads MACD(9,20,9) and StochRSI(14,14,3,3) off every
// watchlist symbol's one-minute chart, purely to put an arrow or fire icon on
// the watchlist -- it never feeds back into screening or entry. It runs on its
// own ticker, apart from engine.Tick, the same way internal/newssentiment does:
// nothing about a cosmetic annotation is time-critical, and it depends only on
// store.Store and a couple of narrow broker interfaces, so it cannot affect
// trading even if a fetch is slow or fails.
package momentum

import (
	"context"
	"log/slog"
	"time"

	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/strategy"
)

// State is one of the four mutually exclusive icons a watchlist row can show.
type State string

const (
	// StatePending means there are not yet enough closed one-minute bars to
	// compute MACD(9,20,9).
	StatePending State = "pending"
	// StateBullish means the MACD line is above its signal line.
	StateBullish State = "bullish"
	// StateAnticipating means the MACD line is still below its signal line, but
	// closing in on a golden cross, and StochRSI agrees.
	StateAnticipating State = "anticipating"
	// StateBearish is the default below-signal state when no cross is anticipated.
	StateBearish State = "bearish"
)

const (
	fastPeriod   = 9
	slowPeriod   = 20
	signalPeriod = 9

	rsiLength   = 14
	stochLength = 14
	kSmoothing  = 3
	dSmoothing  = 3

	// narrowingBars is how many consecutive completed bars the signal-minus-MACD
	// gap must shrink on, while still positive, to count as "anticipating" a
	// cross rather than merely close by chance.
	narrowingBars = 3
	// stochRisingBars is how many consecutive completed bars %K must rise on.
	stochRisingBars = 2
	stochLow        = 20.0
	stochHigh       = 80.0

	// crossEpsilon absorbs binary-float rounding in the signal/MACD difference,
	// the same reason strategy.priceEpsilon exists for price thresholds.
	crossEpsilon = 1e-6

	barInterval = time.Minute
)

// Classify reads MACD(9,20,9) and StochRSI(14,14,3,3) off bars' closes and
// returns which of the four states the latest completed candle is in. now and
// interval let it drop the candle still forming, which the feed includes.
func Classify(bars []domain.Bar, now time.Time, interval time.Duration) State {
	bars = completedBars(bars, now, interval)
	closes := closesOf(bars)

	line, sig := macdSeries(closes, fastPeriod, slowPeriod, signalPeriod)
	if len(line) == 0 {
		return StatePending
	}
	last := len(line) - 1
	if line[last] > sig[last]+crossEpsilon {
		return StateBullish
	}

	k, d := stochRSI(closes, rsiLength, stochLength, kSmoothing, dSmoothing)
	if anticipatingCross(line, sig) && stochAgrees(k, d) {
		return StateAnticipating
	}
	return StateBearish
}

func completedBars(bars []domain.Bar, now time.Time, interval time.Duration) []domain.Bar {
	n := len(bars)
	for n > 0 && bars[n-1].Time.Add(interval).After(now) {
		n--
	}
	return bars[:n]
}

func closesOf(bars []domain.Bar) []float64 {
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	return closes
}

// macdSeries returns the MACD line and signal line, aligned to each other and
// to the tail of closes, using strategy.EMA so the warm-up behaves identically
// to the entry-side MACD filter.
func macdSeries(closes []float64, fast, slow, signal int) (line, sig []float64) {
	fastEMA := strategy.EMA(closes, fast)
	slowEMA := strategy.EMA(closes, slow)
	if len(slowEMA) == 0 {
		return nil, nil
	}
	offset := len(fastEMA) - len(slowEMA)
	diff := make([]float64, len(slowEMA))
	for i := range slowEMA {
		diff[i] = fastEMA[offset+i] - slowEMA[i]
	}
	sig = strategy.EMA(diff, signal)
	if len(sig) == 0 {
		return nil, nil
	}
	diffOffset := len(diff) - len(sig)
	return diff[diffOffset:], sig
}

// anticipatingCross reports whether the gap between the signal and MACD lines
// has narrowed on each of the last narrowingBars completed bars, staying
// positive (MACD still below signal) throughout.
func anticipatingCross(line, sig []float64) bool {
	n := len(line)
	if n < narrowingBars+1 {
		return false
	}
	gaps := make([]float64, narrowingBars+1)
	for i := range gaps {
		idx := n - len(gaps) + i
		gaps[i] = sig[idx] - line[idx]
	}
	for _, g := range gaps {
		if g <= crossEpsilon {
			return false
		}
	}
	for i := 1; i < len(gaps); i++ {
		if gaps[i] >= gaps[i-1] {
			return false
		}
	}
	return true
}

// stochAgrees reports whether %K sits in the 20-80 band, above %D, and has
// risen on each of the last stochRisingBars completed bars.
func stochAgrees(k, d []float64) bool {
	n := len(k)
	if n < stochRisingBars+1 || len(d) == 0 {
		return false
	}
	last := n - 1
	if k[last] < stochLow || k[last] > stochHigh {
		return false
	}
	if k[last] <= d[len(d)-1] {
		return false
	}
	for i := last - stochRisingBars + 1; i <= last; i++ {
		if k[i] <= k[i-1] {
			return false
		}
	}
	return true
}

func rsiSeries(closes []float64, period int) []float64 {
	if len(closes) < period+1 {
		return nil
	}
	gains := make([]float64, len(closes)-1)
	losses := make([]float64, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		if d > 0 {
			gains[i-1] = d
		} else {
			losses[i-1] = -d
		}
	}

	var avgGain, avgLoss float64
	for i := 0; i < period; i++ {
		avgGain += gains[i]
		avgLoss += losses[i]
	}
	avgGain /= float64(period)
	avgLoss /= float64(period)

	out := make([]float64, 0, len(gains)-period+1)
	out = append(out, rsiFromAverages(avgGain, avgLoss))
	for i := period; i < len(gains); i++ {
		avgGain = (avgGain*float64(period-1) + gains[i]) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + losses[i]) / float64(period)
		out = append(out, rsiFromAverages(avgGain, avgLoss))
	}
	return out
}

func rsiFromAverages(avgGain, avgLoss float64) float64 {
	if avgLoss == 0 {
		if avgGain == 0 {
			return 50
		}
		return 100
	}
	return 100 - 100/(1+avgGain/avgLoss)
}

func stochasticOf(values []float64, period int) []float64 {
	if len(values) < period {
		return nil
	}
	out := make([]float64, 0, len(values)-period+1)
	for i := period - 1; i < len(values); i++ {
		window := values[i-period+1 : i+1]
		lo, hi := window[0], window[0]
		for _, v := range window {
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		if hi == lo {
			out = append(out, 50)
			continue
		}
		out = append(out, (values[i]-lo)/(hi-lo)*100)
	}
	return out
}

func smaOf(values []float64, period int) []float64 {
	if len(values) < period {
		return nil
	}
	out := make([]float64, 0, len(values)-period+1)
	var sum float64
	for i, v := range values {
		sum += v
		if i >= period {
			sum -= values[i-period]
		}
		if i >= period-1 {
			out = append(out, sum/float64(period))
		}
	}
	return out
}

// stochRSI returns %K and %D, aligned to each other and to the tail of closes.
func stochRSI(closes []float64, rsiLen, stochLen, kSmooth, dSmooth int) (k, d []float64) {
	raw := stochasticOf(rsiSeries(closes, rsiLen), stochLen)
	k = smaOf(raw, kSmooth)
	d = smaOf(k, dSmooth)
	if len(d) == 0 {
		return nil, nil
	}
	return k[len(k)-len(d):], d
}

// Store is what the worker needs from persistence.
type Store interface {
	// LatestScreenSnapshot is the same screening pass the watchlist page reads —
	// see docs/architecture.md on why there is only one evaluation path.
	LatestScreenSnapshot(date string) ([]domain.Evaluation, time.Time, error)
	SaveMomentumState(date, symbol, state string) error
}

// Bars is the one broker call the worker needs to read a symbol's chart.
type Bars interface {
	IntradayBars(ctx context.Context, symbol string, interval time.Duration, since time.Time) ([]domain.Bar, error)
}

// Calendar is the one broker call the worker needs to anchor a session's
// bars — see dayAnchor.
type Calendar interface {
	Calendar(ctx context.Context, date string) (broker.CalendarDay, error)
}

const defaultInterval = time.Minute

// Worker reads every current watchlist symbol's one-minute chart once a
// minute and writes its MACD/StochRSI read to the store for the page to join
// onto the row.
type Worker struct {
	Store    Store
	Bars     Bars
	Calendar Calendar
	Cfg      *config.Config
	// Now is injected so a sweep's session date is deterministic in tests.
	Now      func() time.Time
	Interval time.Duration
	Log      *slog.Logger

	cachedDate   string
	cachedAnchor time.Time
}

// Run looks for work immediately, then on every tick of Interval, until ctx is
// cancelled.
func (w *Worker) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	w.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

func (w *Worker) sweep(ctx context.Context) {
	now := w.now()
	date := scheduler.SessionDate(now)

	evals, _, err := w.Store.LatestScreenSnapshot(date)
	if err != nil {
		w.logger().Error("load screen snapshot for momentum", "err", err)
		return
	}
	if len(evals) == 0 {
		return
	}

	since, ok := w.dayAnchor(ctx, date)
	if !ok {
		return
	}

	for _, e := range evals {
		if ctx.Err() != nil {
			return
		}
		bars, err := w.Bars.IntradayBars(ctx, e.Symbol, barInterval, since)
		if err != nil {
			w.logger().Warn("intraday bars unavailable for momentum read", "symbol", e.Symbol, "err", err)
			continue
		}
		state := Classify(bars, now, barInterval)
		if err := w.Store.SaveMomentumState(date, e.Symbol, string(state)); err != nil {
			w.logger().Error("save momentum state", "symbol", e.Symbol, "err", err)
		}
	}
}

// dayAnchor is where this session's bars start from — the pre-market open when
// extended hours are on, the regular open otherwise — cached per date so a
// calendar lookup doesn't run every sweep.
//
// Unlike checkSetups' bars, this deliberately does not reset at the
// pre-market/regular/post-market boundary (scheduler.ExtendedStart resets
// there): a MACD read that restarted its own warm-up at every window change
// could not "persist across the whole day", which is what this feature was
// asked to do.
func (w *Worker) dayAnchor(ctx context.Context, date string) (time.Time, bool) {
	if w.cachedDate == date {
		return w.cachedAnchor, true
	}
	day, err := w.Calendar.Calendar(ctx, date)
	if err != nil {
		w.logger().Error("load calendar for momentum anchor", "err", err)
		return time.Time{}, false
	}
	if day.Open.IsZero() {
		return time.Time{}, false
	}
	sess := scheduler.Session{Date: date, Open: day.Open, Close: day.Close}
	b := scheduler.Bounds(sess, w.Cfg)
	anchor := b.Open
	if !b.ExtendedOpen.IsZero() {
		anchor = b.ExtendedOpen
	}
	w.cachedDate, w.cachedAnchor = date, anchor
	return anchor, true
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) logger() *slog.Logger {
	if w.Log == nil {
		return slog.Default()
	}
	return w.Log
}
