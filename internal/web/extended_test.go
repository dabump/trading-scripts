package web

import (
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// enableExtended mirrors the shipped config's extended-hours section.
func (f *fixture) enableExtended() {
	f.cfg.Extended.Enabled = true
	f.cfg.Extended.Start = "07:00"
	f.cfg.Extended.End = "20:00"
	f.cfg.Extended.ScanInterval = 5 * time.Minute
	f.cfg.Extended.MinDollarVolume = 100_000
	f.cfg.Extended.MinVolumeMultiple = 0.5
}

// The exchange badge is a three-way state, not a boolean. Before the bell with
// extended hours on it has to say so, in its own colour: "CLOSED" would misreport an
// agent that is scanning, and "OPEN" would claim a session that has not started.
//
// The badge names the exchange's own session rather than the agent's state, which is
// why it still distinguishes the two halves the single EXTENDED_MARKET state covers.
func TestExchangeBadgeShowsPreMarket(t *testing.T) {
	f := newFixture(t)
	f.enableExtended()
	f.eng.state = domain.StateExtendedMarket
	f.now = time.Date(2026, 9, 28, 7, 30, 0, 0, scheduler.ET)

	code, body := f.get(t, "/")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "Exchange PRE-MARKET") {
		t.Error("the exchange badge must read PRE-MARKET before the bell")
	}
	if !strings.Contains(body, `class="badge pre"`) {
		t.Error("the extended-hours badge must carry its own tone class")
	}
	// The page still counts down to the regular open, which is the boundary that
	// matters: it is when the sentiment gate and ordinary entries begin.
	if !strings.Contains(body, "opens in") {
		t.Error("the countdown to the regular open must still be shown")
	}
}

// The other half of the same badge. After the close the agent is scanning the
// post-market tape, so "CLOSED" would be just as wrong there as it is at 07:30.
func TestExchangeBadgeShowsPostMarket(t *testing.T) {
	f := newFixture(t)
	f.enableExtended()
	f.eng.state = domain.StateExtendedMarket
	f.now = time.Date(2026, 9, 28, 17, 30, 0, 0, scheduler.ET)

	_, body := f.get(t, "/")
	if !strings.Contains(body, "Exchange POST-MARKET") {
		t.Error("the exchange badge must read POST-MARKET after the close")
	}
	if !strings.Contains(body, `class="badge pre"`) {
		t.Error("the extended-hours badge must carry its own tone class after the close too")
	}
	if !strings.Contains(body, string(domain.StateExtendedMarket)) {
		t.Error("the agent state must read EXTENDED_MARKET in both halves")
	}
}

// Past the configured end the day is over, whatever the badge said an hour earlier.
func TestExchangeBadgeClosesAfterTheExtendedEnd(t *testing.T) {
	f := newFixture(t)
	f.enableExtended()
	f.now = time.Date(2026, 9, 28, 20, 30, 0, 0, scheduler.ET)

	_, body := f.get(t, "/")
	if !strings.Contains(body, "Exchange CLOSED") {
		t.Error("past extended.end the badge must read CLOSED")
	}
}

// The colour has to exist, or the tone class renders as unstyled text.
func TestExtendedToneIsDefined(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/")
	for _, want := range []string{"--pre:", ".badge.pre {"} {
		if !strings.Contains(body, want) {
			t.Errorf("stylesheet is missing %q, so the extended-hours tone would render unstyled", want)
		}
	}
}

// With extended hours off the badge is exactly what it always was, at both ends of
// the day.
func TestExchangeBadgeUnchangedWithoutExtendedHours(t *testing.T) {
	f := newFixture(t)
	for _, at := range []time.Time{
		time.Date(2026, 9, 28, 7, 30, 0, 0, scheduler.ET),
		time.Date(2026, 9, 28, 17, 30, 0, 0, scheduler.ET),
	} {
		f.now = at
		_, body := f.get(t, "/")
		if !strings.Contains(body, "Exchange CLOSED") {
			t.Errorf("at %s with extended hours disabled the badge must still read CLOSED",
				at.Format("15:04"))
		}
		if strings.Contains(body, `class="badge pre"`) {
			t.Errorf("at %s the extended tone must not appear when extended hours are off",
				at.Format("15:04"))
		}
	}
}

// EXTENDED_MARKET is a documented state, so it belongs in the legend with a meaning
// and an icon — docs/web-ui.md requires states to be distinguishable without colour.
func TestLegendIncludesExtendedMarket(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/")
	if !strings.Contains(body, string(domain.StateExtendedMarket)) {
		t.Fatal("the legend must list EXTENDED_MARKET")
	}

	entries := legendFor(f.cfg, "")
	var entry LegendEntry
	for _, l := range entries {
		if l.State == domain.StateExtendedMarket {
			entry = l
		}
	}
	if entry.Tone != "pre" || entry.Icon == "" || entry.Meaning == "" {
		t.Errorf("EXTENDED_MARKET legend entry = %+v, want its own tone, icon and meaning", entry)
	}
	// Icons carry the distinction when colour cannot, so no two states may share one.
	seen := map[string]domain.AgentState{}
	for _, l := range entries {
		if other, dup := seen[l.Icon]; dup {
			t.Errorf("%s and %s share the icon %q", other, l.State, l.Icon)
		}
		seen[l.Icon] = l.State
	}
}

// The state's description has to name the hours it covers. "Extended hours" alone
// leaves an operator guessing which ones, and the answer is config — a reader should
// not have to open config.yaml to find out when the agent is awake.
func TestLegendStatesTheExtendedHours(t *testing.T) {
	f := newFixture(t)
	f.enableExtended()

	_, body := f.get(t, "/")
	// The inner bounds come from the session the calendar reported, not from an
	// assumed 09:30/16:00, so an early close is reflected rather than papered over.
	want := "07:00 – 09:30 and 16:00 – 20:00 ET"
	if !strings.Contains(body, want) {
		t.Errorf("the page must state the extended-hours windows (%q)", want)
	}

	var meaning string
	for _, l := range legendFor(f.cfg, extendedWindowText(f.cfg, f.eng.session, true)) {
		if l.State == domain.StateExtendedMarket {
			meaning = l.Meaning
		}
	}
	if !strings.Contains(meaning, want) {
		t.Errorf("EXTENDED_MARKET meaning = %q, want it to carry the configured hours", meaning)
	}
}

// With the section off there are no hours to advertise, and the legend must not
// invent any.
func TestLegendOmitsTheHoursWhenExtendedIsOff(t *testing.T) {
	f := newFixture(t)
	if text := extendedWindowText(f.cfg, f.eng.session, true); text != "" {
		t.Errorf("extendedWindowText = %q, want empty with extended hours disabled", text)
	}
}

// The strategy panel is built from config, so it has to say whether extended hours
// are on — that fact explains an empty 06:00 page without a trip to the config file.
func TestStrategyPanelReportsExtendedHours(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/")
	if !strings.Contains(body, "Extended hours") {
		t.Fatal("the strategy panel must have an extended-hours section")
	}
	if !strings.Contains(body, "disabled") {
		t.Error("with extended hours off the panel must say so")
	}

	f.enableExtended()
	_, body = f.get(t, "/")
	for _, want := range []string{
		"07:00 ET – the open", "the close – 20:00 ET",
		"5m", "$100,000", "0.5x", "blocked",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel is missing %q; it must render the configured values", want)
		}
	}
}
