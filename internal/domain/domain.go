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

// ExitReason records why a position was closed. Mirrors the exit triggers in
// docs/strategy.md §4.
type ExitReason string

const (
	ExitForcedEOD ExitReason = "FORCED_EOD"
	// ExitStopLoss covers both the chart stop the setup defined and the percentage
	// backstop behind it. The stop price recorded on the position says which fired.
	ExitStopLoss ExitReason = "STOP_LOSS"
	// ExitScaleOut is never a position's final ExitReason: it labels the partial
	// sale at the first profit target, which banks part of the trade and leaves a
	// runner open.
	ExitScaleOut ExitReason = "SCALE_OUT"
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

// AccountSnapshot is an account reading with the moment it was taken.
//
// The status page shows the broker balance, but the web layer does not call the
// broker: a page poll must not cost an API request. The trading loop therefore reads
// the account on its own cadence and publishes the result here, and the page renders
// it with its age. Known is false until the first successful read — the page then
// shows nothing rather than a zero balance, which would read as a drained account.
type AccountSnapshot struct {
	Account Account
	At      time.Time
	Known   bool
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
//
// A position can be reduced before it is closed, because the strategy scales out of
// a winner at its first target and lets the rest run. Shares is therefore what was
// bought and SharesOpen is what is still held; BankedDollars accumulates the profit
// and loss already taken off the table.
type Position struct {
	ID          int64
	SessionDate string
	Symbol      string
	// Shares is the original size, kept for the record even after scaling out.
	Shares int
	// SharesOpen is what is still held: equal to Shares until the first target is
	// hit, and zero once the position is closed.
	SharesOpen int
	EntryPrice float64
	EntryTime  time.Time
	PeakPrice  float64
	// StopPrice is the working stop, in dollars rather than a percentage, because
	// the setup derives it from the chart. It moves up to the entry price once the
	// first target is banked.
	StopPrice float64
	// InitialRisk is EntryPrice − the first StopPrice, in dollars per share. Profit
	// targets are multiples of it, so it has to survive the stop being moved.
	InitialRisk float64
	// TargetHit latches once the first profit target has been taken, so a position
	// oscillating around the target is not scaled out of repeatedly.
	TargetHit bool
	// BankedDollars is profit and loss already realised on this position through
	// partial sales.
	BankedDollars float64
	// LastPrice is the most recent mark recorded by the trading loop, used by the
	// status page so it never has to call the market data API itself.
	LastPrice  float64
	Open       bool
	ExitPrice  float64
	ExitTime   time.Time
	ExitReason ExitReason
}

// RMultiple expresses a price as a multiple of the position's initial risk, which
// is the unit the exit targets are stated in.
func (p Position) RMultiple(current float64) float64 {
	if p.InitialRisk <= 0 {
		return 0
	}
	return (current - p.EntryPrice) / p.InitialRisk
}

// UnrealizedPct is the position's P&L percentage at the given price.
func (p Position) UnrealizedPct(current float64) float64 {
	if p.EntryPrice == 0 {
		return 0
	}
	return (current - p.EntryPrice) / p.EntryPrice * 100
}

// UnrealizedDollars is the position's total P&L in dollars at the given price:
// what is already banked from scaling out, plus the mark on what is still held.
func (p Position) UnrealizedDollars(current float64) float64 {
	return p.BankedDollars + (current-p.EntryPrice)*float64(p.SharesOpen)
}

// RealizedPct is the closed position's P&L as a percentage of the capital it
// committed.
//
// This is deliberately not the price move from entry to final exit. Once a position
// can be scaled out of, those are different numbers: half sold at +20% and half at
// −2% did not return −2%. Measuring against entry price × original shares gives the
// return on what was actually put at risk.
func (p Position) RealizedPct() float64 {
	committed := p.EntryPrice * float64(p.Shares)
	if committed == 0 {
		return 0
	}
	return p.BankedDollars / committed * 100
}

// RealizedDollars is the closed position's P&L in dollars, including every partial
// sale along the way.
func (p Position) RealizedDollars() float64 {
	return p.BankedDollars
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
