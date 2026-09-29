package web

import (
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/store"
)

type stubEngine struct {
	state      domain.AgentState
	errMsg     string
	session    scheduler.Session
	tradingDay bool
	next       scheduler.Session
	nextKnown  bool
	account    domain.AccountSnapshot
}

func (s *stubEngine) State() (domain.AgentState, string)     { return s.state, s.errMsg }
func (s *stubEngine) Session() (scheduler.Session, bool)     { return s.session, s.tradingDay }
func (s *stubEngine) NextSession() (scheduler.Session, bool) { return s.next, s.nextKnown }
func (s *stubEngine) Account() domain.AccountSnapshot        { return s.account }

// stubActions stands in for the engine's manual actions.
type stubActions struct {
	check   domain.SentimentCheck
	preview domain.ScreenPreview
	err     error
	calls   int

	// closed records the position ids the page asked to close, and closedResult is
	// what ClosePosition hands back.
	closed       []int64
	closedResult domain.Position
	closeErr     error

	opened     []string
	openResult domain.ManualOpen
	openErr    error

	ignored   int
	ignoreErr error
}

func (a *stubActions) IgnoreHalt(context.Context) error {
	a.calls++
	a.ignored++
	return a.ignoreErr
}

// opened records the symbols the page asked to buy; openResult is what comes back.
func (a *stubActions) OpenPosition(_ context.Context, symbol string) (domain.ManualOpen, error) {
	a.calls++
	a.opened = append(a.opened, symbol)
	if a.openErr != nil {
		return domain.ManualOpen{}, a.openErr
	}
	return a.openResult, nil
}

func (a *stubActions) ClosePosition(_ context.Context, id int64) (domain.Position, error) {
	a.calls++
	a.closed = append(a.closed, id)
	if a.closeErr != nil {
		return domain.Position{}, a.closeErr
	}
	return a.closedResult, nil
}

func (a *stubActions) CheckSentiment(context.Context) (domain.SentimentCheck, error) {
	a.calls++
	return a.check, a.err
}

func (a *stubActions) ScreenNow(context.Context) (domain.ScreenPreview, error) {
	a.calls++
	return a.preview, a.err
}

func testConfig() *config.Config {
	c := &config.Config{}
	c.MarketData = config.MarketData{Feed: "sip"}
	c.Screening = config.Screening{MinIntradayPct: 10,
		MinVolumeMultiple: 5, AvgVolumeLookbackDays: 20, MaxEnriched: 100,
		NewsLookback: 18 * time.Hour, MinPrice: 1, MaxPrice: 20,
		MinDollarVolume: 1_000_000}
	c.Risk = config.Risk{RiskPerTradePct: 1, MaxPositionPct: 33,
		MaxConcurrentPositions: 3, StopLossPct: 10}
	c.Exit = config.Exit{FirstTargetR: 2, FirstTargetFraction: 0.5,
		BreakevenAfterTarget: true, EODExitOffsetMins: 30}
	c.Entry = config.Entry{PatternInterval: time.Minute, EMAPeriod: 9,
		MaxPullbackBars: 2, SurgeBars: 3, MinSurgePct: 3, MaxRetracePct: 50, StopBufferPct: 0.1,
		MinStopDistancePct: 0.5, MaxStopDistancePct: 4}
	c.Timing = config.Timing{SentimentWindow: time.Hour, SentimentPollInterval: 10 * time.Minute,
		EntryWindow: 4 * time.Hour, ScreenerScanInterval: time.Minute,
		PositionPollInterval: 15 * time.Second}
	c.Sentiment = config.Sentiment{Symbols: []string{"SPY", "QQQ", "IWM"}, BearishAvgPct: -0.8}
	c.Web = config.Web{ListenAddr: ":0", PollInterval: 12 * time.Second}
	return c
}

type fixture struct {
	srv     *Server
	store   *store.Store
	eng     *stubEngine
	cfg     *config.Config
	now     time.Time
	date    string
	actions *stubActions
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	open := time.Date(2026, 9, 28, 9, 30, 0, 0, scheduler.ET)
	f := &fixture{
		store: st, cfg: testConfig(), date: "2026-09-28",
		actions: &stubActions{},
		now:     time.Date(2026, 9, 28, 11, 0, 0, 0, scheduler.ET),
		eng: &stubEngine{
			state:      domain.StateScreening,
			tradingDay: true,
			session: scheduler.Session{Date: "2026-09-28", Open: open,
				Close: time.Date(2026, 9, 28, 16, 0, 0, 0, scheduler.ET)},
			next: scheduler.Session{Date: "2026-09-29",
				Open:  time.Date(2026, 9, 29, 9, 30, 0, 0, scheduler.ET),
				Close: time.Date(2026, 9, 29, 16, 0, 0, 0, scheduler.ET)},
			nextKnown: true,
			account: domain.AccountSnapshot{
				Account: domain.Account{PortfolioValue: 25_143.71, Cash: 12_000, Equity: 25_143.71},
				At:      time.Date(2026, 9, 28, 10, 59, 45, 0, scheduler.ET),
				Known:   true,
			},
		},
	}

	srv, err := NewServer(f.cfg, st, f.eng, f.actions, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return f.now }, true)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	return f
}

// get returns the response with HTML entities decoded. html/template escapes "+"
// to "&#43;", so asserting on raw markup would fail on every signed number even
// though the browser renders it correctly. Decoding first tests what a reader
// actually sees.
func (f *fixture) get(t *testing.T, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, html.UnescapeString(rec.Body.String())
}

func (f *fixture) post(t *testing.T, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec.Code, html.UnescapeString(rec.Body.String())
}

func TestPageRendersHeaderBadgesAndLegend(t *testing.T) {
	f := newFixture(t)
	code, body := f.get(t, "/")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	// Market-hours badge and agent-state badge are separate indicators.
	if !strings.Contains(body, "Exchange OPEN") {
		t.Error("market hours badge missing")
	}
	if !strings.Contains(body, "SCREENING") {
		t.Error("agent state badge missing")
	}
	// The full legend must always be present, not only the active state.
	for _, state := range []domain.AgentState{
		domain.StateMarketClosed, domain.StateSentimentCheck, domain.StateScreening,
		domain.StateHaltedBearish, domain.StateEODWindow, domain.StateError,
	} {
		if !strings.Contains(body, string(state)) {
			t.Errorf("legend is missing %q", state)
		}
	}
	if !strings.Contains(body, "paper") {
		t.Error("paper-mode indicator missing")
	}
	// Dark theme is the documented choice.
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Error("page should declare the dark theme")
	}
}

// ERROR and HALTED_BEARISH share a colour, so they must differ by icon/label too.
func TestErrorIsDistinguishableFromBearishHalt(t *testing.T) {
	f := newFixture(t)

	f.eng.state = domain.StateError
	f.eng.errMsg = "poll sentiment: upstream 503"
	_, body := f.get(t, "/")
	if !strings.Contains(body, "Fault:") || !strings.Contains(body, "upstream 503") {
		t.Error("a fault must be shown with its message")
	}
	if !strings.Contains(body, "▲") {
		t.Error("ERROR should carry its own icon")
	}

	f.eng.state = domain.StateHaltedBearish
	f.eng.errMsg = ""
	if err := f.store.SetVerdict(f.date, domain.VerdictBearish, "avg -1.4% across SPY/QQQ/IWM"); err != nil {
		t.Fatal(err)
	}
	_, body = f.get(t, "/")
	if !strings.Contains(body, "Halted for the session") {
		t.Error("a bearish halt must be labelled as a halt")
	}
	if strings.Contains(body, "Fault:") {
		t.Error("a deliberate halt must not be presented as a fault")
	}
	if !strings.Contains(body, "avg -1.4%") {
		t.Error("halt reason should be shown")
	}
}

func TestScreeningTableShowsPerCriterionValues(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SaveScreenSnapshot(f.date, f.now, []domain.Evaluation{
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
	}); err != nil {
		t.Fatal(err)
	}

	_, body := f.get(t, "/")
	for _, want := range []string{
		"News catalyst", "Intraday move", "Rel. volume",
		"2 today", "+14.0%", "6.1x", // values, not just ticks
		"0 today",
		"Qualifies", "fails: News catalyst",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("screening table missing %q", want)
		}
	}
}

func TestOpenPositionsShowLivePnL(t *testing.T) {
	f := newFixture(t)
	id, err := f.store.InsertPosition(domain.Position{
		SessionDate: f.date, Symbol: "ABCD", Shares: 2000,
		EntryPrice: 5.00, EntryTime: f.now, PeakPrice: 5.00,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateMark(id, 5.60, 5.75); err != nil {
		t.Fatal(err)
	}

	_, body := f.get(t, "/")
	for _, want := range []string{
		"ABCD", "2000",
		"$5.00",    // entry
		"$5.60",    // current mark
		"$5.75",    // peak
		"+1200.00", // 2000 x $0.60
		"+12.00%",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("positions table missing %q", want)
		}
	}
	if !strings.Contains(body, "1 of 3 slots") {
		t.Error("exposure summary missing")
	}
}

// The balance comes from the engine's cached reading, never from a broker call in
// the handler, so what the page shows is whatever the last tick published.
func TestAccountBalanceRendersFromEngineSnapshot(t *testing.T) {
	f := newFixture(t)

	_, body := f.get(t, "/")
	for _, want := range []string{
		"Available cash", "$12,000.00",
		"Equity", "$25,143.71",
		"as of 10:59:45 ET",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("account panel missing %q", want)
		}
	}

	// A reading the tick has stopped refreshing is labelled rather than presented as
	// current: the scan interval is a minute here, so twenty minutes is a fault.
	f.now = time.Date(2026, 9, 28, 11, 20, 0, 0, scheduler.ET)
	_, body = f.get(t, "/")
	if !strings.Contains(body, "not refreshed since 10:59:45 ET") {
		t.Error("a stale balance must say so")
	}
	if !strings.Contains(body, "$12,000.00") {
		t.Error("a stale balance is still the last known one and should stay visible")
	}
}

// Before the first successful read there is no balance to show, and a zero would
// read as a drained account.
func TestAccountBalanceAbsentUntilRead(t *testing.T) {
	f := newFixture(t)
	f.eng.account = domain.AccountSnapshot{}

	_, body := f.get(t, "/")
	if strings.Contains(body, "Available cash") {
		t.Error("no balance figures should be rendered before the first read")
	}
	if !strings.Contains(body, "has not read the account balance yet") {
		t.Error("the absence should be explained")
	}
}

// A closed position has to show up the moment it closes, with its result.
//
// It used to live in an end-of-day card that was hidden until the forced-exit mark,
// which was fine while a strategy rule was the only thing that could close a
// position. Now the page can close one by hand, and an operator who has just sold
// something cannot be made to wait four hours to see what it made.
func TestClosedPositionsAppearImmediately(t *testing.T) {
	f := newFixture(t)
	id, err := f.store.InsertPosition(domain.Position{
		SessionDate: f.date, Symbol: "ABCD", Shares: 100,
		EntryPrice: 4.00, EntryTime: f.now, PeakPrice: 4.60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ClosePosition(id, 4.40, f.now, domain.ExitStopLoss); err != nil {
		t.Fatal(err)
	}

	// 11:00, mid-session — hours before any end-of-day mark.
	_, body := f.get(t, "/")
	for _, want := range []string{
		"$4.00",     // opened
		"$4.40",     // closed
		"+10.00%",   // P&L %
		"+40.00",    // P&L dollars, and the net: 100 x $0.40
		"STOP_LOSS", // which rule closed it
		"1 up · 0 down",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("closed position is missing %q mid-session", want)
		}
	}
}

// With nothing closed yet the tab explains itself rather than rendering an empty
// table, and the totals footer stays off a table with no rows.
func TestClosedTabWithNothingClosed(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/")
	if !strings.Contains(body, "Nothing has been closed in this session yet.") {
		t.Error("the closed tab must say why it is empty")
	}
	if strings.Contains(body, "Net P&amp;L") {
		t.Error("the net-P&L footer must not render against an empty table")
	}
}

func TestFragmentHasNoDocumentWrapper(t *testing.T) {
	f := newFixture(t)
	code, body := f.get(t, "/fragment")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<script") {
		t.Error("fragment must not include the document wrapper or scripts")
	}
	if !strings.Contains(body, "Exchange OPEN") {
		t.Error("fragment should still contain the live content")
	}
}

func TestJSONStatusEndpoint(t *testing.T) {
	f := newFixture(t)
	code, body := f.get(t, "/api/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, want := range []string{`"State": "SCREENING"`, `"MarketOpen": true`, `"SessionDate": "2026-09-28"`} {
		if !strings.Contains(body, want) {
			t.Errorf("JSON status missing %s", want)
		}
	}
}

func TestClosedDayRendersWithoutSession(t *testing.T) {
	f := newFixture(t)
	f.eng.tradingDay = false
	f.eng.state = domain.StateMarketClosed
	f.eng.session = scheduler.Session{Date: f.date}

	code, body := f.get(t, "/")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "no session today") {
		t.Error("a non-trading day should be stated plainly")
	}
	if !strings.Contains(body, "Exchange CLOSED") {
		t.Error("market badge should read CLOSED")
	}
}

func TestUnknownPathIs404(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.get(t, "/nope"); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}
