package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

// The change this file exists to protect: passing the screen is not enough to be
// bought. Without the setup gate the agent buys extension at whatever price the scan
// happens to read, with a stop unrelated to the chart — which a one-year backtest
// measured as having no edge.
func TestScreenedCandidateWithoutASetupIsNotBought(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	// Clears news, move and relative volume, but has no intraday bars at all, so no
	// pullback-and-resumption can be read.
	h.addScreenedOnly("NOPE", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	if got := len(h.openPositions()); got != 0 {
		t.Fatalf("bought %d positions with no setup on the chart", got)
	}
	// It must still be visible as a candidate with its reason, not silently dropped:
	// an operator looking at the page needs to see it qualified and why it was passed.
	evals, _, err := h.store.LatestScreenSnapshot(h.date)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range evals {
		if e.Symbol != "NOPE" {
			continue
		}
		found = true
		if !e.Qualifies {
			t.Error("NOPE should still qualify on the screen; only the setup is missing")
		}
		// The reason has to name the setup specifically. Asserting merely that it was
		// not bought is too weak: with the gate removed, sizing refuses the zero entry
		// price a non-triggered setup carries, so nothing would be bought either way
		// and the test would pass while the gate did nothing.
		if !strings.Contains(e.Outcome, "no setup") {
			t.Errorf("outcome = %q, want it to name the missing setup as the reason", e.Outcome)
		}
	}
	if !found {
		t.Error("a screened candidate must appear on the page even when not bought")
	}
}

// A candidate that does print a setup is bought, and the stop it is given comes from
// the chart rather than a percentage of entry.
func TestSetupDefinesTheStopAndTheSize(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	pos := h.openPositions()
	if len(pos) != 1 {
		t.Fatalf("got %d positions, want 1", len(pos))
	}
	p := pos[0]

	// The stop is nowhere near the 10% backstop: it is where the pullback low was.
	backstop := p.EntryPrice * (1 - h.cfg.Risk.StopLossPct/100)
	if p.StopPrice <= backstop {
		t.Errorf("stop %v is at or below the %v percentage backstop, so the chart stop is not being used",
			p.StopPrice, backstop)
	}
	if dist := (p.EntryPrice - p.StopPrice) / p.EntryPrice * 100; dist > h.cfg.Entry.MaxStopDistancePct {
		t.Errorf("stop distance %.2f%% exceeds the %.2f%% limit", dist, h.cfg.Entry.MaxStopDistancePct)
	}
	if p.InitialRisk <= 0 {
		t.Error("initial risk must be recorded so targets have a unit")
	}
	// Risk taken is bounded by the configured budget. It can be less, because the
	// notional cap binds on tight stops, but never more.
	budget := 100_000 * h.cfg.Risk.RiskPerTradePct / 100
	if risked := float64(p.Shares) * p.InitialRisk; risked > budget+1 {
		t.Errorf("risked $%.2f, more than the $%.2f budget", risked, budget)
	}
}

// New positions stop at the entry window, but everything already held is still
// managed to the close.
func TestEntryWindowStopsBuyingButNotManaging(t *testing.T) {
	h := newHarness(t)
	// The harness resolves its sentiment gate an hour after the 09:30 open, so the
	// entry window has to outlast that for any entry to be possible at all — which is
	// exactly what config validation enforces. 90 minutes closes it at 11:00.
	h.cfg.Timing.EntryWindow = 90 * time.Minute
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()

	// 10:35 is inside the window: the position opens.
	h.at(10, 35)
	h.tick()
	if got := len(h.openPositions()); got != 1 {
		t.Fatalf("got %d positions inside the entry window, want 1", got)
	}

	// A second name sets up after the window closes and must not be bought.
	h.addCandidate("WXYZ", 6.00)
	h.at(11, 5)
	h.tick()
	open := h.openPositions()
	if len(open) != 1 {
		t.Fatalf("got %d positions after the entry window closed, want still 1", len(open))
	}
	if open[0].Symbol != "ABCD" {
		t.Errorf("held %s, want the position opened inside the window", open[0].Symbol)
	}
	// The page still has to show what was passed over, and say why.
	evals, _, err := h.store.LatestScreenSnapshot(h.date)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evals {
		if e.Symbol == "WXYZ" && e.Outcome == "" {
			t.Error("a candidate passed over for the window must say so")
		}
	}

	// And the open position is still managed: the stop still works after the window.
	h.fake.SetPrice("ABCD", 3.00)
	h.at(12, 0)
	h.tick()
	if got := len(h.openPositions()); got != 0 {
		t.Fatalf("the stop must still fire after the entry window: %d open", got)
	}
	all, _ := h.store.SessionPositions(h.date)
	for _, p := range all {
		if p.Symbol == "ABCD" && p.ExitReason != domain.ExitStopLoss {
			t.Errorf("exit reason = %q, want STOP_LOSS", p.ExitReason)
		}
	}
}
