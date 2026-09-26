// Package web serves the read-only status page specified in docs/web-ui.md. It
// renders state out of the store and never makes trading decisions.
package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/store"
)

// LegendEntry describes one agent state for the page's legend.
type LegendEntry struct {
	State    domain.AgentState
	Tone     string // drives the CSS class
	Icon     string
	Meaning  string
	IsActive bool
}

// legend is the documented set of states. ERROR and HALTED_BEARISH share a tone
// but carry different icons and labels, because docs/web-ui.md requires them to
// be distinguishable without relying on colour alone.
var legend = []LegendEntry{
	{State: domain.StateMarketClosed, Tone: "idle", Icon: "○",
		Meaning: "Outside exchange hours. Waiting for the next open."},
	{State: domain.StateSentimentCheck, Tone: "warn", Icon: "◐",
		Meaning: "First hour. Polling market sentiment every 10 minutes. No trades placed."},
	{State: domain.StateScreening, Tone: "good", Icon: "●",
		Meaning: "Sentiment gate passed. Screening for candidates every minute."},
	{State: domain.StateEODWindow, Tone: "warn", Icon: "◑",
		Meaning: "Final 30 minutes. Flattening all positions; no new entries."},
	{State: domain.StateHaltedBearish, Tone: "bad", Icon: "■",
		Meaning: "Sentiment was overwhelmingly bearish. Halted for the rest of the session."},
	{State: domain.StateError, Tone: "bad", Icon: "▲",
		Meaning: "A fault needs attention. This is not a deliberate trading halt."},
}

type SentimentRow struct {
	Time           string
	Values         []string
	Classification string
	Tone           string
}

type CriterionCell struct {
	Pass    bool
	Display string
}

type ScreenRow struct {
	Symbol     string
	Cells      []CriterionCell
	Qualifies  bool
	Verdict    string
	FailReason string
	// Outcome is what the entry pass did with a qualifying candidate. Qualifying is
	// not the same as being bought — the position cap, the same-day re-entry rule
	// and available cash all still apply.
	Outcome     string
	OutcomeTone string
}

type PositionRow struct {
	Symbol     string
	Shares     int
	Entry      string
	Current    string
	Peak       string
	PnLDollars string
	PnLPct     string
	Tone       string
	TrailArmed bool
}

type EODRow struct {
	Symbol string
	Shares int
	Open   string
	Close  string
	PnLPct string
	Reason string
	Tone   string
}

// View is everything the template renders.
type View struct {
	GeneratedAt string
	SessionDate string

	MarketOpen  bool
	MarketLabel string
	SessionText string
	EarlyClose  bool

	State        domain.AgentState
	StateTone    string
	StateIcon    string
	StateMeaning string
	ErrorMessage string
	Legend       []LegendEntry

	GateVerdict string
	GateTone    string
	HaltReason  string

	Sentiment       []SentimentRow
	SentimentLabels []string

	ScreenColumns []string
	ScreenRows    []ScreenRow
	ScreenTakenAt string
	ScreenNote    string

	Positions     []PositionRow
	ExposureText  string
	PositionsNote string

	ShowEOD    bool
	EODRows    []EODRow
	EODNetPnL  string
	EODNetTone string
	EODWinLoss string

	PollSeconds int
	PaperMode   bool
}

func money(v float64) string { return fmt.Sprintf("$%.2f", v) }

func signedMoney(v float64) string { return fmt.Sprintf("%+.2f", v) }

func pct(v float64) string { return fmt.Sprintf("%+.2f%%", v) }

func toneForPnL(v float64) string {
	switch {
	case v > 0:
		return "good"
	case v < 0:
		return "bad"
	default:
		return "idle"
	}
}

// BuildView assembles the page from stored state. It deliberately takes no
// market-data dependency: the trading loop persists each position's last mark, so
// a 12-second poll cannot multiply API calls.
func BuildView(
	cfg *config.Config,
	st *store.Store,
	state domain.AgentState,
	errMessage string,
	sess scheduler.Session,
	tradingDay bool,
	now time.Time,
	paperMode bool,
) (*View, error) {
	date := scheduler.SessionDate(now)
	v := &View{
		GeneratedAt: now.In(scheduler.ET).Format("15:04:05 MST"),
		SessionDate: date,
		State:       state,
		Legend:      make([]LegendEntry, len(legend)),
		PollSeconds: int(cfg.Web.PollInterval.Seconds()),
		PaperMode:   paperMode,
	}
	if v.PollSeconds < 1 {
		v.PollSeconds = 12
	}

	copy(v.Legend, legend)
	for i := range v.Legend {
		if v.Legend[i].State == state {
			v.Legend[i].IsActive = true
			v.StateTone = v.Legend[i].Tone
			v.StateIcon = v.Legend[i].Icon
			v.StateMeaning = v.Legend[i].Meaning
		}
	}
	if state == domain.StateError {
		v.ErrorMessage = errMessage
	}

	bounds := scheduler.Bounds(sess, cfg)
	if tradingDay {
		v.MarketOpen = !now.Before(sess.Open) && now.Before(sess.Close)
		v.SessionText = fmt.Sprintf("%s – %s ET",
			sess.Open.In(scheduler.ET).Format("15:04"), sess.Close.In(scheduler.ET).Format("15:04"))
		v.EarlyClose = sess.IsEarlyClose()
	} else {
		v.SessionText = "no session today"
	}
	if v.MarketOpen {
		v.MarketLabel = "OPEN"
	} else {
		v.MarketLabel = "CLOSED"
	}

	rec, err := st.Session(date)
	if err != nil {
		return nil, err
	}
	v.GateVerdict = string(rec.Verdict)
	v.HaltReason = rec.HaltReason
	switch rec.Verdict {
	case domain.VerdictProceed:
		v.GateTone = "good"
	case domain.VerdictBearish:
		v.GateTone = "bad"
	default:
		v.GateTone = "idle"
	}

	readings, err := st.SentimentReadings(date)
	if err != nil {
		return nil, err
	}
	v.SentimentLabels = cfg.Sentiment.Symbols
	for _, r := range readings {
		row := SentimentRow{
			Time:           r.TakenAt.In(scheduler.ET).Format("15:04"),
			Classification: string(r.Classification),
			Tone:           "idle",
		}
		if r.Classification == domain.VerdictBearish {
			row.Tone = "bad"
		} else if r.Classification == domain.VerdictProceed {
			row.Tone = "good"
		}
		for _, sym := range cfg.Sentiment.Symbols {
			if val, ok := r.Percentages[sym]; ok {
				row.Values = append(row.Values, pct(val))
			} else {
				row.Values = append(row.Values, "—")
			}
		}
		v.Sentiment = append(v.Sentiment, row)
	}

	evals, takenAt, err := st.LatestScreenSnapshot(date)
	if err != nil {
		return nil, err
	}
	if !takenAt.IsZero() {
		v.ScreenTakenAt = takenAt.In(scheduler.ET).Format("15:04:05")
	}
	for i, e := range evals {
		if i == 0 {
			for _, c := range e.Criteria {
				v.ScreenColumns = append(v.ScreenColumns, c.Name)
			}
		}
		row := ScreenRow{Symbol: e.Symbol, Qualifies: e.Qualifies, FailReason: e.FailReason}
		for _, c := range e.Criteria {
			row.Cells = append(row.Cells, CriterionCell{Pass: c.Pass, Display: c.Display})
		}
		if e.Qualifies {
			row.Verdict = "Qualifies"
		} else {
			row.Verdict = e.FailReason
		}
		row.Outcome = e.Outcome
		row.OutcomeTone = "idle"
		if strings.HasPrefix(e.Outcome, "bought") {
			row.OutcomeTone = "good"
		}
		v.ScreenRows = append(v.ScreenRows, row)
	}
	if len(v.ScreenRows) == 0 {
		v.ScreenNote = "No screening pass has run yet for this session."
	}

	open, err := st.OpenPositions()
	if err != nil {
		return nil, err
	}
	var exposure float64
	for _, p := range open {
		current := p.LastPrice
		if current <= 0 {
			current = p.EntryPrice
		}
		pnl := p.UnrealizedDollars(current)
		exposure += current * float64(p.Shares)
		v.Positions = append(v.Positions, PositionRow{
			Symbol: p.Symbol, Shares: p.Shares,
			Entry: money(p.EntryPrice), Current: money(current), Peak: money(p.PeakPrice),
			PnLDollars: signedMoney(pnl), PnLPct: pct(p.UnrealizedPct(current)),
			Tone: toneForPnL(pnl), TrailArmed: p.TrailArmed,
		})
	}
	v.ExposureText = fmt.Sprintf("%d of %d slots · %s at risk",
		len(open), cfg.Risk.MaxConcurrentPositions, money(exposure))
	if len(open) == 0 {
		v.PositionsNote = "No open positions."
	}

	// The end-of-day summary appears once the exchange has closed, per
	// docs/web-ui.md, and covers today's session only.
	v.ShowEOD = !tradingDay || !now.Before(sess.Close) || (tradingDay && !now.Before(bounds.EODExit))
	if v.ShowEOD {
		positions, err := st.SessionPositions(date)
		if err != nil {
			return nil, err
		}
		var net float64
		var wins, losses int
		for _, p := range positions {
			if p.Open {
				continue
			}
			realized := p.RealizedDollars()
			net += realized
			if realized > 0 {
				wins++
			} else if realized < 0 {
				losses++
			}
			v.EODRows = append(v.EODRows, EODRow{
				Symbol: p.Symbol, Shares: p.Shares,
				Open: money(p.EntryPrice), Close: money(p.ExitPrice),
				PnLPct: pct(p.RealizedPct()), Reason: string(p.ExitReason),
				Tone: toneForPnL(realized),
			})
		}
		v.EODNetPnL = signedMoney(net)
		v.EODNetTone = toneForPnL(net)
		v.EODWinLoss = fmt.Sprintf("%d up · %d down", wins, losses)
	}

	return v, nil
}
