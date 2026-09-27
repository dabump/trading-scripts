package scheduler

import (
	"testing"
	"time"
)

// The timezone database is embedded (see session.go) because a minimal container
// image carries none, and the fallback in mustLoadET silently loses DST. This test
// exists to catch that fallback engaging: it asserts real DST behaviour, which a
// fixed-offset zone cannot produce.
//
// Without the embed this fails inside a scratch/distroless image while passing on a
// developer machine — the worst possible shape for a bug in a clock-driven system.
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
				t.Errorf("got %s (%+d), want %s (%+d) — the tzdata fallback has engaged, "+
					"so every session boundary is an hour out",
					zone, offset/3600, tt.wantZone, tt.wantOffset/3600)
			}
		})
	}

	// The DST transition must actually be present, not merely a constant offset.
	_, july := time.Date(2026, 7, 15, 12, 0, 0, 0, ET).Zone()
	_, january := time.Date(2026, 1, 15, 12, 0, 0, 0, ET).Zone()
	if july == january {
		t.Error("offset is constant year-round: this is a fixed zone, not America/New_York")
	}
}

// A 09:30 open must be 09:30 ET whatever the host's local timezone is, since the
// container's TZ is not something the daemon should depend on.
func TestSessionBoundariesAreHostTimezoneIndependent(t *testing.T) {
	open := time.Date(2026, 7, 15, 9, 30, 0, 0, ET)

	// The same instant viewed from elsewhere is still 09:30 in exchange time.
	for _, name := range []string{"UTC", "Australia/Sydney", "Europe/London"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatalf("embedded tzdata is missing %s: %v", name, err)
		}
		if got := open.In(loc).In(ET).Format("15:04"); got != "09:30" {
			t.Errorf("via %s the open reads %s, want 09:30", name, got)
		}
	}
}
