package strategy

import (
	"math"
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
		MinPullbackBars:    1,
		MaxPullbackBars:    5,
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

// The setup is the whole point of the change: a screened candidate is only bought
// when it pulls back and resumes, and the stop is what the pullback defines.
func TestFindSetupTriggersOnAPullbackAndResumption(t *testing.T) {
	cfg := setupCfg()

	bars := ramp(12, 9.00, 10.00)
	poleHigh := bars[len(bars)-1].High
	// One flag bar holding below the pole high, with a clear low.
	bars = appendBar(bars, 9.98, poleHigh-0.02, 9.90, 9.93)
	// The trigger bar closes above the pole high.
	bars = appendBar(bars, 9.95, poleHigh+0.08, 9.94, poleHigh+0.05)

	got := FindSetup(bars, cfg)
	if !got.Triggered {
		t.Fatalf("expected a setup, got refusal: %s", got.Reason)
	}
	if got.Entry != poleHigh+0.05 {
		t.Errorf("entry = %v, want the trigger close %v", got.Entry, poleHigh+0.05)
	}
	// The stop sits just under the flag's low, which is what makes the risk knowable
	// before the position is sized.
	wantStop := 9.90 * (1 - 0.1/100)
	if math.Abs(got.Stop-wantStop) > 1e-9 {
		t.Errorf("stop = %v, want %v (just under the 9.90 pullback low)", got.Stop, wantStop)
	}
	if got.RiskPerShare <= 0 || math.Abs(got.RiskPerShare-(got.Entry-got.Stop)) > 1e-9 {
		t.Errorf("risk per share = %v, want entry − stop", got.RiskPerShare)
	}
	if got.FlagBars != 1 {
		t.Errorf("flag bars = %d, want 1", got.FlagBars)
	}
}

func TestFindSetupRefusals(t *testing.T) {
	cfg := setupCfg()

	t.Run("not enough history yet", func(t *testing.T) {
		if got := FindSetup(ramp(5, 9, 10), cfg); got.Triggered {
			t.Error("a setup cannot be called before the EMA has warmed up")
		}
	})

	t.Run("no pullback: the last bar simply made a new high", func(t *testing.T) {
		// A straight ramp has no flag, so the bar before the trigger is the pole and
		// the flag is zero bars long.
		if got := FindSetup(ramp(14, 9, 10), cfg); got.Triggered {
			t.Errorf("a continuous ramp is not a pullback entry: %+v", got)
		}
	})

	t.Run("the pullback has not been reclaimed", func(t *testing.T) {
		bars := ramp(12, 9.00, 10.00)
		pole := bars[len(bars)-1].High
		bars = appendBar(bars, 9.98, pole-0.02, 9.90, 9.93)
		// The last bar closes below the pole high: still inside the pullback.
		bars = appendBar(bars, 9.93, pole-0.01, 9.92, pole-0.03)
		if got := FindSetup(bars, cfg); got.Triggered {
			t.Error("a close below the pullback high is not a trigger")
		}
	})

	t.Run("the pullback lasted too long", func(t *testing.T) {
		bars := ramp(12, 9.00, 10.00)
		pole := bars[len(bars)-1].High
		for i := 0; i < 7; i++ { // more than max_pullback_bars
			bars = appendBar(bars, 9.95, pole-0.05, 9.85, 9.90)
		}
		bars = appendBar(bars, 9.92, pole+0.05, 9.90, pole+0.03)
		if got := FindSetup(bars, cfg); got.Triggered {
			t.Error("a seven-bar flag is a stalled move, not a pause")
		}
	})

	t.Run("the stop would be too far away", func(t *testing.T) {
		bars := ramp(12, 9.00, 10.00)
		pole := bars[len(bars)-1].High
		// A deep flush: the flag low is more than 4% below the trigger close.
		bars = appendBar(bars, 9.98, pole-0.02, 9.00, 9.20)
		bars = appendBar(bars, 9.30, pole+0.05, 9.25, pole+0.03)
		got := FindSetup(bars, cfg)
		if got.Triggered {
			t.Errorf("a stop beyond the limit must be refused, not sized around: %+v", got)
		}
		if got.Reason == "" {
			t.Error("the refusal should say the risk was too wide")
		}
	})

	t.Run("price below the EMA is not strength", func(t *testing.T) {
		// A falling sequence: the trigger clears a local high but sits under the EMA.
		bars := ramp(12, 10.00, 9.00)
		pole := bars[len(bars)-1].High
		bars = appendBar(bars, 9.00, pole-0.01, 8.95, 8.97)
		bars = appendBar(bars, 8.98, pole+0.02, 8.96, pole+0.01)
		if got := FindSetup(bars, cfg); got.Triggered {
			t.Error("the strategy only buys strength; below the EMA is not strength")
		}
	})

	t.Run("VWAP is enforced when configured", func(t *testing.T) {
		// A spike that faded: the trigger bar prints a long upper wick on heavy
		// volume, so its own typical price drags VWAP above its close. The EMA reads
		// closes only, so it is untouched — which is what isolates the VWAP filter.
		bars := ramp(12, 9.00, 10.00)
		for i := range bars {
			bars[i].Volume = 1
		}
		pole := bars[len(bars)-1].High
		bars = appendBar(bars, 9.98, pole-0.02, 9.90, 9.93)
		bars = appendBar(bars, 9.95, 12.00, 9.94, 10.05)
		bars[len(bars)-1].Volume = 5_000_000

		withVWAP := setupCfg()
		withVWAP.Entry.RequireAboveVWAP = true
		if got := FindSetup(bars, withVWAP); got.Triggered {
			t.Error("a close below VWAP must be refused when the filter is on")
		}
		// The same chart passes with the filter off, which proves the filter is what
		// rejected it rather than some other condition.
		noVWAP := setupCfg()
		noVWAP.Entry.RequireAboveVWAP = false
		if got := FindSetup(bars, noVWAP); !got.Triggered {
			t.Errorf("without the VWAP filter this chart should trigger: %s", got.Reason)
		}
	})
}

// A stop sitting implausibly close to entry is widened rather than accepted, because
// ordinary noise would otherwise take the position out immediately.
func TestFindSetupWidensAnImplausiblyTightStop(t *testing.T) {
	cfg := setupCfg()
	bars := ramp(12, 9.00, 10.00)
	pole := bars[len(bars)-1].High
	// The flag barely dips: its low is a fraction of a percent below the trigger.
	bars = appendBar(bars, pole-0.001, pole-0.001, pole-0.004, pole-0.002)
	bars = appendBar(bars, pole, pole+0.02, pole-0.001, pole+0.01)

	got := FindSetup(bars, cfg)
	if !got.Triggered {
		t.Fatalf("expected a setup: %s", got.Reason)
	}
	if math.Abs(got.StopDistancePct-cfg.Entry.MinStopDistancePct) > 1e-9 {
		t.Errorf("stop distance = %.4f%%, want it widened to the %.2f%% minimum",
			got.StopDistancePct, cfg.Entry.MinStopDistancePct)
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
