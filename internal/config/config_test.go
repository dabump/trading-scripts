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
	if c.Risk.RiskPerTradePct != 1 || c.Risk.MaxConcurrentPositions != 3 {
		t.Errorf("risk = %v%% x %d positions, want 1%% x 3 (docs/risk.md)",
			c.Risk.RiskPerTradePct, c.Risk.MaxConcurrentPositions)
	}
	// The notional cap has to sit above risk_per_trade / max_stop_distance, or it
	// binds on every trade and sizing silently reverts to a fixed fraction of the
	// account — the very thing sizing from the stop is meant to replace.
	if ratio := c.Risk.RiskPerTradePct / c.Entry.MaxStopDistancePct * 100; c.Risk.MaxPositionPct <= ratio {
		t.Errorf("max_position_pct %.0f%% is at or below %.0f%%, so the cap binds on every setup",
			c.Risk.MaxPositionPct, ratio)
	}
	// The exits are the stop-loss and the forced end-of-day deadline, and nothing
	// else. A profit target or trailing stop here would reintroduce the single
	// largest measured loss in the strategy's history (docs/decisions.md).
	if c.Exit.EODExitOffsetMins != 5 {
		t.Errorf("eod offset = %d, want 5 (docs/strategy.md §4)", c.Exit.EODExitOffsetMins)
	}
	// A whole-position profit target is the rule that measured t = -9.76. Scaling out
	// must leave a runner behind.
	if c.Exit.FirstTargetFraction >= 1 {
		t.Errorf("first_target_fraction = %v, which sells the whole position",
			c.Exit.FirstTargetFraction)
	}
	// The setup gate is what makes this a pullback strategy rather than a screen.
	if c.Entry.PatternInterval <= 0 || c.Entry.EMAPeriod < 2 {
		t.Errorf("entry pattern settings look unset: %+v", c.Entry)
	}
	// Tradability floors: without them the screen selects warrants and sub-$1 names
	// that cannot be filled (see docs/decisions.md).
	if c.Screening.MinPrice <= 0 || c.Screening.MinDollarVolume <= 0 {
		t.Errorf("tradability floors = $%v / $%v, both must be set",
			c.Screening.MinPrice, c.Screening.MinDollarVolume)
	}
	// The state is a claim about what the exchange is doing, so coverage has to span
	// what the exchange's extended sessions do. The start shipped at 07:00 once, which
	// meant an agent started at 06:15 ET sat in MARKET_CLOSED while the tape was
	// trading — it looked like the feature was broken, and from the operator's side it
	// was. The end is the same claim at the other end of the day.
	if c.Extended.Enabled {
		hour, minute, err := c.Extended.StartClock()
		if err != nil {
			t.Fatalf("extended.start: %v", err)
		}
		if hour*60+minute > 4*60 {
			t.Errorf("extended.start = %s, want 04:00 or earlier — the US pre-market session opens at 04:00 ET, and a later start reports MARKET_CLOSED while the market is in pre-market",
				c.Extended.Start)
		}
		hour, minute, err = c.Extended.EndClock()
		if err != nil {
			t.Fatalf("extended.end: %v", err)
		}
		if hour*60+minute < 20*60 {
			t.Errorf("extended.end = %s, want 20:00 or later — the US post-market session runs to 20:00 ET, and an earlier end reports MARKET_CLOSED while the market is in post-market",
				c.Extended.End)
		}
	}
	// Extended-hours thresholds have to be looser than the regular-session ones, or the
	// extended screen never returns anything and the feature only looks enabled.
	if c.Extended.Enabled {
		if c.Extended.MinDollarVolume >= c.Screening.MinDollarVolume {
			t.Errorf("extended.min_dollar_volume $%v is not below the regular $%v floor, so the extended-hours screen cannot fill",
				c.Extended.MinDollarVolume, c.Screening.MinDollarVolume)
		}
		if c.Extended.MinVolumeMultiple >= c.Screening.MinVolumeMultiple {
			t.Errorf("extended.min_volume_multiple %vx is not below the regular %vx, which no extended session reaches",
				c.Extended.MinVolumeMultiple, c.Screening.MinVolumeMultiple)
		}
		if c.Extended.ScanInterval < c.Timing.ScreenerScanInterval {
			t.Errorf("extended.scan_interval %s is faster than the regular %s; one pass is ~130 requests",
				c.Extended.ScanInterval, c.Timing.ScreenerScanInterval)
		}
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
		MinPrice: 1, MaxPrice: 20, MinDollarVolume: 1_000_000}
	c.Entry = Entry{PatternInterval: time.Minute, EMAPeriod: 9, RequireAboveVWAP: true,
		MaxPullbackBars: 2, SurgeBars: 3, MinSurgePct: 3, MaxRetracePct: 50, StopBufferPct: 0.1,
		MinStopDistancePct: 0.5, MaxStopDistancePct: 4, MaxEntryDriftPct: 1}
	c.Risk = Risk{RiskPerTradePct: 1, MaxPositionPct: 33, MaxConcurrentPositions: 3,
		StopLossPct: 10}
	c.Exit = Exit{FirstTargetR: 2, FirstTargetFraction: 0.5, BreakevenAfterTarget: true,
		EODExitOffsetMins: 30}
	c.Timing = Timing{SentimentPollInterval: 120e9, SentimentWindow: 300e9,
		EntryWindow: 2 * 3600e9, EntryCutoffBuffer: 1800e9,
		ScreenerScanInterval: 60e9, PositionPollInterval: 15e9}
	c.Sentiment = Sentiment{Symbols: []string{"SPY"}, BearishAvgPct: -0.8}
	c.Execution = Execution{OrderType: "market"}
	c.Web = Web{ListenAddr: ":8080", PollInterval: 12e9}
	c.Storage = Storage{DatabasePath: "data/agent.db"}
	c.Audit = Audit{Directory: "logs"}
	return c
}

// enabledExtended is an extended-hours section that passes validation, so a test case
// can mutate the one field it is about.
func enabledExtended() Extended {
	return Extended{
		Enabled: true, Start: "07:00", End: "20:00", ScanInterval: 5 * time.Minute,
		MinDollarVolume: 100_000, MinVolumeMultiple: 0.5,
	}
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
			func(c *Config) { c.Risk.MaxPositionPct = 25; c.Risk.MaxConcurrentPositions = 5 },
			"exceeds 100%",
		},
		{
			"exposure exactly 100% is allowed",
			func(c *Config) { c.Risk.MaxPositionPct = 20; c.Risk.MaxConcurrentPositions = 5 },
			"",
		},
		{
			// Five concurrent trades each risking 6% puts 30% of the account on the
			// line at once, which is the number that actually decides survival.
			"too much simultaneous risk is rejected",
			func(c *Config) { c.Risk.RiskPerTradePct = 6; c.Risk.MaxConcurrentPositions = 5 },
			"at risk simultaneously",
		},
		{
			"a chart stop wider than the backstop is rejected",
			func(c *Config) { c.Entry.MaxStopDistancePct = 12 },
			"must be below risk.stop_loss_pct",
		},
		{
			"a pause of no bars is rejected",
			func(c *Config) { c.Entry.MaxPullbackBars = 0 },
			"entry.max_pullback_bars",
		},
		{
			"a setup with no surge window is rejected",
			func(c *Config) { c.Entry.SurgeBars = 0 },
			"entry.surge_bars",
		},
		{
			"a retrace limit over 100% is rejected",
			func(c *Config) { c.Entry.MaxRetracePct = 150 },
			"entry.max_retrace_pct",
		},
		{
			"selling the whole position at the target is rejected",
			func(c *Config) { c.Exit.FirstTargetFraction = 1 },
			"leave no runner",
		},
		{
			"a price band with no width is rejected",
			func(c *Config) { c.Screening.MaxPrice = 1 },
			"must be greater than screening.min_price",
		},
		{
			"an entry window inside the sentiment window is rejected",
			func(c *Config) { c.Timing.EntryWindow = 2 * time.Minute },
			"no entry is ever possible",
		},
		{
			"a negative entry cutoff buffer is rejected",
			func(c *Config) { c.Timing.EntryCutoffBuffer = -time.Minute },
			"must be >= 0",
		},
		{
			"a buffer longer than the session is rejected",
			func(c *Config) { c.Timing.EntryCutoffBuffer = 7 * time.Hour },
			"leaves no usable entry window",
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
		{
			// Nothing in the section is read while it is off, so disabled extended hours
			// must not be able to fail start-up over numbers nobody will use.
			"a disabled extended-hours section is not validated",
			func(c *Config) {
				c.Extended = Extended{Start: "nonsense", End: "nonsense", ScanInterval: -1}
			},
			"",
		},
		{
			"an enabled extended-hours section is valid",
			func(c *Config) { c.Extended = enabledExtended() },
			"",
		},
		{
			"an extended start that is not a clock time is rejected",
			func(c *Config) {
				c.Extended = enabledExtended()
				c.Extended.Start = "7am"
			},
			"extended.start",
		},
		{
			// A start at or after the bell leaves no pre-market at all, which would
			// read as the feature being broken rather than misconfigured.
			"an extended start at the regular open is rejected",
			func(c *Config) {
				c.Extended = enabledExtended()
				c.Extended.Start = "09:30"
			},
			"must be before the 09:30 regular open",
		},
		{
			"an extended end that is not a clock time is rejected",
			func(c *Config) {
				c.Extended = enabledExtended()
				c.Extended.End = "8pm"
			},
			"extended.end",
		},
		{
			// The mirror of the start rule: an end at or before the bell leaves no
			// post-market, and the two halves should fail start-up the same way rather
			// than one of them silently doing nothing.
			"an extended end at the regular close is rejected",
			func(c *Config) {
				c.Extended = enabledExtended()
				c.Extended.End = "16:00"
			},
			"must be after the 16:00 regular close",
		},
		{
			"extended hours with no scan cadence is rejected",
			func(c *Config) {
				c.Extended = enabledExtended()
				c.Extended.ScanInterval = 0
			},
			"extended.scan_interval",
		},
		{
			// Left at zero the thresholds would admit everything, which on the thin
			// extended tape means every illiquid name in the market.
			"extended-hours thresholds must be set",
			func(c *Config) {
				c.Extended = enabledExtended()
				c.Extended.MinDollarVolume = 0
				c.Extended.MinVolumeMultiple = 0
			},
			"extended.min_dollar_volume",
		},
		{
			// Extended-hours orders are always limit orders, so enabling extended entry
			// must not drag the regular session off market orders with it — that would
			// be an unrelated change to how the rest of the day trades.
			"extended-hours entry does not require limit orders in the regular session",
			func(c *Config) {
				c.Extended = enabledExtended()
				c.Extended.AllowEntry = true
				c.Extended.LimitSlipPct = 1
				c.Execution = Execution{OrderType: "market"}
			},
			"",
		},
		{
			// A limit order with no allowance has no price to be limited to.
			"extended-hours entry with no limit allowance anywhere is rejected",
			func(c *Config) {
				c.Extended = enabledExtended()
				c.Extended.AllowEntry = true
				c.Execution = Execution{OrderType: "market"}
			},
			"needs a limit price allowance",
		},
		{
			// Falling back to the regular allowance is what keeps the extended one
			// optional rather than another thing to remember.
			"extended-hours entry falls back to the regular limit allowance",
			func(c *Config) {
				c.Extended = enabledExtended()
				c.Extended.AllowEntry = true
				c.Execution = Execution{OrderType: "limit", LimitSlipPct: 0.5}
			},
			"",
		},
		{
			"a negative extended limit allowance is rejected",
			func(c *Config) {
				c.Extended = enabledExtended()
				c.Extended.LimitSlipPct = -1
			},
			"extended.limit_slip_pct",
		},
		{
			"extended-hours entry without extended-hours screening is rejected",
			func(c *Config) {
				c.Extended = Extended{AllowEntry: true}
				c.Execution = Execution{OrderType: "limit", LimitSlipPct: 0.5}
			},
			"requires extended.enabled",
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
