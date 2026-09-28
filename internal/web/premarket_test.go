package web

import (
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// enablePreMarket mirrors the shipped config's pre-market section.
func (f *fixture) enablePreMarket() {
	f.cfg.PreMarket.Enabled = true
	f.cfg.PreMarket.Start = "07:00"
	f.cfg.PreMarket.ScanInterval = 5 * time.Minute
	f.cfg.PreMarket.MinDollarVolume = 100_000
	f.cfg.PreMarket.MinVolumeMultiple = 0.5
}

// The exchange badge is a three-way state, not a boolean. Before the bell with
// pre-market on it has to say so, in its own colour: "CLOSED" would misreport an
// agent that is scanning, and "OPEN" would claim a session that has not started.
func TestExchangeBadgeShowsPreMarket(t *testing.T) {
	f := newFixture(t)
	f.enablePreMarket()
	f.eng.state = domain.StatePreMarket
	f.now = time.Date(2026, 9, 28, 7, 30, 0, 0, scheduler.ET)

	code, body := f.get(t, "/")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "Exchange PRE-MARKET") {
		t.Error("the exchange badge must read PRE-MARKET before the bell")
	}
	if !strings.Contains(body, `class="badge pre"`) {
		t.Error("the pre-market badge must carry its own tone class")
	}
	// The page still counts down to the regular open, which is the boundary that
	// matters: it is when the sentiment gate and ordinary entries begin.
	if !strings.Contains(body, "opens in") {
		t.Error("the countdown to the regular open must still be shown")
	}
}

// The colour has to exist, or the tone class renders as unstyled text.
func TestPreMarketToneIsDefined(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/")
	for _, want := range []string{"--pre:", ".badge.pre {"} {
		if !strings.Contains(body, want) {
			t.Errorf("stylesheet is missing %q, so the pre-market tone would render unstyled", want)
		}
	}
}

// With pre-market off the badge is exactly what it always was.
func TestExchangeBadgeUnchangedWithoutPreMarket(t *testing.T) {
	f := newFixture(t)
	f.now = time.Date(2026, 9, 28, 7, 30, 0, 0, scheduler.ET)

	_, body := f.get(t, "/")
	if !strings.Contains(body, "Exchange CLOSED") {
		t.Error("with pre-market disabled the badge must still read CLOSED before the open")
	}
	if strings.Contains(body, `class="badge pre"`) {
		t.Error("the pre-market tone must not appear when pre-market is off")
	}
}

// PRE_MARKET is a documented state, so it belongs in the legend with a meaning and
// an icon — docs/web-ui.md requires states to be distinguishable without colour.
func TestLegendIncludesPreMarket(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/")
	if !strings.Contains(body, string(domain.StatePreMarket)) {
		t.Fatal("the legend must list PRE_MARKET")
	}

	var entry LegendEntry
	for _, l := range legend {
		if l.State == domain.StatePreMarket {
			entry = l
		}
	}
	if entry.Tone != "pre" || entry.Icon == "" || entry.Meaning == "" {
		t.Errorf("PRE_MARKET legend entry = %+v, want its own tone, icon and meaning", entry)
	}
	// Icons carry the distinction when colour cannot, so no two states may share one.
	seen := map[string]domain.AgentState{}
	for _, l := range legend {
		if other, dup := seen[l.Icon]; dup {
			t.Errorf("%s and %s share the icon %q", other, l.State, l.Icon)
		}
		seen[l.Icon] = l.State
	}
}

// The strategy panel is built from config, so it has to say whether pre-market is on
// — that fact explains an empty 06:00 page without a trip to the config file.
func TestStrategyPanelReportsPreMarket(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/")
	if !strings.Contains(body, "Pre-market") {
		t.Fatal("the strategy panel must have a pre-market section")
	}
	if !strings.Contains(body, "disabled") {
		t.Error("with pre-market off the panel must say so")
	}

	f.enablePreMarket()
	_, body = f.get(t, "/")
	for _, want := range []string{"07:00 ET", "5m", "$100,000", "0.5x", "blocked"} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel is missing %q; it must render the configured values", want)
		}
	}
}
