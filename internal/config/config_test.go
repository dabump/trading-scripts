package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The shipped config must actually load and validate; a typo here would only
// show up at daemon start otherwise.
func TestLoadShippedConfig(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config", "config.yaml"))
	if err != nil {
		t.Fatalf("shipped config failed to load: %v", err)
	}
	if c.Screening.MinIntradayPct != 10 || c.Screening.MinVolumeMultiple != 5 {
		t.Errorf("screen thresholds = %v%% / %vx, want 10%% / 5x (docs/strategy.md §2)",
			c.Screening.MinIntradayPct, c.Screening.MinVolumeMultiple)
	}
	if c.Risk.PositionSizePct != 10 || c.Risk.MaxConcurrentPositions != 5 {
		t.Errorf("sizing = %v%% x %d, want 10%% x 5 (docs/risk.md)",
			c.Risk.PositionSizePct, c.Risk.MaxConcurrentPositions)
	}
	// Tradability floors: without them the screen selects warrants and sub-$1 names
	// that cannot be filled (see docs/decisions.md).
	if c.Screening.MinPrice <= 0 || c.Screening.MinDollarVolume <= 0 {
		t.Errorf("tradability floors = $%v / $%v, both must be set",
			c.Screening.MinPrice, c.Screening.MinDollarVolume)
	}
	// The full consolidated tape, without which the volume criterion is meaningless.
	if c.MarketData.Feed != "sip" {
		t.Errorf("feed = %q, want sip", c.MarketData.Feed)
	}
	// Must reach back past the open, or pre-market catalysts are invisible.
	if c.Screening.NewsLookback < 7*time.Hour {
		t.Errorf("news_lookback = %v, want at least 7h", c.Screening.NewsLookback)
	}
	if c.Timing.ScreenerScanInterval.Minutes() != 1 {
		t.Errorf("scan interval = %v, want 1m (docs/strategy.md §2)", c.Timing.ScreenerScanInterval)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("screening:\n  bogus_key: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown field to be rejected")
	}
}

func valid() *Config {
	c := &Config{}
	c.MarketData = MarketData{Feed: "sip"}
	c.Screening = Screening{MinIntradayPct: 10, MinVolumeMultiple: 5,
		AvgVolumeLookbackDays: 20, MaxEnriched: 100, NewsLookback: 18 * time.Hour,
		MinPrice: 1, MinDollarVolume: 1_000_000}
	c.Risk = Risk{PositionSizePct: 10, MaxConcurrentPositions: 5, StopLossPct: 10}
	c.Exit = Exit{ProfitTargetPct: 15, TrailingStopPct: 5, EODExitOffsetMins: 30}
	c.Timing = Timing{SentimentPollInterval: 600e9, SentimentWindow: 3600e9,
		ScreenerScanInterval: 60e9, PositionPollInterval: 15e9}
	c.Sentiment = Sentiment{Symbols: []string{"SPY"}, BearishAvgPct: -0.8}
	c.Execution = Execution{OrderType: "market"}
	c.Web = Web{ListenAddr: ":8080", PollInterval: 12e9}
	c.Storage = Storage{DatabasePath: "data/agent.db"}
	c.Audit = Audit{Directory: "logs"}
	return c
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"baseline is valid", func(*Config) {}, ""},
		{
			"exposure over 100% is rejected",
			func(c *Config) { c.Risk.PositionSizePct = 25; c.Risk.MaxConcurrentPositions = 5 },
			"exceeds 100%",
		},
		{
			"exposure exactly 100% is allowed",
			func(c *Config) { c.Risk.PositionSizePct = 20; c.Risk.MaxConcurrentPositions = 5 },
			"",
		},
		{
			"positive bearish threshold is rejected",
			func(c *Config) { c.Sentiment.BearishAvgPct = 0.5 },
			"must be negative",
		},
		{
			"limit orders need a slip percentage",
			func(c *Config) { c.Execution.OrderType = "limit" },
			"limit_slip_pct",
		},
		{
			"a zero price floor is rejected",
			func(c *Config) { c.Screening.MinPrice = 0 },
			"min_price",
		},
		{
			"a zero dollar-volume floor is rejected",
			func(c *Config) { c.Screening.MinDollarVolume = 0 },
			"min_dollar_volume",
		},
		{
			"stop loss of 100% is rejected",
			func(c *Config) { c.Risk.StopLossPct = 100 },
			"stop_loss_pct",
		},
		{
			"a news lookback that stops short of the market open is rejected",
			func(c *Config) { c.Screening.NewsLookback = 2 * time.Hour },
			"news_lookback",
		},
		{
			"an unknown data feed is rejected",
			func(c *Config) { c.MarketData.Feed = "nasdaq" },
			"market_data.feed",
		},
		{
			"the free IEX feed is accepted, with its caveats left to the operator",
			func(c *Config) { c.MarketData.Feed = "iex" },
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(c)
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestSecretsRequireCredentials(t *testing.T) {
	t.Setenv("ALPACA_API_KEY", "")
	t.Setenv("ALPACA_API_SECRET", "")
	if _, err := LoadSecrets(); err == nil {
		t.Fatal("expected missing credentials to error")
	}

	t.Setenv("ALPACA_API_KEY", "k")
	t.Setenv("ALPACA_API_SECRET", "s")
	t.Setenv("ALPACA_BASE_URL", "")
	s, err := LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if s.IsLive() {
		t.Error("default base URL must be the paper endpoint, not live")
	}

	t.Setenv("ALPACA_BASE_URL", "https://api.alpaca.markets")
	s, err = LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsLive() {
		t.Error("live base URL must be detected as live")
	}
}
