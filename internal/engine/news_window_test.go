package engine

import (
	"context"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// The catalyst behind a gap-up almost always breaks overnight or pre-market, so the
// news window must start before the open. Searching from 09:30 would report "no
// news" for exactly the candidates the strategy is looking for.
func TestNewsWindowReachesBackBeforeTheOpen(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("GAPPER", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	since := h.fake.NewsSince()
	if since.IsZero() {
		t.Fatal("no news query was made")
	}
	if !since.Before(h.open) {
		t.Errorf("news searched from %s, which is at or after the %s open: a pre-market catalyst would be invisible",
			since.In(scheduler.ET).Format("15:04"), h.open.In(scheduler.ET).Format("15:04"))
	}
	// 18h back from 10:35 lands the previous afternoon, covering the prior evening
	// and this morning's pre-market.
	if want := h.now.Add(-18 * time.Hour); !since.Equal(want) {
		t.Errorf("news searched from %v, want %v (now minus the configured lookback)", since, want)
	}
}

// The manual button must use the same window, including when the market is closed —
// otherwise the two paths would disagree about whether a candidate has a catalyst.
func TestManualScreenUsesTheSameNewsWindow(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("GAPPER", 5.00)

	h.at(20, 0) // after the close
	h.tick()

	if _, err := h.eng.ScreenNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	since := h.fake.NewsSince()
	if want := h.now.Add(-18 * time.Hour); !since.Equal(want) {
		t.Errorf("manual screen searched news from %v, want %v", since, want)
	}
	if !since.Before(h.open) {
		t.Error("the manual window must also reach back past the open")
	}
}
