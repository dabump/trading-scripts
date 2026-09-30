// Package broker wraps market data and order execution behind interfaces, so
// strategy and risk code never depends on a particular provider (see the
// reasoning in docs/architecture.md).
package broker

import (
	"context"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

// CalendarDay is one exchange session as the broker reports it. Using the
// broker's calendar rather than a hardcoded holiday list means holidays and
// early closes come from data that stays correct without maintenance.
type CalendarDay struct {
	Date  string
	Open  time.Time
	Close time.Time
}

// OrderRequest is an order to submit.
type OrderRequest struct {
	Symbol        string
	Shares        int
	Side          string // "buy" or "sell"
	Type          string // "market" or "limit"
	LimitPrice    float64
	ClientOrderID string
	// ExtendedHours routes the order to the pre- or post-market session. Alpaca
	// only accepts it on a day limit order — a market order outside 09:30-16:00 is
	// rejected outright — which is why config validation refuses
	// premarket.allow_entry unless execution.order_type is limit.
	ExtendedHours bool
}

// OrderResult is what the broker reports about an order: its acknowledgement when
// placed, and its current state when looked up afterwards.
type OrderResult struct {
	BrokerOrderID string
	Status        string
	FilledPrice   float64
	FilledShares  int
}

// Done reports whether the order can no longer fill. A placed order is usually not
// done yet — Alpaca acknowledges with pending_new, even for a market order that fills
// a second later — which is why the acknowledgement's price cannot be the fill.
func (r OrderResult) Done() bool {
	switch r.Status {
	case "filled", "canceled", "expired", "rejected", "done_for_day":
		return true
	}
	return false
}

// BrokerPosition is a holding as the broker sees it, used to reconcile against
// local state after a restart.
type BrokerPosition struct {
	Symbol       string
	Shares       int
	AvgEntry     float64
	CurrentPrice float64
}

// MarketData is the read side: prices, candles, the candidate universe and news.
type MarketData interface {
	// TradableAssets lists every symbol the screen may consider. It replaces
	// Alpaca's market-movers endpoint, which is hard-capped at 50 results and
	// ordered by percentage change — a selection the strategy does not want, since
	// it ranks candidates by relative volume rather than by size of move.
	TradableAssets(ctx context.Context) ([]string, error)
	// Snapshots may be called with the whole tradable universe; implementations are
	// expected to batch internally rather than building one enormous URL.
	Snapshots(ctx context.Context, symbols []string) (map[string]domain.Snapshot, error)
	// AverageDailyVolume averages the prior sessions' volume, excluding today
	// (today is the number being compared against it).
	AverageDailyVolume(ctx context.Context, symbol string, days int) (float64, error)
	// NewsCounts reports how many stories each symbol has since the given time.
	NewsCounts(ctx context.Context, symbols []string, since time.Time) (map[string]int, error)
	// SessionVolumes reports how many shares each symbol has traded since `since`.
	//
	// It exists because the snapshot endpoint cannot answer this before the opening
	// bell: verified against a live account, `dailyBar` during pre-market is still the
	// *previous* session's bar and no daily bar for today exists yet, so a pre-market
	// symbol's volume-so-far reads as zero. That zero fails both the dollar-volume
	// floor and the relative-volume criterion, which is what made the pre-market
	// screen return nothing at all.
	//
	// Implementations must batch: this runs on every symbol that cleared the price
	// move, and a per-symbol call there would cost more than the rest of the scan
	// combined.
	SessionVolumes(ctx context.Context, symbols []string, since time.Time) (map[string]float64, error)
	// IntradayBars returns this session's candles for one symbol, oldest first, at
	// the given interval and starting no earlier than `since`.
	//
	// This is the expensive call in the scan: one request per symbol. It is made only
	// for candidates that have already passed all three screening criteria, which is
	// a handful of names rather than the whole market — the setup detector needs a
	// chart, and a chart cannot be batched across symbols.
	IntradayBars(ctx context.Context, symbol string, interval time.Duration, since time.Time) ([]domain.Bar, error)
}

// Trading is the write side plus account and calendar queries.
type Trading interface {
	Account(ctx context.Context) (domain.Account, error)
	Calendar(ctx context.Context, date string) (CalendarDay, error)
	// NextSession returns the first trading session on a date strictly after the
	// given one. The status page needs it to count down to the next open: after the
	// close, or on a weekend or holiday, today's calendar entry says nothing about
	// when trading resumes.
	NextSession(ctx context.Context, afterDate string) (CalendarDay, error)
	PlaceOrder(ctx context.Context, req OrderRequest) (OrderResult, error)
	// Order looks up a placed order's current state, which is where the fill is.
	Order(ctx context.Context, brokerOrderID string) (OrderResult, error)
	// CancelOrder withdraws whatever of an order has not filled. It does not report
	// the outcome; look the order up afterwards for what executed before it landed.
	CancelOrder(ctx context.Context, brokerOrderID string) error
	Positions(ctx context.Context) ([]BrokerPosition, error)
}
