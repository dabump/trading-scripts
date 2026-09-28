package web

import (
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// The countdown answers the question someone actually has when glancing at the page:
// how long until something happens.
func TestCountdownAcrossTheDay(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want string
	}{
		{
			// Before today's open, today's own session is the target.
			name: "pre-market counts to today's open",
			now:  time.Date(2026, 9, 28, 7, 45, 0, 0, scheduler.ET),
			want: "opens in 1h 45m",
		},
		{
			name: "at the open it flips to the close",
			now:  time.Date(2026, 9, 28, 9, 30, 0, 0, scheduler.ET),
			want: "closes in 6h 30m",
		},
		{
			name: "midday counts to the close",
			now:  time.Date(2026, 9, 28, 11, 48, 0, 0, scheduler.ET),
			want: "closes in 4h 12m",
		},
		{
			name: "final minutes",
			now:  time.Date(2026, 9, 28, 15, 47, 0, 0, scheduler.ET),
			want: "closes in 13m",
		},
		{
			// Once today is done the target is the next session, which only the
			// engine's cached calendar lookup knows.
			name: "after the close counts to the next session's open",
			now:  time.Date(2026, 9, 28, 17, 0, 0, 0, scheduler.ET),
			want: "opens in 16h 30m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.now = tt.now

			_, body := f.get(t, "/")
			if !strings.Contains(body, tt.want) {
				t.Errorf("page does not show %q", tt.want)
			}
		})
	}
}

// A weekend is the case where "next open" is least guessable and most wanted.
func TestCountdownAcrossAWeekend(t *testing.T) {
	f := newFixture(t)
	// Friday evening; the next session is Monday.
	f.now = time.Date(2026, 9, 25, 18, 0, 0, 0, scheduler.ET)
	f.eng.session = scheduler.Session{
		Date:  "2026-09-25",
		Open:  time.Date(2026, 9, 25, 9, 30, 0, 0, scheduler.ET),
		Close: time.Date(2026, 9, 25, 16, 0, 0, 0, scheduler.ET),
	}
	f.eng.next = scheduler.Session{
		Date:  "2026-09-28",
		Open:  time.Date(2026, 9, 28, 9, 30, 0, 0, scheduler.ET),
		Close: time.Date(2026, 9, 28, 16, 0, 0, 0, scheduler.ET),
	}
	f.eng.nextKnown = true

	_, body := f.get(t, "/")
	if !strings.Contains(body, "opens in 2d 15h") {
		t.Errorf("weekend countdown missing; body has: %s", excerpt(body, "Exchange"))
	}
	// The exact moment is available for a sanity check.
	if !strings.Contains(body, "Mon 28 Sep") {
		t.Error("the next open's date should be shown so the figure can be checked")
	}
}

// A holiday has no session of its own, so the countdown must come from the next one.
func TestCountdownOnANonTradingDay(t *testing.T) {
	f := newFixture(t)
	f.eng.tradingDay = false
	f.eng.session = scheduler.Session{Date: "2026-09-28"}
	f.now = time.Date(2026, 9, 28, 11, 0, 0, 0, scheduler.ET)
	f.eng.next = scheduler.Session{
		Date:  "2026-09-29",
		Open:  time.Date(2026, 9, 29, 9, 30, 0, 0, scheduler.ET),
		Close: time.Date(2026, 9, 29, 16, 0, 0, 0, scheduler.ET),
	}
	f.eng.nextKnown = true

	_, body := f.get(t, "/")
	if !strings.Contains(body, "Exchange CLOSED") {
		t.Error("a non-trading day must still read CLOSED")
	}
	if !strings.Contains(body, "opens in 22h 30m") {
		t.Error("countdown to the next session missing on a non-trading day")
	}
}

// A blank is better than a wrong number: if the calendar lookup failed, show nothing.
func TestCountdownAbsentWhenNextSessionUnknown(t *testing.T) {
	f := newFixture(t)
	f.now = time.Date(2026, 9, 28, 17, 0, 0, 0, scheduler.ET) // after the close
	f.eng.nextKnown = false

	_, body := f.get(t, "/")
	if strings.Contains(body, "opens in") {
		t.Error("a countdown was rendered with no known next session")
	}
	if !strings.Contains(body, "Exchange CLOSED") {
		t.Error("the badge itself must still render")
	}
}

// The countdown lives in the polled fragment, so it refreshes without a reload.
func TestCountdownIsInThePolledFragment(t *testing.T) {
	f := newFixture(t)
	f.now = time.Date(2026, 9, 28, 11, 48, 0, 0, scheduler.ET)

	_, body := f.get(t, "/fragment")
	if !strings.Contains(body, "closes in 4h 12m") {
		t.Error("the fragment must carry the countdown, or it would freeze between reloads")
	}
}

func excerpt(body, around string) string {
	i := strings.Index(body, around)
	if i < 0 {
		return "(not found)"
	}
	end := i + 200
	if end > len(body) {
		end = len(body)
	}
	return body[i:end]
}
