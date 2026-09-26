package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
)

// stubAlpaca serves canned responses shaped like Alpaca's documented payloads.
// This verifies the client's decoding, not the endpoint paths themselves, which
// still need a real account to confirm.
func stubAlpaca(t *testing.T, routes map[string]string) *Alpaca {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for prefix, body := range routes {
			if strings.HasPrefix(r.URL.Path, prefix) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"no stub for `+r.URL.Path+`"}`)
	}))
	t.Cleanup(srv.Close)

	return NewAlpaca(&config.Secrets{
		APIKey: "key", APISecret: "secret", BaseURL: srv.URL, DataURL: srv.URL,
	}, "sip")
}

func TestSnapshotsDecoding(t *testing.T) {
	a := stubAlpaca(t, map[string]string{
		"/v2/stocks/snapshots": `{
			"ABCD": {
				"latestTrade": {"p": 4.56},
				"dailyBar": {"c": 4.50, "v": 6100000, "t": "2026-09-28T20:00:00Z"},
				"prevDailyBar": {"c": 4.00, "v": 900000, "t": "2026-09-25T20:00:00Z"}
			}
		}`,
	})

	got, err := a.Snapshots(context.Background(), []string{"ABCD"})
	if err != nil {
		t.Fatal(err)
	}
	snap := got["ABCD"]
	// The latest trade must win over the daily bar close for the current price.
	if snap.Price != 4.56 {
		t.Errorf("Price = %v, want 4.56 (latest trade)", snap.Price)
	}
	if snap.PrevClose != 4.00 {
		t.Errorf("PrevClose = %v, want 4.00", snap.PrevClose)
	}
	if snap.TodayVolume != 6_100_000 {
		t.Errorf("TodayVolume = %v, want 6100000", snap.TodayVolume)
	}
	if want := 14.0; snap.IntradayPct < want-0.01 || snap.IntradayPct > want+0.01 {
		t.Errorf("IntradayPct = %v, want ~%v", snap.IntradayPct, want)
	}
}

// A snapshot with no trade yet must fall back to the daily bar rather than
// reporting a zero price that would look like bad data downstream.
func TestSnapshotsFallsBackToDailyBar(t *testing.T) {
	a := stubAlpaca(t, map[string]string{
		"/v2/stocks/snapshots": `{"ABCD": {"dailyBar": {"c": 3.25, "v": 100}}}`,
	})
	got, err := a.Snapshots(context.Background(), []string{"ABCD"})
	if err != nil {
		t.Fatal(err)
	}
	if got["ABCD"].Price != 3.25 {
		t.Errorf("Price = %v, want 3.25", got["ABCD"].Price)
	}
}

func TestSnapshotsEmptyInputSkipsRequest(t *testing.T) {
	a := stubAlpaca(t, nil) // any request would 404
	got, err := a.Snapshots(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty symbol list must not call the API: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d snapshots, want 0", len(got))
	}
}

func TestIntradayBarsDecoding(t *testing.T) {
	a := stubAlpaca(t, map[string]string{
		"/v2/stocks/ABCD/bars": `{"bars": [
			{"t": "2026-09-28T13:30:00Z", "o": 4.0, "h": 4.2, "l": 3.9, "c": 4.1, "v": 50000},
			{"t": "2026-09-28T13:45:00Z", "o": 4.1, "h": 4.4, "l": 4.05, "c": 4.35, "v": 61000}
		]}`,
	})

	bars, err := a.IntradayBars(context.Background(), "ABCD", 15, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 {
		t.Fatalf("got %d bars, want 2", len(bars))
	}
	if bars[1].Close != 4.35 || bars[0].Volume != 50000 {
		t.Errorf("bars decoded wrong: %+v", bars)
	}
	if bars[0].Time.After(bars[1].Time) {
		t.Error("bars must stay in chronological order")
	}
}

// The tradable universe drives the whole scan, so the filtering matters: OTC and
// untradable assets must not reach the screener.
func TestTradableAssetsFiltersVenueAndTradability(t *testing.T) {
	a := stubAlpaca(t, map[string]string{
		"/v2/assets": `[
			{"symbol":"GOOD","tradable":true,"exchange":"NASDAQ"},
			{"symbol":"ALSOGOOD","tradable":true,"exchange":"NYSE"},
			{"symbol":"UNTRADABLE","tradable":false,"exchange":"NASDAQ"},
			{"symbol":"PINKSHEET","tradable":true,"exchange":"OTC"}
		]`,
	})

	got, err := a.TradableAssets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GOOD", "ALSOGOOD"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// A full-universe snapshot call must be split into batches, or the symbols would
// build a URL long enough to be rejected in transit.
func TestSnapshotsBatchesLargeUniverse(t *testing.T) {
	var requests int
	var longestURL int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if len(r.URL.RequestURI()) > longestURL {
			longestURL = len(r.URL.RequestURI())
		}
		syms := strings.Split(r.URL.Query().Get("symbols"), ",")
		if len(syms) > snapshotBatchSize {
			t.Errorf("batch of %d symbols exceeds the %d limit", len(syms), snapshotBatchSize)
		}
		if r.URL.Query().Get("feed") != "sip" {
			t.Errorf("feed = %q, want sip", r.URL.Query().Get("feed"))
		}
		io.WriteString(w, `{"`+syms[0]+`":{"latestTrade":{"p":1.0},"dailyBar":{"c":1.0,"v":10},"prevDailyBar":{"c":1.0}}}`)
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")

	universe := make([]string, 1250)
	for i := range universe {
		universe[i] = fmt.Sprintf("SYM%04d", i)
	}
	got, err := a.Snapshots(context.Background(), universe)
	if err != nil {
		t.Fatal(err)
	}

	// 1250 symbols at 500 per batch is three requests.
	if requests != 3 {
		t.Errorf("made %d requests for 1250 symbols, want 3", requests)
	}
	// Results from every batch must be merged, not just the last one.
	if len(got) != 3 {
		t.Errorf("merged %d snapshots, want one per batch", len(got))
	}
	if longestURL > 8000 {
		t.Errorf("longest URL was %d bytes; batching should keep it well under 8KB", longestURL)
	}
}

// News stories carry every symbol they mention; only the screened ones count.
func TestNewsCountsIgnoresUnrequestedSymbols(t *testing.T) {
	a := stubAlpaca(t, map[string]string{
		"/v1beta1/news": `{"news": [
			{"symbols": ["ABCD", "SPY"]},
			{"symbols": ["ABCD"]},
			{"symbols": ["TSLA"]}
		]}`,
	})

	got, err := a.NewsCounts(context.Background(), []string{"ABCD"}, time.Now().Add(-8*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got["ABCD"] != 2 {
		t.Errorf("ABCD count = %d, want 2", got["ABCD"])
	}
	if _, ok := got["SPY"]; ok {
		t.Error("SPY was not requested and must not be counted")
	}
	if _, ok := got["TSLA"]; ok {
		t.Error("TSLA was not requested and must not be counted")
	}
}

// The trading API returns numbers as JSON strings.
func TestAccountParsesStringNumbers(t *testing.T) {
	a := stubAlpaca(t, map[string]string{
		"/v2/account": `{"portfolio_value": "25143.71", "cash": "12000.00", "equity": "25143.71"}`,
	})
	acct, err := a.Account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.PortfolioValue != 25143.71 || acct.Cash != 12000 {
		t.Errorf("account = %+v, want 25143.71 / 12000", acct)
	}
}

func TestPlaceOrderPayloadAndDecoding(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("APCA-API-KEY-ID") != "key" {
			t.Error("credentials must be sent as headers")
		}
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"id":"o-1","status":"filled","filled_qty":"200","filled_avg_price":"4.21"}`)
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "key", APISecret: "secret", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	res, err := a.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "ABCD", Shares: 200, Side: "buy", Type: "market", ClientOrderID: "cid-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	if body["qty"] != "200" || body["side"] != "buy" || body["type"] != "market" {
		t.Errorf("order payload = %v", body)
	}
	// Day-only trading: an order must never survive the session.
	if body["time_in_force"] != "day" {
		t.Errorf("time_in_force = %v, want day", body["time_in_force"])
	}
	if body["client_order_id"] != "cid-1" {
		t.Errorf("client_order_id = %v, want cid-1 for idempotency", body["client_order_id"])
	}
	if _, hasLimit := body["limit_price"]; hasLimit {
		t.Error("market order must not carry a limit price")
	}
	if res.FilledShares != 200 || res.FilledPrice != 4.21 {
		t.Errorf("result = %+v", res)
	}
}

func TestPlaceLimitOrderIncludesPrice(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"id":"o-2","status":"accepted","filled_qty":"0","filled_avg_price":""}`)
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	if _, err := a.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "ABCD", Shares: 10, Side: "buy", Type: "limit", LimitPrice: 4.5,
	}); err != nil {
		t.Fatal(err)
	}
	if body["limit_price"] != "4.50" {
		t.Errorf("limit_price = %v, want \"4.50\"", body["limit_price"])
	}
}

func TestPositionsDecoding(t *testing.T) {
	a := stubAlpaca(t, map[string]string{
		"/v2/positions": `[{"symbol":"ABCD","qty":"200","avg_entry_price":"4.20","current_price":"4.65"}]`,
	})
	got, err := a.Positions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Shares != 200 || got[0].AvgEntry != 4.20 {
		t.Fatalf("positions = %+v", got)
	}
}

func TestCalendarParsesEarlyClose(t *testing.T) {
	a := stubAlpaca(t, map[string]string{
		"/v2/calendar": `[{"date":"2026-11-27","open":"09:30","close":"13:00"}]`,
	})
	day, err := a.Calendar(context.Background(), "2026-11-27")
	if err != nil {
		t.Fatal(err)
	}
	if got := day.Open.Format("15:04"); got != "09:30" {
		t.Errorf("open = %s, want 09:30 ET", got)
	}
	if got := day.Close.Format("15:04"); got != "13:00" {
		t.Errorf("close = %s, want 13:00 ET (early close)", got)
	}
}

// A holiday or weekend returns an empty calendar, which is normal, not an error.
func TestCalendarEmptyForNonSession(t *testing.T) {
	a := stubAlpaca(t, map[string]string{"/v2/calendar": `[]`})
	day, err := a.Calendar(context.Background(), "2026-12-25")
	if err != nil {
		t.Fatalf("a closed day must not be an error: %v", err)
	}
	if !day.Open.IsZero() {
		t.Errorf("got %+v, want a zero day", day)
	}
}

func TestErrorResponseIncludesStatusAndHidesQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"forbidden"}`)
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	_, err := a.Snapshots(context.Background(), []string{"ABCD"})
	if err == nil {
		t.Fatal("want an error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should name the status: %v", err)
	}
	if strings.Contains(err.Error(), "symbols=") {
		t.Errorf("query parameters must be kept out of logged errors: %v", err)
	}
}

func TestAverageVolumeExcludingToday(t *testing.T) {
	now := time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC)
	day := func(d int, v float64) alpacaBar {
		return alpacaBar{T: time.Date(2026, 9, d, 20, 0, 0, 0, time.UTC), V: v}
	}

	// Today's partial volume is the number being compared against the average, so
	// it must not be folded into it.
	bars := []alpacaBar{day(23, 1000), day(24, 2000), day(25, 3000), day(28, 999999)}
	if got := AverageVolumeExcludingToday(bars, 3, now); got != 2000 {
		t.Errorf("average = %v, want 2000 (today excluded)", got)
	}

	// Fewer sessions than requested still averages what exists.
	if got := AverageVolumeExcludingToday([]alpacaBar{day(24, 1000), day(25, 3000)}, 20, now); got != 2000 {
		t.Errorf("average = %v, want 2000", got)
	}

	if got := AverageVolumeExcludingToday([]alpacaBar{day(28, 500)}, 20, now); got != 0 {
		t.Errorf("average = %v, want 0 when only today is available", got)
	}
	if got := AverageVolumeExcludingToday(nil, 20, now); got != 0 {
		t.Errorf("average = %v, want 0 for no data", got)
	}
}
