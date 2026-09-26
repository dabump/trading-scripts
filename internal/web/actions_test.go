package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

func TestButtonsRenderOnPage(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/")

	for _, want := range []string{
		"Check sentiment", "Screen candidates",
		`data-action="actions/sentiment"`, `data-action="actions/screen"`,
		"read-only", // the buttons say what they do not do
		`<dialog class="modal"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

// These endpoints trigger upstream work, so a GET (a prefetch, a crawler, a
// pasted link) must not be able to set them off.
func TestActionsRejectGET(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/actions/sentiment", "/actions/screen"} {
		code, _ := f.get(t, path)
		if code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, code)
		}
	}
	if f.actions.calls != 0 {
		t.Errorf("a GET reached the engine %d times, want 0", f.actions.calls)
	}
}

func TestSentimentModalRendersReading(t *testing.T) {
	f := newFixture(t)
	f.actions.check = domain.SentimentCheck{
		TakenAt:        f.now,
		Percentages:    map[string]float64{"SPY": -1.42, "QQQ": -1.61, "IWM": -1.9},
		Classification: domain.VerdictBearish,
	}

	code, body := f.post(t, "/actions/sentiment")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, want := range []string{
		"Sentiment check",
		"OVERWHELMINGLY_BEARISH",
		"-1.42%", "-1.61%", "-1.90%",
		"would halt trading",
		// The modal must say the check changed nothing.
		"cannot change the session's gate",
		"bearish at avg ≤ -0.8%",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sentiment modal missing %q", want)
		}
	}
}

func TestSentimentModalShowsMissingData(t *testing.T) {
	f := newFixture(t)
	f.actions.check = domain.SentimentCheck{
		TakenAt:        f.now,
		Percentages:    map[string]float64{"SPY": 0.4},
		Classification: domain.VerdictProceed,
		Missing:        []string{"QQQ", "IWM"},
	}

	_, body := f.post(t, "/actions/sentiment")
	if !strings.Contains(body, "No usable data for: QQQ, IWM") {
		t.Errorf("missing symbols not reported: %s", body)
	}
	// Symbols without data render as a dash rather than a misleading 0.00%.
	if !strings.Contains(body, "—") {
		t.Error("absent values should render as a dash")
	}
}

func screenPreview(now time.Time, marketOpen bool) domain.ScreenPreview {
	return domain.ScreenPreview{
		TakenAt:      now,
		UniverseSize: 50,
		MarketOpen:   marketOpen,
		Evaluations: []domain.Evaluation{
			{
				Symbol: "ABCD", Qualifies: true, VolumeMultiple: 6.1,
				Criteria: []domain.Criterion{
					{Name: "News catalyst", Pass: true, Display: "2 today"},
					{Name: "Intraday move", Pass: true, Display: "+14.0%"},
					{Name: "Rel. volume", Pass: true, Display: "6.1x"},
				},
			},
			{
				Symbol: "WXYZ", Qualifies: false, FailReason: "fails: News catalyst",
				Criteria: []domain.Criterion{
					{Name: "News catalyst", Pass: false, Display: "0 today"},
					{Name: "Intraday move", Pass: true, Display: "+11.0%"},
					{Name: "Rel. volume", Pass: true, Display: "5.3x"},
				},
			},
		},
	}
}

func TestScreenModalRendersCandidates(t *testing.T) {
	f := newFixture(t)
	f.actions.preview = screenPreview(f.now, true)

	code, body := f.post(t, "/actions/screen")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, want := range []string{
		"Candidate screen",
		"ABCD", "WXYZ",
		"2 today", "+14.0%", "6.1x", "0 today",
		"fails: News catalyst",
		"Scanned 50 symbols",
		"2 cleared the move filter",
		"1 qualified",
		"nothing was ordered or saved",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("screen modal missing %q", want)
		}
	}
}

// The manual screen applies the same criteria as the automated scan, so
// "qualifies" is the honest word: the agent really would act on such a row during
// trading hours.
func TestScreenModalReportsQualifyingCandidates(t *testing.T) {
	f := newFixture(t)
	f.actions.preview = screenPreview(f.now, true)

	_, body := f.post(t, "/actions/screen")
	if !strings.Contains(body, "Qualifies") {
		t.Error("a qualifying row should be labelled as qualifying")
	}
	if !strings.Contains(body, "1 qualified") {
		t.Error("expected the qualifying count in the summary line")
	}
}

// Running with the exchange closed is supported, but the modal has to say the
// numbers describe the previous session.
func TestScreenModalWarnsWhenMarketClosed(t *testing.T) {
	f := newFixture(t)
	f.actions.preview = screenPreview(f.now, false)

	_, body := f.post(t, "/actions/screen")
	if !strings.Contains(body, "exchange is closed") {
		t.Error("expected a stale-data warning when the market is closed")
	}

	f.actions.preview = screenPreview(f.now, true)
	_, body = f.post(t, "/actions/screen")
	if strings.Contains(body, "exchange is closed") {
		t.Error("the stale-data warning must not show while the market is open")
	}
}

func TestScreenModalHandlesEmptyUniverse(t *testing.T) {
	f := newFixture(t)
	f.actions.preview = domain.ScreenPreview{TakenAt: f.now, MarketOpen: true}

	_, body := f.post(t, "/actions/screen")
	if !strings.Contains(body, "movers list came back empty") {
		t.Errorf("an empty universe should be explained, got: %s", body)
	}
}

// A failed check belongs in the modal where the user is looking, not only in the
// log, and must not take the page down.
func TestActionErrorsRenderInModal(t *testing.T) {
	f := newFixture(t)
	f.actions.err = errors.New("that check is already running")

	for _, path := range []string{"/actions/sentiment", "/actions/screen"} {
		code, body := f.post(t, path)
		if code != http.StatusOK {
			t.Errorf("POST %s = %d, want 200 so the modal can show the message", path, code)
		}
		if !strings.Contains(body, "Check failed") ||
			!strings.Contains(body, "already running") {
			t.Errorf("POST %s did not render the error: %s", path, body)
		}
	}
}

// A server built without the actions wired up must refuse cleanly rather than
// panicking on a nil interface.
func TestActionsUnavailable(t *testing.T) {
	f := newFixture(t)
	srv, err := NewServer(f.cfg, f.store, f.eng, nil, nil, func() time.Time { return f.now }, true)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv

	code, body := f.post(t, "/actions/screen")
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if !strings.Contains(body, "not available") {
		t.Errorf("body = %q", body)
	}
}
