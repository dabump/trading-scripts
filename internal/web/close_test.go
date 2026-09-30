package web

import (
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// postForm posts a urlencoded body, which is how the close button submits its id.
func (f *fixture) postForm(t *testing.T, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	// Decoded like the other helpers: html/template escapes "+" to "&#43;", so
	// asserting on raw markup would fail on every signed number the browser renders
	// correctly.
	return rec.Code, html.UnescapeString(rec.Body.String())
}

// openPosition puts one open position in the store and returns its id.
func (f *fixture) openPosition(t *testing.T, symbol string, shares int, entry float64) int64 {
	t.Helper()
	id, err := f.store.InsertPosition(domain.Position{
		SessionDate: f.date, Symbol: symbol, Shares: shares,
		EntryPrice: entry, EntryTime: f.now, PeakPrice: entry,
		StopPrice: entry * 0.97, InitialRisk: entry * 0.03,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Each open position carries a close button, and the button knows which position it
// belongs to — the id is what survives the twelve-second re-render.
func TestOpenPositionHasACloseButton(t *testing.T) {
	f := newFixture(t)
	id := f.openPosition(t, "ABCD", 100, 4.00)

	_, body := f.get(t, "/")
	if !strings.Contains(body, `data-close="`+itoa(id)+`"`) {
		t.Errorf("no close button carrying position id %d", id)
	}
	if !strings.Contains(body, "data-symbol=\"ABCD\"") {
		t.Error("the button must name its symbol, so the confirmation can too")
	}
}

// The action places an order, so it is POST-only: a prefetch or a crawler following
// a link must not be able to sell a position.
func TestClosePositionRefusesGET(t *testing.T) {
	f := newFixture(t)
	code, _ := f.get(t, "/actions/close?id=1")
	if code != http.StatusMethodNotAllowed {
		t.Errorf("GET /actions/close = %d, want 405", code)
	}
	if len(f.actions.closed) != 0 {
		t.Error("a GET must not reach the engine")
	}
}

// The happy path hands the id to the engine and reports what came back, including
// the realised P&L — which for a scaled position is not the entry-to-exit move.
func TestClosePositionReportsTheResult(t *testing.T) {
	f := newFixture(t)
	f.actions.closedResult = domain.Position{
		Symbol: "ABCD", Shares: 100, EntryPrice: 4.00, ExitPrice: 4.40,
		BankedDollars: 40, ExitReason: domain.ExitManual,
		ExitTime: time.Date(2026, 9, 28, 11, 0, 0, 0, scheduler.ET),
	}

	code, body := f.postForm(t, "/actions/close", "id=7")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(f.actions.closed) != 1 || f.actions.closed[0] != 7 {
		t.Fatalf("engine saw %v, want the id 7 from the form", f.actions.closed)
	}
	for _, want := range []string{"ABCD", "$4.40", "+40.00", "+10.00%", "MANUAL"} {
		if !strings.Contains(body, want) {
			t.Errorf("confirmation is missing %q: %s", want, body)
		}
	}
}

// A refusal from the engine — the exchange shut, the position already gone — belongs
// in front of the operator, not only in the log.
func TestClosePositionShowsTheRefusal(t *testing.T) {
	f := newFixture(t)
	f.actions.closeErr = errors.New("the exchange is closed, so a sell cannot be filled now")

	code, body := f.postForm(t, "/actions/close", "id=7")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so the modal renders the reason", code)
	}
	if !strings.Contains(body, "the exchange is closed") {
		t.Errorf("the refusal must reach the page: %s", body)
	}
}

// A malformed id must not reach the engine at all.
func TestClosePositionRejectsABadID(t *testing.T) {
	f := newFixture(t)
	_, body := f.postForm(t, "/actions/close", "id=not-a-number")
	if !strings.Contains(body, "not valid") {
		t.Errorf("want a validation message, got: %s", body)
	}
	if len(f.actions.closed) != 0 {
		t.Error("a malformed id must not reach the engine")
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// The Open button is on every screened row, qualifying or not (a failing row's button
// overrides the screen as well as the setup), and never on a symbol already held,
// where the engine would refuse the click anyway.
func TestOpenButtonOnEveryScreenedRowNotHeld(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SaveScreenSnapshot(f.date, f.now, []domain.Evaluation{
		{Symbol: "GOOD", Qualifies: true, Criteria: []domain.Criterion{
			{Name: "News catalyst", Pass: true, Display: "2 today"}}},
		{Symbol: "HELD", Qualifies: true, Criteria: []domain.Criterion{
			{Name: "News catalyst", Pass: true, Display: "1 today"}}},
		{Symbol: "BAD", Qualifies: false, FailReason: "fails: Rel. volume",
			Criteria: []domain.Criterion{{Name: "News catalyst", Pass: false, Display: "0 today"}}},
	}); err != nil {
		t.Fatal(err)
	}
	f.openPosition(t, "HELD", 100, 4.00)

	_, body := f.get(t, "/")
	if !strings.Contains(body, `data-open="GOOD"`) {
		t.Error("a qualifying candidate must offer an Open button")
	}
	if !strings.Contains(body, `data-open="BAD" data-qualifies="false"`) {
		t.Error("a failing candidate must offer an Open button marked as not qualifying")
	}
	if strings.Contains(body, `data-open="HELD"`) {
		t.Error("a symbol already held must not offer an Open button")
	}
}

// It places an order, so POST only.
func TestOpenPositionRefusesGET(t *testing.T) {
	f := newFixture(t)
	code, _ := f.get(t, "/actions/open?symbol=ABCD")
	if code != http.StatusMethodNotAllowed {
		t.Errorf("GET /actions/open = %d, want 405", code)
	}
	if len(f.actions.opened) != 0 {
		t.Error("a GET must not reach the engine")
	}
}

// The confirmation has to lead with size and stop — the last moment to notice they
// are not what was expected — and say where the stop came from.
func TestOpenPositionReportsSizeAndStop(t *testing.T) {
	f := newFixture(t)
	f.actions.openResult = domain.ManualOpen{
		Position:    domain.Position{Symbol: "ABCD", EntryTime: f.now},
		Shares:      500,
		Entry:       5.00,
		Stop:        4.80,
		RiskDollar:  100,
		FromSetup:   false,
		SetupReason: "pullback is 0 bars, need at least 1",
	}

	code, body := f.postForm(t, "/actions/open", "symbol=abcd")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(f.actions.opened) != 1 || f.actions.opened[0] != "ABCD" {
		t.Fatalf("engine saw %v, want the symbol upper-cased", f.actions.opened)
	}
	for _, want := range []string{"500", "$5.00", "$4.80", "$100.00", "configured maximum",
		"pullback is 0 bars"} {
		if !strings.Contains(body, want) {
			t.Errorf("confirmation is missing %q", want)
		}
	}
}

// A refusal from the engine belongs in front of the operator.
func TestOpenPositionShowsTheRefusal(t *testing.T) {
	f := newFixture(t)
	f.actions.openErr = errors.New("entry is not allowed right now: position cap reached (3)")
	_, body := f.postForm(t, "/actions/open", "symbol=ABCD")
	if !strings.Contains(body, "position cap reached") {
		t.Errorf("the refusal must reach the page: %s", body)
	}
}

// The Ignore button appears only on a halt the gate could not judge — halted with no
// readings — and never on one it reached on real data.
func TestIgnoreButtonOnlyOnAnUnjudgedHalt(t *testing.T) {
	f := newFixture(t)
	// Session() creates the row; SetVerdict is an UPDATE and would otherwise be a
	// no-op. The engine always goes through Session first, for the same reason.
	if _, err := f.store.Session(f.date); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetVerdict(f.date, domain.VerdictBearish,
		"no sentiment readings were taken during the first hour"); err != nil {
		t.Fatal(err)
	}

	// Asserted against the fragment, not the whole page: the click handler in the
	// page's script mentions the same attribute, so "/" would match either.
	_, body := f.get(t, "/fragment")
	if !strings.Contains(body, "data-ignore-halt") {
		t.Error("a halt with no readings behind it must offer the Ignore button")
	}

	// Now give the session a real reading: the same halt is the kill switch working.
	if err := f.store.AddSentimentReading(domain.SentimentReading{
		SessionDate: f.date, TakenAt: f.now, Classification: domain.VerdictBearish,
		Percentages: map[string]float64{"SPY": -1.4},
	}); err != nil {
		t.Fatal(err)
	}
	_, body = f.get(t, "/fragment")
	if strings.Contains(body, "data-ignore-halt") {
		t.Error("a halt the gate actually reached must not be dismissible from the page")
	}
}

// An unhalted session offers nothing to dismiss.
func TestNoIgnoreButtonWithoutAHalt(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/fragment")
	if strings.Contains(body, "data-ignore-halt") {
		t.Error("the Ignore button must not appear when nothing is halted")
	}
}

// It changes what the agent does, so POST only.
func TestIgnoreHaltRefusesGET(t *testing.T) {
	f := newFixture(t)
	code, _ := f.get(t, "/actions/ignore-halt")
	if code != http.StatusMethodNotAllowed {
		t.Errorf("GET /actions/ignore-halt = %d, want 405", code)
	}
	if f.actions.ignored != 0 {
		t.Error("a GET must not reach the engine")
	}
}

// The confirmation has to say the session now runs without a kill switch.
func TestIgnoreHaltConfirmationIsHonest(t *testing.T) {
	f := newFixture(t)
	code, body := f.postForm(t, "/actions/ignore-halt", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if f.actions.ignored != 1 {
		t.Fatalf("engine calls = %d, want 1", f.actions.ignored)
	}
	for _, want := range []string{"GATE_OVERRIDDEN", "no sentiment kill switch", "Screening resumes"} {
		if !strings.Contains(body, want) {
			t.Errorf("confirmation is missing %q", want)
		}
	}
}

// A refusal reaches the operator rather than only the log.
func TestIgnoreHaltShowsTheRefusal(t *testing.T) {
	f := newFixture(t)
	f.actions.ignoreErr = errors.New("this halt came from actual sentiment readings")
	_, body := f.postForm(t, "/actions/ignore-halt", "")
	if !strings.Contains(body, "actual sentiment readings") {
		t.Errorf("the refusal must reach the page: %s", body)
	}
}
