package scheduler

import (
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// This guards that ET is a real DST-aware zone — it would catch someone replacing
// mustLoadET with a fixed offset, or the fallback engaging.
//
// It does NOT prove the embedded tzdata is present, and an earlier version of this file
// wrongly claimed it did. Go's LoadLocation consults $ZONEINFO, the system zoneinfo
// directory, and $GOROOT/lib/time/zoneinfo.zip before the time/tzdata embed, and that
// GOROOT zip ships with every toolchain — so removing the import breaks nothing here or
// in the build image. The embed is only load-bearing at runtime, where the image
// deliberately installs no tzdata; see the container check in docs/operations.md.
func TestExchangeTimezoneHandlesDST(t *testing.T) {
	tests := []struct {
		name       string
		date       time.Time
		wantZone   string
		wantOffset int // seconds east of UTC
	}{
		{
			name:       "summer is EDT",
			date:       time.Date(2026, 7, 15, 12, 0, 0, 0, ET),
			wantZone:   "EDT",
			wantOffset: -4 * 60 * 60,
		},
		{
			name:       "winter is EST",
			date:       time.Date(2026, 1, 15, 12, 0, 0, 0, ET),
			wantZone:   "EST",
			wantOffset: -5 * 60 * 60,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zone, offset := tt.date.Zone()
			if zone != tt.wantZone || offset != tt.wantOffset {
				t.Errorf("got %s (%+d), want %s (%+d) — ET is not America/New_York, "+
					"so every session boundary is an hour out for part of the year",
					zone, offset/3600, tt.wantZone, tt.wantOffset/3600)
			}
		})
	}

	// The offset must actually change across the year, which a fixed zone cannot do.
	_, july := time.Date(2026, 7, 15, 12, 0, 0, 0, ET).Zone()
	_, january := time.Date(2026, 1, 15, 12, 0, 0, 0, ET).Zone()
	if july == january {
		t.Error("offset is constant year-round: this is a fixed zone, not America/New_York")
	}
}

// Session boundaries must be decided in exchange time regardless of the host's local
// zone, since a container's TZ is not something the daemon should depend on.
//
// The previous version of this test asserted open.In(loc).In(ET) — an identity, because a
// time.Time is an absolute instant and In only changes display, so it passed for every
// input including the degraded fixed-zone fallback. This version changes time.Local,
// which is the thing actually claimed to be irrelevant.
func TestSessionBoundariesIgnoreHostTimezone(t *testing.T) {
	sess := Session{
		Date:  "2026-07-15",
		Open:  time.Date(2026, 7, 15, 9, 30, 0, 0, ET),
		Close: time.Date(2026, 7, 15, 16, 0, 0, 0, ET),
	}
	cfg := &config.Config{}
	cfg.Timing.SentimentWindow = time.Hour
	cfg.Exit.EODExitOffsetMins = 30

	want := map[string]string{
		"open": "09:30", "first hour end": "10:30", "eod exit": "15:30", "close": "16:00",
	}

	for _, zone := range []string{"UTC", "Australia/Sydney", "America/Los_Angeles"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatalf("load %s: %v", zone, err)
			}
			original := time.Local
			time.Local = loc
			t.Cleanup(func() { time.Local = original })

			b := Bounds(sess, cfg)
			got := map[string]string{
				"open":           b.Open.In(ET).Format("15:04"),
				"first hour end": b.FirstHourEnd.In(ET).Format("15:04"),
				"eod exit":       b.EODExit.In(ET).Format("15:04"),
				"close":          b.Close.In(ET).Format("15:04"),
			}
			for name, wantAt := range want {
				if got[name] != wantAt {
					t.Errorf("with host zone %s, %s = %s ET, want %s", zone, name, got[name], wantAt)
				}
			}

			// And the phase decisions must land on the same instants.
			at := func(h, m int) time.Time { return time.Date(2026, 7, 15, h, m, 0, 0, ET) }
			for _, c := range []struct {
				now  time.Time
				want domain.Phase
			}{
				{at(9, 0), domain.PhaseClosed},
				{at(9, 30), domain.PhaseFirstHour},
				{at(10, 30), domain.PhaseTrading},
				{at(15, 30), domain.PhaseEODWindow},
				{at(16, 0), domain.PhaseClosed},
			} {
				if phase := PhaseAt(c.now, b); phase != c.want {
					t.Errorf("with host zone %s, PhaseAt(%s) = %v, want %v",
						zone, c.now.In(ET).Format("15:04"), phase, c.want)
				}
			}
		})
	}
}

// The session key buckets by exchange date, not the host's, so a host ahead of ET does
// not roll the session over early.
func TestSessionDateIgnoresHostTimezone(t *testing.T) {
	// 21:30 ET on the 15th is already the 16th in Sydney.
	instant := time.Date(2026, 7, 15, 21, 30, 0, 0, ET)

	for _, zone := range []string{"UTC", "Australia/Sydney"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatalf("load %s: %v", zone, err)
		}
		original := time.Local
		time.Local = loc

		got := SessionDate(instant)
		time.Local = original

		if got != "2026-07-15" {
			t.Errorf("with host zone %s, SessionDate = %s, want 2026-07-15", zone, got)
		}
	}
}
