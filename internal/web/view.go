// Package web serves the read-only status page specified in docs/web-ui.md. It
// renders state out of the store and never makes trading decisions.
package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/store"
	"github.com/martincoetzee/trading-agent/internal/strategy"
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
//
// PRE_MARKET has a tone of its own — orange, between the grey of a closed market and
// the green of a trading one — because it is neither: the agent is working, but on a
// session where the sentiment gate has not run and, by default, nothing is bought.
// Showing it as MARKET_CLOSED would misreport a scanning agent as idle, and as
// SCREENING would imply entries that are not happening.
var legend = []LegendEntry{
	{State: domain.StateMarketClosed, Tone: "idle", Icon: "○",
		Meaning: "Outside exchange hours. Waiting for the next open."},
	{State: domain.StatePreMarket, Tone: "pre", Icon: "◔",
		Meaning: "Pre-market session. Screening the early tape; the sentiment gate has not run yet."},
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
	// CanOpen drives the Open button: any screened symbol the agent is not already
	// holding, qualifying or not. On a failing row the click overrides the screen as
	// well as the setup gate, and the confirmation says so. The engine re-checks the
	// risk rules regardless — this only keeps the page from offering a click that is
	// certain to be refused.
	CanOpen bool
}

type PositionRow struct {
	// ID is what the close button posts back. The row is otherwise display-only, but
	// an action needs something to name the position by that survives a re-render.
	ID     int64
	Symbol string
	// Shares is what is still held. SharesNote says so when the position has been
	// partly sold, because "200" against an entry of 400 would otherwise misreport
	// both the exposure and the P&L a reader computes in their head.
	Shares     int
	SharesNote string
	Entry      string
	Stop       string
	Current    string
	Peak       string
	RMultiple  string
	PnLDollars string
	PnLPct     string
	Tone       string
	// Manual marks a position opened from the page, which no exit rule touches.
	Manual bool
}

type EODRow struct {
	Symbol string
	Shares int
	Open   string
	Close  string
	// PnLDollars is the realised total, banked partial sales included. Shown beside
	// the percentage because the two answer different questions, and with a scaled-out
	// position they are not each other's obvious restatement.
	PnLDollars string
	PnLPct     string
	Reason     string
	Tone       string
}

// View is everything the template renders.
type View struct {
	GeneratedAt string
	SessionDate string

	MarketOpen bool
	// PreMarket is true during the pre-market session. MarketOpen stays false then —
	// the regular market genuinely is not open — so anything keyed on "is the agent
	// live" has to read both.
	PreMarket   bool
	MarketLabel string
	// ExchangeTone drives the exchange badge's colour: green open, orange pre-market,
	// red closed. Computed here rather than in the template so the three-way choice
	// is not an if/else chain inside markup.
	ExchangeTone string
	SessionText  string
	EarlyClose   bool
	// Countdown is "closes in 4h 12m" while open, or "opens in 15h 42m" while closed.
	// Empty when the next boundary is not known — after the close with a failed
	// calendar lookup, say — because a blank is better than a wrong number.
	Countdown string
	// CountdownAt names the moment being counted towards, so the figure can be
	// sanity-checked at a glance.
	CountdownAt string

	State        domain.AgentState
	StateTone    string
	StateIcon    string
	StateMeaning string
	ErrorMessage string
	Legend       []LegendEntry

	GateVerdict string
	GateTone    string
	HaltReason  string
	// CanIgnoreHalt offers the dismiss button, and only for the halt that exists
	// because the gate had nothing to judge. It is decided structurally — halted with
	// zero readings — rather than by matching the halt's wording, which would break
	// the moment the message was reworded. A halt the gate actually reached on real
	// readings is the kill switch working and is not dismissed from the page.
	CanIgnoreHalt bool

	Sentiment       []SentimentRow
	SentimentLabels []string

	ScreenColumns []string
	ScreenRows    []ScreenRow
	ScreenTakenAt string
	ScreenNote    string

	// The account block is the broker's own balance, as the trading loop last read
	// it. ShowAccount is false until the first successful read, where AccountNote
	// explains the absence — a zero cash figure would read as a drained account
	// rather than as missing data. AccountNote also carries the staleness warning
	// when the reading has stopped being refreshed.
	ShowAccount      bool
	AccountCash      string
	AccountEquity    string
	AccountPortfolio string
	AccountAsOf      string
	AccountNote      string

	Positions     []PositionRow
	ExposureText  string
	PositionsNote string

	// Closed positions are shown for the whole session, not only after the bell.
	// They used to appear in an end-of-day card, which was fine while the only way
	// to close a position was a strategy rule; now that the page can close one by
	// hand, the result has to be visible the moment it happens.
	ClosedRows []EODRow
	ClosedNote string
	EODNetPnL  string
	EODNetTone string
	EODWinLoss string
	// ShowTotals keeps the net-P&L footer off an empty table.
	ShowTotals bool

	// Strategy is the active configuration, rendered for display. Every value is read
	// from config rather than written into the template, so tuning a threshold and
	// restarting is reflected here — a hardcoded panel would quietly start lying the
	// first time a number changed.
	Strategy []StrategySection

	PollSeconds int
	PaperMode   bool
}

// StrategySection groups related settings under a heading.
type StrategySection struct {
	Title string
	Note  string
	Rows  []StrategyRow
}

// StrategyRow is one setting: what it is, its current value, and why it matters where
// that is not obvious from the number alone.
type StrategyRow struct {
	Label string
	Value string
	Note  string
}

// countdown renders the time to the next market boundary: the close while open, the
// next open while closed.
//
// The "next open" is not always today's. Before the open on a trading day it is, but
// after the close — or on a weekend or holiday — it is the following session, which
// only the engine's cached calendar lookup knows. When that is unavailable both
// strings are empty and the page simply shows no countdown, rather than inventing one.
func countdown(now time.Time, sess scheduler.Session, tradingDay bool,
	next scheduler.Session, nextKnown bool) (text, at string) {

	if tradingDay {
		if d, ok := scheduler.UntilClose(now, sess); ok {
			return "closes in " + scheduler.FormatCountdown(d),
				sess.Close.In(scheduler.ET).Format("15:04 MST")
		}
		// Before today's open, today's own session is the one to count towards.
		if d, ok := scheduler.UntilOpen(now, sess); ok {
			return "opens in " + scheduler.FormatCountdown(d),
				sess.Open.In(scheduler.ET).Format("15:04 MST")
		}
	}

	// Either a non-trading day, or today's session has finished.
	if nextKnown {
		if d, ok := scheduler.UntilOpen(now, next); ok {
			return "opens in " + scheduler.FormatCountdown(d),
				next.Open.In(scheduler.ET).Format("Mon 2 Jan, 15:04 MST")
		}
	}
	return "", ""
}

// staleAfter is how old the account balance may be before the page says so.
//
// The engine ticks at least once per scan interval (engine.Run takes the shorter of
// the two poll intervals) and refreshes the balance on every tick, so three scan
// intervals without a new reading is not slow polling — it is the account endpoint
// failing. Zero when the interval is unset, which suppresses the warning rather than
// declaring every reading stale.
func staleAfter(cfg *config.Config) time.Duration {
	if cfg.Timing.ScreenerScanInterval <= 0 {
		return 0
	}
	return 3 * cfg.Timing.ScreenerScanInterval
}

func money(v float64) string { return fmt.Sprintf("$%.2f", v) }

// accountMoney is money with thousands separators. Account balances are the page's
// largest figures by a couple of orders of magnitude, and "$25143.71" is the one
// number here that is easy to misread by a factor of ten at a glance.
func accountMoney(v float64) string { return "$" + groupDigits(fmt.Sprintf("%.2f", v)) }

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
// a 12-second poll cannot multiply API calls. The account balance arrives the same
// way — as a snapshot the engine already read, not as a broker call from here.
func BuildView(
	cfg *config.Config,
	st *store.Store,
	state domain.AgentState,
	errMessage string,
	sess scheduler.Session,
	tradingDay bool,
	next scheduler.Session,
	nextKnown bool,
	acct domain.AccountSnapshot,
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
	v.PreMarket = tradingDay && scheduler.PhaseAt(now, bounds) == domain.PhasePreMarket
	switch {
	case v.MarketOpen:
		v.MarketLabel, v.ExchangeTone = "OPEN", "good"
	case v.PreMarket:
		v.MarketLabel, v.ExchangeTone = "PRE-MARKET", "pre"
	default:
		v.MarketLabel, v.ExchangeTone = "CLOSED", "bad"
	}
	v.Countdown, v.CountdownAt = countdown(now, sess, tradingDay, next, nextKnown)

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
	case domain.VerdictOverridden:
		// Not green: the gate did not pass, it was stood down.
		v.GateTone = "warn"
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
	v.CanIgnoreHalt = rec.Halted && len(readings) == 0

	open, err := st.OpenPositions()
	if err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(open))
	for _, p := range open {
		held[p.Symbol] = true
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
		row.CanOpen = !held[e.Symbol]
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

	v.ShowAccount = acct.Known
	if acct.Known {
		v.AccountCash = accountMoney(acct.Account.Cash)
		v.AccountEquity = accountMoney(acct.Account.Equity)
		v.AccountPortfolio = accountMoney(acct.Account.PortfolioValue)
		v.AccountAsOf = acct.At.In(scheduler.ET).Format("15:04:05")
		// The tick refreshes this every loop, and the loop runs at most one scan
		// interval apart, so several intervals of silence means the account endpoint
		// is failing and the figure is only the last one that arrived.
		if stale := staleAfter(cfg); stale > 0 && now.Sub(acct.At) > stale {
			v.AccountNote = fmt.Sprintf("not refreshed since %s ET — the balance shown is the last one read",
				v.AccountAsOf)
		}
	} else {
		v.AccountNote = "The agent has not read the account balance yet."
	}

	var exposure float64
	for _, p := range open {
		current := p.LastPrice
		if current <= 0 {
			current = p.EntryPrice
		}
		pnl := p.UnrealizedDollars(current)
		exposure += current * float64(p.SharesOpen)

		row := PositionRow{
			ID: p.ID, Symbol: p.Symbol, Shares: p.SharesOpen,
			Entry: money(p.EntryPrice), Stop: money(p.StopPrice),
			Current: money(current), Peak: money(p.PeakPrice),
			PnLDollars: signedMoney(pnl), PnLPct: pct(p.UnrealizedPct(current)),
			Tone: toneForPnL(pnl), Manual: p.Manual,
		}
		if p.Manual {
			// The stored stop only sized it; showing it as a price would suggest it
			// protects the position.
			row.Stop = "manual"
			if p.SessionDate != date {
				row.SharesNote = "held since " + p.SessionDate
			}
		}
		if p.SharesOpen != p.Shares {
			row.SharesNote = fmt.Sprintf("of %d, %s banked",
				p.Shares, signedMoney(p.BankedDollars))
		}
		if p.InitialRisk > 0 {
			row.RMultiple = fmt.Sprintf("%.1fR", p.RMultiple(current))
		}
		v.Positions = append(v.Positions, row)
	}
	v.ExposureText = fmt.Sprintf("%d of %d slots · %s deployed",
		len(open), cfg.Risk.MaxConcurrentPositions, money(exposure))
	if len(open) == 0 {
		v.PositionsNote = "No open positions."
	}

	// Positions closed today, built whenever there are any rather than only after the
	// bell. A position closed by hand at 11:00 has to show up at 11:00 — an operator
	// who has just sold something needs to see the result, not wait four hours for an
	// end-of-day card to appear. Selected by when they closed, not when they opened:
	// a manual position can be held overnight and closed in a later session.
	dayStart, err := time.ParseInLocation("2006-01-02", date, scheduler.ET)
	if err != nil {
		return nil, err
	}
	positions, err := st.PositionsClosedBetween(dayStart, dayStart.AddDate(0, 0, 1))
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
		v.ClosedRows = append(v.ClosedRows, EODRow{
			Symbol: p.Symbol, Shares: p.Shares,
			Open: money(p.EntryPrice), Close: money(p.ExitPrice),
			PnLDollars: signedMoney(realized), PnLPct: pct(p.RealizedPct()),
			Reason: string(p.ExitReason), Tone: toneForPnL(realized),
		})
	}
	v.ShowTotals = len(v.ClosedRows) > 0
	v.EODNetPnL = signedMoney(net)
	v.EODNetTone = toneForPnL(net)
	v.EODWinLoss = fmt.Sprintf("%d up · %d down", wins, losses)
	if len(v.ClosedRows) == 0 {
		v.ClosedNote = "Nothing has been closed in this session yet."
		if !tradingDay || !now.Before(bounds.EODExit) {
			v.ClosedNote = "No positions were closed in this session."
		}
	}

	v.Strategy = strategySections(cfg)

	return v, nil
}

// strategySections describes the running strategy from the loaded configuration.
//
// Everything here is derived from cfg. Values that the code computes rather than
// reads — maximum exposure, the MACD warm-up — are computed the same way the engine
// computes them, so the panel cannot disagree with behaviour. Settings that only
// apply conditionally (a limit order's slippage allowance) appear only when they do.
func strategySections(cfg *config.Config) []StrategySection {
	pctOf := func(v float64) string { return trimNumber(v) + "%" }

	gate := StrategySection{
		Title: "1 · Sentiment gate",
		Note: fmt.Sprintf("No trades are placed during the first %s after the open.",
			durationText(cfg.Timing.SentimentWindow)),
		Rows: []StrategyRow{
			{Label: "Window after open", Value: durationText(cfg.Timing.SentimentWindow)},
			{Label: "Poll interval", Value: durationText(cfg.Timing.SentimentPollInterval)},
			{Label: "Basket", Value: strings.Join(cfg.Sentiment.Symbols, ", ")},
			{
				Label: "Halts the day when",
				Value: fmt.Sprintf("average ≤ %s", pctOf(cfg.Sentiment.BearishAvgPct)),
				Note: map[bool]string{
					true:  "and no symbol in the basket is positive",
					false: "regardless of individual symbols",
				}[cfg.Sentiment.RequireAllNegative],
			},
		},
	}

	screening := StrategySection{
		Title: "2 · Screening",
		Note: "Every tradable US equity is scanned each pass; the price move is checked " +
			"first, and only symbols clearing it incur the per-symbol lookups. Nothing here " +
			"filters on company size.",
		Rows: []StrategyRow{
			{
				Label: "Price band",
				Value: "$" + trimNumber(cfg.Screening.MinPrice) + " – $" + trimNumber(cfg.Screening.MaxPrice),
				Note:  "a tradability floor and the band the strategy trades, not momentum criteria",
			},
			{
				Label: "Minimum traded today",
				Value: "$" + groupNumber(cfg.Screening.MinDollarVolume),
				Note:  "dollar volume before entry; keeps unfillable names out",
			},
			{
				Label: "Intraday move",
				Value: "≥ " + pctOf(cfg.Screening.MinIntradayPct),
				Note:  "no ceiling: capping the move was measured and made things worse",
			},
			{
				Label: "Relative volume",
				Value: "≥ " + trimNumber(cfg.Screening.MinVolumeMultiple) + "x",
				Note: fmt.Sprintf("versus the %d-session average, excluding today",
					cfg.Screening.AvgVolumeLookbackDays),
			},
			{
				Label: "News catalyst",
				Value: "at least one story",
				Note:  "within " + durationText(cfg.Screening.NewsLookback) + ", so pre-market catalysts count",
			},
			{Label: "Scan interval", Value: durationText(cfg.Timing.ScreenerScanInterval)},
			{
				Label: "Enrichment cap",
				Value: fmt.Sprintf("%d symbols", cfg.Screening.MaxEnriched),
				Note:  "the busiest by dollar volume, when more clear the move threshold",
			},
			{
				Label: "Data feed",
				Value: cfg.MarketData.Feed,
				Note: map[bool]string{
					true:  "full consolidated tape",
					false: "single exchange — relative volume is not market-wide on this feed",
				}[cfg.MarketData.Feed == "sip"],
			},
		},
	}

	// Pre-market appears whether or not it is on, because "off" is the fact an
	// operator most needs from this panel: it explains an empty 06:00 page without
	// requiring them to go and read the config file.
	preMarket := StrategySection{
		Title: "2b · Pre-market",
		Note: "The same three criteria, run before the opening bell against a much " +
			"thinner tape — so two of the numbers they are measured against differ.",
		Rows: []StrategyRow{{
			Label: "Screening",
			Value: map[bool]string{true: "enabled", false: "disabled"}[cfg.PreMarket.Enabled],
			Note: map[bool]string{
				true:  "the agent scans before the open and the page fills",
				false: "the agent is idle until the opening bell",
			}[cfg.PreMarket.Enabled],
		}},
	}
	if cfg.PreMarket.Enabled {
		preMarket.Rows = append(preMarket.Rows,
			StrategyRow{Label: "Session start", Value: cfg.PreMarket.Start + " ET"},
			StrategyRow{
				Label: "Scan interval",
				Value: durationText(cfg.PreMarket.ScanInterval),
				Note:  "slower than the regular cadence: one pass is ~130 requests",
			},
			StrategyRow{
				Label: "Minimum traded",
				Value: "$" + groupNumber(cfg.PreMarket.MinDollarVolume),
				Note: "replaces the regular $" + groupNumber(cfg.Screening.MinDollarVolume) +
					" floor, which no pre-market tape clears",
			},
			StrategyRow{
				Label: "Relative volume",
				Value: "≥ " + trimNumber(cfg.PreMarket.MinVolumeMultiple) + "x",
				Note: "against the same " + fmt.Sprintf("%d", cfg.Screening.AvgVolumeLookbackDays) +
					"-session daily average, so a fraction of a day is a far higher bar than it looks",
			},
			StrategyRow{
				Label: "Entries",
				Value: map[bool]string{true: "allowed", false: "blocked"}[cfg.PreMarket.AllowEntry],
				Note: map[bool]string{
					true: "extended-hours limit orders, gated on a live sentiment read " +
						"standing in for the first-hour gate",
					false: "candidates are shown but nothing is bought before the bell",
				}[cfg.PreMarket.AllowEntry],
			},
		)
		if cfg.PreMarket.AllowEntry {
			// Derived the way engine.submit derives it, including the fallback, so the
			// panel cannot claim an allowance the orders are not actually priced off.
			slip := cfg.PreMarket.LimitSlipPct
			note := "pre-market spreads are wider; too tight and orders never fill"
			if slip <= 0 {
				slip = cfg.Execution.LimitSlipPct
				note = "inherited from the regular session — " + note
			}
			preMarket.Rows = append(preMarket.Rows, StrategyRow{
				Label: "Limit allowance", Value: pctOf(slip), Note: note,
			})
		}
	}

	// Derived the same way risk.SizeForRisk and config validation do, so the figures
	// shown are the figures enforced.
	maxExposure := cfg.Risk.MaxPositionPct * float64(cfg.Risk.MaxConcurrentPositions)
	maxAtRisk := cfg.Risk.RiskPerTradePct * float64(cfg.Risk.MaxConcurrentPositions)
	setupNote := fmt.Sprintf("up to %d pause bars after a %s rise over %d bars, "+
		"giving back at most %s of it, on %s candles",
		cfg.Entry.MaxPullbackBars, pctOf(cfg.Entry.MinSurgePct), cfg.Entry.SurgeBars,
		pctOf(cfg.Entry.MaxRetracePct), durationText(cfg.Entry.PatternInterval))
	if cfg.Entry.RequireMACD {
		setupNote += "; MACD above its signal"
	}
	if cfg.Entry.RequireVolumeDecline {
		setupNote += "; lighter volume on the pause"
	}
	entry := StrategySection{
		Title: "3 · Entry — setup, then size",
		Note: "Screening says a name is interesting; the setup says whether now is the " +
			"moment and where the risk sits. Day-trade only; nothing is held overnight.",
		Rows: []StrategyRow{
			{
				Label: "Setup",
				Value: "micro pullback at the high of day, then a close above the pause candle's high",
				Note:  setupNote,
			},
			{
				Label: "Trend filter",
				Value: fmt.Sprintf("above the %d-period EMA", cfg.Entry.EMAPeriod) +
					map[bool]string{true: " and VWAP", false: ""}[cfg.Entry.RequireAboveVWAP],
				Note: "the strategy only buys strength",
			},
			{
				Label: "Stop",
				Value: "just below the pause low",
				Note: fmt.Sprintf("placed %s under it, and refused beyond %s away",
					pctOf(cfg.Entry.StopBufferPct), pctOf(cfg.Entry.MaxStopDistancePct)),
			},
			{
				Label: "Setup warm-up",
				Value: durationText(strategy.WarmupDuration(cfg)),
				Note:  "after the open, before enough candles exist to read a setup",
			},
			{
				Label: "Risk per trade",
				Value: pctOf(cfg.Risk.RiskPerTradePct) + " of the account",
				Note:  "shares = risk budget ÷ distance to the stop, so every trade risks the same",
			},
			{Label: "Concurrent positions", Value: fmt.Sprintf("up to %d", cfg.Risk.MaxConcurrentPositions)},
			{
				Label: "Most at risk at once",
				Value: pctOf(maxAtRisk),
				Note:  "risk per trade × concurrency, if every stop filled",
			},
			{
				Label: "Maximum exposure",
				Value: pctOf(maxExposure),
				Note:  "position cap × concurrency",
			},
			{
				Label: "Entry window",
				Value: "first " + durationText(cfg.Timing.EntryWindow) + " after the open",
				Note: "screening continues afterwards, buying does not; it also closes " +
					durationText(cfg.Timing.EntryCutoffBuffer) + " before the forced exit, " +
					"whichever comes first",
			},
			{Label: "Ranking", Value: "highest relative volume first"},
			{
				Label: "Same-day re-entry",
				Value: map[bool]string{true: "allowed", false: "blocked"}[cfg.Risk.AllowSameDayReentry],
			},
		},
	}
	if cfg.Execution.OrderType == "limit" {
		entry.Rows = append(entry.Rows, StrategyRow{
			Label: "Order type", Value: "limit",
			Note: fmt.Sprintf("priced %s away from the quote", pctOf(cfg.Execution.LimitSlipPct)),
		})
	} else {
		entry.Rows = append(entry.Rows, StrategyRow{
			Label: "Order type", Value: cfg.Execution.OrderType,
			Note: "fills are guaranteed but can slip on thinly traded names",
		})
	}

	// The candle trail replaces "held to the forced exit" for whatever it covers.
	trailRow := StrategyRow{
		Label: "Runner",
		Value: "held to the forced exit",
		Note: "no fixed profit target on it: a +15% target with a 5% trailing stop " +
			"was measured capping gains at +9% while losses ran to −10%",
	}
	switch cfg.Exit.CandleTrail {
	case config.CandleTrailAlways:
		trailRow = StrategyRow{
			Label: "Candle trail",
			Value: "sold on the first candle below the previous candle's low",
			Note: "from the entry candle on: the stop rises to each completed " +
				durationText(cfg.Entry.PatternInterval) + " candle's low",
		}
	case config.CandleTrailAfterTarget:
		trailRow = StrategyRow{
			Label: "Candle trail",
			Value: "runner sold on the first candle below the previous candle's low",
			Note: "once the first target is banked, the stop rises to each completed " +
				durationText(cfg.Entry.PatternInterval) + " candle's low",
		}
	}

	// Listed in the order strategy.EvaluateExit checks them, because that order is
	// the priority: the first match wins, and showing them in any other order would
	// misrepresent which rule takes effect.
	exits := StrategySection{
		Title: "4 · Exits, in priority order",
		Note:  "Whichever triggers first closes the position.",
		Rows: []StrategyRow{
			{
				Label: "Forced end-of-day",
				Value: fmt.Sprintf("%d min before close", cfg.Exit.EODExitOffsetMins),
				Note:  "unconditional, regardless of P&L",
			},
			{
				Label: "Chart stop",
				Value: "the setup's stop",
				Note:  "from the pullback low, and moved to entry once the first target is banked",
			},
			{
				Label: "Gap backstop",
				Value: "−" + pctOf(cfg.Risk.StopLossPct) + " from entry",
				Note:  "only reachable if price gaps straight through the chart stop",
			},
			{
				Label: "First target",
				Value: fmt.Sprintf("%sR — sell %s", trimNumber(cfg.Exit.FirstTargetR),
					pctOf(cfg.Exit.FirstTargetFraction*100)),
				Note: map[bool]string{
					true:  "the rest runs to the close with its stop at breakeven",
					false: "the rest runs to the close",
				}[cfg.Exit.BreakevenAfterTarget],
			},
			trailRow,
		},
	}

	return []StrategySection{gate, screening, preMarket, entry, exits}
}

// trimNumber renders a configured number exactly, dropping only trailing zeros.
//
// Fixed precision would misreport a tuned value: %.1f turns 3.25 into "3.2" (Go rounds
// half to even), so someone who set 3.25 would see 3.2 and reasonably conclude their
// change had not taken effect. The shortest round-tripping form avoids that while still
// printing 10 rather than 10.0.
func trimNumber(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// groupNumber is trimNumber with thousands separators. The dollar-volume floor is
// the one setting large enough to be misread without them ("$1000000"), and grouping
// only inserts separators — it never rounds, so the exactness trimNumber exists to
// preserve still holds.
func groupNumber(v float64) string {
	return groupDigits(trimNumber(v))
}

// groupDigits inserts thousands separators into an already-formatted number.
func groupDigits(s string) string {
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	sign := ""
	if strings.HasPrefix(intPart, "-") {
		sign, intPart = "-", intPart[1:]
	}
	for i := len(intPart) - 3; i > 0; i -= 3 {
		intPart = intPart[:i] + "," + intPart[i:]
	}
	return sign + intPart + frac
}

// durationText renders a config duration the way someone reading the page would say
// it, rather than Go's "1h0m0s".
func durationText(d time.Duration) string {
	switch {
	case d == 0:
		return "0"
	case d%time.Hour == 0 && d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0 && d >= time.Minute:
		if d >= time.Hour {
			return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d/time.Minute)%60)
		}
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	default:
		return d.String()
	}
}
