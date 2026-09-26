package strategy

import (
	"math"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func cfg() *config.Config {
	c := &config.Config{}
	c.Risk.StopLossPct = 10
	c.Exit.ProfitTargetPct = 15
	c.Exit.TrailingStopPct = 5
	c.Exit.MACDFast = 5
	c.Exit.MACDSlow = 10
	c.Exit.MACDSignal = 3
	return c
}

func TestEMASeedsWithSimpleAverage(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	got := EMA(values, 3)

	for i := 0; i < 2; i++ {
		if !math.IsNaN(got[i]) {
			t.Errorf("EMA[%d] = %v, want NaN before warm-up", i, got[i])
		}
	}
	if want := 2.0; got[2] != want { // SMA of 1,2,3
		t.Errorf("EMA[2] = %v, want %v (SMA seed)", got[2], want)
	}
	// k = 2/(3+1) = 0.5; (4-2)*0.5+2 = 3
	if want := 3.0; got[3] != want {
		t.Errorf("EMA[3] = %v, want %v", got[3], want)
	}
	// (5-3)*0.5+3 = 4
	if want := 4.0; got[4] != want {
		t.Errorf("EMA[4] = %v, want %v", got[4], want)
	}
}

func TestEMAHandlesShortInput(t *testing.T) {
	for _, v := range EMA([]float64{1, 2}, 5) {
		if !math.IsNaN(v) {
			t.Fatalf("want all NaN when input is shorter than the period, got %v", v)
		}
	}
}

func TestMACDAlignment(t *testing.T) {
	closes := make([]float64, 20)
	for i := range closes {
		closes[i] = float64(10 + i)
	}
	macdLine, signalLine := MACD(closes, 5, 10, 3)

	// MACD is defined from slow-1; the signal line lags a further signal-1 bars.
	if math.IsNaN(macdLine[9]) {
		t.Error("MACD must be defined at index slow-1 = 9")
	}
	if !math.IsNaN(macdLine[8]) {
		t.Error("MACD must be NaN before index 9")
	}
	if math.IsNaN(signalLine[11]) {
		t.Error("signal must be defined at index 11 (9 + 3 - 1)")
	}
	if !math.IsNaN(signalLine[10]) {
		t.Error("signal must be NaN before index 11")
	}

	// On a constant-slope ramp an EMA lags the series by (period-1)/2 bars, so
	// the MACD line settles at slope * ((slow-1)/2 - (fast-1)/2) = 1 * (4.5-2) =
	// 2.5, and the signal line (an EMA of that constant) converges to the same
	// value. Equality here is the correct outcome, not a missing trend.
	const wantMACD = 2.5
	if math.Abs(macdLine[19]-wantMACD) > 1e-9 {
		t.Errorf("macd[19] = %.9f, want %.1f on a unit-slope ramp", macdLine[19], wantMACD)
	}
	if math.Abs(signalLine[19]-macdLine[19]) > 1e-6 {
		t.Errorf("signal %.9f should converge to the constant macd %.9f", signalLine[19], macdLine[19])
	}
}

func TestMACDBearishCross(t *testing.T) {
	rising := make([]float64, 13)
	for i := range rising {
		rising[i] = float64(100 + i*2)
	}

	// The crossing happens on the first bar that turns down, so that bar must be
	// the last one for the check to see it.
	reversal := append(append([]float64{}, rising...), 118)
	// Several bars into the decline the lines have already crossed; the trigger
	// must not keep reporting it.
	afterReversal := append(append([]float64{}, rising...), 118, 110, 100, 88)

	tests := []struct {
		name   string
		closes []float64
		want   bool
	}{
		{"not enough bars to judge", rising[:12], false},
		{"exactly warm-up length, still rising", rising, false},
		{"first bar of a sharp reversal", reversal, true},
		{"several bars into the decline", afterReversal, false},
		{"empty input", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MACDBearishCross(tt.closes, 5, 10, 3); got != tt.want {
				t.Errorf("MACDBearishCross = %v, want %v", got, tt.want)
			}
		})
	}
}

// The trigger must fire only on the bar where the lines actually cross, not on
// every bar afterwards, so a position is not re-flagged repeatedly.
func TestMACDBearishCrossFiresOnceAtTheCrossing(t *testing.T) {
	closes := []float64{100, 102, 104, 106, 108, 110, 112, 114, 116, 118, 120, 122, 124}
	falling := append([]float64{}, closes...)
	var crossings int
	for _, p := range []float64{118, 112, 104, 96, 90, 86} {
		falling = append(falling, p)
		if MACDBearishCross(falling, 5, 10, 3) {
			crossings++
		}
	}
	if crossings != 1 {
		t.Errorf("got %d crossing reports during one sustained decline, want exactly 1", crossings)
	}
}

func TestEvaluateExitPriority(t *testing.T) {
	base := domain.Position{Symbol: "ABCD", Shares: 10, EntryPrice: 100, PeakPrice: 100}

	// 13 rising bars: enough to compute MACD, but no bearish cross.
	rising := make([]float64, 13)
	for i := range rising {
		rising[i] = float64(100 + i)
	}
	// Ends on the bar where the MACD line crosses below its signal line.
	reversing := append(append([]float64{}, rising...), 104)

	tests := []struct {
		name       string
		in         ExitInput
		wantExit   bool
		wantReason domain.ExitReason
	}{
		{
			name:       "forced EOD beats everything",
			in:         ExitInput{Position: base, Price: 130, Closes: reversing, EODReached: true},
			wantExit:   true,
			wantReason: domain.ExitForcedEOD,
		},
		{
			name:       "stop loss at exactly -10%",
			in:         ExitInput{Position: base, Price: 90, Closes: rising},
			wantExit:   true,
			wantReason: domain.ExitStopLoss,
		},
		{
			name:     "just above the stop stays open",
			in:       ExitInput{Position: base, Price: 90.01, Closes: rising},
			wantExit: false,
		},
		{
			name: "stop loss takes precedence over a bearish MACD cross",
			in: ExitInput{
				Position: base, Price: 85, Closes: reversing,
			},
			wantExit:   true,
			wantReason: domain.ExitStopLoss,
		},
		{
			name:       "MACD cross exits a position that is otherwise fine",
			in:         ExitInput{Position: base, Price: 101, Closes: reversing},
			wantExit:   true,
			wantReason: domain.ExitMACDCrossover,
		},
		{
			name: "trailing stop does not arm below the profit target",
			in: ExitInput{
				// Peaked at +14%, never reached the +15% target, then fell 5% off
				// that peak. The trailing stop must stay disarmed.
				Position: domain.Position{EntryPrice: 100, PeakPrice: 114},
				Price:    108,
				Closes:   rising,
			},
			wantExit: false,
		},
		{
			name: "trailing stop fires once armed",
			in: ExitInput{
				// Peaked at +20% (target reached), now 5% below that peak.
				Position: domain.Position{EntryPrice: 100, PeakPrice: 120},
				Price:    114,
				Closes:   rising,
			},
			wantExit:   true,
			wantReason: domain.ExitTrailingStop,
		},
		{
			name: "armed trailing stop stays armed after dipping below target",
			in: ExitInput{
				Position: domain.Position{EntryPrice: 100, PeakPrice: 116, TrailArmed: true},
				Price:    110,
				Closes:   rising,
			},
			wantExit:   true,
			wantReason: domain.ExitTrailingStop,
		},
		{
			name:     "no trigger with too few bars for MACD",
			in:       ExitInput{Position: base, Price: 105, Closes: rising[:5]},
			wantExit: false,
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
	rising := make([]float64, 13)
	for i := range rising {
		rising[i] = float64(100 + i)
	}

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
			got := EvaluateExit(ExitInput{Position: tt.pos, Price: tt.price, Closes: rising}, c)
			if !got.Exit || got.Reason != tt.wantReason {
				t.Errorf("EvaluateExit = %+v, want exit via %q", got, tt.wantReason)
			}
		})
	}

	// The tolerance must not swallow a genuine move: a cent above the stop stays
	// open.
	got := EvaluateExit(ExitInput{
		Position: domain.Position{EntryPrice: 4.20, PeakPrice: 4.20}, Price: 3.79, Closes: rising,
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
