package broker

import (
	"context"
	"fmt"
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

	// fillPrice and fillLimit make an order execute unlike the quote: at another
	// price, or for fewer shares (0 = none), leaving the rest working until it is
	// cancelled. orders is every order placed, for Order and CancelOrder.
	fillPrice map[string]float64
	fillLimit map[string]int
	orders    map[string]OrderResult
	// resting holds the stop orders that have been placed and not yet triggered,
	// keyed by broker order id. A stop is the one order type that is not supposed
	// to fill when it is sent, so the fake has to model it waiting — otherwise a
	// protective stop would read as an instant exit and no test could tell the
	// difference between a stop that worked and one that sold the position at once.
	resting map[string]restingStop
	// stopOrderErr rejects stop orders only, leaving entries and exits working. It
	// is how a test reaches the case that matters most: a position bought with no
	// resting stop behind it.
	stopOrderErr error

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

		fillPrice: map[string]float64{},
		fillLimit: map[string]int{},
		orders:    map[string]OrderResult{},
		resting:   map[string]restingStop{},
	}
}

// restingStop is a placed-but-untriggered stop order.
type restingStop struct {
	symbol string
	side   string
	shares int
	stop   float64
}

// SetFillPrice makes orders in symbol execute at price rather than at the quote or
// the limit, the way a real fill lands a cent or two away from the price the daemon
// read.
func (f *Fake) SetFillPrice(symbol string, price float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fillPrice[symbol] = price
}

// SetFillLimit caps how many shares of an order in symbol execute; 0 means none do.
// The remainder stays working until CancelOrder, as a thin book leaves a limit order.
func (f *Fake) SetFillLimit(symbol string, shares int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fillLimit[symbol] = shares
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
	f.triggerStops(symbol, price)
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

	id := "fake-" + req.ClientOrderID
	if req.Type == "stop" && f.stopOrderErr != nil {
		return OrderResult{}, f.stopOrderErr
	}
	// A stop order is acknowledged and then waits: it fills when the price reaches
	// its trigger, which here is whenever a caller moves the price.
	if req.Type == "stop" {
		res := OrderResult{BrokerOrderID: id, Status: "new"}
		f.orders[id] = res
		f.resting[id] = restingStop{
			symbol: req.Symbol, side: req.Side, shares: req.Shares, stop: req.StopPrice,
		}
		return res, nil
	}

	price := f.snaps[req.Symbol].Price
	if req.Type == "limit" && req.LimitPrice > 0 {
		price = req.LimitPrice
	}
	if p, ok := f.fillPrice[req.Symbol]; ok {
		price = p
	}
	shares := req.Shares
	if n, ok := f.fillLimit[req.Symbol]; ok && n < shares {
		shares = n
	}
	f.settle(req.Symbol, req.Side, shares, price)

	res := OrderResult{
		BrokerOrderID: id,
		Status:        "filled",
		FilledShares:  shares,
	}
	switch {
	case shares == 0:
		res.Status = "new"
	case shares < req.Shares:
		res.Status = "partially_filled"
	}
	if shares > 0 {
		res.FilledPrice = price
	}
	f.orders[res.BrokerOrderID] = res
	return res, nil
}

// settle moves cash and the holding for a fill. Called with f.mu held.
func (f *Fake) settle(symbol, side string, shares int, price float64) {
	switch side {
	case "buy":
		f.acct.Cash -= price * float64(shares)
		pos := f.positions[symbol]
		pos.Symbol = symbol
		pos.Shares += shares
		pos.AvgEntry = price
		pos.CurrentPrice = price
		if pos.Shares > 0 {
			f.positions[symbol] = pos
		}
	case "sell":
		f.acct.Cash += price * float64(shares)
		pos := f.positions[symbol]
		pos.Shares -= shares
		if pos.Shares <= 0 {
			delete(f.positions, symbol)
		} else {
			f.positions[symbol] = pos
		}
	}
}

// triggerStops fills any resting stop the given price has reached. Called with f.mu
// held, from the price setters, so moving a symbol through its stop exercises the
// same path a real trigger takes: the daemon learns about it by looking the order
// up, not by being told.
//
// It fills at the trigger price rather than the price that crossed it, unless
// SetFillPrice says otherwise. That is the optimistic case on purpose — the point of
// the resting order is that the gap between trigger and fill is the broker's to
// manage, and a test that wants to see slippage should ask for it explicitly.
func (f *Fake) triggerStops(symbol string, price float64) {
	if price <= 0 {
		return
	}
	for id, r := range f.resting {
		if r.symbol != symbol {
			continue
		}
		if r.side == "sell" && price > r.stop {
			continue
		}
		if r.side == "buy" && price < r.stop {
			continue
		}
		fillAt := r.stop
		if p, ok := f.fillPrice[symbol]; ok {
			fillAt = p
		}
		f.settle(r.symbol, r.side, r.shares, fillAt)
		f.orders[id] = OrderResult{
			BrokerOrderID: id, Status: "filled",
			FilledPrice: fillAt, FilledShares: r.shares,
		}
		delete(f.resting, id)
	}
}

// SetStopOrderError makes stop orders fail while everything else keeps working, so
// a test can see what a position does when its protective stop was refused.
func (f *Fake) SetStopOrderError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopOrderErr = err
}

// RestingStops reports how many stop orders are placed and not yet triggered, so a
// test can assert that an exit took its protective stop off the book rather than
// leaving one behind to sell a position the daemon no longer holds.
func (f *Fake) RestingStops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resting)
}

func (f *Fake) Order(_ context.Context, brokerOrderID string) (OrderResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return OrderResult{}, f.err
	}
	res, ok := f.orders[brokerOrderID]
	if !ok {
		return OrderResult{}, fmt.Errorf("order %s not found", brokerOrderID)
	}
	return res, nil
}

func (f *Fake) CancelOrder(_ context.Context, brokerOrderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	res, ok := f.orders[brokerOrderID]
	if !ok {
		return fmt.Errorf("order %s not found", brokerOrderID)
	}
	if !res.Done() {
		res.Status = "canceled"
		f.orders[brokerOrderID] = res
	}
	delete(f.resting, brokerOrderID)
	return nil
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
