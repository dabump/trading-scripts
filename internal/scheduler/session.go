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
//
// The day has three screening windows, not one: pre-market, the regular session, and
// post-market. The two extended ones share their thresholds and their limit-only
// routing, so they share a phase — but they have separate boundaries here, because the
// regular session's open and close sit between them.
type Boundaries struct {
	// ExtendedOpen is when pre-market coverage begins and ExtendedClose is when
	// post-market coverage ends. Either is zero when that half does not exist — the
	// section is off, or the configured clock does not leave room for it — and zero is
	// the only signal PhaseAt needs: a session with no extended mark is simply closed
	// on that side of the bell, exactly as before.
	ExtendedOpen time.Time
	Open         time.Time
	FirstHourEnd time.Time
	// EntryWindowEnd is the last moment a new position may be opened in the regular
	// session. Screening continues after it — the page still shows what is setting up —
	// but nothing is bought. It is the earlier of `open + entry_window` and
	// `EODExit − entry_cutoff_buffer`, so a short session tightens it rather than
	// letting an entry land on top of the forced exit.
	EntryWindowEnd time.Time
	EODExit        time.Time
	Close          time.Time
	// PostEntryEnd and PostEODExit are the post-market session's own copies of
	// EntryWindowEnd and EODExit, derived from ExtendedClose the same way those are
	// derived from Close. The forced exit runs twice a day because the book has to be
	// flat twice: once before the regular close, and again before the extended one, so
	// nothing bought at 17:00 is carried overnight.
	PostEntryEnd  time.Time
	PostEODExit   time.Time
	ExtendedClose time.Time
}

// clockOn resolves an "HH:MM" exchange time against a session's own date.
func clockOn(s Session, clock func() (int, int, error)) (time.Time, bool) {
	hour, minute, err := clock()
	if err != nil {
		return time.Time{}, false
	}
	day := s.Open.In(ET)
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, ET), true
}

// extendedBounds resolves the configured extended-hours clocks against a session's
// own date, filling in the post-market session's entry and forced-exit marks.
//
// Each half is clamped against the session's real open or close rather than the usual
// 09:30 and 16:00, so a day the exchange opens late or closes early cannot end up with
// an "extended" window that overlaps the regular session. A clock that does not parse
// is treated as that half not existing: config validation rejects it at start-up, and
// a running daemon reaching here with a bad value should scan less, never scan at the
// wrong time.
//
// The post-market half additionally has to leave room for its own forced exit. A
// configured end within exit.eod_exit_offset_minutes of the close would otherwise
// produce a post-market session that is nothing but a flattening window, which reads
// as the feature half-working; there is simply no post-market on such a day.
func extendedBounds(s Session, cfg *config.Config, b *Boundaries) {
	if !cfg.Extended.Enabled || s.Open.IsZero() || s.Close.IsZero() {
		return
	}
	if start, ok := clockOn(s, cfg.Extended.StartClock); ok && start.Before(s.Open) {
		b.ExtendedOpen = start
	}
	end, ok := clockOn(s, cfg.Extended.EndClock)
	if !ok {
		return
	}
	eod := time.Duration(cfg.Exit.EODExitOffsetMins) * time.Minute
	if !end.Add(-eod).After(s.Close) {
		return
	}
	b.ExtendedClose = end
	b.PostEODExit = end.Add(-eod)
	b.PostEntryEnd = b.PostEODExit.Add(-cfg.Timing.EntryCutoffBuffer)
	// A buffer wider than the post-market session leaves no entry window at all
	// rather than a window running backwards.
	if b.PostEntryEnd.Before(s.Close) {
		b.PostEntryEnd = s.Close
	}
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
	extendedBounds(s, cfg, &b)
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
// Extended hours only ever occupy time that used to be PhaseClosed, and only when the
// corresponding mark is set. Everything between the bells is untouched. After the
// close the day runs a second EOD window and then closes for good, so a position
// opened post-market is flattened before the extended close rather than carried
// overnight — the book has to be flat at the end of the day the agent traded, and
// post-market is part of that day.
func PhaseAt(now time.Time, b Boundaries) domain.Phase {
	switch {
	case now.Before(b.Open):
		if b.ExtendedOpen.IsZero() || now.Before(b.ExtendedOpen) {
			return domain.PhaseClosed
		}
		return domain.PhaseExtended
	case now.Before(b.Close):
		switch {
		case !now.Before(b.EODExit):
			return domain.PhaseEODWindow
		case now.Before(b.FirstHourEnd):
			return domain.PhaseFirstHour
		default:
			return domain.PhaseTrading
		}
	default:
		if b.ExtendedClose.IsZero() || !now.Before(b.ExtendedClose) {
			return domain.PhaseClosed
		}
		if !now.Before(b.PostEODExit) {
			return domain.PhaseEODWindow
		}
		return domain.PhaseExtended
	}
}

// ExtendedHours reports whether an order placed at this instant has to be routed to
// the extended-hours book.
//
// Derived from the phase rather than from the bells alone, so it cannot claim an
// extended session the agent is not actually in. It is true for both halves of
// PhaseExtended and for the post-market forced-exit window, which trades on the same
// book as the session it is flattening.
func ExtendedHours(now time.Time, b Boundaries) bool {
	switch PhaseAt(now, b) {
	case domain.PhaseExtended:
		return true
	case domain.PhaseEODWindow:
		return !now.Before(b.Close)
	default:
		return false
	}
}

// ExtendedStart is where an extended-hours pass measures its session's volume and
// reads its chart from: the pre-market open before the bell, the regular close after
// it. Zero outside extended hours, which is also where no caller reads it.
func ExtendedStart(now time.Time, b Boundaries) time.Time {
	if now.Before(b.Open) {
		return b.ExtendedOpen
	}
	return b.Close
}

// BeforeTheBell distinguishes the two halves of PhaseExtended. They share a phase and
// every threshold, but not their sentiment authority — a live read before the open,
// the day's resolved gate after the close — and a watchlist from one must never be
// acted on in the other.
func BeforeTheBell(now time.Time, b Boundaries) bool {
	return now.Before(b.Open)
}

// SessionDate formats an instant as the ET calendar date used to key a session.
func SessionDate(t time.Time) string {
	return t.In(ET).Format("2006-01-02")
}
