package web

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

// closeLosingPosition inserts and closes a position at a loss, returning its id so
// a test can save a review against it or query for the "Why?" button.
func (f *fixture) closeLosingPosition(t *testing.T, symbol string) int64 {
	t.Helper()
	at := f.now.Add(-time.Hour)
	id, err := f.store.InsertPosition(domain.Position{
		SessionDate: f.date, Symbol: symbol, Shares: 100,
		EntryPrice: 5.00, EntryTime: at, PeakPrice: 5.00,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ClosePosition(id, 4.80, at.Add(10*time.Minute), domain.ExitStopLoss); err != nil {
		t.Fatal(err)
	}
	return id
}

// A winning trade never gets a "Why?" button or an "analyzing…" placeholder — a
// review never runs for one in the first place.
func TestWinningTradeHasNoReviewAffordance(t *testing.T) {
	f := newFixture(t)
	at := f.now.Add(-time.Hour)
	id, err := f.store.InsertPosition(domain.Position{
		SessionDate: f.date, Symbol: "WINS", Shares: 100,
		EntryPrice: 5.00, EntryTime: at, PeakPrice: 5.50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ClosePosition(id, 5.50, at.Add(10*time.Minute), domain.ExitStopLoss); err != nil {
		t.Fatal(err)
	}

	_, body := f.get(t, "/")
	if strings.Contains(body, "data-review=") || strings.Contains(body, "analyzing…") {
		t.Error("a winning trade should not show any review affordance")
	}
}

// A loss with no review yet shows a pending placeholder, not a button — clicking
// something that fetches nothing yet would just render "not reviewed yet" in the
// modal, which is a worse experience than not showing a button at all.
func TestLosingTradeWithNoReviewYetShowsPending(t *testing.T) {
	f := newFixture(t)
	f.closeLosingPosition(t, "LOSS")

	_, body := f.get(t, "/")
	if !strings.Contains(body, "analyzing…") {
		t.Error("expected a pending placeholder for a loss with no review yet")
	}
	if strings.Contains(body, "data-review=") {
		t.Error("should not show a button before a review exists")
	}
}

// Once a review exists, the row gets the button, and clicking it (the GET the
// button's JS performs) renders the stored report.
func TestLosingTradeWithReviewShowsButtonAndReport(t *testing.T) {
	f := newFixture(t)
	id := f.closeLosingPosition(t, "LOSS")
	if err := f.store.SaveTradeReview(id, "the stop did its job", ""); err != nil {
		t.Fatal(err)
	}

	_, body := f.get(t, "/")
	if !strings.Contains(body, `data-review="`) {
		t.Error("expected a review button once a review exists")
	}

	code, modalBody := f.get(t, "/actions/trade-review?id="+strconv.FormatInt(id, 10))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(modalBody, "the stop did its job") {
		t.Errorf("modal body = %s, want the stored report", modalBody)
	}
	if !strings.Contains(modalBody, "LOSS") {
		t.Error("modal should name the symbol")
	}
}

func TestFailedReviewShowsTheErrorInTheModal(t *testing.T) {
	f := newFixture(t)
	id := f.closeLosingPosition(t, "LOSS")
	if err := f.store.SaveTradeReview(id, "", "claude: not logged in"); err != nil {
		t.Fatal(err)
	}

	code, body := f.get(t, "/actions/trade-review?id="+strconv.FormatInt(id, 10))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "not logged in") {
		t.Errorf("body = %s, want the failure reason surfaced", body)
	}
}

func TestTradeReviewForAnUnreviewedPositionSaysSo(t *testing.T) {
	f := newFixture(t)
	id := f.closeLosingPosition(t, "LOSS")

	code, body := f.get(t, "/actions/trade-review?id="+strconv.FormatInt(id, 10))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "not reviewed") {
		t.Errorf("body = %s, want it to say there is no review yet", body)
	}
}

func TestTradeReviewForAnUnknownPositionIs404ish(t *testing.T) {
	f := newFixture(t)
	code, body := f.get(t, "/actions/trade-review?id=999999")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the error renders in the modal)", code)
	}
	if !strings.Contains(body, "could not be found") {
		t.Errorf("body = %s, want a not-found message", body)
	}
}

func TestTradeReviewRejectsAGarbageID(t *testing.T) {
	f := newFixture(t)
	code, body := f.get(t, "/actions/trade-review?id=not-a-number")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the error renders in the modal)", code)
	}
	if !strings.Contains(body, "not valid") {
		t.Errorf("body = %s, want a validation message", body)
	}
}
