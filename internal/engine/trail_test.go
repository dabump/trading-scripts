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

// The runner, end to end under the shipped exit: 12c target, 75% sold, breakeven,
// then after_target. The stop climbs to each completed candle's low, holds when a
// candle makes a lower low, and the resting order at the broker follows it until a
// new low sells the runner well above the entry.
func TestAfterTargetTrailsTheRunnerUpAndSellsIt(t *testing.T) {
	h := newHarness(t)
	h.cfg.Exit.FirstTargetCents = 0.12
	h.cfg.Exit.FirstTargetFraction = 0.75
	h.cfg.Exit.CandleTrail = config.CandleTrailAfterTarget
	pos := h.buyOne(t, "ABCD", 5.00)
	e := pos.EntryPrice

	at := func(mm int) time.Time { return time.Date(2026, 9, 28, 10, mm, 0, 0, scheduler.ET) }
	bar := func(mm int, low float64) domain.Bar {
		return domain.Bar{Time: at(mm), Open: e + low + 0.02, High: e + low + 0.10,
			Low: e + low, Close: e + low + 0.05, Volume: 10_000}
	}
	h.fake.SetBars("ABCD", []domain.Bar{
		bar(31, -0.02), bar(32, 0.01), bar(33, 0.08),
		bar(34, 0.20), bar(35, 0.35), bar(36, 0.30),
	})

	stopIs := func(step string, want float64) {
		t.Helper()
		open := h.openPositions()
		if len(open) != 1 {
			t.Fatalf("%s: runner not open: %+v", step, open)
		}
		if math.Abs(open[0].StopPrice-want) > 1e-9 {
			t.Fatalf("%s: stop = %v, want %v", step, open[0].StopPrice, want)
		}
		shares, resting, ok := h.fake.RestingStop("ABCD")
		if !ok || shares != open[0].SharesOpen || math.Abs(resting-want) > 1e-9 {
			t.Fatalf("%s: broker stop = %d @ %v (ok %v), want %d @ %v",
				step, shares, resting, ok, open[0].SharesOpen, want)
		}
	}

	// The target: 75% sold, the runner's stop at breakeven, not yet trailed.
	h.at(10, 33)
	h.fake.SetPrice("ABCD", e+0.13)
	h.tick()
	runner := h.openPositions()
	if len(runner) != 1 || !runner[0].TargetHit {
		t.Fatalf("want the runner open after the target: %+v", runner)
	}
	if want := pos.Shares - int(math.Floor(float64(pos.Shares)*0.75)); runner[0].SharesOpen != want {
		t.Fatalf("runner = %d shares, want %d of %d", runner[0].SharesOpen, want, pos.Shares)
	}
	stopIs("at the target", e)

	// Each completed candle's low raises it.
	h.at(10, 34)
	h.fake.SetPrice("ABCD", e+0.15)
	h.tick()
	stopIs("after the 10:33 candle", e+0.08)

	h.at(10, 35)
	h.fake.SetPrice("ABCD", e+0.30)
	h.tick()
	stopIs("after the 10:34 candle", e+0.20)

	h.at(10, 36)
	h.fake.SetPrice("ABCD", e+0.40)
	h.tick()
	stopIs("after the 10:35 candle", e+0.35)

	// A lower low does not give ground back.
	h.at(10, 37)
	h.fake.SetPrice("ABCD", e+0.38)
	h.tick()
	stopIs("after the lower 10:36 low", e+0.35)

	// Through the trailed stop: the broker's order sells the runner.
	h.now = h.now.Add(20 * time.Second)
	h.fake.SetPrice("ABCD", e+0.34)
	h.tick()
	h.now = h.now.Add(20 * time.Second)
	h.tick()

	all, _ := h.store.SessionPositions(h.date)
	if len(all) != 1 || all[0].Open {
		t.Fatalf("runner should have been sold on the trailed stop: %+v", all)
	}
	c := all[0]
	if c.ExitReason != domain.ExitStopLoss {
		t.Errorf("exit reason = %q, want STOP_LOSS", c.ExitReason)
	}
	if math.Abs(c.ExitPrice-(e+0.35)) > 1e-9 {
		t.Errorf("runner sold at %v, want the trailed %v rather than breakeven %v", c.ExitPrice, e+0.35, e)
	}
	if h.fake.RestingStops() != 0 {
		t.Errorf("%d stop orders left at the broker after the runner closed", h.fake.RestingStops())
	}
}
