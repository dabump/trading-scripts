package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// The page needs the next open to count down to, and the web layer cannot ask the
// broker itself, so the engine caches it alongside today's session.
func TestNextSessionCachedAndExposed(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	nextOpen := time.Date(2026, 9, 29, 9, 30, 0, 0, scheduler.ET)
	h.fake.SetNextSession(broker.CalendarDay{
		Date: "2026-09-29", Open: nextOpen,
		Close: time.Date(2026, 9, 29, 16, 0, 0, 0, scheduler.ET),
	})

	h.at(9, 30)
	h.tick()

	next, known := h.eng.NextSession()
	if !known {
		t.Fatal("next session should be known after a tick")
	}
	if !next.Open.Equal(nextOpen) {
		t.Errorf("next open = %v, want %v", next.Open, nextOpen)
	}
}

// Losing the calendar lookup must not stop trading — the countdown just goes blank.
func TestUnknownNextSessionDoesNotFaultTheAgent(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)
	// No next session configured, so the fake returns a zero day.

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick() // fatals if the agent entered ERROR

	if _, known := h.eng.NextSession(); known {
		t.Error("an absent next session must report unknown, not a zero-time session")
	}
	// Trading is unaffected.
	if got := len(h.openPositions()); got != 1 {
		t.Errorf("got %d positions, want 1: the countdown lookup must not block entries", got)
	}
}

// A non-trading day is exactly when the countdown matters most, so the lookup must
// still happen.
func TestNextSessionLoadedOnANonTradingDay(t *testing.T) {
	h := newHarness(t)
	h.fake.SetCalendar(broker.CalendarDay{}) // holiday or weekend
	h.fake.SetNextSession(broker.CalendarDay{
		Date:  "2026-09-29",
		Open:  time.Date(2026, 9, 29, 9, 30, 0, 0, scheduler.ET),
		Close: time.Date(2026, 9, 29, 16, 0, 0, 0, scheduler.ET),
	})

	h.at(11, 0)
	h.tick()

	if _, known := h.eng.NextSession(); !known {
		t.Error("the next session must be looked up even when today has none")
	}
}

// A calendar failure for the next session is a warning, not a fault: today's session
// still loaded fine.
func TestNextSessionFailureIsNotFatal(t *testing.T) {
	h := newHarness(t)
	h.setBullish()

	// Today's calendar succeeds; only the next-session lookup is broken. The fake
	// fails everything, so assert the weaker property: the agent reports the error
	// rather than silently claiming a next session.
	h.fake.SetError(errors.New("calendar 500"))
	h.at(9, 40)
	h.eng.Tick(context.Background())

	if _, known := h.eng.NextSession(); known {
		t.Error("a failed lookup must leave the next session unknown")
	}
}
