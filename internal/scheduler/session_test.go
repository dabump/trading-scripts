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
		{"before the open with pre-market off", at(9, 0), domain.PhaseClosed},
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

// With extended hours enabled the day gains a window at each end: closed until the
// configured start, then PhaseExtended to the bell; and after the close, PhaseExtended
// again until its own forced exit, then closed for good. Everything between the bells
// must be exactly what it was.
func TestPhaseAtWithExtendedHours(t *testing.T) {
	cfg := testCfg()
	cfg.Extended.Enabled = true
	cfg.Extended.Start = "07:00"
	cfg.Extended.End = "20:00"

	b := Bounds(normalSession(t), cfg)
	at := func(h, m int) time.Time { return time.Date(2026, 9, 28, h, m, 0, 0, ET) }

	tests := []struct {
		name string
		now  time.Time
		want domain.Phase
	}{
		{"overnight, before pre-market opens", at(3, 0), domain.PhaseClosed},
		{"a minute before the pre-market start", at(6, 59), domain.PhaseClosed},
		{"exactly at the pre-market start", at(7, 0), domain.PhaseExtended},
		{"mid pre-market", at(8, 15), domain.PhaseExtended},
		{"a minute before the bell", at(9, 29), domain.PhaseExtended},
		{"at the bell, the regular day resumes", at(9, 30), domain.PhaseFirstHour},
		{"midday is unaffected", at(13, 0), domain.PhaseTrading},
		{"at the EOD mark", at(15, 30), domain.PhaseEODWindow},
		{"a minute before the close", at(15, 59), domain.PhaseEODWindow},
		// The close no longer ends the day. The second extended session starts there,
		// on the same thresholds and the same limit-only book as the first.
		{"at the close, post-market begins", at(16, 0), domain.PhaseExtended},
		{"mid post-market", at(18, 0), domain.PhaseExtended},
		{"a minute before the post-market EOD mark", at(19, 29), domain.PhaseExtended},
		// 20:00 minus the 30-minute offset testCfg uses: the book is flattened a
		// second time so nothing bought after the close is carried overnight.
		{"at the post-market EOD mark", at(19, 30), domain.PhaseEODWindow},
		{"a minute before the extended close", at(19, 59), domain.PhaseEODWindow},
		{"at the extended close the day is over", at(20, 0), domain.PhaseClosed},
		{"late evening", at(22, 0), domain.PhaseClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PhaseAt(tt.now, b); got != tt.want {
				t.Errorf("PhaseAt(%s) = %v, want %v", tt.now.Format("15:04"), got, tt.want)
			}
		})
	}
}

// Routing is derived from the phase, not from the bells alone, because an order sent
// to the wrong book is rejected: Alpaca takes only a day limit order outside the
// bells, and will not accept an extended-hours flag inside them. The post-market
// flattening window trades on the extended book like the session it is flattening.
func TestExtendedHoursRouting(t *testing.T) {
	cfg := testCfg()
	cfg.Extended.Enabled = true
	cfg.Extended.Start = "07:00"
	cfg.Extended.End = "20:00"

	b := Bounds(normalSession(t), cfg)
	at := func(h, m int) time.Time { return time.Date(2026, 9, 28, h, m, 0, 0, ET) }

	for _, tt := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"overnight, outside coverage", at(3, 0), false},
		{"pre-market", at(8, 0), true},
		{"the regular session", at(13, 0), false},
		{"the regular forced exit", at(15, 45), false},
		{"post-market", at(18, 0), true},
		{"the post-market forced exit", at(19, 45), true},
		{"past the extended close", at(21, 0), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtendedHours(tt.now, b); got != tt.want {
				t.Errorf("ExtendedHours(%s) = %v, want %v", tt.now.Format("15:04"), got, tt.want)
			}
		})
	}
}

// An extended pass measures its session's volume from the start of the session it is
// actually in. Reading the pre-market open at 18:00 would sum the whole day, which is
// the regular session's number and clears the deliberately low extended floor for
// every name on the tape.
func TestExtendedStartNamesTheRunningSession(t *testing.T) {
	cfg := testCfg()
	cfg.Extended.Enabled = true
	cfg.Extended.Start = "07:00"
	cfg.Extended.End = "20:00"

	sess := normalSession(t)
	b := Bounds(sess, cfg)
	if got := ExtendedStart(time.Date(2026, 9, 28, 8, 0, 0, 0, ET), b); !got.Equal(b.ExtendedOpen) {
		t.Errorf("ExtendedStart before the bell = %v, want the pre-market open %v", got, b.ExtendedOpen)
	}
	if got := ExtendedStart(time.Date(2026, 9, 28, 18, 0, 0, 0, ET), b); !got.Equal(sess.Close) {
		t.Errorf("ExtendedStart after the close = %v, want the regular close %v", got, sess.Close)
	}
}

// The post-market entry window gets the same quiet time before its forced exit that
// the regular one does, for the same reason: nothing should be bought that is about to
// be required to sell.
func TestPostMarketEntryWindowLeavesQuietTime(t *testing.T) {
	cfg := testCfg()
	cfg.Extended.Enabled = true
	cfg.Extended.Start = "07:00"
	cfg.Extended.End = "20:00"
	cfg.Timing.EntryCutoffBuffer = 45 * time.Minute

	b := Bounds(normalSession(t), cfg)
	// 20:00 − 30m forced-exit offset − 45m buffer.
	want := time.Date(2026, 9, 28, 18, 45, 0, 0, ET)
	if !b.PostEntryEnd.Equal(want) {
		t.Errorf("PostEntryEnd = %s, want %s", b.PostEntryEnd.Format("15:04"), want.Format("15:04"))
	}
	if gap := b.PostEODExit.Sub(b.PostEntryEnd); gap != cfg.Timing.EntryCutoffBuffer {
		t.Errorf("gap between the last post-market entry and its forced exit = %s, want %s",
			gap, cfg.Timing.EntryCutoffBuffer)
	}
}

// A configured end that does not clear the session's close by more than the forced
// exit offset leaves no post-market rather than one that is nothing but a flattening
// window — and an unparseable end is treated the same way, for the same reason the
// start is: validation rejects it at start-up, and a daemon reaching here should scan
// less, never at the wrong time.
func TestPostMarketNeedsRoomForItsForcedExit(t *testing.T) {
	cfg := testCfg()
	cfg.Extended.Enabled = true
	cfg.Extended.Start = "07:00"

	for _, end := range []string{"16:00", "16:20", "nonsense"} {
		cfg.Extended.End = end
		b := Bounds(normalSession(t), cfg)
		if !b.ExtendedClose.IsZero() {
			t.Errorf("extended.end %q gave ExtendedClose = %v, want zero", end, b.ExtendedClose)
		}
		if got := PhaseAt(time.Date(2026, 9, 28, 17, 0, 0, 0, ET), b); got != domain.PhaseClosed {
			t.Errorf("extended.end %q: PhaseAt(17:00) = %v, want PhaseClosed", end, got)
		}
		// The morning half is independent of the evening one failing.
		if b.ExtendedOpen.IsZero() {
			t.Errorf("extended.end %q also lost the pre-market open", end)
		}
	}
}

// An early close moves the regular bells, and the extended windows are clamped
// against the calendar's own open and close rather than the usual 09:30 and 16:00.
// Post-market then starts at the early close, which is what the exchange does.
func TestExtendedHoursFollowAnEarlyClose(t *testing.T) {
	cfg := testCfg()
	cfg.Extended.Enabled = true
	cfg.Extended.Start = "07:00"
	cfg.Extended.End = "20:00"

	half := Session{
		Date:  "2026-11-27",
		Open:  time.Date(2026, 11, 27, 9, 30, 0, 0, ET),
		Close: time.Date(2026, 11, 27, 13, 0, 0, 0, ET),
	}
	b := Bounds(half, cfg)
	if got := PhaseAt(time.Date(2026, 11, 27, 14, 0, 0, 0, ET), b); got != domain.PhaseExtended {
		t.Errorf("PhaseAt(14:00) on a half day = %v, want PhaseExtended from the early close", got)
	}
}

// A pre-market start that does not sit before the session's own open leaves no
// pre-market rather than one overlapping the regular session. The clamp is against
// the calendar's open, not a hardcoded 09:30, so a late-opening day is handled too.
func TestPreMarketStartMustPrecedeTheOpen(t *testing.T) {
	cfg := testCfg()
	cfg.Extended.Enabled = true
	cfg.Extended.Start = "11:00"

	late := Session{
		Date:  "2026-09-28",
		Open:  time.Date(2026, 9, 28, 11, 0, 0, 0, ET),
		Close: time.Date(2026, 9, 28, 16, 0, 0, 0, ET),
	}
	if b := Bounds(late, cfg); !b.ExtendedOpen.IsZero() {
		t.Errorf("ExtendedOpen = %v, want zero when the start is not before the open", b.ExtendedOpen)
	}

	// An unparseable start is treated the same way. Validation rejects it at
	// start-up, so a daemon reaching here should scan less, never at the wrong time.
	cfg.Extended.Start = "half past six"
	if b := Bounds(normalSession(t), cfg); !b.ExtendedOpen.IsZero() {
		t.Errorf("ExtendedOpen = %v, want zero for an unparseable start", b.ExtendedOpen)
	}
}

// Pre-market boundaries are exchange-local, so the DST offset has to come from the
// timezone database rather than a fixed hour — the same reason the package embeds
// tzdata at all.
func TestPreMarketStartTracksDST(t *testing.T) {
	cfg := testCfg()
	cfg.Extended.Enabled = true
	cfg.Extended.Start = "07:00"

	for _, date := range []struct {
		day       time.Time
		wantZone  string
		wantHours int
	}{
		{time.Date(2026, 1, 15, 9, 30, 0, 0, ET), "EST", -5},
		{time.Date(2026, 7, 15, 9, 30, 0, 0, ET), "EDT", -4},
	} {
		sess := Session{
			Date:  date.day.Format("2006-01-02"),
			Open:  date.day,
			Close: date.day.Add(6*time.Hour + 30*time.Minute),
		}
		start := Bounds(sess, cfg).ExtendedOpen
		zone, offset := start.Zone()
		if zone != date.wantZone || offset != date.wantHours*3600 {
			t.Errorf("%s pre-market start is %s (%s, %ds), want %s at %d hours",
				sess.Date, start.Format("15:04"), zone, offset, date.wantZone, date.wantHours)
		}
		if start.Format("15:04") != "07:00" {
			t.Errorf("%s pre-market start = %s, want 07:00 exchange time",
				sess.Date, start.Format("15:04"))
		}
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

// The three windows must not overlap. The regular session is the one the strategy was
// measured on, and an extended window reaching into it would apply the thin-tape
// thresholds — a 0.5x relative-volume floor and a $100,000 turnover floor — to the
// regular market, which admits essentially everything. In the other direction, a
// regular window reaching outside the bells would send a market order to a book that
// takes only limit orders.
//
// Checked by walking the whole day a minute at a time rather than by asserting on the
// boundary fields, because what matters is the answer PhaseAt gives, and that is what
// every caller reads.
func TestExtendedWindowsDoNotOverlapTheRegularSession(t *testing.T) {
	cfg := testCfg()
	cfg.Extended.Enabled = true
	cfg.Extended.Start = "04:00"
	cfg.Extended.End = "20:00"

	sess := normalSession(t)
	b := Bounds(sess, cfg)

	// The boundaries themselves, first: each extended mark has to sit strictly outside
	// the bell it brackets, and the post-market forced exit strictly after the close.
	if !b.ExtendedOpen.Before(sess.Open) {
		t.Errorf("ExtendedOpen %s is not before the open %s",
			b.ExtendedOpen.Format("15:04"), sess.Open.Format("15:04"))
	}
	if !b.ExtendedClose.After(sess.Close) {
		t.Errorf("ExtendedClose %s is not after the close %s",
			b.ExtendedClose.Format("15:04"), sess.Close.Format("15:04"))
	}
	if !b.PostEODExit.After(sess.Close) {
		t.Errorf("PostEODExit %s is not after the close %s",
			b.PostEODExit.Format("15:04"), sess.Close.Format("15:04"))
	}
	// testCfg leaves entry_cutoff_buffer at zero, so these may coincide; what must not
	// happen is the last entry landing after the forced exit, or before the close.
	if b.PostEntryEnd.After(b.PostEODExit) || b.PostEntryEnd.Before(sess.Close) {
		t.Errorf("PostEntryEnd %s must sit between the close %s and the post-market forced exit %s",
			b.PostEntryEnd.Format("15:04"), sess.Close.Format("15:04"), b.PostEODExit.Format("15:04"))
	}

	day := time.Date(2026, 9, 28, 0, 0, 0, 0, ET)
	for i := 0; i < 24*60; i++ {
		now := day.Add(time.Duration(i) * time.Minute)
		phase := PhaseAt(now, b)
		inRegular := !now.Before(sess.Open) && now.Before(sess.Close)

		if inRegular && phase == domain.PhaseExtended {
			t.Fatalf("PhaseAt(%s) = PhaseExtended inside the regular session",
				now.Format("15:04"))
		}
		if !inRegular && (phase == domain.PhaseFirstHour || phase == domain.PhaseTrading) {
			t.Fatalf("PhaseAt(%s) = %v outside the regular session", now.Format("15:04"), phase)
		}
		// The same question the order path asks. Inside the bells an extended-hours
		// flag is rejected; outside them its absence is.
		if got := ExtendedHours(now, b); inRegular && got {
			t.Fatalf("ExtendedHours(%s) is true inside the regular session", now.Format("15:04"))
		}
		if phase == domain.PhaseExtended && !ExtendedHours(now, b) {
			t.Fatalf("ExtendedHours(%s) is false while PhaseExtended", now.Format("15:04"))
		}
		// And the volume window an extended pass sums from never starts inside the
		// regular session either, which is what keeps a post-market relative-volume
		// reading from being the whole day's.
		if phase == domain.PhaseExtended {
			start := ExtendedStart(now, b)
			if start.After(sess.Open) && start.Before(sess.Close) {
				t.Fatalf("ExtendedStart(%s) = %s, inside the regular session",
					now.Format("15:04"), start.Format("15:04"))
			}
		}
	}
}

// A start or end that would overlap the regular session is dropped rather than
// clamped to the bell, so the overlap cannot be reached by misconfiguration either.
// Validation rejects both at start-up; this is the second line.
func TestOverlappingExtendedClocksAreDropped(t *testing.T) {
	sess := normalSession(t)
	for _, tt := range []struct{ name, start, end string }{
		{"a start after the open", "10:00", "20:00"},
		{"a start at the open", "09:30", "20:00"},
		{"an end before the close", "04:00", "15:00"},
		{"an end at the close", "04:00", "16:00"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.Extended.Enabled = true
			cfg.Extended.Start = tt.start
			cfg.Extended.End = tt.end

			b := Bounds(sess, cfg)
			day := time.Date(2026, 9, 28, 0, 0, 0, 0, ET)
			for i := 0; i < 24*60; i++ {
				now := day.Add(time.Duration(i) * time.Minute)
				inRegular := !now.Before(sess.Open) && now.Before(sess.Close)
				if inRegular && PhaseAt(now, b) == domain.PhaseExtended {
					t.Fatalf("PhaseAt(%s) = PhaseExtended with start %s / end %s",
						now.Format("15:04"), tt.start, tt.end)
				}
			}
		})
	}
}
