package web

import (
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// closeOn records a position opened and closed on one day, which is what the date
// arrows navigate between. The exit instant is what files it under a day — a
// position can be opened in one session and closed in a later one.
func (f *fixture) closeOn(t *testing.T, day string, symbol string, shares int,
	entry, exit float64) {
	t.Helper()
	at, err := time.ParseInLocation("2006-01-02", day, scheduler.ET)
	if err != nil {
		t.Fatal(err)
	}
	at = at.Add(15 * time.Hour) // mid-afternoon ET
	id, err := f.store.InsertPosition(domain.Position{
		SessionDate: day, Symbol: symbol, Shares: shares,
		EntryPrice: entry, EntryTime: at, PeakPrice: exit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ClosePosition(id, exit, at.Add(30*time.Minute), domain.ExitStopLoss); err != nil {
		t.Fatal(err)
	}
}

// hasDeadArrow reports whether an arrow leading nowhere is rendered disabled. The
// attributes span lines in the template, so the two are matched as a pair rather
// than as one string.
func hasDeadArrow(body string) bool {
	for _, chunk := range strings.Split(body, `data-closed-date=""`)[1:] {
		head, _, _ := strings.Cut(chunk, ">")
		if strings.Contains(head, "disabled") {
			return true
		}
	}
	return false
}

// The closed list shows one day and the arrows step through the days that have
// closes. Today is 2026-09-28 with nothing closed, so the back arrow has to be live
// on an empty day — that is the main reason the arrows exist.
func TestClosedDayArrowsPointAtDaysWithCloses(t *testing.T) {
	f := newFixture(t)
	f.closeOn(t, "2026-09-24", "AAAA", 100, 2.00, 2.50)
	f.closeOn(t, "2026-09-25", "BBBB", 50, 4.00, 4.40)

	_, body := f.get(t, "/")
	if !strings.Contains(body, `data-closed-date="2026-09-25"`) {
		t.Error("back arrow should lead to the most recent day with closes")
	}
	if !strings.Contains(body, "Mon 28 Sep · today") {
		t.Error("the shown day should be labelled, and today said so")
	}
	if !strings.Contains(body, "Nothing has been closed in this session yet.") {
		t.Error("today is still empty and should say so")
	}
	// Nothing closed after today, so forward is dead.
	if !hasDeadArrow(body) {
		t.Error("forward arrow should be disabled on the newest day")
	}
}

// Stepping back shows that day's positions and that day's net P&L — not a running
// total across sessions.
func TestClosedDayShowsThatDayOnly(t *testing.T) {
	f := newFixture(t)
	f.closeOn(t, "2026-09-24", "AAAA", 100, 2.00, 2.50) // +$50
	f.closeOn(t, "2026-09-25", "BBBB", 50, 4.00, 4.40)  // +$20
	f.closeOn(t, "2026-09-25", "CCCC", 10, 6.00, 5.00)  // -$10

	_, body := f.get(t, "/?closed=2026-09-25")
	if !strings.Contains(body, "Fri 25 Sep") || strings.Contains(body, "· today") {
		t.Error("a past day should be labelled as itself, not as today")
	}
	for _, want := range []string{"BBBB", "CCCC", "+10.00", "1 up · 1 down"} {
		if !strings.Contains(body, want) {
			t.Errorf("closed day 2026-09-25 is missing %q", want)
		}
	}
	if strings.Contains(body, "AAAA") {
		t.Error("a position closed on another day must not appear")
	}
	if !strings.Contains(body, `data-closed-date="2026-09-24"`) {
		t.Error("back arrow should lead to the next earlier day with closes")
	}
	// 09-28 has no closes, but forward must still reach the live session.
	if !strings.Contains(body, `data-closed-date="2026-09-28"`) {
		t.Error("forward arrow should return to today even with nothing closed there")
	}
}

// The oldest day has nowhere further back to go.
func TestClosedDayStopsAtTheOldestDay(t *testing.T) {
	f := newFixture(t)
	f.closeOn(t, "2026-09-24", "AAAA", 100, 2.00, 2.50)

	_, body := f.get(t, "/?closed=2026-09-24")
	if !hasDeadArrow(body) {
		t.Error("back arrow should be disabled on the oldest day with closes")
	}
	if !strings.Contains(body, "Thu 24 Sep") {
		t.Error("the oldest day should still be labelled")
	}
}

// A day with no closes still renders: the arrows skip them, but a hand-edited URL
// can land on one and must not break the page.
func TestClosedDayWithNoClosesExplainsItself(t *testing.T) {
	f := newFixture(t)
	f.closeOn(t, "2026-09-24", "AAAA", 100, 2.00, 2.50)

	_, body := f.get(t, "/?closed=2026-09-23")
	if !strings.Contains(body, "No positions were closed on Wed 23 Sep.") {
		t.Error("an empty past day should name the day it is empty for")
	}
	if strings.Contains(body, "Net P&L") {
		t.Error("the net-P&L footer must not render against an empty table")
	}
}

// An unusable date shows the live session rather than a 500: it is a browser
// navigation parameter, not an instruction the agent has to honour.
func TestClosedDayFallsBackToTodayWhenUnusable(t *testing.T) {
	f := newFixture(t)
	f.closeOn(t, "2026-09-28", "DDDD", 100, 3.00, 3.30)

	for _, q := range []string{"/?closed=not-a-date", "/?closed=2026-10-05", "/?closed="} {
		code, body := f.get(t, q)
		if code != 200 {
			t.Fatalf("%s: status = %d, want 200", q, code)
		}
		if !strings.Contains(body, "Mon 28 Sep · today") {
			t.Errorf("%s should fall back to today", q)
		}
		if !strings.Contains(body, "DDDD") {
			t.Errorf("%s should show today's closed positions", q)
		}
	}
}

// The poll carries the day back, or the list would snap to today every twelve
// seconds while someone was reading an earlier session.
func TestFragmentKeepsTheRequestedDay(t *testing.T) {
	f := newFixture(t)
	f.closeOn(t, "2026-09-25", "BBBB", 50, 4.00, 4.40)

	_, body := f.get(t, "/fragment?closed=2026-09-25")
	if !strings.Contains(body, "BBBB") || !strings.Contains(body, "Fri 25 Sep") {
		t.Error("the fragment must render the requested day, not today")
	}
}

func TestAdjacentClosedDays(t *testing.T) {
	days := []string{"2026-09-25", "2026-09-24", "2026-09-18"}
	cases := []struct {
		name, shown, today, prev, next string
	}{
		{"today with earlier closes", "2026-09-28", "2026-09-28", "2026-09-25", ""},
		{"middle day", "2026-09-24", "2026-09-28", "2026-09-18", "2026-09-25"},
		{"oldest day", "2026-09-18", "2026-09-28", "", "2026-09-24"},
		{"newest closed day returns to today", "2026-09-25", "2026-09-28", "2026-09-24", "2026-09-28"},
		{"a day between closes", "2026-09-22", "2026-09-28", "2026-09-18", "2026-09-24"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev, next := adjacentClosedDays(days, tc.shown, tc.today)
			if prev != tc.prev || next != tc.next {
				t.Errorf("prev, next = %q, %q; want %q, %q", prev, next, tc.prev, tc.next)
			}
		})
	}

	if prev, next := adjacentClosedDays(nil, "2026-09-28", "2026-09-28"); prev != "" || next != "" {
		t.Errorf("with no closed positions both arrows must be dead; got %q, %q", prev, next)
	}
}

// Each closed row carries a fold-out line with when it was opened and closed and
// how long it was held, keyed by the position so the toggle survives the poll.
func TestClosedRowFoldsOutTimesAndDuration(t *testing.T) {
	f := newFixture(t)
	f.closeOn(t, "2026-09-25", "BBBB", 50, 4.00, 4.40)

	_, body := f.get(t, "/?closed=2026-09-25")
	for _, want := range []string{
		`data-fold="1"`, `id="fold-1"`,
		"Fri 25 Sep, 15:00:00 EDT", "Fri 25 Sep, 15:30:00 EDT", "30m 0s",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("closed row fold-out missing %q", want)
		}
	}
}

func TestHeldText(t *testing.T) {
	at := time.Date(2026, 9, 25, 13, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{42 * time.Second, "42s"},
		{12*time.Minute + 5*time.Second, "12m 5s"},
		{3*time.Hour + 7*time.Minute + 9*time.Second, "3h 7m"},
		{50 * time.Hour, "2d 2h"},
	} {
		if got := heldText(at, at.Add(c.d)); got != c.want {
			t.Errorf("heldText(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	if got := heldText(time.Time{}, at); got != "—" {
		t.Errorf("unrecorded entry should read as a dash, got %q", got)
	}
}
