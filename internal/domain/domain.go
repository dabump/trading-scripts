// Package domain holds the types shared across packages.
package domain

import "time"

// AgentState is the status shown on the web page's legend.
type AgentState string

const (
	StateMarketClosed   AgentState = "MARKET_CLOSED"
	StateSentimentCheck AgentState = "SENTIMENT_CHECK"
	StateScreening      AgentState = "SCREENING"
	StateHaltedBearish  AgentState = "HALTED_BEARISH"
	StateEODWindow      AgentState = "EOD_WINDOW"
	StateError          AgentState = "ERROR"
)

// Phase is where the clock sits in the trading day, independent of what the
// agent decided to do about it.
type Phase int

const (
	PhaseClosed Phase = iota
	PhaseFirstHour
	PhaseTrading
	PhaseEODWindow
)

// Verdict is the outcome of the first-hour sentiment gate.
type Verdict string

const (
	VerdictPending Verdict = "PENDING"
	VerdictProceed Verdict = "PROCEED"
	VerdictBearish Verdict = "OVERWHELMINGLY_BEARISH"
)

// ExitReason records why a position was closed. Mirrors the four exit triggers
// in docs/strategy.md §4.
type ExitReason string

const (
	ExitForcedEOD    ExitReason = "FORCED_EOD"
	ExitStopLoss     ExitReason = "STOP_LOSS"
	ExitTrailingStop ExitReason = "TRAILING_STOP"
	// ExitReconciled is not a strategy exit: it records a position the broker no
	// longer holds, found during restart reconciliation. Kept distinct so the
	// end-of-day summary never attributes a disappearance to a strategy rule.
	ExitReconciled ExitReason = "RECONCILED"
)

// Bar is a single OHLCV candle.
type Bar struct {
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

// Account is the broker's view of the portfolio.
type Account struct {
	PortfolioValue float64
	Cash           float64
	Equity         float64
}

// Snapshot is the per-symbol market data the screener needs.
type Snapshot struct {
	Symbol      string
	Price       float64
	PrevClose   float64
	TodayVolume float64
	IntradayPct float64
}

// Position is a holding, open or closed.
type Position struct {
	ID          int64
	SessionDate string
	Symbol      string
	Shares      int
	EntryPrice  float64
	EntryTime   time.Time
	PeakPrice   float64
	// LastPrice is the most recent mark recorded by the trading loop, used by the
	// status page so it never has to call the market data API itself.
	LastPrice  float64
	TrailArmed bool
	Open       bool
	ExitPrice  float64
	ExitTime   time.Time
	ExitReason ExitReason
}

// UnrealizedPct is the position's P&L percentage at the given price.
func (p Position) UnrealizedPct(current float64) float64 {
	if p.EntryPrice == 0 {
		return 0
	}
	return (current - p.EntryPrice) / p.EntryPrice * 100
}

// UnrealizedDollars is the position's P&L in dollars at the given price.
func (p Position) UnrealizedDollars(current float64) float64 {
	return (current - p.EntryPrice) * float64(p.Shares)
}

// RealizedPct is the closed position's P&L percentage.
func (p Position) RealizedPct() float64 {
	if p.EntryPrice == 0 {
		return 0
	}
	return (p.ExitPrice - p.EntryPrice) / p.EntryPrice * 100
}

// RealizedDollars is the closed position's P&L in dollars.
func (p Position) RealizedDollars() float64 {
	return (p.ExitPrice - p.EntryPrice) * float64(p.Shares)
}

// Criterion is one screening rule's outcome for one symbol. Display carries the
// underlying value so the web page can show the number, not just a tick.
type Criterion struct {
	Name    string
	Pass    bool
	Display string
}

// Evaluation is the full screening result for one symbol.
type Evaluation struct {
	Symbol     string
	Criteria   []Criterion
	Qualifies  bool
	FailReason string
	// VolumeMultiple is retained for ranking when more candidates qualify than
	// there are free position slots.
	VolumeMultiple float64
	// Outcome records what the entry pass did with a qualifying candidate —
	// "bought", or why it was skipped. Qualifying is not the same as being bought:
	// the position cap, the same-day re-entry rule and available cash all still
	// apply, and without this the reason lived only in the log.
	Outcome string
}

// SentimentCheck is the result of a manually triggered sentiment read. It is
// deliberately not a SentimentReading: a manual check is never persisted, because
// the gate verdict is decided by the most recent stored reading and a mid-session
// manual check would otherwise be able to overturn what the first hour concluded.
type SentimentCheck struct {
	TakenAt        time.Time
	Percentages    map[string]float64
	Classification Verdict
	// Missing lists configured symbols that returned no usable data.
	Missing []string
}

// ScreenPreview is the result of a manually triggered screening pass. Nothing is
// persisted and no order is ever placed from this path.
type ScreenPreview struct {
	TakenAt      time.Time
	UniverseSize int
	Evaluations  []Evaluation
	// MarketOpen is false when this ran outside exchange hours, in which case the
	// prices and percentages describe the last session, not a live move.
	MarketOpen bool
}

// SentimentReading is one 10-minute poll during the first hour.
type SentimentReading struct {
	TakenAt        time.Time
	SessionDate    string
	Percentages    map[string]float64
	Classification Verdict
}
