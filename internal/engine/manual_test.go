package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// The manual sentiment check must not write a reading: the gate verdict is decided
// by the newest stored reading, so persisting one would let a click overturn what
// the first hour concluded.
func TestCheckSentimentPersistsNothing(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.at(14, 0)

	check, err := h.eng.CheckSentiment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if check.Classification != domain.VerdictProceed {
		t.Errorf("classification = %q, want PROCEED", check.Classification)
	}
	if got := len(check.Percentages); got != 3 {
		t.Errorf("got %d percentages, want 3", got)
	}

	readings, err := h.store.SentimentReadings(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 0 {
		t.Errorf("got %d stored readings, want 0: a manual check must not be recorded", len(readings))
	}
	rec, _ := h.store.Session(h.date)
	if rec.Verdict != domain.VerdictPending {
		t.Errorf("session verdict = %q, want it untouched at PENDING", rec.Verdict)
	}
}

func TestCheckSentimentReportsBearishWithoutHalting(t *testing.T) {
	h := newHarness(t)
	h.setBearish()
	h.at(14, 0)

	check, err := h.eng.CheckSentiment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if check.Classification != domain.VerdictBearish {
		t.Fatalf("classification = %q, want OVERWHELMINGLY_BEARISH", check.Classification)
	}
	// Reporting a bearish reading must not itself halt the session.
	rec, _ := h.store.Session(h.date)
	if rec.Halted {
		t.Error("a manual check must never halt the session")
	}
}

// Missing data is reported rather than silently reducing the basket.
func TestCheckSentimentReportsMissingSymbols(t *testing.T) {
	h := newHarness(t)
	h.fake.SetSnapshot("SPY", 101, 100, 1_000_000)
	h.at(14, 0)

	check, err := h.eng.CheckSentiment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Missing) != 2 {
		t.Errorf("missing = %v, want QQQ and IWM listed", check.Missing)
	}
}

// The whole point of the manual screen: it must never reach the order path, even
// with a fully qualifying candidate during trading hours with the gate open.
func TestScreenNowNeverPlacesOrders(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	// Open the gate legitimately so the automated path *would* buy.
	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()

	// Flatten anything the loop opened so the count below is unambiguous.
	for _, p := range h.openPositions() {
		if err := h.store.ClosePosition(p.ID, p.EntryPrice, h.now, domain.ExitForcedEOD); err != nil {
			t.Fatal(err)
		}
	}
	before := len(h.fake.Placed())

	h.at(11, 0)
	preview, err := h.eng.ScreenNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Evaluations) != 1 {
		t.Fatalf("got %d evaluations, want 1", len(preview.Evaluations))
	}
	if !preview.Evaluations[0].Qualifies {
		t.Errorf("candidate should pass the three checkable criteria: %+v", preview.Evaluations[0])
	}
	if got := len(h.fake.Placed()); got != before {
		t.Errorf("placed %d new orders, want 0: the manual screen must never trade", got-before)
	}
}

// It must also leave the page's screening snapshot alone, so the displayed table
// still describes the automated loop rather than the last button press.
func TestScreenNowPersistsNothing(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)
	h.at(11, 0)

	if _, err := h.eng.ScreenNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	evals, _, err := h.store.LatestScreenSnapshot(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if evals != nil {
		t.Errorf("got a stored snapshot, want none: the manual screen must not persist")
	}
}

// The manual screen must apply exactly the criteria the automated scan does, so
// that "qualifies" in the modal means the agent really would act on it.
func TestScreenNowUsesTheSameCriteriaAsTheAutomatedScan(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	// A tick first, so the session is loaded for both paths to read.
	h.at(11, 0)
	h.tick()

	preview, err := h.eng.ScreenNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manual := preview.Evaluations[0]
	if got := len(manual.Criteria); got != 3 {
		t.Fatalf("got %d criteria, want 3", got)
	}
	if !manual.Qualifies {
		t.Errorf("candidate should qualify outright: %+v", manual)
	}

	// The automated path must reach the same verdict on the same data.
	auto, err := h.eng.screen(context.Background(), mustSession(t, h))
	if err != nil {
		t.Fatal(err)
	}
	if len(auto) != 1 || auto[0].Qualifies != manual.Qualifies {
		t.Errorf("automated verdict %+v disagrees with the manual one %+v", auto, manual)
	}
	if len(auto[0].Criteria) != len(manual.Criteria) {
		t.Errorf("automated path evaluated %d criteria, manual %d — they must match",
			len(auto[0].Criteria), len(manual.Criteria))
	}
}

func mustSession(t *testing.T, h *harness) scheduler.Session {
	t.Helper()
	sess, ok := h.eng.Session()
	if !ok {
		t.Fatal("expected a trading session to be loaded")
	}
	return sess
}

// Running with the exchange closed is the explicitly requested case.
func TestScreenNowWorksWhileMarketClosed(t *testing.T) {
	h := newHarness(t)
	h.fake.SetCalendar(broker.CalendarDay{}) // no session today
	h.addCandidate("ABCD", 5.00)

	h.at(20, 0)
	h.tick() // loads the (absent) session
	h.wantState(domain.StateMarketClosed)

	preview, err := h.eng.ScreenNow(context.Background())
	if err != nil {
		t.Fatalf("screening must work with the market closed: %v", err)
	}
	if preview.MarketOpen {
		t.Error("MarketOpen must be false so the UI can warn the data is stale")
	}
	if len(preview.Evaluations) != 1 {
		t.Fatalf("got %d evaluations, want 1", len(preview.Evaluations))
	}
	if got := len(h.fake.Placed()); got != 0 {
		t.Errorf("placed %d orders with the market closed, want 0", got)
	}
}

// Results are ranked strongest first but failures are kept: seeing what nearly
// qualified is the point of the view.
func TestScreenNowRanksAndKeepsFailures(t *testing.T) {
	h := newHarness(t)
	h.setBullish()

	h.addCandidate("WEAK", 5.00)
	h.fake.SetAverageVolume("WEAK", 1_200_000) // ~5.1x

	h.addCandidate("STRONG", 5.00)
	h.fake.SetAverageVolume("STRONG", 300_000) // ~20x

	h.addCandidate("NONEWS", 5.00)
	h.fake.SetNews("NONEWS", 0)

	h.at(11, 0)

	preview, err := h.eng.ScreenNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Evaluations) != 3 {
		t.Fatalf("got %d evaluations, want all 3 including the failure", len(preview.Evaluations))
	}
	if preview.Evaluations[0].Symbol != "STRONG" {
		t.Errorf("first = %s, want STRONG (highest relative volume)", preview.Evaluations[0].Symbol)
	}
	last := preview.Evaluations[2]
	if last.Symbol != "NONEWS" || last.Qualifies {
		t.Errorf("last = %+v, want the failing NONEWS row ranked last but still present", last)
	}
}

func TestManualActionsRejectConcurrentRuns(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.at(11, 0)

	h.eng.manual.sentiment.Lock()
	_, err := h.eng.CheckSentiment(context.Background())
	h.eng.manual.sentiment.Unlock()
	if !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy while a check is in flight", err)
	}

	h.eng.manual.screen.Lock()
	_, err = h.eng.ScreenNow(context.Background())
	h.eng.manual.screen.Unlock()
	if !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy while a screen is in flight", err)
	}

	// The guards are independent: a running screen must not block a sentiment check.
	h.eng.manual.screen.Lock()
	defer h.eng.manual.screen.Unlock()
	if _, err := h.eng.CheckSentiment(context.Background()); err != nil {
		t.Errorf("sentiment check blocked by an unrelated screen: %v", err)
	}
}

// A manual check must not put the agent into ERROR: a failed button press is the
// clicker's problem, not a fault in the trading loop.
func TestManualFailureDoesNotChangeAgentState(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.at(9, 40)
	h.tick()
	h.wantState(domain.StateSentimentCheck)

	h.fake.SetError(errors.New("upstream 503"))
	if _, err := h.eng.CheckSentiment(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := h.eng.ScreenNow(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	if state, _ := h.eng.State(); state != domain.StateSentimentCheck {
		t.Errorf("state = %q, want it unchanged at SENTIMENT_CHECK", state)
	}
}

// The manual check reports the clock it was asked at, which the modal displays.
func TestCheckSentimentUsesInjectedClock(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.at(15, 45)

	check, err := h.eng.CheckSentiment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !check.TakenAt.Equal(h.now) {
		t.Errorf("TakenAt = %v, want %v", check.TakenAt, h.now)
	}
	_ = time.Now
}
