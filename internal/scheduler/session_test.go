package scheduler

import (
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func testCfg() *config.Config {
	c := &config.Config{}
	c.Timing.SentimentWindow = time.Hour
	c.Exit.EODExitOffsetMins = 30
	return c
}

func normalSession(t *testing.T) Session {
	t.Helper()
	return Session{
		Date:  "2026-09-28",
		Open:  time.Date(2026, 9, 28, 9, 30, 0, 0, ET),
		Close: time.Date(2026, 9, 28, 16, 0, 0, 0, ET),
	}
}

func TestPhaseAt(t *testing.T) {
	b := Bounds(normalSession(t), testCfg())
	at := func(h, m int) time.Time {
		return time.Date(2026, 9, 28, h, m, 0, 0, ET)
	}

	tests := []struct {
		name string
		now  time.Time
		want domain.Phase
	}{
		{"pre-market", at(9, 0), domain.PhaseClosed},
		{"at the open", at(9, 30), domain.PhaseFirstHour},
		{"mid first hour", at(10, 0), domain.PhaseFirstHour},
		{"one minute before gate", at(10, 29), domain.PhaseFirstHour},
		{"at the gate", at(10, 30), domain.PhaseTrading},
		{"midday", at(13, 0), domain.PhaseTrading},
		{"one minute before EOD mark", at(15, 29), domain.PhaseTrading},
		{"at EOD exit mark", at(15, 30), domain.PhaseEODWindow},
		{"before close", at(15, 59), domain.PhaseEODWindow},
		{"at the close", at(16, 0), domain.PhaseClosed},
		{"after hours", at(18, 0), domain.PhaseClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PhaseAt(tt.now, b); got != tt.want {
				t.Errorf("PhaseAt(%s) = %v, want %v", tt.now.Format("15:04"), got, tt.want)
			}
		})
	}
}

// An early close squeezes the day so hard that the EOD exit mark lands inside
// the first hour. Flattening must win over the sentiment poll.
func TestPhaseAtEarlyCloseOverlapsFirstHour(t *testing.T) {
	s := Session{
		Date:  "2026-11-27",
		Open:  time.Date(2026, 11, 27, 9, 30, 0, 0, ET),
		Close: time.Date(2026, 11, 27, 10, 15, 0, 0, ET), // deliberately extreme
	}
	b := Bounds(s, testCfg())

	// EOD mark is 09:45, inside the 09:30-10:30 first hour.
	if got := PhaseAt(time.Date(2026, 11, 27, 9, 50, 0, 0, ET), b); got != domain.PhaseEODWindow {
		t.Errorf("got %v, want PhaseEODWindow: flattening must take precedence", got)
	}
	if got := PhaseAt(time.Date(2026, 11, 27, 9, 35, 0, 0, ET), b); got != domain.PhaseFirstHour {
		t.Errorf("got %v, want PhaseFirstHour before the EOD mark", got)
	}
}

func TestIsEarlyClose(t *testing.T) {
	if normalSession(t).IsEarlyClose() {
		t.Error("16:00 close must not be flagged as early")
	}
	early := Session{
		Open:  time.Date(2026, 11, 27, 9, 30, 0, 0, ET),
		Close: time.Date(2026, 11, 27, 13, 0, 0, 0, ET),
	}
	if !early.IsEarlyClose() {
		t.Error("13:00 close must be flagged as early")
	}
}

func TestBounds(t *testing.T) {
	b := Bounds(normalSession(t), testCfg())
	if got, want := b.FirstHourEnd.Format("15:04"), "10:30"; got != want {
		t.Errorf("FirstHourEnd = %s, want %s", got, want)
	}
	if got, want := b.EODExit.Format("15:04"), "15:30"; got != want {
		t.Errorf("EODExit = %s, want %s (30 min before close)", got, want)
	}
}

// The session key must follow the exchange's calendar day, not the host's.
func TestSessionDateUsesExchangeTimezone(t *testing.T) {
	// 01:30 UTC on the 29th is 21:30 ET on the 28th.
	utc := time.Date(2026, 9, 29, 1, 30, 0, 0, time.UTC)
	if got := SessionDate(utc); got != "2026-09-28" {
		t.Errorf("SessionDate = %s, want 2026-09-28", got)
	}
}

// The entry window closes early enough to leave quiet time before the forced exit,
// and it is the *earlier* of the two limits that wins. Getting this wrong on a short
// session means buying something the agent is about to be required to sell.
func TestEntryWindowLeavesQuietTimeBeforeTheForcedExit(t *testing.T) {
	cfg := &config.Config{}
	cfg.Timing.SentimentWindow = 5 * time.Minute
	cfg.Timing.EntryWindow = 5*time.Hour + 30*time.Minute
	cfg.Timing.EntryCutoffBuffer = 30 * time.Minute
	cfg.Exit.EODExitOffsetMins = 30

	at := func(h, m int) time.Time { return time.Date(2026, 9, 28, h, m, 0, 0, ET) }

	t.Run("a regular session: the window from the open is the binding limit", func(t *testing.T) {
		b := Bounds(Session{Date: "2026-09-28", Open: at(9, 30), Close: at(16, 0)}, cfg)
		if !b.EODExit.Equal(at(15, 30)) {
			t.Fatalf("forced exit = %s, want 15:30", b.EODExit.Format("15:04"))
		}
		// 09:30 + 5h30m = 15:00, which already clears the 30-minute buffer.
		if !b.EntryWindowEnd.Equal(at(15, 0)) {
			t.Errorf("entry window ends %s, want 15:00", b.EntryWindowEnd.Format("15:04"))
		}
		if gap := b.EODExit.Sub(b.EntryWindowEnd); gap != 30*time.Minute {
			t.Errorf("gap before the forced exit = %s, want 30m", gap)
		}
	})

	t.Run("a half day: the buffer is the binding limit", func(t *testing.T) {
		// Close at 13:00, so the forced exit is 12:30. Measured from the open the
		// window would run to 15:00 — two and a half hours after the position had to
		// be flat.
		b := Bounds(Session{Date: "2026-11-27", Open: at(9, 30), Close: at(13, 0)}, cfg)
		if !b.EODExit.Equal(at(12, 30)) {
			t.Fatalf("forced exit = %s, want 12:30", b.EODExit.Format("15:04"))
		}
		if !b.EntryWindowEnd.Equal(at(12, 0)) {
			t.Errorf("entry window ends %s, want 12:00 — the buffer must still hold",
				b.EntryWindowEnd.Format("15:04"))
		}
		if gap := b.EODExit.Sub(b.EntryWindowEnd); gap != 30*time.Minute {
			t.Errorf("gap on a half day = %s, want the same 30m", gap)
		}
	})

	t.Run("a session shorter than the buffer yields no entry window, not a negative one", func(t *testing.T) {
		// Contrived, but a calendar oddity must not produce a window running backwards.
		b := Bounds(Session{Date: "2026-12-24", Open: at(9, 30), Close: at(10, 0)}, cfg)
		if b.EntryWindowEnd.Before(b.Open) {
			t.Errorf("entry window ends %s, before the %s open",
				b.EntryWindowEnd.Format("15:04"), b.Open.Format("15:04"))
		}
	})

	t.Run("a zero buffer puts the last entry on the forced exit", func(t *testing.T) {
		noBuf := *cfg
		noBuf.Timing.EntryCutoffBuffer = 0
		noBuf.Timing.EntryWindow = 12 * time.Hour // deliberately past the close
		b := Bounds(Session{Date: "2026-09-28", Open: at(9, 30), Close: at(16, 0)}, &noBuf)
		if !b.EntryWindowEnd.Equal(b.EODExit) {
			t.Errorf("entry window ends %s, want it clamped to the %s forced exit",
				b.EntryWindowEnd.Format("15:04"), b.EODExit.Format("15:04"))
		}
	})
}
