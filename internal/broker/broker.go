// Package broker wraps market data and order execution behind interfaces, so
// strategy and risk code never depends on a particular provider (see the
// reasoning in docs/architecture.md).
package broker

import (
	"context"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

// Mover is a symbol surfaced by the market-movers screen, which is the source of
// the candidate universe.
type Mover struct {
	Symbol    string
	Price     float64
	ChangePct float64
}

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
}

// OrderResult is the broker's acknowledgement.
type OrderResult struct {
	BrokerOrderID string
	Status        string
	FilledPrice   float64
	FilledShares  int
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
	Snapshots(ctx context.Context, symbols []string) (map[string]domain.Snapshot, error)
	// IntradayBars returns the current session's candles at the given interval,
	// oldest first.
	IntradayBars(ctx context.Context, symbol string, intervalMins int, since time.Time) ([]domain.Bar, error)
	// AverageDailyVolume averages the prior sessions' volume, excluding today
	// (today is the number being compared against it).
	AverageDailyVolume(ctx context.Context, symbol string, days int) (float64, error)
	Movers(ctx context.Context, top int) ([]Mover, error)
	// NewsCounts reports how many stories each symbol has since the given time.
	NewsCounts(ctx context.Context, symbols []string, since time.Time) (map[string]int, error)
}

// Trading is the write side plus account and calendar queries.
type Trading interface {
	Account(ctx context.Context) (domain.Account, error)
	Calendar(ctx context.Context, date string) (CalendarDay, error)
	PlaceOrder(ctx context.Context, req OrderRequest) (OrderResult, error)
	Positions(ctx context.Context) ([]BrokerPosition, error)
}

// FloatProvider supplies share float, which is not part of a broker's normal
// market data feed. Kept separate because it is the one screening input whose
// source is still an open item in docs/decisions.md.
type FloatProvider interface {
	// FloatShares reports the symbol's float. The bool is false when the value is
	// simply unavailable, which the screener treats as a failed criterion rather
	// than a skipped one.
	FloatShares(ctx context.Context, symbol string) (float64, bool, error)
}

// NoFloatProvider is the default: it never supplies a float, so the float
// criterion fails closed and nothing qualifies. This is deliberate — trading on
// an unverified float would silently drop a documented entry requirement.
type NoFloatProvider struct{}

func (NoFloatProvider) FloatShares(context.Context, string) (float64, bool, error) {
	return 0, false, nil
}
