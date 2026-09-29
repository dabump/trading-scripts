package strategy

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func setupCfg() *config.Config {
	c := &config.Config{}
	c.Entry = config.Entry{
		PatternInterval:    time.Minute,
		EMAPeriod:          9,
		RequireAboveVWAP:   false,
		MaxPullbackBars:    2,
		SurgeBars:          3,
		MinSurgePct:        2,
		MaxRetracePct:      50,
		StopBufferPct:      0.1,
		MinStopDistancePct: 0.5,
		MaxStopDistancePct: 4,
	}
	return c
}

var barStart = time.Date(2026, 9, 28, 9, 30, 0, 0, time.UTC)

// ramp builds a rising sequence long enough for the EMA to warm up, so the trend
// filter is satisfied and the test is about the pattern rather than the warm-up.
func ramp(n int, from, to float64) []domain.Bar {
	out := make([]domain.Bar, n)
	step := (to - from) / float64(n-1)
	for i := 0; i < n; i++ {
		c := from + step*float64(i)
		out[i] = domain.Bar{
			Time: barStart.Add(time.Duration(i) * time.Minute),
			Open: c - step/2, High: c + step/4, Low: c - step, Close: c, Volume: 1000,
		}
	}
	return out
}

func appendBar(bars []domain.Bar, o, h, l, c float64) []domain.Bar {
	return append(bars, domain.Bar{
		Time: barStart.Add(time.Duration(len(bars)) * time.Minute),
		Open: o, High: h, Low: l, Close: c, Volume: 1000,
	})
}

// surgeThenPause is a steady climb to ~10.02 followed by one candle that ticks a
// marginal new high and closes red — the candle the flag detector reads as a new pole.
func surgeThenPause() []domain.Bar {
	bars := ramp(12, 9.00, 10.00)
	return appendBar(bars, 10.00, 10.05, 9.95, 9.96)
}

// The pause candle here ticks a marginal new high before closing red. That is the
// candle the old flag detector read as a new pole — the KNRX refusal in
// docs/decisions.md — and it has to count as a pause.
func TestFindSetupTriggersAboveThePauseCandlesHigh(t *testing.T) {
	cfg := setupCfg()
	bars := appendBar(surgeThenPause(), 9.97, 10.10, 9.96, 10.08)

	s := FindSetup(bars, cfg)
	if !s.Triggered {
		t.Fatalf("expected a micro pullback, got: %s", s.Reason)
	}
	if s.Entry != 10.08 {
		t.Errorf("entry = %v, want the trigger close 10.08", s.Entry)
	}
	if want := 9.95 * (1 - cfg.Entry.StopBufferPct/100); math.Abs(s.Stop-want) > 1e-9 {
		t.Errorf("stop = %v, want just under the pause low (%v)", s.Stop, want)
	}
	if s.PauseBars != 1 || s.PauseHigh != 10.05 {
		t.Errorf("recorded %d bars reclaiming %.2f, want 1 bar reclaiming 10.05", s.PauseBars, s.PauseHigh)
	}
}

func TestFindSetupRefusals(t *testing.T) {
	cases := []struct {
		name   string
		bars   func() []domain.Bar
		cfg    func(*config.Config)
		reason string
	}{
		{
			name: "trigger has not cleared the pause high",
			// Traded through 10.05 intrabar but closed back under it.
			bars:   func() []domain.Bar { return appendBar(surgeThenPause(), 9.97, 10.10, 9.96, 10.04) },
			reason: "has not cleared",
		},
		{
			name: "pause longer than a micro pullback",
			bars: func() []domain.Bar {
				b := surgeThenPause()
				b = appendBar(b, 9.96, 10.00, 9.94, 9.97)
				b = appendBar(b, 9.97, 9.99, 9.93, 9.95)
				return appendBar(b, 9.96, 10.10, 9.95, 10.08)
			},
			reason: "more than the 2",
		},
		{
			name:   "surge too small",
			bars:   func() []domain.Bar { return appendBar(surgeThenPause(), 9.97, 10.10, 9.96, 10.08) },
			cfg:    func(c *config.Config) { c.Entry.MinSurgePct = 10 },
			reason: "surge is",
		},
		{
			name: "pause gives back too much of the surge",
			bars: func() []domain.Bar {
				b := ramp(12, 9.00, 10.00)
				b = appendBar(b, 10.00, 10.05, 9.72, 9.80)
				return appendBar(b, 9.81, 10.10, 9.80, 10.08)
			},
			reason: "gave back",
		},
		{
			name: "surge is not at the high of day",
			bars: func() []domain.Bar {
				b := ramp(12, 9.00, 10.00)
				// An earlier spike to 11 means the pause is not at the high of day.
				b[2].High = 11
				b = appendBar(b, 10.00, 10.05, 9.95, 9.96)
				return appendBar(b, 9.97, 10.10, 9.96, 10.08)
			},
			reason: "high of day",
		},
		{
			name:   "pause volume not lighter than the surge",
			bars:   func() []domain.Bar { return appendBar(surgeThenPause(), 9.97, 10.10, 9.96, 10.08) },
			cfg:    func(c *config.Config) { c.Entry.RequireVolumeDecline = true },
			reason: "volume is not lighter",
		},
		{
			name: "no pause at all",
			bars: func() []domain.Bar {
				return appendBar(ramp(12, 9.00, 10.00), 10.00, 10.10, 9.99, 10.08)
			},
			reason: "no pullback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setupCfg()
			if tc.cfg != nil {
				tc.cfg(cfg)
			}
			s := FindSetup(tc.bars(), cfg)
			if s.Triggered {
				t.Fatalf("triggered, want a refusal containing %q", tc.reason)
			}
			if !strings.Contains(s.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to contain %q", s.Reason, tc.reason)
			}
		})
	}
}

func TestFindSetupVolumeDeclinePasses(t *testing.T) {
	cfg := setupCfg()
	cfg.Entry.RequireVolumeDecline = true
	bars := surgeThenPause()
	bars[len(bars)-1].Volume = 300
	bars = appendBar(bars, 9.97, 10.10, 9.96, 10.08)
	if s := FindSetup(bars, cfg); !s.Triggered {
		t.Fatalf("a light pause should pass: %s", s.Reason)
	}
}

// Armed reads the same pattern a candle earlier: with the pause as the latest bar, a
// buy-stop sits at its high.
func TestArmMicroPullbackGivesTheBuyStop(t *testing.T) {
	cfg := setupCfg()
	s := ArmMicroPullback(surgeThenPause(), cfg)
	if !s.Triggered {
		t.Fatalf("expected an armed setup: %s", s.Reason)
	}
	if s.BuyStop != 10.05 || s.Entry != 10.05 {
		t.Errorf("buy stop %v / entry %v, want both at the pause high 10.05", s.BuyStop, s.Entry)
	}
	if s.Stop >= 9.95 {
		t.Errorf("stop %v should sit under the pause low 9.95", s.Stop)
	}

	// Once the next candle breaks out, it is no longer a pause, so nothing is armed.
	if s := ArmMicroPullback(appendBar(surgeThenPause(), 9.97, 10.10, 9.96, 10.08), cfg); s.Triggered {
		t.Error("armed on a breakout candle")
	}
}

func TestMACD(t *testing.T) {
	if _, _, ok := MACD(make([]float64, 30)); ok {
		t.Error("30 closes cannot give a signal line; want ok=false")
	}
	// An accelerating rise pulls the MACD line ahead of its own average.
	closes := make([]float64, 60)
	for i := range closes {
		closes[i] = 10 * math.Pow(1.01, float64(i))
	}
	line, signal, ok := MACD(closes)
	if !ok || line <= 0 || line <= signal {
		t.Errorf("accelerating rise: line %v signal %v ok %v, want line > signal > 0", line, signal, ok)
	}
}

func TestFindSetupMACDFilter(t *testing.T) {
	cfg := setupCfg()
	cfg.Entry.RequireMACD = true
	// A long rise that has flattened out: MACD has rolled under its signal line
	// while price still sits above the 9-EMA.
	var bars []domain.Bar
	for i := 0; i < 40; i++ {
		c := 5 + 5*math.Pow(float64(i)/39, 0.3)
		bars = appendBar(bars, c-0.01, c+0.01, c-0.02, c)
	}
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	if line, signal, _ := MACD(closes); line > signal {
		t.Skipf("fixture does not roll the MACD over (line %v > signal %v)", line, signal)
	}
	if s := FindSetup(bars, cfg); s.Triggered || !strings.Contains(s.Reason, "MACD") {
		t.Errorf("triggered=%v reason=%q, want a MACD refusal", s.Triggered, s.Reason)
	}
}

// A spike that faded: the trigger bar prints a long upper wick on heavy volume, so
// its own typical price drags VWAP above its close. The EMA reads closes only, so it
// is untouched — which is what isolates the VWAP filter.
func TestFindSetupEnforcesVWAPWhenConfigured(t *testing.T) {
	bars := surgeThenPause()
	for i := range bars {
		bars[i].Volume = 1
	}
	bars = appendBar(bars, 9.97, 12.00, 9.96, 10.08)
	bars[len(bars)-1].Volume = 5_000_000

	withVWAP := setupCfg()
	withVWAP.Entry.RequireAboveVWAP = true
	if got := FindSetup(bars, withVWAP); got.Triggered || !strings.Contains(got.Reason, "VWAP") {
		t.Errorf("triggered=%v reason=%q, want a VWAP refusal", got.Triggered, got.Reason)
	}
	// The same chart passes with the filter off, which proves the filter is what
	// rejected it rather than some other condition.
	if got := FindSetup(bars, setupCfg()); !got.Triggered {
		t.Errorf("without the VWAP filter this chart should trigger: %s", got.Reason)
	}
}

// A stop sitting implausibly close to entry is widened rather than accepted, because
// ordinary noise would otherwise take the position out immediately.
func TestFindSetupWidensAnImplausiblyTightStop(t *testing.T) {
	cfg := setupCfg()
	bars := ramp(12, 9.00, 10.00)
	// The pause barely dips: its low is a fraction of a percent below the trigger.
	bars = appendBar(bars, 10.02, 10.03, 10.015, 10.016)
	bars = appendBar(bars, 10.02, 10.05, 10.018, 10.04)

	got := FindSetup(bars, cfg)
	if !got.Triggered {
		t.Fatalf("expected a setup: %s", got.Reason)
	}
	if math.Abs(got.StopDistancePct-cfg.Entry.MinStopDistancePct) > 1e-9 {
		t.Errorf("stop distance = %.4f%%, want it widened to the %.2f%% minimum",
			got.StopDistancePct, cfg.Entry.MinStopDistancePct)
	}
	if math.Abs(got.RiskPerShare-(got.Entry-got.Stop)) > 1e-9 {
		t.Errorf("risk per share = %v, want entry − stop", got.RiskPerShare)
	}
}

func TestEMASeedsWithASimpleAverage(t *testing.T) {
	// A flat series must produce a flat EMA at the same level, which a bad seed
	// (starting from the first value, or from zero) would not.
	flat := make([]float64, 20)
	for i := range flat {
		flat[i] = 7.5
	}
	out := EMA(flat, 9)
	if len(out) != len(flat)-9+1 {
		t.Fatalf("length = %d, want %d", len(out), len(flat)-9+1)
	}
	for i, v := range out {
		if math.Abs(v-7.5) > 1e-9 {
			t.Fatalf("EMA[%d] = %v, want 7.5 throughout", i, v)
		}
	}
	if EMA(flat, 21) != nil {
		t.Error("an EMA longer than the series must report no value rather than guess")
	}
}

func TestVWAPWeightsByVolume(t *testing.T) {
	bars := []domain.Bar{
		{High: 10, Low: 10, Close: 10, Volume: 1},
		{High: 20, Low: 20, Close: 20, Volume: 99},
	}
	// Typical prices are 10 and 20; weighted by 1 and 99 the answer is 19.9.
	if got := VWAP(bars); math.Abs(got-19.9) > 1e-9 {
		t.Errorf("VWAP = %v, want 19.9 — it must weight by volume, not average prices", got)
	}
	if got := VWAP([]domain.Bar{{High: 5, Low: 5, Close: 5}}); got != 0 {
		t.Errorf("VWAP with no volume = %v, want 0 so callers can treat it as no opinion", got)
	}
}
