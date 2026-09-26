package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped config must actually load and validate; a typo here would only
// show up at daemon start otherwise.
func TestLoadShippedConfig(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config", "config.yaml"))
	if err != nil {
		t.Fatalf("shipped config failed to load: %v", err)
	}
	if c.Screening.MaxFloatShares != 10_000_000 {
		t.Errorf("max float = %v, want 10000000 (docs/strategy.md §2)", c.Screening.MaxFloatShares)
	}
	if c.Risk.PositionSizePct != 10 || c.Risk.MaxConcurrentPositions != 5 {
		t.Errorf("sizing = %v%% x %d, want 10%% x 5 (docs/risk.md)",
			c.Risk.PositionSizePct, c.Risk.MaxConcurrentPositions)
	}
	if c.Exit.MACDIntervalMins != 15 {
		t.Errorf("macd interval = %d, want 15 (docs/strategy.md §4)", c.Exit.MACDIntervalMins)
	}
	if c.Timing.ScreenerScanInterval.Minutes() != 1 {
		t.Errorf("scan interval = %v, want 1m (docs/strategy.md §2)", c.Timing.ScreenerScanInterval)
	}
	// Screening must fail closed until a float provider is actually chosen.
	if c.Screening.FloatProvider != "none" {
		t.Errorf("float_provider = %q, want \"none\" while the data source is an open item",
			c.Screening.FloatProvider)
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
	c.Screening = Screening{MaxFloatShares: 1e7, MinIntradayPct: 10, MinVolumeMultiple: 5,
		AvgVolumeLookbackDays: 20, UniverseSize: 50, FloatProvider: "none"}
	c.Risk = Risk{PositionSizePct: 10, MaxConcurrentPositions: 5, StopLossPct: 10}
	c.Exit = Exit{ProfitTargetPct: 15, TrailingStopPct: 5, MACDFast: 5, MACDSlow: 10,
		MACDSignal: 3, MACDIntervalMins: 15, EODExitOffsetMins: 30}
	c.Timing = Timing{SentimentPollInterval: 600e9, SentimentWindow: 3600e9,
		ScreenerScanInterval: 60e9, PositionPollInterval: 15e9}
	c.Sentiment = Sentiment{Symbols: []string{"SPY"}, BearishAvgPct: -0.8}
	c.Execution = Execution{OrderType: "market"}
	c.Web = Web{ListenAddr: ":8080", PollInterval: 12e9}
	c.Storage = Storage{DatabasePath: "data/agent.db"}
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
			"macd fast must be below slow",
			func(c *Config) { c.Exit.MACDFast = 10; c.Exit.MACDSlow = 10 },
			"must be < exit.macd_slow",
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
			"an unimplemented float provider is rejected",
			func(c *Config) { c.Screening.FloatProvider = "alpaca" },
			"float_provider",
		},
		{
			"stop loss of 100% is rejected",
			func(c *Config) { c.Risk.StopLossPct = 100 },
			"stop_loss_pct",
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

func TestMACDWarmupBars(t *testing.T) {
	c := valid()
	// slow=10 + signal=3 = 13 bars of 15 minutes = 3h15m after the open, which is
	// later than the 2.5h docs/strategy.md §4 estimates.
	if got := c.MACDWarmupBars(); got != 13 {
		t.Errorf("warmup = %d bars, want 13", got)
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
