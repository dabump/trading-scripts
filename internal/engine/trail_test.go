package engine

import (
	"math"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// openForTrail buys ABCD at 10:35 and then replaces its chart with two candles held
// through: 10:35 (low 4.97) and 10:36 (low 5.02).
func openForTrail(t *testing.T, mode string) *harness {
	t.Helper()
	h := newHarness(t)
	h.cfg.Exit.CandleTrail = mode
	h.setBullish()
	h.addCandidate("ABCD", 5.00)
	h.at(9, 30)
	h.tick()
	h.at(10, 35)
	h.tick()
	if len(h.openPositions()) != 1 {
		t.Fatal("expected a position to be open")
	}

	at := func(mm int) time.Time { return time.Date(2026, 9, 28, 10, mm, 0, 0, scheduler.ET) }
	h.fake.SetBars("ABCD", []domain.Bar{
		{Time: at(35), Open: 5.00, High: 5.10, Low: 4.97, Close: 5.05, Volume: 10_000},
		{Time: at(36), Open: 5.05, High: 5.20, Low: 5.02, Close: 5.15, Volume: 10_000},
		// Still forming at 10:37: its low must not be trailed on.
		{Time: at(37), Open: 5.15, High: 5.16, Low: 5.12, Close: 5.13, Volume: 1_000},
	})
	return h
}

// The micro pullback's exit: the first candle to trade below the previous candle's
// low sells the position, well above the chart stop under the pause.
func TestCandleTrailSellsOnTheFirstNewLow(t *testing.T) {
	h := openForTrail(t, config.CandleTrailAlways)
	initial := h.openPositions()[0].StopPrice

	h.at(10, 37)
	h.fake.SetPrice("ABCD", 5.10)
	h.tick()
	pos := h.openPositions()
	if len(pos) != 1 {
		t.Fatal("position should still be open above the 10:36 low")
	}
	if math.Abs(pos[0].StopPrice-5.02) > 1e-9 {
		t.Fatalf("stop = %v, want raised from %v to the 5.02 low of the last completed candle",
			pos[0].StopPrice, initial)
	}

	h.now = h.now.Add(30 * time.Second)
	h.fake.SetPrice("ABCD", 5.01) // under 5.02, far above the chart stop
	h.tick()
	all, _ := h.store.SessionPositions(h.date)
	if len(all) != 1 || all[0].Open {
		t.Fatalf("position should have been sold on the new low: %+v", all)
	}
	if all[0].ExitReason != domain.ExitStopLoss {
		t.Errorf("exit reason = %q, want STOP_LOSS (the trail works through the stop)", all[0].ExitReason)
	}
}

// With the trail limited to the runner, the full position keeps its chart stop until
// the first target is banked.
func TestCandleTrailWaitsForTheTargetWhenAsked(t *testing.T) {
	for _, mode := range []string{config.CandleTrailAfterTarget, config.CandleTrailOff} {
		h := openForTrail(t, mode)
		initial := h.openPositions()[0].StopPrice

		h.at(10, 37)
		h.fake.SetPrice("ABCD", 5.01)
		h.tick()
		pos := h.openPositions()
		if len(pos) != 1 {
			t.Fatalf("%s: sold before the target, want the chart stop to hold", mode)
		}
		if pos[0].StopPrice != initial {
			t.Errorf("%s: stop moved from %v to %v before the target", mode, initial, pos[0].StopPrice)
		}
	}
}
