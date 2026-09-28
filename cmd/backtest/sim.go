package main

// Stage 2 of the backtest: replay each session bar by bar, making entry and exit
// decisions with the daemon's own screener, risk and strategy packages.
//
// Nothing here reimplements a rule. screener.Tradable, screener.Evaluate,
// screener.Qualifying, sentiment.Classify, risk.AllowEntry, risk.Size,
// strategy.EvaluateExit and strategy.TrailArmed are the same functions cmd/agent
// calls, so a tuning change to the daemon changes this measurement too — which is
// the point. A backtest with its own copy of the rules eventually reports on a
// strategy nobody is running.
//
// The work is split in two because the parameter sweep varies only the exit
// settings: `prepare` runs the screening criteria once per session, and `simulate`
// replays entry gating and exits over that. Consequently the sweep must not vary
// anything under cfg.Screening — those values are baked into the prepared days.

import (
	"math"
	"sort"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/risk"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/screener"
	"github.com/martincoetzee/trading-agent/internal/sentiment"
	"github.com/martincoetzee/trading-agent/internal/strategy"
)

// qualifier is one candidate that passed all three criteria at one instant, with
// the price a scan at that instant would have read.
type qualifier struct {
	Symbol  string
	Price   float64
	VolMult float64
	MovePct float64
}

// preparedDay is one session reduced to the decisions the screen produced, plus
// the bar data the exit rules need.
type preparedDay struct {
	Date        string
	EODAt       time.Time
	LastBarAt   time.Time
	Boundaries  []time.Time
	GateBearish bool
	// NoGateData marks a session whose sentiment basket could not be read, which
	// is excluded rather than assumed tradeable.
	NoGateData bool

	// bars is indexed by symbol then bar-start unix seconds, so a lookup while
	// walking the clock is constant time rather than a scan.
	bars   map[string]map[int64]Bar
	series map[string][]Bar

	// qualifiers[i] are the ranked candidates at Boundaries[i].
	qualifiers [][]qualifier

	funnel funnel
}

// funnel counts how a session's candidates were whittled down, once per
// symbol-day rather than once per scan.
type funnel struct {
	SymbolDays     int
	FailedTradable int
	PassedTradable int
	EverPassedNews int
	EverPassedMove int
	EverPassedVol  int
	EverQualified  int
}

// prepare applies the sentiment gate and the three screening criteria to every
// session, leaving the exit rules to simulate.
func prepare(cfg *config.Config, days []*DayData, benchPrev map[string]map[string]float64) []*preparedDay {
	out := make([]*preparedDay, 0, len(days))
	for _, day := range days {
		sess, ok := sessionFor(day.Date)
		if !ok {
			continue
		}
		p := &preparedDay{
			Date:   day.Date,
			EODAt:  sess.close.Add(-time.Duration(cfg.Exit.EODExitOffsetMins) * time.Minute),
			bars:   map[string]map[int64]Bar{},
			series: day.Intraday,
		}
		for sym, bs := range day.Intraday {
			idx := make(map[int64]Bar, len(bs))
			for _, b := range bs {
				idx[b.T.Unix()] = b
			}
			p.bars[sym] = idx
		}

		gateAt := sess.open.Add(cfg.Timing.SentimentWindow)
		pcts := map[string]float64{}
		for _, sym := range cfg.Sentiment.Symbols {
			prev := benchPrev[day.Date][sym]
			px, ok := priceAt(day.Intraday[sym], gateAt)
			if !ok || prev <= 0 {
				continue
			}
			pcts[sym] = (px - prev) / prev * 100
		}
		// An unreadable basket must not become a trading day. sentiment.Classify
		// returns PENDING for an empty reading, which the daemon treats as "keep
		// polling" — here it would silently mean "proceed", turning missing data
		// into trades the agent would never have made.
		if len(pcts) == 0 {
			p.NoGateData = true
			out = append(out, p)
			continue
		}
		p.GateBearish = sentiment.Classify(pcts, cfg) == domain.VerdictBearish

		cands := make(map[string]CandidateDay, len(day.Candidates))
		for _, cd := range day.Candidates {
			if len(day.Intraday[cd.Symbol]) > 0 {
				cands[cd.Symbol] = cd
			}
		}
		p.funnel.SymbolDays = len(cands)
		p.Boundaries = boundaries(day, gateAt, sess.close)
		if n := len(p.Boundaries); n > 0 {
			p.LastBarAt = p.Boundaries[n-1]
		}

		if p.GateBearish {
			// A halted session still contributes its candidate count, but nothing
			// downstream of the gate ran, so no criterion tallies are recorded.
			out = append(out, p)
			continue
		}

		cumVol := map[string]float64{}
		type flags struct{ tradable, news, move, vol, qualified bool }
		seen := map[string]*flags{}
		for sym := range cands {
			seen[sym] = &flags{}
		}

		p.qualifiers = make([][]qualifier, len(p.Boundaries))
		for i, t := range p.Boundaries {
			if !t.Before(p.EODAt) {
				// Past the forced-exit mark the agent only closes positions.
				advanceVolume(p, cands, cumVol, t)
				continue
			}
			evals := make([]domain.Evaluation, 0, len(cands))
			prices := make(map[string]float64, len(cands))
			moves := make(map[string]float64, len(cands))
			for sym, cd := range cands {
				bar, ok := p.barAt(sym, t)
				if !ok {
					continue
				}
				price := bar.O
				vol := cumVol[sym]
				if ok, _ := screener.Tradable(price, price*vol, cfg); !ok {
					continue
				}
				seen[sym].tradable = true
				in := screener.Input{
					Symbol:      sym,
					Price:       price,
					IntradayPct: (price - cd.PrevClose) / cd.PrevClose * 100,
					TodayVolume: vol,
					AvgVolume:   cd.AvgVolume,
					NewsCount:   newsCount(day.News[sym], t, cfg.Screening.NewsLookback),
				}
				e := screener.Evaluate(in, cfg)
				for _, c := range e.Criteria {
					if !c.Pass {
						continue
					}
					switch c.Name {
					case screener.CriterionNews:
						seen[sym].news = true
					case screener.CriterionMove:
						seen[sym].move = true
					case screener.CriterionVolume:
						seen[sym].vol = true
					}
				}
				if e.Qualifies {
					seen[sym].qualified = true
				}
				evals = append(evals, e)
				prices[sym] = price
				moves[sym] = in.IntradayPct
			}
			for _, e := range screener.Qualifying(evals) {
				p.qualifiers[i] = append(p.qualifiers[i], qualifier{
					Symbol: e.Symbol, Price: prices[e.Symbol], VolMult: e.VolumeMultiple,
					MovePct: moves[e.Symbol],
				})
			}
			advanceVolume(p, cands, cumVol, t)
		}

		for _, f := range seen {
			if f.tradable {
				p.funnel.PassedTradable++
			} else {
				p.funnel.FailedTradable++
			}
			if f.news {
				p.funnel.EverPassedNews++
			}
			if f.move {
				p.funnel.EverPassedMove++
			}
			if f.vol {
				p.funnel.EverPassedVol++
			}
			if f.qualified {
				p.funnel.EverQualified++
			}
		}
		out = append(out, p)
	}
	return out
}

func (p *preparedDay) barAt(symbol string, t time.Time) (Bar, bool) {
	b, ok := p.bars[symbol][t.Unix()]
	return b, ok
}

// Trade is one completed round trip, with the diagnostics needed to judge whether
// the exit rules gave up money.
type Trade struct {
	Symbol     string
	Date       string
	EntryTime  time.Time
	EntryPrice float64
	Shares     int
	ExitTime   time.Time
	ExitPrice  float64
	Reason     domain.ExitReason
	ReturnPct  float64
	PnL        float64
	VolMult    float64
	MovePct    float64
	HoldMins   float64

	// MFE/MAE are the best and worst the position reached, measured to the end of
	// the session rather than only up to the exit. MFEAfterExit − ReturnPct is how
	// much higher it went once the rule had sold: money a different rule could
	// have captured. CloseIfHeldPct is what simply holding to the bell would have
	// returned.
	MFEPct         float64
	MAEPct         float64
	MFEBeforeExit  float64
	MFEAfterExit   float64
	CloseIfHeldPct float64
}

// Stats summarises one run.
type Stats struct {
	Trades        []Trade
	StartEquity   float64
	EndEquity     float64
	MaxDrawdownPc float64
	TradingDays   int
	HaltedDays    int
	SkippedNoData int
	// PositionSizePct is echoed from the config so the report can label the
	// affordability gate with the size actually used rather than a literal.
	PositionSizePct float64

	Funnel funnel
	// Blocked* count symbol-days that qualified at some point but were never
	// bought, attributed to the first gate that turned them away. Counting every
	// scan instead would multiply each one by the number of minutes it stayed
	// qualified.
	BlockedByCap    int
	BlockedByCash   int
	BlockedSameDay  int
	BlockedHeld     int
	MaxDayTrades5Bd int
}

// simulate walks every prepared session, opening positions the entry gates allow
// and closing them with strategy.EvaluateExit.
//
// slippagePct is charged against the trader on both sides: 0.25 means buying 0.25%
// higher and selling 0.25% lower than the modelled price.
func simulate(cfg *config.Config, days []*preparedDay, slippagePct float64) Stats {
	const startEquity = 10_000.0

	st := Stats{StartEquity: startEquity, PositionSizePct: cfg.Risk.PositionSizePct}
	cash := startEquity
	peakEquity := startEquity
	dayTrades := map[string]int{}
	sessions := make([]string, 0, len(days))

	for _, day := range days {
		if day.NoGateData {
			st.SkippedNoData++
			continue
		}
		st.TradingDays++
		sessions = append(sessions, day.Date)
		st.Funnel.SymbolDays += day.funnel.SymbolDays
		st.Funnel.FailedTradable += day.funnel.FailedTradable
		st.Funnel.PassedTradable += day.funnel.PassedTradable
		st.Funnel.EverPassedNews += day.funnel.EverPassedNews
		st.Funnel.EverPassedMove += day.funnel.EverPassedMove
		st.Funnel.EverPassedVol += day.funnel.EverPassedVol
		st.Funnel.EverQualified += day.funnel.EverQualified

		if day.GateBearish {
			st.HaltedDays++
			continue
		}

		open := map[string]*domain.Position{}
		volMult := map[string]float64{}
		movePct := map[string]float64{}
		tradedToday := map[string]bool{}
		qualifiedToday := map[string]bool{}
		enteredToday := map[string]bool{}
		firstBlock := map[string]string{}

		for i, t := range day.Boundaries {
			// Positions are managed before screening, as engine.Tick does: a stop
			// that fires on this bar frees its slot for this bar's entries.
			for sym, pos := range open {
				bar, ok := day.barAt(sym, t)
				if !ok {
					continue
				}
				forced := !t.Before(day.EODAt)
				if reason, price := checkExit(pos, bar, forced, cfg); reason != "" {
					st.Trades = append(st.Trades, closeTrade(&cash, day, sym, pos,
						price*(1-slippagePct/100), t, reason, volMult[sym], movePct[sym]))
					delete(open, sym)
					dayTrades[day.Date]++
				}
			}
			if !t.Before(day.EODAt) {
				continue
			}

			for _, q := range day.qualifiers[i] {
				qualifiedToday[q.Symbol] = true
				allowed, reason, _ := risk.AllowEntry(q.Symbol, openSymbols(open),
					tradedToday, len(open), cfg)
				if !allowed {
					if _, seen := firstBlock[q.Symbol]; !seen {
						firstBlock[q.Symbol] = reason
					}
					continue
				}
				equity := cash + marketValue(open, day, t)
				sz := risk.Size(domain.Account{
					PortfolioValue: equity, Cash: cash, Equity: equity,
				}, q.Price, cfg)
				if !sz.OK {
					if _, seen := firstBlock[q.Symbol]; !seen {
						firstBlock[q.Symbol] = "unaffordable: " + sz.Reason
					}
					continue
				}
				fill := q.Price * (1 + slippagePct/100)
				cash -= fill * float64(sz.Shares)
				open[q.Symbol] = &domain.Position{
					SessionDate: day.Date, Symbol: q.Symbol, Shares: sz.Shares,
					EntryPrice: fill, EntryTime: t, PeakPrice: fill, LastPrice: fill,
					Open: true,
				}
				volMult[q.Symbol] = q.VolMult
				movePct[q.Symbol] = q.MovePct
				tradedToday[q.Symbol] = true
				enteredToday[q.Symbol] = true
			}
		}

		// Anything still open is exited at the last available price. The forced
		// exit means this should only happen when a symbol's bars stop early.
		for sym, pos := range open {
			bars := day.series[sym]
			if len(bars) == 0 {
				continue
			}
			last := bars[len(bars)-1]
			st.Trades = append(st.Trades, closeTrade(&cash, day, sym, pos,
				last.C*(1-slippagePct/100), last.T.In(scheduler.ET),
				domain.ExitForcedEOD, volMult[sym], movePct[sym]))
			dayTrades[day.Date]++
		}

		// Attribute every symbol that qualified but never got bought to whichever
		// gate first turned it away.
		for sym := range qualifiedToday {
			if enteredToday[sym] {
				continue
			}
			switch r := firstBlock[sym]; {
			case indexOf(r, "cap") >= 0:
				st.BlockedByCap++
			case indexOf(r, "unaffordable") >= 0:
				st.BlockedByCash++
			case indexOf(r, "traded today") >= 0:
				st.BlockedSameDay++
			case indexOf(r, "holding") >= 0:
				st.BlockedHeld++
			}
		}

		if cash > peakEquity {
			peakEquity = cash
		}
		if dd := (peakEquity - cash) / peakEquity * 100; dd > st.MaxDrawdownPc {
			st.MaxDrawdownPc = dd
		}
	}

	st.EndEquity = cash
	st.MaxDayTrades5Bd = maxDayTradesInWindow(dayTrades, sessions)
	return st
}

// checkExit asks strategy.EvaluateExit about one bar, testing the bar's low so an
// intrabar trigger is not missed, and returns the price the fill is modelled at.
//
// The low is tested before the peak is raised, which is the unfavourable ordering:
// within a single bar the adverse extreme is assumed to come first.
func checkExit(pos *domain.Position, bar Bar, forced bool, cfg *config.Config) (domain.ExitReason, float64) {
	if forced {
		return domain.ExitForcedEOD, bar.O
	}
	if d := strategy.EvaluateExit(strategy.ExitInput{Position: *pos, Price: bar.L}, cfg); d.Exit {
		return d.Reason, thresholdPrice(pos, d.Reason, cfg, bar)
	}
	// The peak has no role in an exit any more, but it is still what the daemon
	// records and what the status page shows, so it is still tracked here.
	if bar.H > pos.PeakPrice {
		pos.PeakPrice = bar.H
	}
	pos.LastPrice = bar.C
	return "", 0
}

// thresholdPrice is the price a trigger is modelled as filling at: the stop itself,
// not the bar's low. A real stop gaps through, which is what the slippage sweep
// exists to bound.
func thresholdPrice(pos *domain.Position, reason domain.ExitReason, cfg *config.Config, bar Bar) float64 {
	px := bar.C
	if reason == domain.ExitStopLoss {
		px = pos.EntryPrice * (1 - cfg.Risk.StopLossPct/100)
	}
	// A bar that opened below the threshold could not have filled at it.
	if bar.O < px {
		px = bar.O
	}
	return px
}

// capped drops qualifiers whose move at entry exceeds pct, which is how a ceiling on
// the move is measured without adding a config setting for it.
func capped(days []*preparedDay, pct float64) []*preparedDay {
	out := make([]*preparedDay, len(days))
	for i, d := range days {
		copyDay := *d
		copyDay.qualifiers = make([][]qualifier, len(d.qualifiers))
		for j, qs := range d.qualifiers {
			for _, q := range qs {
				if q.MovePct <= pct {
					copyDay.qualifiers[j] = append(copyDay.qualifiers[j], q)
				}
			}
		}
		out[i] = &copyDay
	}
	return out
}

// reranked returns the same sessions with each instant's qualifier list reordered,
// so a tie-break rule can be measured without touching the screening pass. The bar
// data is shared, not copied: simulate only reads it.
func reranked(days []*preparedDay, less func(a, b qualifier) bool) []*preparedDay {
	out := make([]*preparedDay, len(days))
	for i, d := range days {
		copyDay := *d
		copyDay.qualifiers = make([][]qualifier, len(d.qualifiers))
		for j, qs := range d.qualifiers {
			if len(qs) == 0 {
				continue
			}
			cp := append([]qualifier(nil), qs...)
			sort.SliceStable(cp, func(a, b int) bool { return less(cp[a], cp[b]) })
			copyDay.qualifiers[j] = cp
		}
		out[i] = &copyDay
	}
	return out
}

func closeTrade(cash *float64, day *preparedDay, sym string, pos *domain.Position,
	fill float64, at time.Time, reason domain.ExitReason, volMult, movePct float64) Trade {

	*cash += fill * float64(pos.Shares)
	mfe, mae, mfeBefore, mfeAfter, closeIfHeld := excursions(day.series[sym], pos, at)
	return Trade{
		Symbol: sym, Date: day.Date, EntryTime: pos.EntryTime, EntryPrice: pos.EntryPrice,
		Shares: pos.Shares, ExitTime: at, ExitPrice: fill, Reason: reason,
		ReturnPct: (fill - pos.EntryPrice) / pos.EntryPrice * 100,
		PnL:       (fill - pos.EntryPrice) * float64(pos.Shares),
		VolMult:   volMult,
		MovePct:   movePct,
		HoldMins:  at.Sub(pos.EntryTime).Minutes(),
		MFEPct:    mfe, MAEPct: mae, MFEBeforeExit: mfeBefore, MFEAfterExit: mfeAfter,
		CloseIfHeldPct: closeIfHeld,
	}
}

// excursions measures how far the trade went in each direction, split at the exit,
// which is what distinguishes "the exit was early" from "the exit was right".
func excursions(bars []Bar, pos *domain.Position, exitAt time.Time) (mfe, mae, mfeBefore, mfeAfter, closeIfHeld float64) {
	entry := pos.EntryPrice
	mfe, mae = math.Inf(-1), math.Inf(1)
	mfeBefore, mfeAfter = math.Inf(-1), math.Inf(-1)
	var lastClose float64
	for _, b := range bars {
		t := b.T.In(scheduler.ET)
		if t.Before(pos.EntryTime) {
			continue
		}
		hi := (b.H - entry) / entry * 100
		lo := (b.L - entry) / entry * 100
		mfe = math.Max(mfe, hi)
		mae = math.Min(mae, lo)
		if t.Before(exitAt) {
			mfeBefore = math.Max(mfeBefore, hi)
		} else {
			mfeAfter = math.Max(mfeAfter, hi)
		}
		lastClose = b.C
	}
	for _, v := range []*float64{&mfe, &mae, &mfeBefore, &mfeAfter} {
		if math.IsInf(*v, 0) {
			*v = 0
		}
	}
	if lastClose > 0 {
		closeIfHeld = (lastClose - entry) / entry * 100
	}
	return
}

type session struct{ open, close time.Time }

func sessionFor(date string) (session, bool) {
	d, err := time.ParseInLocation("2006-01-02", date, scheduler.ET)
	if err != nil {
		return session{}, false
	}
	return session{
		open:  d.Add(9*time.Hour + 30*time.Minute),
		close: d.Add(16 * time.Hour),
	}, true
}

// boundaries lists the decision instants in the session, derived from the bars
// actually present so a half-day needs no special case.
func boundaries(day *DayData, from, to time.Time) []time.Time {
	seen := map[int64]bool{}
	var out []time.Time
	for _, bars := range day.Intraday {
		for _, b := range bars {
			t := b.T.In(scheduler.ET)
			if t.Before(from) || !t.Before(to) || seen[t.Unix()] {
				continue
			}
			seen[t.Unix()] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// priceAt is the first traded price at or after t, which is what a scan running at
// t would read.
func priceAt(bars []Bar, t time.Time) (float64, bool) {
	for _, b := range bars {
		if !b.T.In(scheduler.ET).Before(t) {
			return b.O, true
		}
	}
	return 0, false
}

// advanceVolume folds the bar at t into the running total, after every decision at
// t has been made. This is what keeps the volume figure free of look-ahead.
func advanceVolume(p *preparedDay, cands map[string]CandidateDay, cum map[string]float64, t time.Time) {
	for sym := range cands {
		if b, ok := p.barAt(sym, t); ok {
			cum[sym] += b.V
		}
	}
}

func newsCount(times []time.Time, at time.Time, lookback time.Duration) int {
	n := 0
	from := at.Add(-lookback)
	for _, ts := range times {
		if ts.After(from) && !ts.After(at) {
			n++
		}
	}
	return n
}

func openSymbols(open map[string]*domain.Position) map[string]bool {
	out := make(map[string]bool, len(open))
	for s := range open {
		out[s] = true
	}
	return out
}

func marketValue(open map[string]*domain.Position, day *preparedDay, t time.Time) float64 {
	var v float64
	for sym, pos := range open {
		px := pos.LastPrice
		if b, ok := day.barAt(sym, t); ok {
			px = b.O
		}
		v += px * float64(pos.Shares)
	}
	return v
}

// maxDayTradesInWindow reports the worst rolling five-session day-trade count,
// which is the figure the Pattern Day Trader rule caps at 3 for a sub-$25k margin
// account.
//
// The window slides over `sessions` — every session in the period — rather than
// over the days that happened to trade. Sliding over trade-bearing days only would
// stretch a "5-day" window across weeks whenever trading was sparse, and report a
// breach the rule would not have counted.
func maxDayTradesInWindow(perDay map[string]int, sessions []string) int {
	if len(perDay) == 0 || len(sessions) == 0 {
		return 0
	}
	ordered := append([]string(nil), sessions...)
	sort.Strings(ordered)

	worst := 0
	for i := range ordered {
		sum := 0
		for j := i; j < len(ordered) && j < i+5; j++ {
			sum += perDay[ordered[j]]
		}
		if sum > worst {
			worst = sum
		}
	}
	return worst
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
