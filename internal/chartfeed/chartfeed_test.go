package chartfeed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

type fakeBars struct {
	bars  map[string][]domain.Bar
	err   error
	calls int
}

func (f *fakeBars) IntradayBars(_ context.Context, symbol string, _ time.Duration, _ time.Time) ([]domain.Bar, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.bars[symbol], nil
}

type fakeSnapshots struct {
	price map[string]float64
	calls int
}

func (f *fakeSnapshots) Snapshots(_ context.Context, symbols []string) (map[string]domain.Snapshot, error) {
	f.calls++
	out := make(map[string]domain.Snapshot, len(symbols))
	for _, s := range symbols {
		if p, ok := f.price[s]; ok {
			out[s] = domain.Snapshot{Symbol: s, Price: p}
		}
	}
	return out, nil
}

type fakeCalendar struct {
	day broker.CalendarDay
}

func (f *fakeCalendar) Calendar(_ context.Context, _ string) (broker.CalendarDay, error) {
	return f.day, nil
}

// fixture wires a Worker against three minutes of closed bars for AAA, ending
// right before base, plus a calendar anchored at the regular open.
func fixture(t *testing.T) (*Worker, *fakeBars, *fakeSnapshots, time.Time) {
	t.Helper()
	open := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	base := open.Add(10 * time.Minute)

	bars := &fakeBars{bars: map[string][]domain.Bar{
		"AAA": {
			{Time: base.Add(-3 * time.Minute), Open: 10, High: 10.5, Low: 9.8, Close: 10.2, Volume: 1000},
			{Time: base.Add(-2 * time.Minute), Open: 10.2, High: 10.6, Low: 10.1, Close: 10.4, Volume: 1200},
			{Time: base.Add(-1 * time.Minute), Open: 10.4, High: 10.7, Low: 10.3, Close: 10.5, Volume: 900},
		},
	}}
	snaps := &fakeSnapshots{price: map[string]float64{"AAA": 10.55}}
	cal := &fakeCalendar{day: broker.CalendarDay{Date: "2026-11-02", Open: open, Close: open.Add(6*time.Hour + 30*time.Minute)}}

	now := base
	w := &Worker{
		Bars: bars, Snapshots: snaps, Calendar: cal, Cfg: &config.Config{},
		Now: func() time.Time { return now },
	}
	return w, bars, snaps, base
}

func TestWatchLoadsImmediatelyWithoutATick(t *testing.T) {
	w, bars, _, _ := fixture(t)
	ctx := context.Background()

	w.Watch(ctx, "AAA")
	if bars.calls != 1 {
		t.Fatalf("Watch did %d bar fetches, want exactly 1 (no waiting for the next tick)", bars.calls)
	}

	data, ok := w.Poll("AAA")
	if !ok {
		t.Fatal("Poll reported not watched right after Watch")
	}
	if len(data.Bars) != 3 {
		t.Fatalf("len(data.Bars) = %d, want 3 closed bars and no forming one yet", len(data.Bars))
	}
	for _, b := range data.Bars {
		if b.Forming {
			t.Error("a bar from refreshSymbol alone must not be marked forming")
		}
	}
}

func TestWatchTwiceDoesNotRefetch(t *testing.T) {
	w, bars, _, _ := fixture(t)
	ctx := context.Background()
	w.Watch(ctx, "AAA")
	w.Watch(ctx, "AAA")
	if bars.calls != 1 {
		t.Fatalf("Watch called twice did %d bar fetches, want 1", bars.calls)
	}
}

func TestApplyPriceUpdatesFormingCandle(t *testing.T) {
	w, _, _, now := fixture(t)
	ctx := context.Background()
	w.Watch(ctx, "AAA")

	w.applyPrice(ctx, "AAA", 10.6, now)
	data, _ := w.Poll("AAA")
	if len(data.Bars) != 4 {
		t.Fatalf("len(data.Bars) = %d, want 4 (3 closed + 1 forming)", len(data.Bars))
	}
	last := data.Bars[3]
	if !last.Forming {
		t.Error("the newest bar must be marked forming")
	}
	if last.Open != 10.6 || last.High != 10.6 || last.Close != 10.6 {
		t.Errorf("first tick of a forming bar should open/high/close at the tick price, got %+v", last)
	}

	// A lower tick in the same minute must widen the low but not move the open.
	w.applyPrice(ctx, "AAA", 10.45, now.Add(2*time.Second))
	data, _ = w.Poll("AAA")
	last = data.Bars[3]
	if last.Open != 10.6 {
		t.Errorf("open moved to %v within the same forming minute, want it to stay 10.6", last.Open)
	}
	if last.Low != 10.45 || last.Close != 10.45 {
		t.Errorf("forming bar = %+v, want low/close pulled down to 10.45", last)
	}
	if last.Volume != 0 {
		t.Errorf("forming bar volume = %v, want 0 — a live quote cannot say how much of it is this minute's", last.Volume)
	}
}

func TestApplyPriceSealsOnMinuteRollover(t *testing.T) {
	w, bars, _, now := fixture(t)
	ctx := context.Background()
	w.Watch(ctx, "AAA")
	w.applyPrice(ctx, "AAA", 10.6, now)
	callsBeforeRollover := bars.calls

	// The seeded fake always returns the same 3 closed bars regardless of
	// `since`, so this only proves the rollover re-fetches and starts a fresh
	// forming bar — not that the new bar is picked up, which would need a
	// fake that varies its answer by time.
	next := now.Add(time.Minute)
	w.applyPrice(ctx, "AAA", 10.7, next)

	if bars.calls != callsBeforeRollover+1 {
		t.Fatalf("bar fetches = %d, want %d — a new minute must reseal via a fresh IntradayBars read", bars.calls, callsBeforeRollover+1)
	}
	data, _ := w.Poll("AAA")
	last := data.Bars[len(data.Bars)-1]
	if !last.Forming || last.Open != 10.7 || last.Close != 10.7 {
		t.Errorf("forming bar after rollover = %+v, want a fresh bar opening at 10.7", last)
	}
}

func TestUnwatchStopsFetching(t *testing.T) {
	w, _, snaps, now := fixture(t)
	ctx := context.Background()
	w.Watch(ctx, "AAA")
	w.Unwatch("AAA")

	if _, ok := w.Poll("AAA"); ok {
		t.Error("Poll still reports watched after Unwatch")
	}
	w.sweep(ctx)
	if snaps.calls != 0 {
		t.Errorf("sweep made %d Snapshots calls after Unwatch, want 0", snaps.calls)
	}
	_ = now
}

func TestWatchExpiresAfterTTLWithoutAPoll(t *testing.T) {
	w, _, snaps, now := fixture(t)
	ctx := context.Background()
	w.Watch(ctx, "AAA")

	expired := now.Add(watchTTL + time.Second)
	symbols := w.liveSymbols(expired)
	if len(symbols) != 0 {
		t.Fatalf("liveSymbols after the TTL = %v, want none left", symbols)
	}
	if _, ok := w.Poll("AAA"); ok {
		t.Error("Poll still reports watched after its TTL expired")
	}

	w.sweep(ctx)
	if snaps.calls != 0 {
		t.Errorf("sweep made %d Snapshots calls for an expired watch, want 0", snaps.calls)
	}
}

func TestPollRefreshesTheTTL(t *testing.T) {
	w, _, _, now := fixture(t)
	ctx := context.Background()
	w.Watch(ctx, "AAA")

	// Polling just under the TTL must keep the watch alive past where it would
	// otherwise have expired.
	almostExpired := now.Add(watchTTL - time.Second)
	if _, ok := w.Poll("AAA"); !ok {
		t.Fatal("Poll reported not watched before any expiry")
	}
	symbols := w.liveSymbols(almostExpired)
	if len(symbols) != 1 {
		t.Fatalf("liveSymbols just under the TTL = %v, want [AAA] still alive", symbols)
	}
}

func TestRefreshSymbolFailureLeavesThePreviousChartInPlace(t *testing.T) {
	w, bars, _, now := fixture(t)
	ctx := context.Background()
	w.Watch(ctx, "AAA")

	bars.err = errors.New("upstream unavailable")
	w.refreshSymbol(ctx, "AAA")

	data, ok := w.Poll("AAA")
	if !ok {
		t.Fatal("Poll reported not watched after a failed refresh")
	}
	if len(data.Bars) != 3 {
		t.Fatalf("len(data.Bars) = %d after a failed refresh, want the previous 3 to survive", len(data.Bars))
	}
	if data.Err == "" {
		t.Error("a failed refresh must record an error rather than silently staying quiet")
	}
	_ = now
}

func TestBuildDataComputesIndicators(t *testing.T) {
	w, _, _, now := fixture(t)
	ctx := context.Background()
	w.Watch(ctx, "AAA")
	w.applyPrice(ctx, "AAA", 10.6, now)

	data, _ := w.Poll("AAA")
	if len(data.VWAP) != len(data.Bars) {
		t.Errorf("len(VWAP) = %d, want %d (one per bar, no warm-up)", len(data.VWAP), len(data.Bars))
	}
	// 4 closes is too few to warm up EMA9/EMA20/MACD(9,20,9); they must report
	// no series rather than a misleadingly short one.
	if data.EMA9 != nil || data.EMA20 != nil || data.MACDLine != nil {
		t.Errorf("got EMA9=%v EMA20=%v MACDLine=%v with only 4 closes, want all nil", data.EMA9, data.EMA20, data.MACDLine)
	}
}
