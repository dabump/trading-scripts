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

// sessionStamp is an RFC3339 timestamp for a bar `daysAgo` exchange days back,
// stamped at 20:00 UTC the way Alpaca stamps a daily bar. The dates have to be
// relative to the clock, because which bar counts as "today" is exactly what the
// decoder reads them for.
func sessionStamp(daysAgo int) string {
	return time.Now().In(ET()).AddDate(0, 0, -daysAgo).Format("2006-01-02") + "T20:00:00Z"
}

func TestSnapshotsDecoding(t *testing.T) {
	a := stubAlpaca(t, map[string]string{
		"/v2/stocks/snapshots": `{
			"ABCD": {
				"latestTrade": {"p": 4.56},
				"dailyBar": {"c": 4.50, "v": 6100000, "t": "` + sessionStamp(0) + `"},
				"prevDailyBar": {"c": 4.00, "v": 900000, "t": "` + sessionStamp(3) + `"}
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
	// Day by default: an entry or exit the agent sends must never survive the session,
	// because the reason it sent it will not. The one order that does outlive the day
	// is a protective stop, which asks for gtc explicitly — see the test below.
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

// A protective stop carries its trigger, asks for gtc, and carries no limit price.
// The gtc is the point: it is the one order meant to outlive the session, because
// the position it protects does.
func TestPlaceStopOrderCarriesTriggerAndSurvivesTheDay(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"id":"o-3","status":"new","filled_qty":"0","filled_avg_price":""}`)
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	res, err := a.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "ABCD", Shares: 10, Side: "sell", Type: "stop",
		StopPrice: 3.78, TimeInForce: "gtc", ClientOrderID: "cid-stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["type"] != "stop" || body["side"] != "sell" {
		t.Errorf("order payload = %v", body)
	}
	if body["stop_price"] != "3.78" {
		t.Errorf("stop_price = %v, want \"3.78\"", body["stop_price"])
	}
	if body["time_in_force"] != "gtc" {
		t.Errorf("time_in_force = %v, want gtc: a day stop expires at the close and leaves "+
			"the position bare overnight", body["time_in_force"])
	}
	if _, hasLimit := body["limit_price"]; hasLimit {
		t.Error("a stop order must not carry a limit price")
	}
	if _, extended := body["extended_hours"]; extended {
		t.Error("extended_hours is accepted only on a day limit order")
	}
	// Acknowledged and working, not filled — which is what every caller depends on.
	if res.BrokerOrderID != "o-3" || res.FilledShares != 0 || res.Done() {
		t.Errorf("result = %+v, want a working order with an id", res)
	}
}

// A buy carrying its stop goes as an "oto" order, and the stop leg's id comes back so
// the position can be tied to the order that protects it.
func TestPlaceOrderWithAttachedStop(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"id":"o-4","type":"market","status":"accepted","filled_qty":"0",
			"filled_avg_price":null,"order_class":"oto",
			"legs":[{"id":"o-4-leg","type":"stop","status":"held","filled_qty":"0"}]}`)
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	res, err := a.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "ABCD", Shares: 10, Side: "buy", Type: "market", StopLoss: 4.85, ClientOrderID: "cid-oto",
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["order_class"] != "oto" {
		t.Errorf("order_class = %v, want oto", body["order_class"])
	}
	leg, _ := body["stop_loss"].(map[string]any)
	if leg["stop_price"] != "4.85" {
		t.Errorf("stop_loss = %v, want a stop_price of \"4.85\"", body["stop_loss"])
	}
	if res.BrokerOrderID != "o-4" || res.StopLegID != "o-4-leg" {
		t.Errorf("result = %+v, want the parent's id and the stop leg's", res)
	}

	// A plain order carries neither.
	body = nil
	if _, err := a.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "ABCD", Shares: 10, Side: "sell", Type: "market", ClientOrderID: "cid-plain",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["order_class"]; ok {
		t.Error("a plain order must not set order_class")
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

// Before the opening bell the daily bar may still be the previous session's, and
// which one is "yesterday" has to be read from the bar's own date rather than from
// its field name. Taking prevDailyBar as the reference close in that state would
// measure every pre-market move against the close from two sessions ago — and that
// percentage is the whole basis of the screen.
func TestSnapshotsPreMarketReferenceClose(t *testing.T) {
	a := stubAlpaca(t, map[string]string{
		"/v2/stocks/snapshots": `{
			"ABCD": {
				"latestTrade": {"p": 5.50},
				"dailyBar": {"c": 5.00, "v": 900000, "t": "` + sessionStamp(1) + `"},
				"prevDailyBar": {"c": 2.00, "v": 800000, "t": "` + sessionStamp(2) + `"}
			}
		}`,
	})
	got, err := a.Snapshots(context.Background(), []string{"ABCD"})
	if err != nil {
		t.Fatal(err)
	}
	snap := got["ABCD"]
	if snap.PrevClose != 5.00 {
		t.Errorf("PrevClose = %v, want 5.00 — the last completed session, not the one before it",
			snap.PrevClose)
	}
	if want := 10.0; snap.IntradayPct < want-0.01 || snap.IntradayPct > want+0.01 {
		t.Errorf("IntradayPct = %v, want ~%v (+10%% pre-market, not +175%%)", snap.IntradayPct, want)
	}
	// Today has printed no daily bar, so there is no volume to report for it. A
	// stale figure would be worse: it is the numerator of the relative-volume test.
	if snap.TodayVolume != 0 {
		t.Errorf("TodayVolume = %v, want 0 before today's daily bar exists", snap.TodayVolume)
	}
}

// Extended-hours routing is opt-in. Alpaca accepts the flag only on a day limit
// order, so sending it unconditionally would break every regular-session order.
func TestPlaceOrderExtendedHours(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"id":"o-3","status":"accepted","filled_qty":"0","filled_avg_price":""}`)
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	if _, err := a.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "ABCD", Shares: 10, Side: "buy", Type: "limit", LimitPrice: 4.5,
		ExtendedHours: true,
	}); err != nil {
		t.Fatal(err)
	}
	if body["extended_hours"] != true {
		t.Errorf("extended_hours = %v, want true", body["extended_hours"])
	}
	if body["time_in_force"] != "day" {
		t.Errorf("time_in_force = %v, want day — Alpaca rejects extended hours on anything else",
			body["time_in_force"])
	}

	body = nil
	if _, err := a.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "ABCD", Shares: 10, Side: "buy", Type: "market",
	}); err != nil {
		t.Fatal(err)
	}
	if _, present := body["extended_hours"]; present {
		t.Error("a regular-session order must not carry extended_hours at all")
	}
}
