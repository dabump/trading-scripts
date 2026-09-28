package strategy

import (
	"testing"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func cfg() *config.Config {
	c := &config.Config{}
	c.Risk.StopLossPct = 10
	c.Exit.ProfitTargetPct = 15
	c.Exit.TrailingStopPct = 5
	return c
}

func TestEvaluateExitPriority(t *testing.T) {
	base := domain.Position{Symbol: "ABCD", Shares: 10, EntryPrice: 100, PeakPrice: 100}

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
			name:       "stop loss at exactly -10%",
			in:         ExitInput{Position: base, Price: 90},
			wantExit:   true,
			wantReason: domain.ExitStopLoss,
		},
		{
			name:     "just above the stop stays open",
			in:       ExitInput{Position: base, Price: 90.01},
			wantExit: false,
		},
		{
			name: "trailing stop does not arm below the profit target",
			in: ExitInput{
				// Peaked at +14%, never reached the +15% target, then fell 5% off
				// that peak. The trailing stop must stay disarmed.
				Position: domain.Position{EntryPrice: 100, PeakPrice: 114},
				Price:    108,
			},
			wantExit: false,
		},
		{
			name: "trailing stop fires once armed",
			in: ExitInput{
				// Peaked at +20% (target reached), now 5% below that peak.
				Position: domain.Position{EntryPrice: 100, PeakPrice: 120},
				Price:    114,
			},
			wantExit:   true,
			wantReason: domain.ExitTrailingStop,
		},
		{
			name: "armed trailing stop stays armed after dipping below target",
			in: ExitInput{
				Position: domain.Position{EntryPrice: 100, PeakPrice: 116, TrailArmed: true},
				Price:    110,
			},
			wantExit:   true,
			wantReason: domain.ExitTrailingStop,
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
			name:       "exactly 5% below a 6.00 peak",
			pos:        domain.Position{EntryPrice: 5.00, PeakPrice: 6.00, TrailArmed: true},
			price:      5.70,
			wantReason: domain.ExitTrailingStop,
		},
		{
			name:       "exactly 10% below a 4.20 entry",
			pos:        domain.Position{EntryPrice: 4.20, PeakPrice: 4.20},
			price:      3.78,
			wantReason: domain.ExitStopLoss,
		},
		{
			name:       "exactly 5% below a 1.13 peak",
			pos:        domain.Position{EntryPrice: 1.00, PeakPrice: 1.13, TrailArmed: true},
			price:      1.0735,
			wantReason: domain.ExitTrailingStop,
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
		Position: domain.Position{EntryPrice: 4.20, PeakPrice: 4.20}, Price: 3.79,
	}, c)
	if got.Exit {
		t.Errorf("a price above the stop must not exit: %+v", got)
	}
}

// The profit target must also arm at its exact boundary.
func TestTrailArmsAtExactTarget(t *testing.T) {
	c := cfg()
	// 4.20 * 1.15 = 4.829999999999999 in binary.
	p := domain.Position{EntryPrice: 4.20, PeakPrice: 4.83}
	if !TrailArmed(p, c) {
		t.Error("a peak exactly at the profit target must arm the trailing stop")
	}
}

func TestTrailArmed(t *testing.T) {
	c := cfg()
	tests := []struct {
		name string
		pos  domain.Position
		want bool
	}{
		{"below target", domain.Position{EntryPrice: 100, PeakPrice: 114}, false},
		{"exactly at target", domain.Position{EntryPrice: 100, PeakPrice: 115}, true},
		{"above target", domain.Position{EntryPrice: 100, PeakPrice: 130}, true},
		{"already latched", domain.Position{EntryPrice: 100, PeakPrice: 101, TrailArmed: true}, true},
		{"peak below entry", domain.Position{EntryPrice: 100, PeakPrice: 90}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TrailArmed(tt.pos, c); got != tt.want {
				t.Errorf("TrailArmed = %v, want %v", got, tt.want)
			}
		})
	}
}
