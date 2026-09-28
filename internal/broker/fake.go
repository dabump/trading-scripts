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
	bars      map[string][]domain.Bar
	news      map[string]int
	avgVolume map[string]float64
	day       CalendarDay
	nextDay   CalendarDay
	positions map[string]BrokerPosition
	placed    []OrderRequest
	err       error

	// Call counters, so tests can assert the scan's cost profile: a full-market
	// scan is only affordable if the expensive per-symbol calls stay rare.
	assetCalls     int
	avgVolumeCalls map[string]int
	newsSince      time.Time
}

func NewFake(acct domain.Account) *Fake {
	return &Fake{
		acct:      acct,
		snaps:     map[string]domain.Snapshot{},
		bars:      map[string][]domain.Bar{},
		news:      map[string]int{},
		avgVolume: map[string]float64{},
		positions: map[string]BrokerPosition{},

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

func (f *Fake) SetBars(symbol string, closes []float64, start time.Time, intervalMins int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bars := make([]domain.Bar, len(closes))
	for i, c := range closes {
		bars[i] = domain.Bar{
			Time: start.Add(time.Duration(i*intervalMins) * time.Minute),
			Open: c, High: c, Low: c, Close: c, Volume: 1000,
		}
	}
	f.bars[symbol] = bars
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

func (f *Fake) IntradayBars(_ context.Context, symbol string, _ int, _ time.Time) ([]domain.Bar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]domain.Bar(nil), f.bars[symbol]...), nil
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

// SetAverageVolume sets the value AverageDailyVolume will report.
func (f *Fake) SetAverageVolume(symbol string, v float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.avgVolume[symbol] = v
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
