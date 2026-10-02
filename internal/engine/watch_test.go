package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/risk"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// The setup is read on a closed candle; by the time the order goes the price has
// moved. On 2026-10-02 QTEX was sized at 1.135 and filled at 1.20, carrying 2.8x the
// risk budget. A price that has run past the trigger is now refused before ordering.
func TestEntryRefusedWhenPriceHasRunPastTheTrigger(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)
	h.fake.SetPrice("ABCD", 5.30) // 6% past the 5.00 trigger close

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	if got := len(h.openPositions()); got != 0 {
		t.Fatalf("bought %d positions 6%% past the trigger", got)
	}
	if placed := h.fake.Placed(); len(placed) != 0 {
		t.Fatalf("placed %d orders, want none", len(placed))
	}
	evals, _, err := h.store.LatestScreenSnapshot(h.date)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evals {
		if e.Symbol == "ABCD" && !strings.Contains(e.Outcome, "price moved") {
			t.Errorf("outcome = %q, want it to say the price moved off the setup", e.Outcome)
		}
	}
}

// And SDEV's case: price already back under the stop when the order would go.
func TestEntryRefusedWhenPriceIsUnderTheStop(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)
	h.fake.SetPrice("ABCD", 4.50) // still +14%-ish on the day, but under the pause

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	if got := len(h.openPositions()); got != 0 {
		t.Fatalf("bought %d positions under the setup's stop", got)
	}
}

// Inside the drift allowance the trade goes ahead, sized from the price it can get
// rather than the trigger close, so the risk taken is the risk budgeted.
func TestEntrySizedFromTheLivePrice(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)
	h.fake.SetPrice("ABCD", 5.04) // 0.8% past the trigger, inside the 1% allowance

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	pos := h.openPositions()
	if len(pos) != 1 {
		t.Fatalf("got %d positions, want 1", len(pos))
	}
	p := pos[0]
	want := risk.SizeForRisk(domain.Account{PortfolioValue: 100_000, Cash: 100_000, Equity: 100_000},
		5.04, p.StopPrice, h.cfg)
	if p.Shares != want.Shares {
		t.Errorf("bought %d shares, want %d: sized from the live 5.04, not the 5.00 trigger",
			p.Shares, want.Shares)
	}
}

// A candidate's chart is read once per completed candle, not on every tick.
func TestSetupReadOncePerCandle(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addScreenedOnly("NOPE", 5.00)
	calls := &countingBars{Fake: h.fake}
	h.eng.data = calls

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()
	first := calls.n
	if first == 0 {
		t.Fatal("the setup check never read the chart")
	}
	// Past the grace for the candle that closed at 10:35, still inside the next one.
	for _, s := range []int{12, 20, 40, 58} {
		h.now = time.Date(2026, 9, 28, 10, 35, s, 0, scheduler.ET)
		h.tick()
	}
	if calls.n != first {
		t.Errorf("chart read %d more times within one candle, want 0", calls.n-first)
	}
	h.now = time.Date(2026, 9, 28, 10, 36, 1, 0, scheduler.ET)
	h.tick()
	if calls.n == first {
		t.Error("the next candle's close must be read")
	}
}

type countingBars struct {
	*broker.Fake
	n int
}

func (c *countingBars) IntradayBars(ctx context.Context, symbol string, interval time.Duration, since time.Time) ([]domain.Bar, error) {
	c.n++
	return c.Fake.IntradayBars(ctx, symbol, interval, since)
}

// The point of moving the screen off the tick: while a screen is running, stops are
// still checked. Before, a tick that started a screen did not return until the screen
// finished, so a stop could go unwatched for most of a minute.
func TestStopsAreCheckedWhileAScreenRuns(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	clock := &lockedClock{t: time.Date(2026, 9, 28, 9, 30, 0, 0, scheduler.ET)}
	blocking := &blockingUniverse{Fake: h.fake, release: make(chan struct{})}
	h.eng = New(Deps{
		Config: h.cfg, Store: h.store, Data: blocking, Trading: h.fake,
		Logger: discardLogger(), Now: clock.now,
	})
	h.eng.background = true
	ctx := context.Background()

	h.eng.Tick(ctx) // 09:30, sentiment
	clock.set(time.Date(2026, 9, 28, 10, 25, 0, 0, scheduler.ET))
	h.eng.Tick(ctx)

	// A held position whose stop is about to be hit.
	if _, err := h.store.InsertPosition(domain.Position{
		SessionDate: h.date, Symbol: "HELD", Shares: 100, EntryPrice: 10,
		EntryTime: clock.now(), PeakPrice: 10, StopPrice: 9.5, InitialRisk: 0.5,
	}); err != nil {
		t.Fatal(err)
	}
	h.fake.SetSnapshot("HELD", 10, 9, 1_000_000)

	clock.set(time.Date(2026, 9, 28, 10, 35, 0, 0, scheduler.ET))
	returned := make(chan struct{})
	go func() {
		h.eng.Tick(ctx) // starts the screen, which blocks on the universe
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		close(blocking.release)
		t.Fatal("Tick waited on the screen")
	}

	h.fake.SetPrice("HELD", 9.40)
	clock.set(time.Date(2026, 9, 28, 10, 35, 2, 0, scheduler.ET))
	h.eng.Tick(ctx)
	for _, p := range h.openPositions() {
		if p.Symbol == "HELD" {
			close(blocking.release)
			t.Fatal("the stop was not checked while the screen ran")
		}
	}
	if len(h.openPositions()) != 0 {
		t.Fatal("bought before the screen had finished")
	}

	// Once the screen publishes, the next tick acts on it.
	close(blocking.release)
	h.eng.screens.Wait()
	clock.set(time.Date(2026, 9, 28, 10, 35, 4, 0, scheduler.ET))
	h.eng.Tick(ctx)
	pos := h.openPositions()
	if len(pos) != 1 || pos[0].Symbol != "ABCD" {
		t.Fatalf("open positions = %+v, want ABCD bought from the published watchlist", pos)
	}
}

type lockedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lockedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lockedClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type blockingUniverse struct {
	*broker.Fake
	release chan struct{}
}

func (b *blockingUniverse) TradableAssets(ctx context.Context) ([]string, error) {
	<-b.release
	return b.Fake.TradableAssets(ctx)
}
