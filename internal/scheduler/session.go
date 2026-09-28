// Package scheduler knows the shape of a US trading day and drives the daily
// loop described in docs/architecture.md.
package scheduler

import (
	"time"

	// The timezone database is embedded in the binary rather than read from the
	// host. Every session boundary in this package is defined in exchange time, and
	// a minimal container image carries no tzdata — without this the fallback below
	// would engage and put every boundary an hour out from March to November, which
	// is most of the trading year. ~450KB to remove an entire class of bug.
	_ "time/tzdata"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// ET is the exchange's timezone. All session boundaries are defined in it, so
// the daemon behaves the same regardless of the host's local timezone.
var ET = mustLoadET()

func mustLoadET() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		// Unreachable in practice now that tzdata is embedded above. Kept as a last
		// resort, but it loses DST correctness, so reaching it means something is
		// badly wrong with the build.
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
	// PreMarketOpen is when pre-market coverage begins. It is zero when pre-market
	// is disabled, and zero is the only signal PhaseAt needs: a session with no
	// pre-market mark is simply closed until its open, exactly as before.
	PreMarketOpen time.Time
	Open          time.Time
	FirstHourEnd  time.Time
	// EntryWindowEnd is the last moment a new position may be opened. Screening
	// continues after it — the page still shows what is setting up — but nothing is
	// bought. It is the earlier of `open + entry_window` and
	// `EODExit − entry_cutoff_buffer`, so a short session tightens it rather than
	// letting an entry land on top of the forced exit.
	EntryWindowEnd time.Time
	EODExit        time.Time
	Close          time.Time
}

// preMarketOpen resolves the configured pre-market start against a session's own
// date, or reports false when there is no pre-market to enter.
//
// The start is clamped against the session's real open rather than the usual 09:30,
// so a day the exchange opens late cannot end up with a "pre-market" that overlaps
// the regular session. A start that does not parse is treated as no pre-market:
// config validation rejects it at start-up, and a running daemon reaching here with
// a bad value should scan less, never scan at the wrong time.
func preMarketOpen(s Session, cfg *config.Config) (time.Time, bool) {
	if !cfg.PreMarket.Enabled || s.Open.IsZero() {
		return time.Time{}, false
	}
	hour, minute, err := cfg.PreMarket.StartClock()
	if err != nil {
		return time.Time{}, false
	}
	open := s.Open.In(ET)
	start := time.Date(open.Year(), open.Month(), open.Day(), hour, minute, 0, 0, ET)
	if !start.Before(s.Open) {
		return time.Time{}, false
	}
	return start, true
}

// Bounds derives the session's decision points from config.
func Bounds(s Session, cfg *config.Config) Boundaries {
	b := Boundaries{
		Open:           s.Open,
		FirstHourEnd:   s.Open.Add(cfg.Timing.SentimentWindow),
		EntryWindowEnd: s.Open.Add(cfg.Timing.EntryWindow),
		EODExit:        s.Close.Add(-time.Duration(cfg.Exit.EODExitOffsetMins) * time.Minute),
		Close:          s.Close,
	}
	// Keep quiet time before the forced exit. Without this a half day — where the
	// close, and therefore the forced exit, arrives hours earlier than the entry
	// window measured from the open — would let the agent buy something it was about
	// to be required to sell.
	if cutoff := b.EODExit.Add(-cfg.Timing.EntryCutoffBuffer); b.EntryWindowEnd.After(cutoff) {
		b.EntryWindowEnd = cutoff
	}
	// A session so short that the cutoff precedes the open leaves no entry window at
	// all rather than a window running backwards.
	if b.EntryWindowEnd.Before(b.Open) {
		b.EntryWindowEnd = b.Open
	}
	if start, ok := preMarketOpen(s, cfg); ok {
		b.PreMarketOpen = start
	}
	return b
}

// PhaseAt reports where the given instant sits within the session.
//
// An early close can push the EOD exit mark before the first hour has even
// finished (a 13:00 close with a 09:30 open leaves only 3.5 hours). In that case
// the EOD window wins: flattening positions before the close matters more than
// completing the sentiment poll, and entering trades with less than the normal
// runway was never the intent.
//
// Pre-market only ever occupies time that used to be PhaseClosed, and only when
// PreMarketOpen is set. Everything from the opening bell onwards is untouched, and
// after the close the day is closed again — the post-market session is deliberately
// not covered, because the forced end-of-day exit has already flattened the book.
func PhaseAt(now time.Time, b Boundaries) domain.Phase {
	switch {
	case !now.Before(b.Close):
		return domain.PhaseClosed
	case now.Before(b.Open):
		if b.PreMarketOpen.IsZero() || now.Before(b.PreMarketOpen) {
			return domain.PhaseClosed
		}
		return domain.PhasePreMarket
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
