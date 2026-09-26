package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The page and the buy decision must come from one evaluation, or they could drift
// into disagreeing about what qualifies.
func TestDisplayedScreenAndBuyDecisionShareOneEvaluation(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("BUYME", 5.00)
	h.fake.SetSnapshot("NONEWS", 5.00, 4.386, 6_000_000) // +14%, but no news
	h.fake.SetAverageVolume("NONEWS", 1_000_000)
	h.fake.SetNews("NONEWS", 0)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	evals, _, err := h.store.LatestScreenSnapshot(h.date)
	if err != nil {
		t.Fatal(err)
	}
	byMsymbol := map[string]bool{}
	for _, e := range evals {
		byMsymbol[e.Symbol] = e.Qualifies
	}
	if !byMsymbol["BUYME"] {
		t.Error("BUYME should be shown as qualifying")
	}
	if byMsymbol["NONEWS"] {
		t.Error("NONEWS should not be shown as qualifying")
	}

	// And exactly the qualifying one was bought.
	open := h.openPositions()
	if len(open) != 1 || open[0].Symbol != "BUYME" {
		t.Fatalf("positions = %+v, want only BUYME", open)
	}
}

// Qualifying is not the same as being bought, and the page has to say which.
func TestOutcomeRecordedPerQualifyingCandidate(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.cfg.Risk.MaxConcurrentPositions = 2

	// Four candidates that all clear the 5x volume bar, with decreasing relative
	// volume so the ranking order is unambiguous: 20x, 15x, 10x, 6x.
	for _, c := range []struct {
		symbol string
		avgVol float64
	}{
		{"AAA", 300_000}, {"BBB", 400_000}, {"CCC", 600_000}, {"DDD", 1_000_000},
	} {
		h.addCandidate(c.symbol, 5.00)
		h.fake.SetAverageVolume(c.symbol, c.avgVol)
	}

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	evals, _, err := h.store.LatestScreenSnapshot(h.date)
	if err != nil {
		t.Fatal(err)
	}

	var bought, capped int
	for _, e := range evals {
		if !e.Qualifies {
			continue
		}
		switch {
		case strings.HasPrefix(e.Outcome, "bought"):
			bought++
		case strings.Contains(e.Outcome, "position cap reached"):
			capped++
		default:
			t.Errorf("%s qualified but has no outcome recorded (%q)", e.Symbol, e.Outcome)
		}
	}
	if bought != 2 {
		t.Errorf("%d candidates recorded as bought, want 2 (the cap)", bought)
	}
	// Every candidate the cap turned away must say so, rather than showing blank.
	if capped != 2 {
		t.Errorf("%d candidates explained by the cap, want 2", capped)
	}
}

// A blip affecting one candidate must not discard the others: on a one-minute
// cadence, aborting the pass silently costs every remaining qualifier its entry.
func TestPerCandidateFailureDoesNotAbortTheEntryPass(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("GOOD", 5.00)
	h.fake.SetAverageVolume("GOOD", 500_000) // strong relative volume

	// BROKEN qualifies but has no price, so sizing cannot proceed for it.
	h.fake.SetSnapshot("BROKEN", 5.00, 4.386, 12_000_000) // higher dollar volume: ranked first
	h.fake.SetAverageVolume("BROKEN", 200_000)
	h.fake.SetNews("BROKEN", 1)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()

	// Remove BROKEN's price just before entry.
	h.fake.SetSnapshot("BROKEN", 0, 4.386, 12_000_000)
	h.at(10, 35)
	h.tick()

	// The agent must not be in ERROR over one bad symbol.
	if state, msg := h.eng.State(); state != "SCREENING" {
		t.Errorf("state = %q (%s), want SCREENING: one bad candidate must not fault the agent", state, msg)
	}
	// And the healthy candidate must still have been bought.
	var boughtGood bool
	for _, p := range h.openPositions() {
		if p.Symbol == "GOOD" {
			boughtGood = true
		}
	}
	if !boughtGood {
		t.Error("GOOD was not bought; a failure on another candidate aborted the pass")
	}
}

// A store failure is not candidate-specific, so it should still surface.
func TestNonCandidateFailureStillSurfaces(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()

	// Close the store so the position lookup inside entry fails.
	h.store.Close()
	h.at(10, 35)
	h.eng.Tick(context.Background())

	if state, _ := h.eng.State(); state != "ERROR" {
		t.Errorf("state = %q, want ERROR when the failure is not candidate-specific", state)
	}
	_ = errors.New
}
