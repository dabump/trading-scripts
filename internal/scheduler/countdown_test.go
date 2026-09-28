package scheduler

import (
	"testing"
	"time"
)

func session(t *testing.T) Session {
	t.Helper()
	return Session{
		Date:  "2026-09-28",
		Open:  time.Date(2026, 9, 28, 9, 30, 0, 0, ET),
		Close: time.Date(2026, 9, 28, 16, 0, 0, 0, ET),
	}
}

func TestUntilClose(t *testing.T) {
	s := session(t)
	at := func(h, m int) time.Time { return time.Date(2026, 9, 28, h, m, 0, 0, ET) }

	tests := []struct {
		name    string
		now     time.Time
		wantDur time.Duration
		wantOK  bool
	}{
		{"before the open", at(8, 0), 0, false},
		{"at the open", at(9, 30), 6*time.Hour + 30*time.Minute, true},
		{"midday", at(12, 15), 3*time.Hour + 45*time.Minute, true},
		{"a minute before the close", at(15, 59), time.Minute, true},
		{"at the close", at(16, 0), 0, false},
		{"after the close", at(18, 0), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := UntilClose(tt.now, s)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.wantDur {
				t.Errorf("duration = %v, want %v", got, tt.wantDur)
			}
		})
	}

	// A day with no session cannot be counted down.
	if _, ok := UntilClose(at(12, 0), Session{Date: "2026-12-25"}); ok {
		t.Error("a non-trading day must not report time until close")
	}
}

func TestUntilOpen(t *testing.T) {
	s := session(t)
	at := func(h, m int) time.Time { return time.Date(2026, 9, 28, h, m, 0, 0, ET) }

	if got, ok := UntilOpen(at(8, 0), s); !ok || got != 90*time.Minute {
		t.Errorf("got %v,%v want 1h30m,true", got, ok)
	}

	// Once the session has opened the countdown is meaningless, and reporting it
	// would let a stale session render a countdown that already elapsed.
	for _, now := range []time.Time{at(9, 30), at(12, 0), at(18, 0)} {
		if _, ok := UntilOpen(now, s); ok {
			t.Errorf("UntilOpen at %s returned ok, want false", now.Format("15:04"))
		}
	}

	if _, ok := UntilOpen(at(8, 0), Session{}); ok {
		t.Error("an unknown session must not report time until open")
	}
}

// A weekend gap spans days, so the format has to stay readable across that range.
func TestUntilOpenAcrossAWeekend(t *testing.T) {
	fridayEvening := time.Date(2026, 9, 25, 18, 0, 0, 0, ET)
	monday := Session{
		Date:  "2026-09-28",
		Open:  time.Date(2026, 9, 28, 9, 30, 0, 0, ET),
		Close: time.Date(2026, 9, 28, 16, 0, 0, 0, ET),
	}

	got, ok := UntilOpen(fridayEvening, monday)
	if !ok {
		t.Fatal("want a countdown across the weekend")
	}
	if want := 63*time.Hour + 30*time.Minute; got != want {
		t.Errorf("duration = %v, want %v", got, want)
	}
	if formatted := FormatCountdown(got); formatted != "2d 15h" {
		t.Errorf("FormatCountdown = %q, want %q", formatted, "2d 15h")
	}
}

func TestFormatCountdown(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "under a minute"},
		{30 * time.Second, "under a minute"},
		{-5 * time.Second, "under a minute"}, // clock skew must not print a negative
		{time.Minute, "1m"},
		{45 * time.Minute, "45m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h"},
		{time.Hour + time.Minute, "1h 1m"},
		{4*time.Hour + 12*time.Minute, "4h 12m"},
		{23*time.Hour + 59*time.Minute, "23h 59m"},
		{24 * time.Hour, "1d"},
		{25 * time.Hour, "1d 1h"},
		{63*time.Hour + 30*time.Minute, "2d 15h"},
	}
	for _, tt := range tests {
		t.Run(tt.in.String(), func(t *testing.T) {
			if got := FormatCountdown(tt.in); got != tt.want {
				t.Errorf("FormatCountdown(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Resolution stops at minutes on purpose: the page polls every ~12s, so a seconds
// figure would advance in 12-second jumps.
func TestFormatCountdownIgnoresSeconds(t *testing.T) {
	base := 4*time.Hour + 12*time.Minute
	for _, extra := range []time.Duration{0, 5 * time.Second, 30 * time.Second, 59 * time.Second} {
		if got := FormatCountdown(base + extra); got != "4h 12m" {
			t.Errorf("FormatCountdown(%v) = %q, want it stable at minute resolution",
				base+extra, got)
		}
	}
}
