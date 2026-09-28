package strategy

import (
	"testing"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func cfg() *config.Config {
	c := &config.Config{}
	c.Risk.StopLossPct = 10
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
			// The removed trailing stop would have closed this at +14%. Nothing may
			// now take a winner off the table before the bell: the strategy's whole
			// return is in the right tail, and a rule that trims it was measured
			// turning a positive per-trade mean negative.
			name: "a position well off its peak is still held",
			in: ExitInput{
				Position: domain.Position{EntryPrice: 100, PeakPrice: 160},
				Price:    120,
			},
			wantExit: false,
		},
		{
			name: "a large gain is held to the close",
			in: ExitInput{
				Position: domain.Position{EntryPrice: 100, PeakPrice: 250},
				Price:    250,
			},
			wantExit: false,
		},
		{
			// Only the stop-loss reads the peak-independent floor, so a position
			// that peaked high and then collapsed past the stop still stops out.
			name: "a collapse past the stop still exits on the stop",
			in: ExitInput{
				Position: domain.Position{EntryPrice: 100, PeakPrice: 200},
				Price:    89,
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
			name:       "exactly 10% below a 4.20 entry",
			pos:        domain.Position{EntryPrice: 4.20, PeakPrice: 4.20},
			price:      3.78,
			wantReason: domain.ExitStopLoss,
		},
		{
			// 1.00 * 0.9 is 0.9000000000000001 in binary, so an exact 0.90 reads as
			// above the stop without the epsilon.
			name:       "exactly 10% below a 1.00 entry",
			pos:        domain.Position{EntryPrice: 1.00, PeakPrice: 1.00},
			price:      0.90,
			wantReason: domain.ExitStopLoss,
		},
		{
			name:       "exactly 10% below a 33.33 entry",
			pos:        domain.Position{EntryPrice: 33.33, PeakPrice: 40.00},
			price:      29.997,
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
		Position: domain.Position{EntryPrice: 4.20, PeakPrice: 4.20}, Price: 3.79,
	}, c)
	if got.Exit {
		t.Errorf("a price above the stop must not exit: %+v", got)
	}
}
