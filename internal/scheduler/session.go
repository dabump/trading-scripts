// Package scheduler knows the shape of a US trading day and drives the daily
// loop described in docs/architecture.md.
package scheduler

import (
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// ET is the exchange's timezone. All session boundaries are defined in it, so
// the daemon behaves the same regardless of the host's local timezone.
var ET = mustLoadET()

func mustLoadET() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		// Fall back to a fixed offset only if tzdata is unavailable; this loses
		// DST correctness, so it is better to know about it than to silently
		// drift by an hour twice a year.
		return time.FixedZone("EST", -5*60*60)
	}
	return loc
}

// Session is one trading day's boundaries, as reported by the exchange calendar
// rather than assumed, so holidays and early closes are handled by data instead
// of a hardcoded list.
type Session struct {
	Date  string // YYYY-MM-DD in ET
	Open  time.Time
	Close time.Time
}

// IsEarlyClose reports whether this session closes before the usual 16:00 ET.
func (s Session) IsEarlyClose() bool {
	normal := time.Date(s.Close.In(ET).Year(), s.Close.In(ET).Month(), s.Close.In(ET).Day(),
		16, 0, 0, 0, ET)
	return s.Close.Before(normal)
}

// Boundaries are the decision points within a session.
type Boundaries struct {
	Open         time.Time
	FirstHourEnd time.Time
	EODExit      time.Time
	Close        time.Time
}

// Bounds derives the session's decision points from config.
func Bounds(s Session, cfg *config.Config) Boundaries {
	return Boundaries{
		Open:         s.Open,
		FirstHourEnd: s.Open.Add(cfg.Timing.SentimentWindow),
		EODExit:      s.Close.Add(-time.Duration(cfg.Exit.EODExitOffsetMins) * time.Minute),
		Close:        s.Close,
	}
}

// PhaseAt reports where the given instant sits within the session.
//
// An early close can push the EOD exit mark before the first hour has even
// finished (a 13:00 close with a 09:30 open leaves only 3.5 hours). In that case
// the EOD window wins: flattening positions before the close matters more than
// completing the sentiment poll, and entering trades with less than the normal
// runway was never the intent.
func PhaseAt(now time.Time, b Boundaries) domain.Phase {
	switch {
	case now.Before(b.Open), !now.Before(b.Close):
		return domain.PhaseClosed
	case !now.Before(b.EODExit):
		return domain.PhaseEODWindow
	case now.Before(b.FirstHourEnd):
		return domain.PhaseFirstHour
	default:
		return domain.PhaseTrading
	}
}

// SessionDate formats an instant as the ET calendar date used to key a session.
func SessionDate(t time.Time) string {
	return t.In(ET).Format("2006-01-02")
}
