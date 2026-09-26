package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// Alpaca implements MarketData and Trading against Alpaca's REST API.
//
// The endpoint paths and payload shapes here have not been exercised against a
// live account — no credentials were available when this was written. Treat the
// first real run as a verification step, and check responses against Alpaca's
// current API reference if a call fails to decode.
type Alpaca struct {
	keyID   string
	secret  string
	baseURL string
	dataURL string
	http    *http.Client
}

func NewAlpaca(s *config.Secrets) *Alpaca {
	return &Alpaca{
		keyID:   s.APIKey,
		secret:  s.APISecret,
		baseURL: strings.TrimRight(s.BaseURL, "/"),
		dataURL: strings.TrimRight(s.DataURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (a *Alpaca) do(ctx context.Context, method, rawURL string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("APCA-API-KEY-ID", a.keyID)
	req.Header.Set("APCA-API-SECRET-KEY", a.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, redact(rawURL), err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Truncated so a large HTML error page does not flood the log.
		snippet := string(payload)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return fmt.Errorf("%s %s: %s: %s", method, redact(rawURL), resp.Status, snippet)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode %s: %w", redact(rawURL), err)
	}
	return nil
}

// redact keeps query parameters out of error messages, which end up in logs.
func redact(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// parseFloat handles Alpaca returning numbers as JSON strings on the trading API.
func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

type alpacaBar struct {
	T time.Time `json:"t"`
	O float64   `json:"o"`
	H float64   `json:"h"`
	L float64   `json:"l"`
	C float64   `json:"c"`
	V float64   `json:"v"`
}

func (b alpacaBar) toDomain() domain.Bar {
	return domain.Bar{Time: b.T, Open: b.O, High: b.H, Low: b.L, Close: b.C, Volume: b.V}
}

func (a *Alpaca) Snapshots(ctx context.Context, symbols []string) (map[string]domain.Snapshot, error) {
	if len(symbols) == 0 {
		return map[string]domain.Snapshot{}, nil
	}
	u := fmt.Sprintf("%s/v2/stocks/snapshots?symbols=%s",
		a.dataURL, url.QueryEscape(strings.Join(symbols, ",")))

	var raw map[string]struct {
		LatestTrade *struct {
			P float64 `json:"p"`
		} `json:"latestTrade"`
		DailyBar     *alpacaBar `json:"dailyBar"`
		PrevDailyBar *alpacaBar `json:"prevDailyBar"`
	}
	if err := a.do(ctx, http.MethodGet, u, nil, &raw); err != nil {
		return nil, err
	}

	out := make(map[string]domain.Snapshot, len(raw))
	for sym, v := range raw {
		snap := domain.Snapshot{Symbol: sym}
		if v.LatestTrade != nil {
			snap.Price = v.LatestTrade.P
		}
		if v.DailyBar != nil {
			snap.TodayVolume = v.DailyBar.V
			if snap.Price == 0 {
				snap.Price = v.DailyBar.C
			}
		}
		if v.PrevDailyBar != nil {
			snap.PrevClose = v.PrevDailyBar.C
		}
		if snap.PrevClose > 0 && snap.Price > 0 {
			snap.IntradayPct = (snap.Price - snap.PrevClose) / snap.PrevClose * 100
		}
		out[sym] = snap
	}
	return out, nil
}

func (a *Alpaca) IntradayBars(ctx context.Context, symbol string, intervalMins int, since time.Time) ([]domain.Bar, error) {
	u := fmt.Sprintf("%s/v2/stocks/%s/bars?timeframe=%dMin&start=%s&limit=1000&adjustment=raw",
		a.dataURL, url.PathEscape(symbol), intervalMins,
		url.QueryEscape(since.UTC().Format(time.RFC3339)))

	var resp struct {
		Bars []alpacaBar `json:"bars"`
	}
	if err := a.do(ctx, http.MethodGet, u, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]domain.Bar, 0, len(resp.Bars))
	for _, b := range resp.Bars {
		out = append(out, b.toDomain())
	}
	return out, nil
}

func (a *Alpaca) AverageDailyVolume(ctx context.Context, symbol string, days int) (float64, error) {
	// Request a wider window than `days` because weekends and holidays mean
	// calendar days and trading days differ.
	start := time.Now().UTC().AddDate(0, 0, -(days*2 + 10))
	u := fmt.Sprintf("%s/v2/stocks/%s/bars?timeframe=1Day&start=%s&limit=1000&adjustment=raw",
		a.dataURL, url.PathEscape(symbol), url.QueryEscape(start.Format(time.RFC3339)))

	var resp struct {
		Bars []alpacaBar `json:"bars"`
	}
	if err := a.do(ctx, http.MethodGet, u, nil, &resp); err != nil {
		return 0, err
	}
	return AverageVolumeExcludingToday(resp.Bars, days, time.Now()), nil
}

// AverageVolumeExcludingToday averages the most recent `days` sessions, skipping
// today. Today's partial volume is the value being compared against the average,
// so including it would dilute the very spike the criterion looks for.
func AverageVolumeExcludingToday(bars []alpacaBar, days int, now time.Time) float64 {
	today := now.UTC().Format("2006-01-02")
	var sum float64
	var n int
	for i := len(bars) - 1; i >= 0 && n < days; i-- {
		if bars[i].T.UTC().Format("2006-01-02") == today {
			continue
		}
		sum += bars[i].V
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func (a *Alpaca) Movers(ctx context.Context, top int) ([]Mover, error) {
	u := fmt.Sprintf("%s/v1beta1/screener/stocks/movers?top=%d", a.dataURL, top)

	var resp struct {
		Gainers []struct {
			Symbol        string  `json:"symbol"`
			Price         float64 `json:"price"`
			PercentChange float64 `json:"percent_change"`
		} `json:"gainers"`
	}
	if err := a.do(ctx, http.MethodGet, u, nil, &resp); err != nil {
		return nil, err
	}
	// Only gainers matter: the strategy buys strength, never shorts weakness.
	out := make([]Mover, 0, len(resp.Gainers))
	for _, g := range resp.Gainers {
		out = append(out, Mover{Symbol: g.Symbol, Price: g.Price, ChangePct: g.PercentChange})
	}
	return out, nil
}

func (a *Alpaca) NewsCounts(ctx context.Context, symbols []string, since time.Time) (map[string]int, error) {
	counts := make(map[string]int, len(symbols))
	if len(symbols) == 0 {
		return counts, nil
	}
	u := fmt.Sprintf("%s/v1beta1/news?symbols=%s&start=%s&limit=50",
		a.dataURL, url.QueryEscape(strings.Join(symbols, ",")),
		url.QueryEscape(since.UTC().Format(time.RFC3339)))

	var resp struct {
		News []struct {
			Symbols []string `json:"symbols"`
		} `json:"news"`
	}
	if err := a.do(ctx, http.MethodGet, u, nil, &resp); err != nil {
		return nil, err
	}
	requested := make(map[string]bool, len(symbols))
	for _, s := range symbols {
		requested[s] = true
	}
	for _, item := range resp.News {
		for _, sym := range item.Symbols {
			// A story can be tagged with symbols beyond those requested; only the
			// ones being screened should be counted.
			if requested[sym] {
				counts[sym]++
			}
		}
	}
	return counts, nil
}

func (a *Alpaca) Account(ctx context.Context) (domain.Account, error) {
	var resp struct {
		PortfolioValue string `json:"portfolio_value"`
		Cash           string `json:"cash"`
		Equity         string `json:"equity"`
	}
	if err := a.do(ctx, http.MethodGet, a.baseURL+"/v2/account", nil, &resp); err != nil {
		return domain.Account{}, err
	}
	return domain.Account{
		PortfolioValue: parseFloat(resp.PortfolioValue),
		Cash:           parseFloat(resp.Cash),
		Equity:         parseFloat(resp.Equity),
	}, nil
}

func (a *Alpaca) Calendar(ctx context.Context, date string) (CalendarDay, error) {
	u := fmt.Sprintf("%s/v2/calendar?start=%s&end=%s", a.baseURL,
		url.QueryEscape(date), url.QueryEscape(date))

	var resp []struct {
		Date  string `json:"date"`
		Open  string `json:"open"`
		Close string `json:"close"`
	}
	if err := a.do(ctx, http.MethodGet, u, nil, &resp); err != nil {
		return CalendarDay{}, err
	}
	if len(resp) == 0 {
		// Not an error: weekends and holidays legitimately have no session.
		return CalendarDay{}, nil
	}
	return ParseCalendarDay(resp[0].Date, resp[0].Open, resp[0].Close)
}

// ParseCalendarDay converts the calendar's date plus "HH:MM" local exchange times
// into absolute instants.
func ParseCalendarDay(date, open, close string) (CalendarDay, error) {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		et = time.FixedZone("EST", -5*60*60)
	}
	day, err := time.ParseInLocation("2006-01-02", date, et)
	if err != nil {
		return CalendarDay{}, fmt.Errorf("parse calendar date %q: %w", date, err)
	}
	parseClock := func(s string) (time.Time, error) {
		t, err := time.Parse("15:04", s)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse calendar time %q: %w", s, err)
		}
		return time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, et), nil
	}
	o, err := parseClock(open)
	if err != nil {
		return CalendarDay{}, err
	}
	c, err := parseClock(close)
	if err != nil {
		return CalendarDay{}, err
	}
	return CalendarDay{Date: date, Open: o, Close: c}, nil
}

func (a *Alpaca) PlaceOrder(ctx context.Context, req OrderRequest) (OrderResult, error) {
	payload := map[string]any{
		"symbol":          req.Symbol,
		"qty":             strconv.Itoa(req.Shares),
		"side":            req.Side,
		"type":            req.Type,
		"time_in_force":   "day",
		"client_order_id": req.ClientOrderID,
	}
	if req.Type == "limit" {
		payload["limit_price"] = strconv.FormatFloat(req.LimitPrice, 'f', 2, 64)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return OrderResult{}, err
	}

	var resp struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FilledQty   string `json:"filled_qty"`
		FilledPrice string `json:"filled_avg_price"`
	}
	if err := a.do(ctx, http.MethodPost, a.baseURL+"/v2/orders",
		strings.NewReader(string(body)), &resp); err != nil {
		return OrderResult{}, err
	}
	return OrderResult{
		BrokerOrderID: resp.ID,
		Status:        resp.Status,
		FilledPrice:   parseFloat(resp.FilledPrice),
		FilledShares:  int(parseFloat(resp.FilledQty)),
	}, nil
}

func (a *Alpaca) Positions(ctx context.Context) ([]BrokerPosition, error) {
	var resp []struct {
		Symbol       string `json:"symbol"`
		Qty          string `json:"qty"`
		AvgEntry     string `json:"avg_entry_price"`
		CurrentPrice string `json:"current_price"`
	}
	if err := a.do(ctx, http.MethodGet, a.baseURL+"/v2/positions", nil, &resp); err != nil {
		return nil, err
	}
	out := make([]BrokerPosition, 0, len(resp))
	for _, p := range resp {
		out = append(out, BrokerPosition{
			Symbol:       p.Symbol,
			Shares:       int(parseFloat(p.Qty)),
			AvgEntry:     parseFloat(p.AvgEntry),
			CurrentPrice: parseFloat(p.CurrentPrice),
		})
	}
	return out, nil
}
