// Package config loads the tunable strategy parameters documented in
// docs/operations.md. Secrets are deliberately not here: they come from the
// environment (see Secrets).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	MarketData MarketData `yaml:"market_data"`
	Screening  Screening  `yaml:"screening"`
	Entry      Entry      `yaml:"entry"`
	Risk       Risk       `yaml:"risk"`
	Exit       Exit       `yaml:"exit"`
	Timing     Timing     `yaml:"timing"`
	Sentiment  Sentiment  `yaml:"sentiment"`
	Execution  Execution  `yaml:"execution"`
	Web        Web        `yaml:"web"`
	Storage    Storage    `yaml:"storage"`
	Audit      Audit      `yaml:"audit"`
}

// MarketData selects which Alpaca data feed to use.
type MarketData struct {
	// Feed is "sip" (full consolidated tape, paid plan) or "iex" (free, a few
	// percent of consolidated volume). This is not a cosmetic setting: the
	// relative-volume criterion is meaningless on IEX data, because it would be
	// comparing one exchange's activity against the whole market's average.
	Feed string `yaml:"feed"`
}

type Screening struct {
	MinIntradayPct float64 `yaml:"min_intraday_pct"`
	// MaxIntradayPct is a ceiling on the move, and it exists because the strategy's
	// own premise stops holding past a point.
	//
	// A one-year backtest over the full universe found return falling monotonically
	// as the entry gap widened: +15-25% returned -0.90% a trade, +25-50% returned
	// -1.11%, +50-100% returned -2.14%, and above +100% returned -3.13% with a
	// median of -10.00% — that last bucket is 280 trades, so the gap between it and
	// the +15-25% bucket is roughly three standard errors, not noise. Names up
	// several hundred percent intraday are exhausted moves and halt candidates, not
	// momentum entries.
	MaxIntradayPct        float64 `yaml:"max_intraday_pct"`
	MinVolumeMultiple     float64 `yaml:"min_volume_multiple"`
	AvgVolumeLookbackDays int     `yaml:"avg_volume_lookback_days"`
	// MinPrice and MinDollarVolume are tradability floors, not momentum criteria.
	//
	// Without them the screen is dominated by instruments the strategy was never
	// meant to hold: a backtest over 2024-2026 surfaced warrants at $0.07, SPAC units
	// whose 20-session average volume was 15 shares, and sub-$1 names averaging a few
	// hundred shares a day. A $1,000 position in those is not executable at any
	// sensible price, and sub-$1 names were also the worst-performing price bucket
	// measured. Both are checked from the snapshot, so they cost no extra API calls
	// and reject candidates before the expensive per-symbol lookups.
	MinPrice float64 `yaml:"min_price"`
	// MaxPrice is the top of the price band. The strategy being followed trades
	// roughly $1-$20: above that, a 10% intraday move on 5x volume is a different
	// kind of event in a different kind of name, and the pullback entries below are
	// not what moves it.
	MaxPrice float64 `yaml:"max_price"`
	// MinDollarVolume is price × volume traded so far today, at the moment of the
	// decision — a liquidity measure, not a size one.
	MinDollarVolume float64 `yaml:"min_dollar_volume"`
	// MaxEnriched bounds how many symbols get the expensive per-symbol news and
	// average-volume lookups after the cheap price-move filter. On a violent day
	// hundreds of names clear +10%, and enriching all of them every minute would
	// be wasteful; the busiest by dollar volume are kept.
	MaxEnriched int `yaml:"max_enriched"`
	// NewsLookback is how far back to search for a catalyst, measured from now.
	//
	// It has to reach past the market open. The catalyst behind a gap-up almost
	// always breaks overnight or in the pre-market session, so searching only from
	// 09:30 would miss the very story that caused the move and report "no news" for
	// exactly the candidates the strategy wants.
	NewsLookback time.Duration `yaml:"news_lookback"`
}

// Entry is the setup gate: the pattern a screened candidate has to print before it
// is bought.
//
// This is the difference between screening and trading. The three screening criteria
// say a stock is *interesting*; they say nothing about whether this instant is a
// sensible moment to buy it or where the risk sits. Buying on the screen alone means
// buying extension at whatever price the scan happens to read, with a stop unrelated
// to anything on the chart — which a one-year backtest measured as having no edge.
type Entry struct {
	// PatternInterval is the candle size the setup is read on.
	PatternInterval time.Duration `yaml:"pattern_interval"`
	// EMAPeriod is the trend reference the price must hold above.
	EMAPeriod int `yaml:"ema_period"`
	// RequireAboveVWAP additionally demands price above the session VWAP, which is
	// the line that separates a stock being accumulated from one being distributed.
	RequireAboveVWAP bool `yaml:"require_above_vwap"`
	// MinPullbackBars and MaxPullbackBars bound the flag. One bar is the shallowest
	// pullback worth calling one; too many and it is no longer a pause inside a
	// move but a stalled trend.
	MinPullbackBars int `yaml:"min_pullback_bars"`
	MaxPullbackBars int `yaml:"max_pullback_bars"`
	// StopBufferPct places the stop just under the flag's low rather than exactly on
	// it, so the obvious price does not take the position out.
	StopBufferPct float64 `yaml:"stop_buffer_pct"`
	// MaxStopDistancePct refuses a setup whose stop is too far below entry. This is
	// a real part of the strategy, not a safety rail: if the risk is wide the trade
	// is not taken, because position size would have to shrink to the point where
	// the winner cannot pay for the losers.
	MaxStopDistancePct float64 `yaml:"max_stop_distance_pct"`
	// MinStopDistancePct widens a stop that sits implausibly close to entry, where
	// ordinary noise would trigger it.
	MinStopDistancePct float64 `yaml:"min_stop_distance_pct"`
}

type Risk struct {
	// RiskPerTradePct is the core of the sizing rule: a fixed fraction of the
	// account is put at risk on every trade, and the share count follows from how
	// far away the stop is. Sizing by a fixed fraction of *portfolio value* instead
	// makes the dollar risk swing with the stock's volatility, which is how a single
	// wide-stop trade ends up costing several times what a narrow-stop one does.
	RiskPerTradePct float64 `yaml:"risk_per_trade_pct"`
	// MaxPositionPct caps one position's notional regardless of how tight its stop
	// is. Without it a stop 0.5% away would size to a position larger than the
	// account.
	MaxPositionPct         float64 `yaml:"max_position_pct"`
	MaxConcurrentPositions int     `yaml:"max_concurrent_positions"`
	// StopLossPct is now a backstop, not the working stop. The working stop comes
	// from the chart (see Entry.MaxStopDistancePct, which is tighter), so this only
	// fires when price gaps straight through it.
	StopLossPct         float64 `yaml:"stop_loss_pct"`
	AllowSameDayReentry bool    `yaml:"allow_same_day_reentry"`
}

// Exit describes scaling out of a winner and the end-of-day deadline.
//
// FirstTargetR is expressed in multiples of the initial risk rather than a fixed
// percentage, which is the point: a trade risking 2% and a trade risking 5% should
// not take profit at the same price move. A fixed-percentage profit target with a
// trailing stop used to live here and was removed after it measured a t-statistic of
// -9.76 over 1,697 trades; scaling out part of a position and letting the rest run is
// a different rule with a different payoff, and it is measured separately.
type Exit struct {
	// FirstTargetR is the first profit target, in multiples of initial risk.
	FirstTargetR float64 `yaml:"first_target_r"`
	// FirstTargetFraction is how much of the position is sold there. The remainder
	// is the runner that pays for the losing trades.
	FirstTargetFraction float64 `yaml:"first_target_fraction"`
	// BreakevenAfterTarget moves the runner's stop to the entry price once the first
	// target is banked, so a winner cannot become a loser.
	BreakevenAfterTarget bool `yaml:"breakeven_after_target"`
	EODExitOffsetMins    int  `yaml:"eod_exit_offset_minutes"`
}

type Timing struct {
	SentimentPollInterval time.Duration `yaml:"sentiment_poll_interval"`
	// SentimentWindow is how long the gate takes to resolve after the open.
	//
	// It used to be an hour, during which no trading happened at all. That is the
	// single worst hour to sit out for this strategy: the moves it looks for begin
	// in the first fifteen minutes. The window is now short enough to keep the
	// kill switch while giving most of the opening range back.
	SentimentWindow time.Duration `yaml:"sentiment_window"`
	// EntryWindow is how long after the open new positions may be opened. Positions
	// already open are still managed to the close.
	EntryWindow time.Duration `yaml:"entry_window"`
	// EntryCutoffBuffer is quiet time between the last possible entry and the forced
	// end-of-day exit, so nothing is bought that is about to be required to sell.
	//
	// It is a separate setting rather than arithmetic on EntryWindow because the two
	// answer to different things: EntryWindow is measured from the open, the forced
	// exit from the close, and on a half day those collide. Capping the window at the
	// forced exit alone would let an early close open a position one minute before it
	// had to be liquidated.
	EntryCutoffBuffer    time.Duration `yaml:"entry_cutoff_buffer"`
	ScreenerScanInterval time.Duration `yaml:"screener_scan_interval"`
	PositionPollInterval time.Duration `yaml:"position_poll_interval"`
}

type Sentiment struct {
	Symbols []string `yaml:"symbols"`
	// BearishAvgPct is the average % change across Symbols at or below which the
	// session is classified overwhelmingly bearish, provided RequireAllNegative
	// is also satisfied.
	BearishAvgPct      float64 `yaml:"bearish_avg_pct"`
	RequireAllNegative bool    `yaml:"require_all_negative"`
}

type Execution struct {
	// OrderType is "market" or "limit". Market guarantees a fill but can slip badly
	// on thinly traded names; see the open item in docs/decisions.md.
	OrderType    string  `yaml:"order_type"`
	LimitSlipPct float64 `yaml:"limit_slip_pct"`
}

type Web struct {
	ListenAddr   string        `yaml:"listen_addr"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

type Storage struct {
	DatabasePath string `yaml:"database_path"`
}

// Audit configures the decision-and-action trail.
type Audit struct {
	// Directory holds one append-only JSON-lines file per session date.
	Directory string `yaml:"directory"`
}

// Secrets are the credentials loaded from the environment, never from YAML.
type Secrets struct {
	APIKey    string
	APISecret string
	BaseURL   string
	DataURL   string
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate rejects configurations that would misbehave at runtime rather than
// letting them fail mid-session with real positions open.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.Screening.MinIntradayPct <= 0 {
		add("screening.min_intraday_pct must be > 0")
	}
	if c.Screening.MinVolumeMultiple <= 0 {
		add("screening.min_volume_multiple must be > 0")
	}
	if c.Screening.AvgVolumeLookbackDays < 1 {
		add("screening.avg_volume_lookback_days must be >= 1")
	}
	if c.Screening.MaxEnriched < 1 {
		add("screening.max_enriched must be >= 1")
	}
	if c.Screening.MinPrice <= 0 {
		add("screening.min_price must be > 0")
	}
	if c.Screening.MinDollarVolume <= 0 {
		add("screening.min_dollar_volume must be > 0")
	}
	if c.Screening.MaxPrice <= c.Screening.MinPrice {
		add("screening.max_price (%.2f) must be greater than screening.min_price (%.2f)",
			c.Screening.MaxPrice, c.Screening.MinPrice)
	}

	if c.Entry.PatternInterval <= 0 {
		add("entry.pattern_interval must be > 0")
	}
	if c.Entry.EMAPeriod < 2 {
		add("entry.ema_period must be >= 2")
	}
	if c.Entry.MinPullbackBars < 1 {
		add("entry.min_pullback_bars must be >= 1")
	}
	if c.Entry.MaxPullbackBars < c.Entry.MinPullbackBars {
		add("entry.max_pullback_bars (%d) must be >= entry.min_pullback_bars (%d)",
			c.Entry.MaxPullbackBars, c.Entry.MinPullbackBars)
	}
	if c.Entry.StopBufferPct < 0 || c.Entry.StopBufferPct >= 100 {
		add("entry.stop_buffer_pct must be in [0, 100)")
	}
	if c.Entry.MinStopDistancePct <= 0 {
		add("entry.min_stop_distance_pct must be > 0")
	}
	if c.Entry.MaxStopDistancePct <= c.Entry.MinStopDistancePct {
		add("entry.max_stop_distance_pct (%.2f) must be greater than entry.min_stop_distance_pct (%.2f)",
			c.Entry.MaxStopDistancePct, c.Entry.MinStopDistancePct)
	}
	// The chart stop is the working stop and the percentage stop is the backstop
	// behind it. If the backstop were the tighter of the two it would fire first and
	// the technical stop would never be reached, silently reverting the strategy to
	// a fixed-percentage stop.
	if c.Entry.MaxStopDistancePct >= c.Risk.StopLossPct {
		add("entry.max_stop_distance_pct (%.2f) must be below risk.stop_loss_pct (%.2f), which is the gap backstop behind it",
			c.Entry.MaxStopDistancePct, c.Risk.StopLossPct)
	}
	// Anything shorter than the trading day itself would start the search after the
	// open, which is the bug this setting exists to prevent.
	if c.Screening.NewsLookback < 7*time.Hour {
		add("screening.news_lookback must be at least 7h so it reaches back past the market open")
	}
	switch c.MarketData.Feed {
	case "sip", "iex":
	default:
		add("market_data.feed must be one of: sip, iex")
	}
	if c.Risk.RiskPerTradePct <= 0 || c.Risk.RiskPerTradePct > 100 {
		add("risk.risk_per_trade_pct must be in (0, 100]")
	}
	if c.Risk.MaxPositionPct <= 0 || c.Risk.MaxPositionPct > 100 {
		add("risk.max_position_pct must be in (0, 100]")
	}
	if c.Risk.MaxConcurrentPositions < 1 {
		add("risk.max_concurrent_positions must be >= 1")
	}
	if c.Risk.StopLossPct <= 0 || c.Risk.StopLossPct >= 100 {
		add("risk.stop_loss_pct must be in (0, 100)")
	}
	// Exposure above 100% would mean ordering with money the account does not
	// have; the broker would reject the order mid-session.
	if exposure := c.Risk.MaxPositionPct * float64(c.Risk.MaxConcurrentPositions); exposure > 100 {
		add("risk.max_position_pct * risk.max_concurrent_positions = %.0f%% exceeds 100%% of the portfolio", exposure)
	}
	// Total risk is what actually matters for survival: five concurrent trades each
	// risking 2% is 10% of the account on the line at once.
	if atRisk := c.Risk.RiskPerTradePct * float64(c.Risk.MaxConcurrentPositions); atRisk > 25 {
		add("risk.risk_per_trade_pct * risk.max_concurrent_positions = %.0f%% of the account at risk simultaneously, which exceeds the 25%% ceiling", atRisk)
	}

	if c.Exit.FirstTargetR <= 0 {
		add("exit.first_target_r must be > 0")
	}
	if c.Exit.FirstTargetFraction <= 0 || c.Exit.FirstTargetFraction >= 1 {
		add("exit.first_target_fraction must be in (0, 1) — 1 would sell the whole position and leave no runner")
	}
	if c.Exit.EODExitOffsetMins < 1 {
		add("exit.eod_exit_offset_minutes must be >= 1")
	}

	if c.Timing.SentimentPollInterval <= 0 {
		add("timing.sentiment_poll_interval must be > 0")
	}
	if c.Timing.SentimentWindow <= 0 {
		add("timing.sentiment_window must be > 0")
	}
	if c.Timing.EntryWindow <= 0 {
		add("timing.entry_window must be > 0")
	}
	if c.Timing.EntryCutoffBuffer < 0 {
		add("timing.entry_cutoff_buffer must be >= 0")
	}
	// A regular session is 6h30m. A buffer approaching that leaves no time to enter
	// at all, which would look like a broken scanner rather than a setting.
	if c.Timing.EntryCutoffBuffer >= 6*time.Hour {
		add("timing.entry_cutoff_buffer (%s) leaves no usable entry window in a 6h30m session",
			c.Timing.EntryCutoffBuffer)
	}
	// Entries have to be possible after the gate resolves, or the agent would screen
	// all morning and never buy.
	if c.Timing.EntryWindow <= c.Timing.SentimentWindow {
		add("timing.entry_window (%s) must be longer than timing.sentiment_window (%s), or no entry is ever possible",
			c.Timing.EntryWindow, c.Timing.SentimentWindow)
	}
	if c.Timing.ScreenerScanInterval <= 0 {
		add("timing.screener_scan_interval must be > 0")
	}
	if c.Timing.PositionPollInterval <= 0 {
		add("timing.position_poll_interval must be > 0")
	}

	if len(c.Sentiment.Symbols) == 0 {
		add("sentiment.symbols must list at least one symbol")
	}
	if c.Sentiment.BearishAvgPct >= 0 {
		add("sentiment.bearish_avg_pct must be negative (it is a bearish threshold)")
	}

	switch c.Execution.OrderType {
	case "market":
	case "limit":
		if c.Execution.LimitSlipPct <= 0 {
			add("execution.limit_slip_pct must be > 0 when order_type is limit")
		}
	default:
		add("execution.order_type must be one of: market, limit")
	}

	if c.Web.ListenAddr == "" {
		add("web.listen_addr must be set")
	}
	if c.Web.PollInterval <= 0 {
		add("web.poll_interval must be > 0")
	}
	if c.Storage.DatabasePath == "" {
		add("storage.database_path must be set")
	}
	if c.Audit.Directory == "" {
		add("audit.directory must be set")
	}

	return errors.Join(errs...)
}

// LoadSecrets reads credentials from the environment. It does not fall back to
// defaults for the key or secret: running with an empty credential would
// produce confusing 401s rather than an obvious configuration error.
func LoadSecrets() (*Secrets, error) {
	s := &Secrets{
		APIKey:    os.Getenv("ALPACA_API_KEY"),
		APISecret: os.Getenv("ALPACA_API_SECRET"),
		BaseURL:   os.Getenv("ALPACA_BASE_URL"),
		DataURL:   os.Getenv("ALPACA_DATA_URL"),
	}
	if s.APIKey == "" || s.APISecret == "" {
		return nil, errors.New("ALPACA_API_KEY and ALPACA_API_SECRET must be set (see .env.example)")
	}
	if s.BaseURL == "" {
		s.BaseURL = "https://paper-api.alpaca.markets"
	}
	if s.DataURL == "" {
		s.DataURL = "https://data.alpaca.markets"
	}
	return s, nil
}

// IsLive reports whether the configured base URL points at the live trading
// endpoint rather than the paper one. Callers use this to require an explicit
// opt-in before trading real money.
func (s *Secrets) IsLive() bool {
	return s.BaseURL == "https://api.alpaca.markets"
}
