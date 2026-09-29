package broker

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

// Fake is an in-memory broker used by tests and by the offline demo run. It
// fills orders immediately at the current snapshot price, which is enough to
// exercise the whole daily loop without credentials or a network.
type Fake struct {
	mu sync.Mutex

	acct      domain.Account
	assets    []string
	snaps     map[string]domain.Snapshot
	news      map[string]int
	avgVolume map[string]float64
	day       CalendarDay
	nextDay   CalendarDay
	positions map[string]BrokerPosition
	placed    []OrderRequest
	bars      map[string][]domain.Bar
	// sessionVolume is what SessionVolumes reports, standing in for the pre-market
	// volume a snapshot cannot carry. Unset means the symbol has not printed.
	sessionVolume map[string]float64
	err           error

	// Call counters, so tests can assert the scan's cost profile: a full-market
	// scan is only affordable if the expensive per-symbol calls stay rare.
	assetCalls         int
	sessionVolumeCalls int
	avgVolumeCalls     map[string]int
	newsSince          time.Time
}

func NewFake(acct domain.Account) *Fake {
	return &Fake{
		acct:      acct,
		snaps:     map[string]domain.Snapshot{},
		news:      map[string]int{},
		avgVolume: map[string]float64{},
		positions: map[string]BrokerPosition{},
		bars:      map[string][]domain.Bar{},

		sessionVolume:  map[string]float64{},
		avgVolumeCalls: map[string]int{},
	}
}

// SetError makes every subsequent call fail, for exercising error handling.
func (f *Fake) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *Fake) SetCalendar(day CalendarDay) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.day = day
}

// SetNextSession defines what NextSession reports, for the status page's countdown.
func (f *Fake) SetNextSession(day CalendarDay) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextDay = day
}

func (f *Fake) NextSession(_ context.Context, _ string) (CalendarDay, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return CalendarDay{}, f.err
	}
	return f.nextDay, nil
}

// SetAssets defines the tradable universe the screener will scan.
func (f *Fake) SetAssets(symbols ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assets = symbols
}

func (f *Fake) TradableAssets(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assetCalls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]string(nil), f.assets...), nil
}

// AssetCalls reports how many times the universe was fetched.
func (f *Fake) AssetCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.assetCalls
}

// NewsSince reports the start of the window the last news query asked for, so a
// test can assert the catalyst search reaches back past the market open.
func (f *Fake) NewsSince() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newsSince
}

// EnrichedSymbols lists the symbols that incurred an average-volume lookup, which
// is the expensive per-symbol step the screen tries to avoid.
func (f *Fake) EnrichedSymbols() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.avgVolumeCalls))
	for sym := range f.avgVolumeCalls {
		out = append(out, sym)
	}
	slices.Sort(out)
	return out
}

// SetSnapshot sets a symbol's price and volume. Percentage change is derived so
// callers cannot set a price and a contradictory change.
func (f *Fake) SetSnapshot(symbol string, price, prevClose, volume float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap := domain.Snapshot{
		Symbol: symbol, Price: price, PrevClose: prevClose, TodayVolume: volume,
	}
	if prevClose > 0 {
		snap.IntradayPct = (price - prevClose) / prevClose * 100
	}
	f.snaps[symbol] = snap
	if !slices.Contains(f.assets, symbol) {
		// A symbol with market data is implicitly part of the universe, so tests
		// don't have to declare it twice.
		f.assets = append(f.assets, symbol)
	}
	if p, ok := f.positions[symbol]; ok {
		p.CurrentPrice = price
		f.positions[symbol] = p
	}
}

// SetPrice updates only the traded price, keeping the previous close and volume.
func (f *Fake) SetPrice(symbol string, price float64) {
	f.mu.Lock()
	prev := f.snaps[symbol].PrevClose
	vol := f.snaps[symbol].TodayVolume
	f.mu.Unlock()
	f.SetSnapshot(symbol, price, prev, vol)
}

func (f *Fake) SetNews(symbol string, count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.news[symbol] = count
}

// Placed returns every order submitted so far.
func (f *Fake) Placed() []OrderRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]OrderRequest(nil), f.placed...)
}

func (f *Fake) Snapshots(_ context.Context, symbols []string) (map[string]domain.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]domain.Snapshot, len(symbols))
	for _, s := range symbols {
		if snap, ok := f.snaps[s]; ok {
			out[s] = snap
		}
	}
	return out, nil
}

func (f *Fake) AverageDailyVolume(_ context.Context, symbol string, _ int) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.avgVolumeCalls[symbol]++
	if f.err != nil {
		return 0, f.err
	}
	// Stored directly so a test can set a relative-volume multiple without
	// simulating 20 days of daily bars.
	return f.avgVolume[symbol], nil
}

// SessionVolumes reports the volume traded since `since`.
//
// The fake ignores `since` and returns what SetSessionVolume installed: a test is
// asserting on the screening arithmetic, not on bar aggregation. A symbol with
// nothing set is absent from the result, which is how the real implementation reports
// a symbol that has not printed.
func (f *Fake) SessionVolumes(_ context.Context, symbols []string, _ time.Time) (map[string]float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.sessionVolumeCalls++
	out := make(map[string]float64, len(symbols))
	for _, sym := range symbols {
		if v, ok := f.sessionVolume[sym]; ok {
			out[sym] = v
		}
	}
	return out, nil
}

// SetSessionVolume sets what SessionVolumes will report for a symbol.
func (f *Fake) SetSessionVolume(symbol string, v float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionVolume[symbol] = v
}

// SessionVolumeCalls is how many batched requests SessionVolumes made, so a test can
// assert it is batched rather than per-symbol.
func (f *Fake) SessionVolumeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionVolumeCalls
}

// SetAverageVolume sets the value AverageDailyVolume will report.
func (f *Fake) SetAverageVolume(symbol string, v float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.avgVolume[symbol] = v
}

// SetBars installs a symbol's intraday candles, for exercising the setup detector.
func (f *Fake) SetBars(symbol string, bars []domain.Bar) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bars[symbol] = bars
}

// SetSetupBars builds a textbook micro pullback and installs it, so a test does not
// have to hand-write a chart every time it wants a valid entry.
//
// The shape is: a steep rising run to the high of day (about 5% over its last three
// candles), one lighter pause candle that holds below that high and gives back about
// a third of the surge, then a trigger bar closing above the pause candle's high.
// Prices are scaled to `top`, which becomes the trigger close.
func (f *Fake) SetSetupBars(symbol string, top float64, start time.Time, interval time.Duration) {
	// A rising ramp long enough for the EMA to warm up, so the close sits above it.
	const ramp = 14
	bars := make([]domain.Bar, 0, ramp+2)
	base := top * 0.80
	step := (top*0.985 - base) / float64(ramp-1)
	for i := 0; i < ramp; i++ {
		c := base + step*float64(i)
		bars = append(bars, domain.Bar{
			Time: start.Add(time.Duration(i) * interval),
			Open: c - step/2, High: c + step/4, Low: c - step, Close: c, Volume: 50_000,
		})
	}
	high := bars[len(bars)-1].High
	// One pause candle: it pulls back and stays under the high of day.
	pauseLow := high * 0.985
	bars = append(bars, domain.Bar{
		Time: start.Add(time.Duration(ramp) * interval),
		Open: high * 0.995, High: high * 0.998, Low: pauseLow, Close: pauseLow * 1.002,
		Volume: 30_000,
	})
	// The trigger bar closes above the pause candle's high.
	bars = append(bars, domain.Bar{
		Time: start.Add(time.Duration(ramp+1) * interval),
		Open: pauseLow * 1.004, High: top * 1.001, Low: pauseLow * 1.001, Close: top,
		Volume: 80_000,
	})
	f.SetBars(symbol, bars)
}

func (f *Fake) IntradayBars(_ context.Context, symbol string, _ time.Duration, since time.Time) ([]domain.Bar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.Bar
	for _, b := range f.bars[symbol] {
		if !b.Time.Before(since) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *Fake) NewsCounts(_ context.Context, symbols []string, since time.Time) (map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newsSince = since
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]int, len(symbols))
	for _, s := range symbols {
		out[s] = f.news[s]
	}
	return out, nil
}

func (f *Fake) Account(context.Context) (domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return domain.Account{}, f.err
	}
	return f.acct, nil
}

func (f *Fake) Calendar(_ context.Context, _ string) (CalendarDay, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return CalendarDay{}, f.err
	}
	return f.day, nil
}

func (f *Fake) PlaceOrder(_ context.Context, req OrderRequest) (OrderResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return OrderResult{}, f.err
	}
	f.placed = append(f.placed, req)

	price := f.snaps[req.Symbol].Price
	if req.Type == "limit" && req.LimitPrice > 0 {
		price = req.LimitPrice
	}

	switch req.Side {
	case "buy":
		f.acct.Cash -= price * float64(req.Shares)
		pos := f.positions[req.Symbol]
		pos.Symbol = req.Symbol
		pos.Shares += req.Shares
		pos.AvgEntry = price
		pos.CurrentPrice = price
		f.positions[req.Symbol] = pos
	case "sell":
		f.acct.Cash += price * float64(req.Shares)
		pos := f.positions[req.Symbol]
		pos.Shares -= req.Shares
		if pos.Shares <= 0 {
			delete(f.positions, req.Symbol)
		} else {
			f.positions[req.Symbol] = pos
		}
	}

	return OrderResult{
		BrokerOrderID: "fake-" + req.ClientOrderID,
		Status:        "filled",
		FilledPrice:   price,
		FilledShares:  req.Shares,
	}, nil
}

func (f *Fake) Positions(context.Context) ([]BrokerPosition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]BrokerPosition, 0, len(f.positions))
	for _, p := range f.positions {
		out = append(out, p)
	}
	return out, nil
}

// SeedPosition installs a holding that the local store does not know about, for
// testing restart reconciliation.
func (f *Fake) SeedPosition(p BrokerPosition) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.positions[p.Symbol] = p
}
