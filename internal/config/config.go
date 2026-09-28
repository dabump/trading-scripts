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
	MinIntradayPct        float64 `yaml:"min_intraday_pct"`
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

type Risk struct {
	PositionSizePct        float64 `yaml:"position_size_pct"`
	MaxConcurrentPositions int     `yaml:"max_concurrent_positions"`
	StopLossPct            float64 `yaml:"stop_loss_pct"`
	AllowSameDayReentry    bool    `yaml:"allow_same_day_reentry"`
}

type Exit struct {
	ProfitTargetPct   float64 `yaml:"profit_target_pct"`
	TrailingStopPct   float64 `yaml:"trailing_stop_pct"`
	EODExitOffsetMins int     `yaml:"eod_exit_offset_minutes"`
}

type Timing struct {
	SentimentPollInterval time.Duration `yaml:"sentiment_poll_interval"`
	SentimentWindow       time.Duration `yaml:"sentiment_window"`
	ScreenerScanInterval  time.Duration `yaml:"screener_scan_interval"`
	PositionPollInterval  time.Duration `yaml:"position_poll_interval"`
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
	if c.Risk.PositionSizePct <= 0 || c.Risk.PositionSizePct > 100 {
		add("risk.position_size_pct must be in (0, 100]")
	}
	if c.Risk.MaxConcurrentPositions < 1 {
		add("risk.max_concurrent_positions must be >= 1")
	}
	if c.Risk.StopLossPct <= 0 || c.Risk.StopLossPct >= 100 {
		add("risk.stop_loss_pct must be in (0, 100)")
	}
	// Exposure above 100% would mean ordering with money the account does not
	// have; the broker would reject the order mid-session.
	if exposure := c.Risk.PositionSizePct * float64(c.Risk.MaxConcurrentPositions); exposure > 100 {
		add("risk.position_size_pct * risk.max_concurrent_positions = %.0f%% exceeds 100%% of the portfolio", exposure)
	}

	if c.Exit.ProfitTargetPct <= 0 {
		add("exit.profit_target_pct must be > 0")
	}
	if c.Exit.TrailingStopPct <= 0 || c.Exit.TrailingStopPct >= 100 {
		add("exit.trailing_stop_pct must be in (0, 100)")
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
