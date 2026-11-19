// Package chartfeed serves the live per-symbol chart the status page's "Chart"
// button opens: 1-minute candles plus EMA9/EMA20/VWAP/MACD(9,20,9) and volume.
//
// It exists so the web layer never has to call the broker directly for this,
// keeping the rule in CLAUDE.md intact ("the web layer never calls the broker"
// except through the two read-only checks and Open/Close). Modeled on
// internal/momentum.Worker, but keyed on a dynamic watch set rather than the
// whole watchlist: a symbol is only fetched while at least one open chart is
// looking at it, which is what keeps this bounded regardless of how many names
// the screen has produced.
package chartfeed

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/strategy"
)

const (
	fastPeriod    = 9
	slowPeriod    = 20
	signalPeriod  = 9
	emaFastPeriod = 9
	emaSlowPeriod = 20

	barInterval = time.Minute

	// priceInterval is how often a watched symbol's live price is read, to move
	// the forming candle. This is new broker-call volume the scan does not
	// already pay for, so it is bounded to symbols with a chart actually open —
	// see the package doc — and kept in the same order of magnitude as the
	// page's own fragment poll (10-15s) rather than tracking every tick.
	priceInterval = 3 * time.Second

	// watchTTL drops a symbol the moment nobody has polled it in a while. This is
	// the real safety net for a closed tab, a lost network or a crashed browser —
	// none of which reliably fire the explicit Unwatch a clean dialog close does.
	watchTTL = 20 * time.Second
)

// Bar is one candle as the chart draws it: a domain.Bar plus whether it is
// still forming. Forming's volume is always 0 until the bar closes and
// refreshSymbol reads the real one from IntradayBars — a live quote has no way
// to say how much of today's cumulative volume happened in this one minute, so
// guessing would show a number nobody could stand behind.
type Bar struct {
	Time    time.Time
	Open    float64
	High    float64
	Low     float64
	Close   float64
	Volume  float64
	Forming bool
}

// Data is one watched symbol's whole chart. Each indicator slice is aligned to
// the tail of Bars, exactly like strategy.EMA/MACD elsewhere: shorter than Bars
// by its own warm-up, not padded.
type Data struct {
	Symbol     string
	Bars       []Bar
	EMA9       []float64
	EMA20      []float64
	VWAP       []float64
	MACDLine   []float64
	MACDSignal []float64
	UpdatedAt  time.Time
	// Err is the last fetch failure, if any. It never blanks out Bars — a stale
	// chart next to an error is more useful than an empty one, same reasoning as
	// the page's account balance.
	Err string
}

// Bars is the one broker call this package needs to read a symbol's chart.
type Bars interface {
	IntradayBars(ctx context.Context, symbol string, interval time.Duration, since time.Time) ([]domain.Bar, error)
}

// Snapshots is the one broker call this package needs for a live price —
// the same batched call scanning already uses, so watching a chart adds no new
// broker method, only new call volume.
type Snapshots interface {
	Snapshots(ctx context.Context, symbols []string) (map[string]domain.Snapshot, error)
}

// Calendar is the one broker call this package needs to anchor a session's
// bars — see dayAnchor.
type Calendar interface {
	Calendar(ctx context.Context, date string) (broker.CalendarDay, error)
}

type watchState struct {
	lastPolled time.Time
	closed     []domain.Bar
	forming    *domain.Bar
	formingAt  time.Time
	data       Data
}

// Worker fetches live data only for symbols a chart is currently open on, and
// publishes it to an in-memory cache the web layer reads through Poll. Watch
// and Poll are both called from HTTP handler goroutines; Run's own goroutine
// calls sweep on its ticker — mu guards the watch set against that.
type Worker struct {
	Bars      Bars
	Snapshots Snapshots
	Calendar  Calendar
	Cfg       *config.Config
	// Now is injected so a test's session date and minute boundaries are
	// deterministic.
	Now func() time.Time
	Log *slog.Logger

	mu      sync.Mutex
	watches map[string]*watchState

	anchorMu     sync.Mutex
	cachedDate   string
	cachedAnchor time.Time
}

// Watch registers a symbol for live updates, if it is not already watched, and
// loads its chart immediately so opening one does not sit empty for up to
// priceInterval waiting on the next tick.
func (w *Worker) Watch(ctx context.Context, symbol string) {
	w.mu.Lock()
	if w.watches == nil {
		w.watches = map[string]*watchState{}
	}
	_, exists := w.watches[symbol]
	if !exists {
		w.watches[symbol] = &watchState{}
	}
	w.watches[symbol].lastPolled = w.now()
	w.mu.Unlock()

	if !exists {
		w.refreshSymbol(ctx, symbol)
	}
}

// Unwatch stops fetching a symbol immediately, called on a clean dialog close.
// A tab that goes away without one is still caught by watchTTL.
func (w *Worker) Unwatch(symbol string) {
	w.mu.Lock()
	delete(w.watches, symbol)
	w.mu.Unlock()
}

// Poll is what the page's chart poll reads. Polling also refreshes the watch's
// TTL — the poll itself is the signal that a browser is still looking.
func (w *Worker) Poll(symbol string) (Data, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.watches[symbol]
	if !ok {
		return Data{}, false
	}
	st.lastPolled = w.now()
	return st.data, true
}

// Run reads every currently watched symbol's live price every priceInterval,
// until ctx is cancelled. A fetch failure here is cosmetic, exactly like
// momentum.Worker.sweep's: it logs and leaves the chart stale rather than
// faulting the agent.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(priceInterval)
	defer ticker.Stop()
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
	symbols := w.liveSymbols(now)
	if len(symbols) == 0 {
		return
	}

	snaps, err := w.Snapshots.Snapshots(ctx, symbols)
	if err != nil {
		w.logger().Warn("chart feed: live price unavailable", "err", err)
		return
	}
	for _, sym := range symbols {
		if snap, ok := snaps[sym]; ok {
			w.applyPrice(ctx, sym, snap.Price, now)
		}
	}
}

// liveSymbols evicts any watch idle past watchTTL and returns what is left.
func (w *Worker) liveSymbols(now time.Time) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for sym, st := range w.watches {
		if now.Sub(st.lastPolled) > watchTTL {
			delete(w.watches, sym)
			continue
		}
		out = append(out, sym)
	}
	return out
}

// applyPrice moves the forming candle to a new live price, sealing the
// previous one via refreshSymbol first if the wall-clock minute has rolled
// over since it started.
func (w *Worker) applyPrice(ctx context.Context, symbol string, price float64, now time.Time) {
	minute := now.Truncate(barInterval)

	w.mu.Lock()
	st, ok := w.watches[symbol]
	rollover := ok && (st.formingAt.IsZero() || minute.After(st.formingAt))
	w.mu.Unlock()
	if !ok {
		return
	}
	if rollover {
		w.refreshSymbol(ctx, symbol)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok = w.watches[symbol]
	if !ok {
		return
	}
	if st.forming == nil {
		st.forming = &domain.Bar{Time: minute, Open: price, High: price, Low: price, Close: price}
		st.formingAt = minute
	} else {
		st.forming.Close = price
		if price > st.forming.High {
			st.forming.High = price
		}
		if price < st.forming.Low {
			st.forming.Low = price
		}
	}
	st.data = w.buildData(symbol, st)
}

// refreshSymbol re-reads closed bars from the broker and starts a fresh
// forming candle from whatever closed last. A failure here leaves the
// previously loaded chart in place and only records the error, the same
// "stale rather than dropped" rule the page's account balance follows.
func (w *Worker) refreshSymbol(ctx context.Context, symbol string) {
	date := scheduler.SessionDate(w.now())
	since, ok := w.dayAnchor(ctx, date)
	if !ok {
		return
	}

	bars, err := w.Bars.IntradayBars(ctx, symbol, barInterval, since)
	if err != nil {
		w.logger().Warn("chart feed: bars unavailable", "symbol", symbol, "err", err)
		w.mu.Lock()
		if st, ok := w.watches[symbol]; ok {
			st.data.Err = err.Error()
		}
		w.mu.Unlock()
		return
	}
	closed := completedBars(bars, w.now(), barInterval)

	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.watches[symbol]
	if !ok {
		return
	}
	st.closed = closed
	st.forming = nil
	st.formingAt = time.Time{}
	st.data = w.buildData(symbol, st)
}

// buildData computes every series the chart draws from a watch's current
// bars, closed plus forming.
func (w *Worker) buildData(symbol string, st *watchState) Data {
	allBars := st.closed
	bars := make([]Bar, 0, len(st.closed)+1)
	for _, b := range st.closed {
		bars = append(bars, Bar{Time: b.Time, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume})
	}
	if st.forming != nil {
		bars = append(bars, Bar{
			Time: st.forming.Time, Open: st.forming.Open, High: st.forming.High,
			Low: st.forming.Low, Close: st.forming.Close, Volume: st.forming.Volume, Forming: true,
		})
		allBars = append(append([]domain.Bar{}, st.closed...), *st.forming)
	}

	closes := make([]float64, len(allBars))
	for i, b := range allBars {
		closes[i] = b.Close
	}
	line, sig := strategy.MACDSeries(closes, fastPeriod, slowPeriod, signalPeriod)

	return Data{
		Symbol:     symbol,
		Bars:       bars,
		EMA9:       strategy.EMA(closes, emaFastPeriod),
		EMA20:      strategy.EMA(closes, emaSlowPeriod),
		VWAP:       strategy.VWAPSeries(allBars),
		MACDLine:   line,
		MACDSignal: sig,
		UpdatedAt:  w.now(),
	}
}

// dayAnchor is where this session's bars start from — the pre-market open when
// extended hours are on, the regular open otherwise — cached per date so a
// calendar lookup doesn't run on every refresh. Guarded by its own mutex,
// separate from the watch set, so a calendar fetch for a new date does not
// block price updates for symbols that have nothing to do with it.
func (w *Worker) dayAnchor(ctx context.Context, date string) (time.Time, bool) {
	w.anchorMu.Lock()
	defer w.anchorMu.Unlock()
	if w.cachedDate == date {
		return w.cachedAnchor, true
	}
	day, err := w.Calendar.Calendar(ctx, date)
	if err != nil {
		w.logger().Error("chart feed: load calendar for anchor", "err", err)
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

// completedBars drops the forming candle the feed includes alongside the
// closed ones — the same filter internal/momentum and internal/engine/watch.go
// each apply to their own bar reads.
func completedBars(bars []domain.Bar, now time.Time, interval time.Duration) []domain.Bar {
	n := len(bars)
	for n > 0 && bars[n-1].Time.Add(interval).After(now) {
		n--
	}
	return bars[:n]
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
