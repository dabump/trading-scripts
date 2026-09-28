package engine

import (
	"context"
	"fmt"
	"testing"
)

// The universe is fetched once per session, not on every one-minute scan.
func TestUniverseCachedPerSession(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()
	// Several more scans across the day.
	for _, m := range []int{40, 45, 50} {
		h.at(10, m)
		h.tick()
	}

	if got := h.fake.AssetCalls(); got != 1 {
		t.Errorf("fetched the universe %d times in one session, want 1", got)
	}
}

// A full-market scan is only affordable because symbols that fail the cheap
// price-move filter never incur the per-symbol lookups.
func TestOnlyMoversAreEnriched(t *testing.T) {
	h := newHarness(t)
	h.setBullish()

	// One genuine mover among a crowd of quiet symbols.
	h.addCandidate("MOVER", 5.00)
	for i := 0; i < 50; i++ {
		sym := fmt.Sprintf("QUIET%02d", i)
		h.fake.SetSnapshot(sym, 10.00, 9.98, 500_000) // +0.2%: nowhere near +10%
		h.fake.SetAverageVolume(sym, 100_000)
	}

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	enriched := h.fake.EnrichedSymbols()
	for _, sym := range enriched {
		if sym != "MOVER" && sym != "SPY" && sym != "QQQ" && sym != "IWM" {
			t.Errorf("symbol %q was enriched despite failing the move filter", sym)
		}
	}
	if len(enriched) == 0 {
		t.Error("the actual mover should have been enriched")
	}
}

// The selection fix: when more names clear the move threshold than the enrichment
// budget allows, the busiest by dollar volume are kept. Sorting by percentage
// change instead would discard a heavily traded modest mover for a thin extreme
// one — the opposite of what the strategy ranks on.
func TestEnrichmentBudgetKeepsBusiestNotBiggestMovers(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.cfg.Screening.MaxEnriched = 2

	// THIN has by far the biggest percentage move but trades almost nothing.
	h.fake.SetSnapshot("THIN", 2.00, 1.00, 50_000) // +100%, $100k traded
	h.fake.SetAverageVolume("THIN", 5_000)
	h.fake.SetNews("THIN", 1)

	// These two move less but trade enormously.
	h.fake.SetSnapshot("BUSY1", 22.00, 20.00, 40_000_000) // +10%, $880m
	h.fake.SetAverageVolume("BUSY1", 4_000_000)
	h.fake.SetNews("BUSY1", 1)

	h.fake.SetSnapshot("BUSY2", 33.00, 30.00, 20_000_000) // +10%, $660m
	h.fake.SetAverageVolume("BUSY2", 2_000_000)
	h.fake.SetNews("BUSY2", 1)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)

	preview, err := h.eng.ScreenNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Evaluations) != 2 {
		t.Fatalf("evaluated %d symbols, want the 2 allowed by the budget", len(preview.Evaluations))
	}

	seen := map[string]bool{}
	for _, e := range preview.Evaluations {
		seen[e.Symbol] = true
	}
	if !seen["BUSY1"] || !seen["BUSY2"] {
		t.Errorf("kept %v, want the two busiest by dollar volume", preview.Evaluations)
	}
	if seen["THIN"] {
		t.Error("a thin +100% name took a slot from a heavily traded +10% one")
	}
	// The whole universe is still reported as scanned, not just the enriched subset.
	if preview.UniverseSize < 3 {
		t.Errorf("UniverseSize = %d, want the full scanned universe", preview.UniverseSize)
	}
}

// An empty universe must not be an error; it just means nothing to evaluate.
func TestEmptyUniverseIsNotAnError(t *testing.T) {
	h := newHarness(t)
	h.at(11, 0)
	h.tick()

	preview, err := h.eng.ScreenNow(context.Background())
	if err != nil {
		t.Fatalf("an empty universe must not error: %v", err)
	}
	if len(preview.Evaluations) != 0 {
		t.Errorf("got %d evaluations, want 0", len(preview.Evaluations))
	}
}

// The tradability floors run before enrichment, so an untradable mover must cost
// nothing beyond its snapshot: no news query, no average-volume lookup, no
// evaluation and no order. The backtest found the screen otherwise dominated by
// warrants and SPAC units — names a $10k account cannot fill.
func TestUntradableMoversAreRejectedBeforeEnrichment(t *testing.T) {
	h := newHarness(t)
	h.setBullish()

	// Both clear +10% and 5x volume; both fail a floor.
	//
	// A warrant at $0.07 that trades plenty of dollars: only the price floor stops it.
	h.fake.SetSnapshot("GIBOW", 0.07, 0.06, 30_000_000)
	h.fake.SetAverageVolume("GIBOW", 1_000_000)
	h.fake.SetNews("GIBOW", 3)
	// A SPAC unit at a respectable price whose whole day is $980 of turnover.
	h.fake.SetSnapshot("QETAU", 9.80, 8.50, 100)
	h.fake.SetAverageVolume("QETAU", 15)
	h.fake.SetNews("QETAU", 3)

	// One legitimate candidate, so a pass that rejects everything is not mistaken
	// for the floors working.
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	for _, sym := range h.fake.EnrichedSymbols() {
		if sym == "GIBOW" || sym == "QETAU" {
			t.Errorf("%q was enriched despite failing a tradability floor", sym)
		}
	}

	// It must also be absent from the screen the page shows, not merely unbought:
	// an untradable name is not a candidate the strategy considered.
	evals, _, err := h.store.LatestScreenSnapshot(h.date)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range evals {
		seen[e.Symbol] = true
	}
	if seen["GIBOW"] || seen["QETAU"] {
		t.Errorf("an untradable symbol reached the screen snapshot: %v", seen)
	}
	if !seen["ABCD"] {
		t.Error("the tradable candidate should still have been screened")
	}

	for _, o := range h.fake.Placed() {
		if o.Symbol == "GIBOW" || o.Symbol == "QETAU" {
			t.Errorf("placed an order for untradable %q", o.Symbol)
		}
	}
}
