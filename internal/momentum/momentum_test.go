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
