package momentum

import (
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

func barsFromCloses(closes []float64) []domain.Bar {
	base := time.Date(2026, 10, 31, 9, 30, 0, 0, time.UTC)
	bars := make([]domain.Bar, len(closes))
	for i, c := range closes {
		bars[i] = domain.Bar{Time: base.Add(time.Duration(i) * time.Minute), Close: c}
	}
	return bars
}

func lastBarNow(bars []domain.Bar) time.Time {
	return bars[len(bars)-1].Time.Add(time.Minute)
}

func TestFillGapsSynthesizesFlatCandles(t *testing.T) {
	base := time.Date(2026, 10, 30, 18, 0, 0, 0, time.UTC)
	bars := []domain.Bar{
		{Time: base, Open: 4, High: 6, Low: 3, Close: 5, Volume: 100},
		{Time: base.Add(3 * time.Minute), Open: 7, High: 9, Low: 6, Close: 8, Volume: 50},
	}
	got := fillGaps(bars, time.Minute)
	if len(got) != 4 {
		t.Fatalf("got %d bars, want 4 (two real plus two synthesized)", len(got))
	}
	for i, want := range []struct {
		offset time.Duration
		close  float64
		volume float64
	}{
		{0, 5, 100},
		{time.Minute, 5, 0},
		{2 * time.Minute, 5, 0},
		{3 * time.Minute, 8, 50},
	} {
		if !got[i].Time.Equal(base.Add(want.offset)) {
			t.Fatalf("bar %d: got time %s, want %s", i, got[i].Time, base.Add(want.offset))
		}
		if got[i].Close != want.close {
			t.Fatalf("bar %d: got close %v, want %v", i, got[i].Close, want.close)
		}
		if got[i].Volume != want.volume {
			t.Fatalf("bar %d: got volume %v, want %v", i, got[i].Volume, want.volume)
		}
	}
	// A synthesized candle is flat at the prior close, not a copy of the next
	// real bar's OHLC.
	if got[1].Open != 5 || got[1].High != 5 || got[1].Low != 5 {
		t.Fatalf("synthesized bar should be flat at the prior close, got %+v", got[1])
	}
}

func TestFillGapsLeavesContiguousBarsUnchanged(t *testing.T) {
	bars := barsFromCloses([]float64{1, 2, 3, 4})
	got := fillGaps(bars, time.Minute)
	if len(got) != len(bars) {
		t.Fatalf("got %d bars, want %d (no gaps to fill)", len(got), len(bars))
	}
}

func TestFillGapsLeavesTooFewBarsAlone(t *testing.T) {
	if got := fillGaps(nil, time.Minute); len(got) != 0 {
		t.Fatalf("got %d bars from nil input, want 0", len(got))
	}
	one := barsFromCloses([]float64{1})
	if got := fillGaps(one, time.Minute); len(got) != 1 {
		t.Fatalf("got %d bars from a single bar, want 1", len(got))
	}
}

func TestClassifyPendingOnTooFewBars(t *testing.T) {
	closes := make([]float64, 10)
	for i := range closes {
		closes[i] = 10
	}
	bars := barsFromCloses(closes)
	if got := Classify(bars, lastBarNow(bars), time.Minute); got != StatePending {
		t.Fatalf("got %s, want %s", got, StatePending)
	}
}

func TestClassifyBullishOnAnAcceleratingRise(t *testing.T) {
	// MACD measures acceleration, not direction: a perfectly linear ramp keeps the
	// line and signal exactly equal (the diff series is constant from the first
	// bar), so the rise has to accelerate to push the line above its own lagging
	// signal.
	closes := make([]float64, 60)
	for i := range closes {
		closes[i] = 10 + 0.01*float64(i)*float64(i)
	}
	bars := barsFromCloses(closes)
	if got := Classify(bars, lastBarNow(bars), time.Minute); got != StateBullish {
		t.Fatalf("got %s, want %s", got, StateBullish)
	}
}

func TestClassifyDropsTheFormingCandle(t *testing.T) {
	closes := make([]float64, 60)
	for i := range closes {
		closes[i] = 10 + 0.01*float64(i)*float64(i)
	}
	bars := barsFromCloses(closes)
	// A forming last candle that would otherwise read as a sharp drop must not
	// move the classification, because it has not closed yet.
	bars[len(bars)-1].Close = 0.01
	now := bars[len(bars)-1].Time.Add(30 * time.Second)
	if got := Classify(bars, now, time.Minute); got != StateBullish {
		t.Fatalf("got %s, want %s (forming candle should have been dropped)", got, StateBullish)
	}
}

func TestAnticipatingCrossRequiresMonotonicNarrowingWhileStillBelow(t *testing.T) {
	cases := []struct {
		name string
		sig  []float64
		line []float64
		want bool
	}{
		{"narrowing and still below", []float64{1, 1, 1, 1}, []float64{0, 0.3, 0.6, 0.9}, true},
		{"already crossed inside the window", []float64{1, 1, 1, 1}, []float64{0, 0.5, 1.0, 1.1}, false},
		{"not narrowing", []float64{1, 1, 1, 1}, []float64{0.9, 0.6, 0.3, 0}, false},
		{"flat gap", []float64{1, 1, 1, 1}, []float64{0.5, 0.5, 0.5, 0.5}, false},
		{"too short", []float64{1, 1}, []float64{0, 0.5}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := anticipatingCross(c.line, c.sig); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestStochAgreesRequiresBandRisingAndAboveD(t *testing.T) {
	cases := []struct {
		name string
		k, d []float64
		want bool
	}{
		{"in band, rising, above d", []float64{30, 40, 50}, []float64{45}, true},
		{"above the band", []float64{70, 80, 90}, []float64{85}, false},
		{"below the band", []float64{5, 10, 15}, []float64{12}, false},
		{"falling", []float64{50, 40, 30}, []float64{35}, false},
		{"k at or below d", []float64{30, 40, 40}, []float64{45}, false},
		{"too short", []float64{40, 50}, []float64{45}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stochAgrees(c.k, c.d); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestRSISeriesIsNeutralOnAFlatPrice(t *testing.T) {
	closes := make([]float64, 30)
	for i := range closes {
		closes[i] = 10
	}
	rsi := rsiSeries(closes, rsiLength)
	if len(rsi) == 0 {
		t.Fatal("expected a non-empty RSI series")
	}
	for _, v := range rsi {
		if v != 50 {
			t.Fatalf("flat price should read RSI 50 throughout, got %v", v)
		}
	}
}

func TestStochRSIAlignsKAndD(t *testing.T) {
	closes := make([]float64, 60)
	for i := range closes {
		closes[i] = 10 + float64(i%5)
	}
	k, d := stochRSI(closes, rsiLength, stochLength, kSmoothing, dSmoothing)
	if len(k) == 0 || len(d) == 0 {
		t.Fatal("expected non-empty %K/%D series")
	}
	if len(k) != len(d) {
		t.Fatalf("expected %%K and %%D aligned to equal length, got %d vs %d", len(k), len(d))
	}
}
