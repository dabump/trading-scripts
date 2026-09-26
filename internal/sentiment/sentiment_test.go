package sentiment

import (
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func cfg() *config.Config {
	c := &config.Config{}
	c.Sentiment.Symbols = []string{"SPY", "QQQ", "IWM"}
	c.Sentiment.BearishAvgPct = -0.8
	c.Sentiment.RequireAllNegative = true
	return c
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		pcts map[string]float64
		all  bool
		want domain.Verdict
	}{
		{
			name: "broad selloff is bearish",
			pcts: map[string]float64{"SPY": -1.2, "QQQ": -1.5, "IWM": -1.8},
			all:  true,
			want: domain.VerdictBearish,
		},
		{
			name: "exactly at the threshold is bearish",
			pcts: map[string]float64{"SPY": -0.8, "QQQ": -0.8, "IWM": -0.8},
			all:  true,
			want: domain.VerdictBearish,
		},
		{
			name: "just above the threshold proceeds",
			pcts: map[string]float64{"SPY": -0.7, "QQQ": -0.7, "IWM": -0.7},
			all:  true,
			want: domain.VerdictProceed,
		},
		{
			name: "rally proceeds",
			pcts: map[string]float64{"SPY": 0.9, "QQQ": 1.4, "IWM": 1.1},
			all:  true,
			want: domain.VerdictProceed,
		},
		{
			name: "neutral/mixed proceeds rather than halting",
			pcts: map[string]float64{"SPY": 0.1, "QQQ": -0.2, "IWM": 0.0},
			all:  true,
			want: domain.VerdictProceed,
		},
		{
			name: "deep average but one index green does not halt when all-negative is required",
			// Average is -0.87, past the threshold, but IWM is up.
			pcts: map[string]float64{"SPY": -1.5, "QQQ": -1.4, "IWM": 0.3},
			all:  true,
			want: domain.VerdictProceed,
		},
		{
			name: "same data halts when all-negative is not required",
			pcts: map[string]float64{"SPY": -1.5, "QQQ": -1.4, "IWM": 0.3},
			all:  false,
			want: domain.VerdictBearish,
		},
		{
			name: "no data stays pending",
			pcts: map[string]float64{},
			all:  true,
			want: domain.VerdictPending,
		},
		{
			name: "only unrelated symbols stays pending",
			pcts: map[string]float64{"TSLA": -5},
			all:  true,
			want: domain.VerdictPending,
		},
		{
			name: "partial data still classifies on what arrived",
			pcts: map[string]float64{"SPY": -2.0},
			all:  true,
			want: domain.VerdictBearish,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cfg()
			c.Sentiment.RequireAllNegative = tt.all
			if got := Classify(tt.pcts, c); got != tt.want {
				t.Errorf("Classify = %q, want %q", got, tt.want)
			}
		})
	}
}

// The gate uses the reading at the end of the hour, so an intraday dip that
// recovered must not halt the session.
func TestGateVerdictUsesFinalReading(t *testing.T) {
	c := cfg()
	base := time.Date(2026, 9, 28, 9, 40, 0, 0, time.UTC)
	readings := []domain.SentimentReading{
		{TakenAt: base, Percentages: map[string]float64{"SPY": -2.0, "QQQ": -2.2, "IWM": -2.4}},
		{TakenAt: base.Add(20 * time.Minute), Percentages: map[string]float64{"SPY": -1.0, "QQQ": -1.1, "IWM": -1.2}},
		{TakenAt: base.Add(50 * time.Minute), Percentages: map[string]float64{"SPY": 0.4, "QQQ": 0.6, "IWM": 0.2}},
	}
	if got := GateVerdict(readings, c); got != domain.VerdictProceed {
		t.Errorf("GateVerdict = %q, want PROCEED: the recovery reading decides", got)
	}

	// And the reverse: a tape that deteriorated into the gate must halt.
	readings[2].Percentages = map[string]float64{"SPY": -1.4, "QQQ": -1.6, "IWM": -1.9}
	if got := GateVerdict(readings, c); got != domain.VerdictBearish {
		t.Errorf("GateVerdict = %q, want OVERWHELMINGLY_BEARISH", got)
	}

	if got := GateVerdict(nil, c); got != domain.VerdictPending {
		t.Errorf("GateVerdict(nil) = %q, want PENDING", got)
	}
}

func TestPercentChange(t *testing.T) {
	if got, ok := PercentChange(110, 100); !ok || got != 10 {
		t.Errorf("PercentChange(110,100) = %v,%v want 10,true", got, ok)
	}
	if got, ok := PercentChange(90, 100); !ok || got != -10 {
		t.Errorf("PercentChange(90,100) = %v,%v want -10,true", got, ok)
	}
	if _, ok := PercentChange(100, 0); ok {
		t.Error("a zero previous close must not produce a percentage")
	}
}
