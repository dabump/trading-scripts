package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// rowValue finds a setting's rendered value in the JSON view, so assertions target a
// specific label rather than matching loose substrings that could collide (several
// settings legitimately read "10.0%").
func rowValue(t *testing.T, f *fixture, label string) (value, note string) {
	t.Helper()
	code, body := f.get(t, "/api/status")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	var v struct {
		Strategy []struct {
			Title string
			Rows  []struct{ Label, Value, Note string }
		}
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, sect := range v.Strategy {
		for _, row := range sect.Rows {
			if row.Label == label {
				return row.Value, row.Note
			}
		}
	}
	t.Fatalf("no strategy row labelled %q", label)
	return "", ""
}

// The requirement: the panel must reflect the loaded configuration, so tuning a
// threshold changes what the page says. A hardcoded panel would pass a "does it
// render" test and still be wrong — so each case sets a value that differs from the
// default and asserts the *new* number appears.
func TestStrategyPanelReadsFromConfig(t *testing.T) {
	tests := []struct {
		name      string
		label     string
		mutate    func(f *fixture)
		wantValue string
	}{
		{
			name:      "minimum intraday move",
			label:     "Intraday move",
			mutate:    func(f *fixture) { f.cfg.Screening.MinIntradayPct = 7.5 },
			wantValue: "≥ 7.5%",
		},
		{
			name:      "relative volume multiple",
			label:     "Relative volume",
			mutate:    func(f *fixture) { f.cfg.Screening.MinVolumeMultiple = 3.25 },
			wantValue: "≥ 3.25x",
		},
		{
			name:      "scan interval",
			label:     "Scan interval",
			mutate:    func(f *fixture) { f.cfg.Timing.ScreenerScanInterval = 30 * time.Second },
			wantValue: "30s",
		},
		{
			name:      "enrichment cap",
			label:     "Enrichment cap",
			mutate:    func(f *fixture) { f.cfg.Screening.MaxEnriched = 42 },
			wantValue: "42 symbols",
		},
		{
			name:      "position size",
			label:     "Position size",
			mutate:    func(f *fixture) { f.cfg.Risk.PositionSizePct = 4 },
			wantValue: "4% of portfolio",
		},
		{
			name:      "concurrent positions",
			label:     "Concurrent positions",
			mutate:    func(f *fixture) { f.cfg.Risk.MaxConcurrentPositions = 3 },
			wantValue: "up to 3",
		},
		{
			name:      "stop loss",
			label:     "Stop-loss",
			mutate:    func(f *fixture) { f.cfg.Risk.StopLossPct = 6.5 },
			wantValue: "−6.5% from entry",
		},
		{
			name:      "forced end-of-day offset",
			label:     "Forced end-of-day",
			mutate:    func(f *fixture) { f.cfg.Exit.EODExitOffsetMins = 45 },
			wantValue: "45 min before close",
		},
		{
			name:      "minimum price floor",
			label:     "Minimum price",
			mutate:    func(f *fixture) { f.cfg.Screening.MinPrice = 2.5 },
			wantValue: "$2.5",
		},
		{
			name:      "minimum dollar volume floor",
			label:     "Minimum traded today",
			mutate:    func(f *fixture) { f.cfg.Screening.MinDollarVolume = 2_500_000 },
			wantValue: "$2,500,000",
		},
		{
			name:      "sentiment window",
			label:     "Window after open",
			mutate:    func(f *fixture) { f.cfg.Timing.SentimentWindow = 90 * time.Minute },
			wantValue: "1h 30m",
		},
		{
			name:      "sentiment basket",
			label:     "Basket",
			mutate:    func(f *fixture) { f.cfg.Sentiment.Symbols = []string{"SPY", "DIA"} },
			wantValue: "SPY, DIA",
		},
		{
			name:      "bearish threshold",
			label:     "Halts the day when",
			mutate:    func(f *fixture) { f.cfg.Sentiment.BearishAvgPct = -1.75 },
			wantValue: "average ≤ -1.75%",
		},
		{
			name:      "same-day re-entry",
			label:     "Same-day re-entry",
			mutate:    func(f *fixture) { f.cfg.Risk.AllowSameDayReentry = true },
			wantValue: "allowed",
		},
		{
			// A tuned value must be shown exactly: fixed precision would round 10.25
			// to 10.3 and make the change look like it had not applied.
			name:      "a fractional threshold is shown exactly",
			label:     "Intraday move",
			mutate:    func(f *fixture) { f.cfg.Screening.MinIntradayPct = 10.25 },
			wantValue: "≥ 10.25%",
		},
		{
			name:      "data feed",
			label:     "Data feed",
			mutate:    func(f *fixture) { f.cfg.MarketData.Feed = "iex" },
			wantValue: "iex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.mutate(f)

			if got, _ := rowValue(t, f, tt.label); got != tt.wantValue {
				t.Errorf("%s = %q, want %q — the panel is not reading this from config",
					tt.label, got, tt.wantValue)
			}
		})
	}
}

// Derived figures must be computed the same way the engine computes them, or the panel
// and the behaviour disagree.
func TestStrategyPanelDerivesValues(t *testing.T) {
	t.Run("maximum exposure is size times concurrency", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Risk.PositionSizePct = 7
		f.cfg.Risk.MaxConcurrentPositions = 4

		if got, _ := rowValue(t, f, "Maximum exposure"); got != "28%" {
			t.Errorf("exposure = %q, want 28%% (7 × 4)", got)
		}
	})

	t.Run("relative volume names the configured lookback", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Screening.AvgVolumeLookbackDays = 50
		if _, note := rowValue(t, f, "Relative volume"); !strings.Contains(note, "50-session") {
			t.Errorf("note = %q, want it to name the 50-session lookback", note)
		}
	})

	t.Run("news catalyst names the configured lookback", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Screening.NewsLookback = 36 * time.Hour
		if _, note := rowValue(t, f, "News catalyst"); !strings.Contains(note, "36h") {
			t.Errorf("note = %q, want it to name the 36h window", note)
		}
	})
}

// Settings whose meaning is conditional must not be shown as unconditional.
func TestStrategyPanelReflectsConditionalSettings(t *testing.T) {
	t.Run("all-negative requirement changes the halt description", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Sentiment.RequireAllNegative = true
		_, note := rowValue(t, f, "Halts the day when")
		if !strings.Contains(note, "no symbol") {
			t.Errorf("note = %q, want it to state the all-negative requirement", note)
		}

		f2 := newFixture(t)
		f2.cfg.Sentiment.RequireAllNegative = false
		_, note2 := rowValue(t, f2, "Halts the day when")
		if strings.Contains(note2, "no symbol") {
			t.Errorf("note = %q, must not claim an all-negative requirement that is off", note2)
		}
	})

	t.Run("limit orders show their slippage allowance", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Execution.OrderType = "limit"
		f.cfg.Execution.LimitSlipPct = 0.75

		value, note := rowValue(t, f, "Order type")
		if value != "limit" {
			t.Errorf("order type = %q, want limit", value)
		}
		if !strings.Contains(note, "0.75%") {
			t.Errorf("note = %q, want the slippage allowance shown for a limit order", note)
		}
	})

	t.Run("market orders do not mention a slippage allowance", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Execution.OrderType = "market"
		f.cfg.Execution.LimitSlipPct = 0.75 // set but irrelevant

		_, note := rowValue(t, f, "Order type")
		if strings.Contains(note, "0.75%") || strings.Contains(note, "away from the quote") {
			t.Errorf("note = %q, must not show a limit-only setting for a market order", note)
		}
	})

	t.Run("the iex feed is flagged as not market-wide", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.MarketData.Feed = "iex"
		if _, note := rowValue(t, f, "Data feed"); !strings.Contains(note, "single exchange") {
			t.Errorf("note = %q, want the IEX caveat", note)
		}

		f2 := newFixture(t)
		f2.cfg.MarketData.Feed = "sip"
		if _, note := rowValue(t, f2, "Data feed"); !strings.Contains(note, "consolidated") {
			t.Errorf("note = %q, want sip described as the consolidated tape", note)
		}
	})
}

// The exits are listed in the order strategy.EvaluateExit checks them, because the
// first match wins — any other order would misrepresent which rule takes effect.
func TestStrategyPanelListsExitsInPriorityOrder(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/")

	want := []string{"Forced end-of-day", "Stop-loss", "Take profit"}
	prev := -1
	for _, label := range want {
		at := strings.Index(body, label)
		if at < 0 {
			t.Fatalf("exit %q missing from the page", label)
		}
		if at < prev {
			t.Errorf("%q appears out of priority order; expected %v", label, want)
		}
		prev = at
	}

	// The panel must say the order means priority, not just list them.
	if !strings.Contains(body, "priority order") {
		t.Error("the exits section should state that the order is the priority")
	}
}

// It renders at the bottom of the page, after the live state, and inside the polled
// fragment so a tuned value appears without a manual reload.
func TestStrategyPanelPlacementAndFragment(t *testing.T) {
	f := newFixture(t)

	_, page := f.get(t, "/")
	strategyAt := strings.Index(page, "Active strategy")
	positionsAt := strings.Index(page, "Open positions")
	if strategyAt < 0 {
		t.Fatal("the strategy section is missing")
	}
	if positionsAt < 0 || strategyAt < positionsAt {
		t.Error("the strategy section should sit below the live state, not above it")
	}

	_, fragment := f.get(t, "/fragment")
	if !strings.Contains(fragment, "Active strategy") {
		t.Error("the strategy section must be in the polled fragment, or a tuned value " +
			"would not appear until a manual reload")
	}
}

func TestDurationText(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0"},
		{30 * time.Second, "30s"},
		{time.Minute, "1m"},
		{10 * time.Minute, "10m"},
		{time.Hour, "1h"},
		{90 * time.Minute, "1h 30m"},
		{18 * time.Hour, "18h"},
		{36 * time.Hour, "36h"},
	}
	for _, tt := range tests {
		if got := durationText(tt.in); got != tt.want {
			t.Errorf("durationText(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Grouping must only insert separators. If it ever rounded, it would reintroduce the
// exact bug trimNumber was written to avoid: a tuned value the page reports as a
// different number.
func TestGroupNumberSeparatesWithoutRounding(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1_000_000, "1,000,000"},
		{25_000_000, "25,000,000"},
		{1234.5, "1,234.5"},
		{1_000_000.25, "1,000,000.25"},
		{-1_500_000, "-1,500,000"},
		// The precision trimNumber preserves must survive grouping.
		{2_500_000.125, "2,500,000.125"},
	}
	for _, tt := range tests {
		if got := groupNumber(tt.in); got != tt.want {
			t.Errorf("groupNumber(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
