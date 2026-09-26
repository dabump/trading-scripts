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
