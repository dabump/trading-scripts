package strategy

import (
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func cfg() *config.Config {
	c := &config.Config{}
	c.Risk.StopLossPct = 10
	c.Exit.FirstTargetR = 2
	c.Exit.FirstTargetFraction = 0.5
	c.Exit.BreakevenAfterTarget = true
	return c
}

// pos builds an open position with a chart stop, which is now the normal shape: entry
// 100, stop 96, so initial risk is 4 a share and 1R is 4 of price.
func pos(shares int) domain.Position {
	return domain.Position{
		Symbol: "ABCD", Shares: shares, SharesOpen: shares,
		EntryPrice: 100, PeakPrice: 100, StopPrice: 96, InitialRisk: 4,
	}
}

func TestEvaluateExitPriority(t *testing.T) {
	base := pos(10)

	tests := []struct {
		name       string
		in         ExitInput
		wantExit   bool
		wantReason domain.ExitReason
	}{
		{
			name:       "forced EOD beats everything",
			in:         ExitInput{Position: base, Price: 130, EODReached: true},
			wantExit:   true,
			wantReason: domain.ExitForcedEOD,
		},
		{
			// The chart stop, not the percentage backstop: 96 is where the setup put
			// it, and it is what fires.
			name:       "the chart stop at exactly its price",
			in:         ExitInput{Position: base, Price: 96},
			wantExit:   true,
			wantReason: domain.ExitStopLoss,
		},
		{
			name:     "a cent above the chart stop stays open",
			in:       ExitInput{Position: base, Price: 96.01},
			wantExit: false,
		},
		{
			// With no chart stop — a position opened by an older version, or adopted
			// during reconciliation — the percentage backstop is the only floor.
			name: "the percentage backstop catches a position with no chart stop",
			in: ExitInput{
				Position: domain.Position{Shares: 10, SharesOpen: 10, EntryPrice: 100,
					PeakPrice: 100},
				Price: 90,
			},
			wantExit:   true,
			wantReason: domain.ExitStopLoss,
		},
		{
			// The removed trailing stop would have closed this. Nothing may now take
			// a whole winner off the table before the bell: the strategy's return is
			// in the right tail, and a rule that trims it was measured turning a
			// positive per-trade mean negative.
			name: "a runner well off its peak is still held",
			in: ExitInput{
				Position: func() domain.Position {
					p := pos(10)
					p.PeakPrice, p.TargetHit, p.SharesOpen = 160, true, 5
					p.StopPrice = 100 // moved to breakeven after the target
					return p
				}(),
				Price: 120,
			},
			wantExit: false,
		},
		{
			// The breakeven stop is a stop like any other: it fires.
			name: "a runner falling back to breakeven stops out",
			in: ExitInput{
				Position: func() domain.Position {
					p := pos(10)
					p.PeakPrice, p.TargetHit, p.SharesOpen = 160, true, 5
					p.StopPrice = 100
					return p
				}(),
				Price: 100,
			},
			wantExit:   true,
			wantReason: domain.ExitStopLoss,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateExit(tt.in, cfg())
			if got.Exit != tt.wantExit {
				t.Fatalf("Exit = %v, want %v (reason %q)", got.Exit, tt.wantExit, got.Reason)
			}
			if tt.wantExit && got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

// Thresholds are computed products like peak*0.95, which binary floating point
// cannot represent exactly: 6.00*0.95 is 5.699999999999999. A price sitting
// exactly on a documented threshold must still trigger.
func TestExitThresholdsTriggerAtExactBoundaries(t *testing.T) {
	c := cfg()

	tests := []struct {
		name       string
		pos        domain.Position
		price      float64
		wantReason domain.ExitReason
	}{
		{
			// The backstop path: no chart stop, so entry*0.9 is the floor. 4.20*0.9
			// evaluates to 3.7800000000000002, so an exact 3.78 reads as above it
			// without the epsilon.
			name:       "exactly 10% below a 4.20 entry",
			pos:        domain.Position{Shares: 1, SharesOpen: 1, EntryPrice: 4.20, PeakPrice: 4.20},
			price:      3.78,
			wantReason: domain.ExitStopLoss,
		},
		{
			name:       "exactly 10% below a 1.00 entry",
			pos:        domain.Position{Shares: 1, SharesOpen: 1, EntryPrice: 1.00, PeakPrice: 1.00},
			price:      0.90,
			wantReason: domain.ExitStopLoss,
		},
		{
			// The chart-stop path at its exact price, where the stop is a product of
			// a percentage buffer and just as unrepresentable.
			name: "exactly on a chart stop derived from a buffer",
			pos: domain.Position{Shares: 1, SharesOpen: 1, EntryPrice: 10.00,
				PeakPrice: 10.00, StopPrice: 9.87 * (1 - 0.1/100), InitialRisk: 0.14},
			price:      9.87 * (1 - 0.1/100),
			wantReason: domain.ExitStopLoss,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateExit(ExitInput{Position: tt.pos, Price: tt.price}, c)
			if !got.Exit || got.Reason != tt.wantReason {
				t.Errorf("EvaluateExit = %+v, want exit via %q", got, tt.wantReason)
			}
		})
	}

	// The tolerance must not swallow a genuine move: a cent above the stop stays
	// open.
	got := EvaluateExit(ExitInput{
		Position: domain.Position{Shares: 1, SharesOpen: 1, EntryPrice: 4.20, PeakPrice: 4.20},
		Price:    3.79,
	}, c)
	if got.Exit {
		t.Errorf("a price above the stop must not exit: %+v", got)
	}
}

// Scaling out is the rule that replaced the removed profit target, and the thing that
// makes it different is that it leaves a runner. A rule that sold everything at the
// target would be the old mistake wearing a new name.
func TestScaleOutAtTheFirstTarget(t *testing.T) {
	c := cfg() // 2R target, sell half, then breakeven

	t.Run("half is sold at 2R and the rest keeps running", func(t *testing.T) {
		// Entry 100, risk 4 a share, so 2R is 108.
		got := EvaluateExit(ExitInput{Position: pos(100), Price: 108}, c)
		if got.Exit {
			t.Fatal("the target must not close the position")
		}
		if !got.Scale {
			t.Fatal("the target must scale out")
		}
		if got.ScaleShares != 50 {
			t.Errorf("scaled %d shares, want 50 of 100", got.ScaleShares)
		}
		if got.NewStop != 100 {
			t.Errorf("new stop = %v, want the 100 entry price", got.NewStop)
		}
	})

	t.Run("a cent below the target does nothing", func(t *testing.T) {
		if got := EvaluateExit(ExitInput{Position: pos(100), Price: 107.99}, c); got.Scale {
			t.Error("scaled out below the target")
		}
	})

	t.Run("the target only pays once", func(t *testing.T) {
		p := pos(100)
		p.TargetHit, p.SharesOpen = true, 50
		if got := EvaluateExit(ExitInput{Position: p, Price: 130}, c); got.Scale {
			t.Error("a latched target must not scale out again")
		}
	})

	t.Run("the stop wins on a price that is both", func(t *testing.T) {
		// Cannot happen from one price, but the caller passes the bar's low and high
		// separately and the stop has to take priority when both are true.
		p := pos(100)
		if got := EvaluateExit(ExitInput{Position: p, Price: 96}, c); !got.Exit || got.Scale {
			t.Errorf("got %+v, want the stop to close it", got)
		}
	})

	t.Run("the forced exit outranks the target", func(t *testing.T) {
		got := EvaluateExit(ExitInput{Position: pos(100), Price: 130, EODReached: true}, c)
		if !got.Exit || got.Reason != domain.ExitForcedEOD || got.Scale {
			t.Errorf("got %+v, want a forced close", got)
		}
	})

	t.Run("breakeven can be turned off", func(t *testing.T) {
		noBE := cfg()
		noBE.Exit.BreakevenAfterTarget = false
		if got := EvaluateExit(ExitInput{Position: pos(100), Price: 108}, noBE); got.NewStop != 0 {
			t.Errorf("new stop = %v, want none when breakeven is disabled", got.NewStop)
		}
	})

	t.Run("a position with no initial risk cannot have a target", func(t *testing.T) {
		p := pos(100)
		p.InitialRisk = 0
		if got := EvaluateExit(ExitInput{Position: p, Price: 500}, c); got.Scale {
			t.Error("without an initial risk there is no R to measure a target in")
		}
	})
}

// A position too small to split must run rather than being closed at the target,
// because closing it whole is exactly the truncation the strategy cannot afford.
func TestScaleOutOnTinyPositions(t *testing.T) {
	c := cfg()

	t.Run("a single share is never scaled or closed at the target", func(t *testing.T) {
		got := EvaluateExit(ExitInput{Position: pos(1), Price: 200}, c)
		if got.Scale || got.Exit {
			t.Errorf("got %+v, want it left alone to run", got)
		}
	})

	t.Run("two shares sell one and keep one", func(t *testing.T) {
		got := EvaluateExit(ExitInput{Position: pos(2), Price: 108}, c)
		if !got.Scale || got.ScaleShares != 1 {
			t.Errorf("got %+v, want one of two shares sold", got)
		}
	})

	t.Run("a fraction that rounds to everything still leaves one share", func(t *testing.T) {
		greedy := cfg()
		greedy.Exit.FirstTargetFraction = 0.99
		got := EvaluateExit(ExitInput{Position: pos(3), Price: 108}, greedy)
		if !got.Scale || got.ScaleShares != 2 {
			t.Errorf("got %+v, want 2 of 3 sold so a runner survives", got)
		}
	})
}

func TestCandleTrailStop(t *testing.T) {
	entry := time.Date(2026, 9, 28, 10, 35, 20, 0, time.UTC)
	candle := func(mm int, low float64) domain.Bar {
		return domain.Bar{Time: time.Date(2026, 9, 28, 10, mm, 0, 0, time.UTC), Low: low}
	}
	cfgFor := func(mode string) *config.Config {
		c := &config.Config{}
		c.Entry.PatternInterval = time.Minute
		c.Exit.CandleTrail = mode
		return c
	}
	pos := domain.Position{EntryTime: entry}
	scaled := domain.Position{EntryTime: entry, TargetHit: true}

	cases := []struct {
		name string
		mode string
		pos  domain.Position
		bar  domain.Bar
		want float64
	}{
		{"always trails from the entry candle", config.CandleTrailAlways, pos, candle(35, 4.97), 4.97},
		{"a candle closed before entry is not held through", config.CandleTrailAlways, pos, candle(34, 4.90), 0},
		{"after_target waits for the target", config.CandleTrailAfterTarget, pos, candle(36, 5.02), 0},
		{"after_target trails the runner", config.CandleTrailAfterTarget, scaled, candle(36, 5.02), 5.02},
		{"off never trails", config.CandleTrailOff, scaled, candle(36, 5.02), 0},
		{"empty means off", "", scaled, candle(36, 5.02), 0},
	}
	for _, tc := range cases {
		if got := CandleTrailStop(tc.pos, tc.bar, cfgFor(tc.mode)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLastCompletedBarSkipsTheFormingCandle(t *testing.T) {
	at := func(mm, ss int) time.Time { return time.Date(2026, 9, 28, 10, mm, ss, 0, time.UTC) }
	bars := []domain.Bar{{Time: at(35, 0), Low: 1}, {Time: at(36, 0), Low: 2}, {Time: at(37, 0), Low: 3}}

	if b, ok := LastCompletedBar(bars, at(37, 10), time.Minute); !ok || b.Low != 2 {
		t.Errorf("at 10:37:10 got %+v ok=%v, want the 10:36 candle", b, ok)
	}
	if b, ok := LastCompletedBar(bars, at(38, 0), time.Minute); !ok || b.Low != 3 {
		t.Errorf("at 10:38:00 got %+v ok=%v, want the 10:37 candle, which has just closed", b, ok)
	}
	if _, ok := LastCompletedBar(bars, at(35, 30), time.Minute); ok {
		t.Error("at 10:35:30 no candle has closed yet")
	}
}
