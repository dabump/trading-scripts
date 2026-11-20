package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/chartfeed"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func TestChartScriptLoadsOnThePage(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/")
	if !strings.Contains(body, `<script src="/static/lightweight-charts.js"></script>`) {
		t.Error("page missing the vendored charting library's script tag")
	}
}

func TestStaticLibraryIsServed(t *testing.T) {
	f := newFixture(t)
	code, body := f.get(t, "/static/lightweight-charts.js")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "LightweightCharts") {
		t.Error("served file does not look like the vendored library")
	}
}

// These two trigger new background fetching, so a GET (a prefetch, a
// crawler, a pasted link) must not be able to set them off — same rule as
// every other action.
func TestChartActionsRejectGET(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/actions/chart", "/actions/chart/stop"} {
		code, _ := f.get(t, path)
		if code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, code)
		}
	}
	if len(f.chart.watched) != 0 || len(f.chart.unwatched) != 0 {
		t.Error("a GET reached the chart feed worker, want neither Watch nor Unwatch called")
	}
}

func TestHandleChartRegistersAWatchAndRendersTheShell(t *testing.T) {
	f := newFixture(t)
	code, body := f.postForm(t, "/actions/chart", "symbol=demo")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(f.chart.watched) != 1 || f.chart.watched[0] != "DEMO" {
		t.Fatalf("watched = %v, want [DEMO] — the symbol must be upper-cased", f.chart.watched)
	}
	for _, want := range []string{"DEMO", `id="chart-price-pane"`, `id="chart-macd-pane"`, "EMA9", "EMA20", "VWAP", "MACD(9,20)"} {
		if !strings.Contains(body, want) {
			t.Errorf("chart modal missing %q", want)
		}
	}
	// The dismiss button closes the chart, not the agent's position — it must
	// never read as a trading action now that one sits right beside it.
	if !strings.Contains(body, `data-close-modal title="Close this chart"`) {
		t.Error("chart modal missing the dismiss button, or it no longer says what it closes")
	}
}

// A symbol with no open position shows Open, never Close — the same choice
// the screening table's CanOpen already makes, just read fresh here.
func TestHandleChartShowsOpenWhenNotHeld(t *testing.T) {
	f := newFixture(t)
	_, body := f.postForm(t, "/actions/chart", "symbol=demo")
	// Both buttons are always in the DOM so a later poll can toggle `hidden`
	// without tearing the chart down — so the assertion is on which one is
	// visible, not on which one exists.
	if !strings.Contains(body, `id="chart-open-btn" data-open="DEMO" title="Buy DEMO now">Open position`) {
		t.Errorf("chart modal's Open action is not visible for an unheld symbol, got %s", body)
	}
	if !strings.Contains(body, `title="Sell the 0 shares still held, now" hidden>Close position`) {
		t.Errorf("chart modal's Close action is not hidden for a symbol with no open position, got %s", body)
	}
	if !strings.Contains(body, `id="chart-pnl" title="Unrealized P&L on the shares still held" hidden>`) {
		t.Errorf("chart modal's P&L is not hidden for a symbol with no open position, got %s", body)
	}
}

// A symbol with an open position shows Close, sized and keyed off that
// position — not Open, which would let the page buy a second lot of
// something already held straight from the chart.
func TestHandleChartShowsCloseWhenHeld(t *testing.T) {
	f := newFixture(t)
	id, err := f.store.InsertPosition(domain.Position{
		SessionDate: f.date, Symbol: "DEMO", Shares: 200,
		EntryPrice: 4.50, EntryTime: f.now, StopPrice: 4.00,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A mark above entry, so the P&L shown is a real computation off the
	// trading loop's last price rather than the breakeven InsertPosition seeds.
	if err := f.store.UpdateMark(id, 5.00, 5.00); err != nil {
		t.Fatal(err)
	}

	_, body := f.postForm(t, "/actions/chart", "symbol=demo")
	want := fmt.Sprintf(`data-close="%d"`, id)
	if !strings.Contains(body, want) {
		t.Errorf("chart modal missing %q for the held position, got %s", want, body)
	}
	if !strings.Contains(body, `title="Sell the 200 shares still held, now">Close position`) {
		t.Error("chart modal's Close action is not visible, or does not report the shares still held")
	}
	if !strings.Contains(body, `id="chart-open-btn" data-open="DEMO" title="Buy DEMO now" hidden>Open position`) {
		t.Errorf("chart modal's Open action is not hidden for a symbol already held, got %s", body)
	}
	// (5.00 - 4.50) * 200 shares = +100.00, 11.11% move from entry — the same
	// domain.Position.UnrealizedDollars/UnrealizedPct the positions table uses.
	// Colored good/bad (green/red), never left at the dismiss button's neutral
	// styling, so a glance at the header says whether closing now is a gain.
	for _, want := range []string{"+100.00", "+11.11%", `id="chart-pnl" title="Unrealized P&L on the shares still held">+100.00 (+11.11%)`} {
		if !strings.Contains(body, want) {
			t.Errorf("chart modal missing %q (unrealized P&L), got %s", want, body)
		}
	}
	if !strings.Contains(body, `class="meta good" id="chart-pnl"`) {
		t.Error("chart modal's P&L is not colored good (green) for a gain")
	}
}

func TestHandleChartRejectsAnEmptySymbol(t *testing.T) {
	f := newFixture(t)
	_, body := f.postForm(t, "/actions/chart", "symbol=")
	if len(f.chart.watched) != 0 {
		t.Error("an empty symbol must not reach Watch")
	}
	if !strings.Contains(body, "No symbol was given") {
		t.Errorf("body = %q, want the no-symbol error", body)
	}
}

// The chart's own poll endpoint carries the same live position/P&L info the
// modal's header shows, re-computed on every call rather than only at the
// moment the chart opened — that is what lets the page's JS keep the header
// current without a full re-render.
func TestHandleChartDataIncludesLivePositionInfo(t *testing.T) {
	f := newFixture(t)
	f.chart.data["DEMO"] = chartfeed.Data{Symbol: "DEMO"}

	code, body := f.get(t, "/actions/chart/data?symbol=demo")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, `"held":false`) {
		t.Errorf("chart data for an unheld symbol = %s, want held:false", body)
	}

	id, err := f.store.InsertPosition(domain.Position{
		SessionDate: f.date, Symbol: "DEMO", Shares: 200,
		EntryPrice: 4.50, EntryTime: f.now, StopPrice: 4.00,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateMark(id, 5.00, 5.00); err != nil {
		t.Fatal(err)
	}

	_, body = f.get(t, "/actions/chart/data?symbol=demo")
	for _, want := range []string{
		`"held":true`, fmt.Sprintf(`"positionId":%d`, id), `"shares":200`,
		`"pnlDollars":"+100.00"`, `"pnlPct":"+11.11%"`, `"tone":"good"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("chart data after opening a position missing %q, got %s", want, body)
		}
	}
}

func TestHandleChartDataServesThePolledSymbol(t *testing.T) {
	f := newFixture(t)
	f.chart.data["DEMO"] = chartfeed.Data{
		Symbol: "DEMO",
		Bars: []chartfeed.Bar{
			{Time: time.Date(2026, 9, 28, 10, 59, 0, 0, time.UTC), Open: 4, High: 4.2, Low: 3.9, Close: 4.1, Volume: 500},
			{Time: time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC), Open: 4.1, High: 4.3, Low: 4.0, Close: 4.2, Forming: true},
		},
		EMA9: []float64{4.15}, // aligned to the tail: only the newest bar has a value
	}

	code, body := f.get(t, "/actions/chart/data?symbol=demo")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, want := range []string{
		`"symbol":"DEMO"`, `"forming":true`, `"close":4.2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("chart data missing %q, got %s", want, body)
		}
	}
	// EMA9 has one point, half of Bars' two — it must be re-paired with the
	// *second* bar's timestamp, not the first, since indicator slices are
	// aligned to the tail.
	if !strings.Contains(body, `"ema9":[{"time":1790593200,"value":4.15}]`) {
		t.Errorf("ema9 not aligned to the newest bar's timestamp, got %s", body)
	}
}

func TestHandleChartDataReportsNotFoundWhenUnwatched(t *testing.T) {
	f := newFixture(t)
	code, _ := f.get(t, "/actions/chart/data?symbol=NOPE")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestHandleChartStopUnwatches(t *testing.T) {
	f := newFixture(t)
	code, _ := f.postForm(t, "/actions/chart/stop", "symbol=demo")
	if code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	if len(f.chart.unwatched) != 1 || f.chart.unwatched[0] != "DEMO" {
		t.Fatalf("unwatched = %v, want [DEMO]", f.chart.unwatched)
	}
}
