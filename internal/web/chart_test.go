package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/chartfeed"
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
