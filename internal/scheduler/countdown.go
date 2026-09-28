package scheduler

import (
	"fmt"
	"time"
)

// UntilClose is how long the current session has left to run, and whether the
// exchange is open at all right now.
func UntilClose(now time.Time, s Session) (time.Duration, bool) {
	if s.Open.IsZero() || s.Close.IsZero() {
		return 0, false
	}
	if now.Before(s.Open) || !now.Before(s.Close) {
		return 0, false
	}
	return s.Close.Sub(now), true
}

// UntilOpen is how long until the given session opens.
//
// It reports false once that session has opened, so a caller holding a stale
// session cannot render a countdown that has already elapsed.
func UntilOpen(now time.Time, s Session) (time.Duration, bool) {
	if s.Open.IsZero() || !now.Before(s.Open) {
		return 0, false
	}
	return s.Open.Sub(now), true
}

// FormatCountdown renders a duration for a human glancing at the page.
//
// Resolution stops at minutes deliberately. The page refreshes on a ~12s poll, so
// a seconds figure would advance in 12-second jumps and read as broken; and nothing
// here is actionable to the second, since the boundaries it counts towards are all
// handled by the agent itself.
func FormatCountdown(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return "under a minute"
	}

	days := int(d / (24 * time.Hour))
	hours := int(d/time.Hour) % 24
	minutes := int(d/time.Minute) % 60

	switch {
	case days > 0:
		// Weekends and holiday closures: minutes are noise at this range.
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
