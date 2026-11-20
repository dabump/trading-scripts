package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/martincoetzee/trading-agent/internal/chartfeed"
)

// ChartFeed is what the "Chart" button's popup needs: register interest in a
// symbol's live data while its dialog is open, read the latest of it, and
// stop once the dialog closes. See internal/chartfeed for why this is its own
// worker rather than the web layer calling the broker directly — the rule in
// CLAUDE.md ("the web layer never calls the broker" except through the two
// read-only checks and Open/Close) holds for this feature too.
type ChartFeed interface {
	Watch(ctx context.Context, symbol string)
	Unwatch(symbol string)
	Poll(symbol string) (chartfeed.Data, bool)
}

// chartPosition is the open-position context the chart modal's header shows
// alongside the chart: whether the symbol is held, and if so its id, shares
// and unrealized P&L. Looked up fresh both when the chart opens
// (handleChart) and on every subsequent poll (handleChartData) — the
// trading button and the P&L next to it are "live" in the same sense the
// candles are, not a one-time snapshot taken at open.
type chartPosition struct {
	Held       bool   `json:"held"`
	PositionID int64  `json:"positionId"`
	Shares     int    `json:"shares"`
	PnLDollars string `json:"pnlDollars"`
	PnLPct     string `json:"pnlPct"`
	Tone       string `json:"tone"`
}

// lookupChartPosition finds symbol among the open positions and computes its
// unrealized P&L the same way the Open positions table does
// (domain.Position.UnrealizedDollars/UnrealizedPct off the trading loop's
// last mark, falling back to entry price before the first mark lands).
func (s *Server) lookupChartPosition(symbol string) chartPosition {
	var info chartPosition
	open, err := s.store.OpenPositions()
	if err != nil {
		s.log.Error("load open positions for chart", "err", err)
		return info
	}
	for _, p := range open {
		if p.Symbol != symbol {
			continue
		}
		info.Held, info.PositionID, info.Shares = true, p.ID, p.SharesOpen
		current := p.LastPrice
		if current <= 0 {
			current = p.EntryPrice
		}
		pnl := p.UnrealizedDollars(current)
		info.PnLDollars, info.PnLPct, info.Tone = signedMoney(pnl), pct(p.UnrealizedPct(current)), toneForPnL(pnl)
		break
	}
	return info
}

// ChartModal is the shell the "Chart" button's popup renders. The chart data
// itself is loaded separately, by the dialog's own JS polling
// handleChartData — this only needs to exist long enough for that JS to find
// the chart containers it draws into, plus whichever one trading action
// (Open or Close) applies to the symbol right now.
type ChartModal struct {
	Title  string
	Symbol string
	chartPosition
}

// handleChart opens a symbol's live chart: registers it with the chart feed
// worker and renders the modal shell the page's JS then draws into.
//
// POST only, like the other actions: this starts new background fetching, so
// a prefetch or crawler following a link must not be able to set it off.
func (s *Server) handleChart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.chart == nil {
		s.renderActionError(w, "The live chart is not available.", http.StatusServiceUnavailable)
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(r.FormValue("symbol")))
	if symbol == "" {
		s.renderActionError(w, "No symbol was given.", http.StatusOK)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	s.chart.Watch(ctx, symbol)

	m := &ChartModal{Title: symbol + " — live chart", Symbol: symbol, chartPosition: s.lookupChartPosition(symbol)}
	s.renderModal(w, "chartModal", m)
}

// chartPoint is one candle, shaped for the vendored charting library: a UNIX
// second timestamp plus OHLCV. Forming marks the one bar still updating, so
// the page can draw it distinctly.
type chartPoint struct {
	Time    int64   `json:"time"`
	Open    float64 `json:"open"`
	High    float64 `json:"high"`
	Low     float64 `json:"low"`
	Close   float64 `json:"close"`
	Volume  float64 `json:"volume"`
	Forming bool    `json:"forming,omitempty"`
}

type linePoint struct {
	Time  int64   `json:"time"`
	Value float64 `json:"value"`
}

type chartResponse struct {
	Symbol     string       `json:"symbol"`
	Bars       []chartPoint `json:"bars"`
	EMA9       []linePoint  `json:"ema9"`
	EMA20      []linePoint  `json:"ema20"`
	VWAP       []linePoint  `json:"vwap"`
	MACDLine   []linePoint  `json:"macdLine"`
	MACDSignal []linePoint  `json:"macdSignal"`
	UpdatedAt  string       `json:"updatedAt"`
	Err        string       `json:"err,omitempty"`
	// chartPosition is re-looked-up on every poll, not just when the chart
	// opens, so the header's trading button and its P&L stay live exactly
	// like the candles do — a position closed, opened or simply marked since
	// the last poll shows up on the next one.
	chartPosition
}

// handleChartData is polled by an open chart's own JS every few seconds. It
// never touches the broker — Poll only reads the chart feed worker's
// in-memory cache — so the web layer's "never calls the broker" rule holds
// for this endpoint too.
func (s *Server) handleChartData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if s.chart == nil {
		http.Error(w, "chart feed not available", http.StatusServiceUnavailable)
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if symbol == "" {
		http.Error(w, "no symbol given", http.StatusBadRequest)
		return
	}
	data, ok := s.chart.Poll(symbol)
	if !ok {
		http.Error(w, "not watched", http.StatusNotFound)
		return
	}

	resp := chartResponse{
		Symbol: data.Symbol, UpdatedAt: data.UpdatedAt.Format(time.RFC3339), Err: data.Err,
		chartPosition: s.lookupChartPosition(symbol),
	}
	for _, b := range data.Bars {
		resp.Bars = append(resp.Bars, chartPoint{
			Time: b.Time.Unix(), Open: b.Open, High: b.High, Low: b.Low, Close: b.Close,
			Volume: b.Volume, Forming: b.Forming,
		})
	}
	// Each indicator slice is aligned to the tail of data.Bars — shorter by its
	// own warm-up, the same convention strategy.EMA/MACD use everywhere else —
	// so it is re-paired with the bar timestamps it belongs to here rather than
	// assumed to start at index 0.
	align := func(values []float64) []linePoint {
		if len(values) == 0 {
			return nil
		}
		offset := len(data.Bars) - len(values)
		out := make([]linePoint, len(values))
		for i, v := range values {
			out[i] = linePoint{Time: data.Bars[offset+i].Time.Unix(), Value: v}
		}
		return out
	}
	resp.EMA9 = align(data.EMA9)
	resp.EMA20 = align(data.EMA20)
	resp.VWAP = align(data.VWAP)
	resp.MACDLine = align(data.MACDLine)
	resp.MACDSignal = align(data.MACDSignal)

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("encode chart data", "symbol", symbol, "err", err)
	}
}

// handleChartStop ends a chart's watch immediately, called when its dialog
// closes cleanly. chartfeed's own watchTTL is the backstop for when it
// doesn't — a closed tab or a lost network never fires this.
func (s *Server) handleChartStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.chart != nil {
		symbol := strings.ToUpper(strings.TrimSpace(r.FormValue("symbol")))
		if symbol != "" {
			s.chart.Unwatch(symbol)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
