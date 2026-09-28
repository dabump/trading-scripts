package main

// Alpaca market-data access for the backtest, with on-disk caching.
//
// Every fetch is a GET. The backtest never touches /v2/orders, and it uses the
// data host rather than the trading host, so it cannot place an order even by
// accident.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Bar is one OHLCV candle. Field names match Alpaca's wire format so the
// response decodes straight into it.
type Bar struct {
	T time.Time `json:"t"`
	O float64   `json:"o"`
	H float64   `json:"h"`
	L float64   `json:"l"`
	C float64   `json:"c"`
	V float64   `json:"v"`
}

type client struct {
	key, secret string
	dataURL     string
	tradeURL    string
	feed        string
	cacheDir    string
	http        *http.Client
}

// barsPerSymbolRequest bounds how many symbols go into one multi-symbol request.
// The symbol list travels in the query string, so a large universe has to be
// batched or the URL exceeds what the server will accept.
const barsPerSymbolRequest = 150

// newsBatchSize matches internal/broker's batching, for the same reason: a news
// page caps at 50 stories, so asking about too many symbols at once makes the
// tail of the list look newsless.
const newsBatchSize = 25

func (c *client) get(rawURL string, out any) error {
	const attempts = 6
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("APCA-API-KEY-ID", c.key)
		req.Header.Set("APCA-API-SECRET-KEY", c.secret)

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		// 429 and 5xx are worth retrying; a 4xx is a bug in the request.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 300))
		}
		return json.Unmarshal(body, out)
	}
	return fmt.Errorf("giving up after %d attempts: %w", attempts, lastErr)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// cached runs fn unless name already holds a result, so re-running the backtest
// after a code change costs no API calls. The data is immutable history, so a
// cache hit is always as good as a fetch.
func cached[T any](c *client, name string, fn func() (T, error)) (T, error) {
	var zero T
	path := filepath.Join(c.cacheDir, name)
	if raw, err := os.ReadFile(path); err == nil {
		var out T
		if json.Unmarshal(raw, &out) == nil {
			return out, nil
		}
	}
	out, err := fn()
	if err != nil {
		return zero, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return zero, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return zero, err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return zero, err
	}
	return out, nil
}

// tradableAssets mirrors broker.Alpaca.TradableAssets: the same filter the daemon
// scans, so the backtest measures the same universe.
func (c *client) tradableAssets() ([]string, error) {
	return cached(c, "universe.json", func() ([]string, error) {
		var resp []struct {
			Symbol     string `json:"symbol"`
			Tradable   bool   `json:"tradable"`
			Status     string `json:"status"`
			Exchange   string `json:"exchange"`
			Fractional bool   `json:"fractionable"`
		}
		u := c.tradeURL + "/v2/assets?status=active&asset_class=us_equity"
		if err := c.get(u, &resp); err != nil {
			return nil, err
		}
		out := make([]string, 0, len(resp))
		for _, a := range resp {
			if !a.Tradable || a.Status != "active" || a.Exchange == "OTC" {
				continue
			}
			out = append(out, a.Symbol)
		}
		return out, nil
	})
}

// bars fetches one timeframe for many symbols, batching the symbol list and
// following next_page_token within each batch.
//
// Split-adjusted, unlike broker.AverageDailyVolume's raw request. Over a year of
// history a reverse split — common among exactly the distressed microcaps this
// strategy screens for — leaves a raw series looking like a several-hundred-percent
// overnight gain, which would manufacture candidates that never existed. Within the
// daemon's own 20-session window the distinction rarely matters; across a year it
// does.
func (c *client) bars(symbols []string, timeframe string, start, end time.Time) (map[string][]Bar, error) {
	out := map[string][]Bar{}
	for i := 0; i < len(symbols); i += barsPerSymbolRequest {
		batch := symbols[i:min(i+barsPerSymbolRequest, len(symbols))]
		token := ""
		for {
			u := fmt.Sprintf(
				"%s/v2/stocks/bars?symbols=%s&timeframe=%s&start=%s&end=%s&feed=%s&adjustment=split&limit=10000&sort=asc",
				c.dataURL, url.QueryEscape(strings.Join(batch, ",")), url.QueryEscape(timeframe),
				url.QueryEscape(start.Format(time.RFC3339)), url.QueryEscape(end.Format(time.RFC3339)),
				url.QueryEscape(c.feed))
			if token != "" {
				u += "&page_token=" + url.QueryEscape(token)
			}
			var resp struct {
				Bars          map[string][]Bar `json:"bars"`
				NextPageToken *string          `json:"next_page_token"`
			}
			if err := c.get(u, &resp); err != nil {
				return nil, fmt.Errorf("bars %s: %w", timeframe, err)
			}
			for sym, bs := range resp.Bars {
				out[sym] = append(out[sym], bs...)
			}
			if resp.NextPageToken == nil || *resp.NextPageToken == "" {
				break
			}
			token = *resp.NextPageToken
		}
	}
	return out, nil
}

// newsTimes returns each symbol's story timestamps in the window. Timestamps are
// kept rather than a count, because the agent's news window is relative to the
// moment it scans: a story published at 14:00 is invisible to an 11:00 scan.
func (c *client) newsTimes(symbols []string, start, end time.Time) (map[string][]time.Time, error) {
	out := map[string][]time.Time{}
	for i := 0; i < len(symbols); i += newsBatchSize {
		batch := symbols[i:min(i+newsBatchSize, len(symbols))]
		token := ""
		for {
			u := fmt.Sprintf("%s/v1beta1/news?symbols=%s&start=%s&end=%s&limit=50&sort=asc",
				c.dataURL, url.QueryEscape(strings.Join(batch, ",")),
				url.QueryEscape(start.Format(time.RFC3339)), url.QueryEscape(end.Format(time.RFC3339)))
			if token != "" {
				u += "&page_token=" + url.QueryEscape(token)
			}
			var resp struct {
				News []struct {
					CreatedAt time.Time `json:"created_at"`
					Symbols   []string  `json:"symbols"`
				} `json:"news"`
				NextPageToken *string `json:"next_page_token"`
			}
			if err := c.get(u, &resp); err != nil {
				return nil, fmt.Errorf("news: %w", err)
			}
			inBatch := map[string]bool{}
			for _, s := range batch {
				inBatch[s] = true
			}
			for _, story := range resp.News {
				for _, sym := range story.Symbols {
					// A story can name symbols outside the batch; those counts
					// would be incomplete, so they are discarded.
					if inBatch[sym] {
						out[sym] = append(out[sym], story.CreatedAt)
					}
				}
			}
			if resp.NextPageToken == nil || *resp.NextPageToken == "" {
				break
			}
			token = *resp.NextPageToken
		}
	}
	return out, nil
}
