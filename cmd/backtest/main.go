// Command backtest measures the configured strategy against real Alpaca history.
//
// It is a measurement tool, not part of the daemon: it makes only GET requests to
// the market-data host and can place no orders. It deliberately imports
// internal/screener, internal/risk, internal/sentiment and internal/strategy so
// that the rules it measures are the rules cmd/agent runs — a backtest with its own
// copy of the logic drifts from the daemon and then reports on a strategy nobody is
// running.
//
// Usage:
//
//	go run ./cmd/backtest -from 2025-09-28 -to 2026-09-28
//
// Needs ALPACA_API_KEY / ALPACA_API_SECRET (see .env.example). Fetched data is
// cached under -cache, so re-running after a config change costs no API calls.
package main

import (
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

func main() {
	var (
		cfgPath   = flag.String("config", "config/config.yaml", "strategy configuration to measure")
		fromFlag  = flag.String("from", "", "first session (YYYY-MM-DD); defaults to one year before -to")
		toFlag    = flag.String("to", "", "last session (YYYY-MM-DD); defaults to today")
		cacheDir  = flag.String("cache", "/tmp/backtest-cache", "directory for cached market data")
		timeframe = flag.String("timeframe", "5Min", "intraday bar size used to replay each session")
		limit     = flag.Int("symbols", 0, "cap the universe (0 = every tradable US equity)")
		grid      = flag.Bool("grid", true, "sweep exit parameters and report the surface")
	)
	flag.Parse()

	if err := run(*cfgPath, *fromFlag, *toFlag, *cacheDir, *timeframe, *limit, *grid); err != nil {
		fmt.Fprintf(os.Stderr, "backtest: %v\n", err)
		os.Exit(1)
	}
}

func run(cfgPath, fromFlag, toFlag, cacheDir, timeframe string, limit int, grid bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	secrets, err := config.LoadSecrets()
	if err != nil {
		return err
	}

	to := time.Now().In(scheduler.ET)
	if toFlag != "" {
		if to, err = time.ParseInLocation("2006-01-02", toFlag, scheduler.ET); err != nil {
			return err
		}
	}
	from := to.AddDate(-1, 0, 0)
	if fromFlag != "" {
		if from, err = time.ParseInLocation("2006-01-02", fromFlag, scheduler.ET); err != nil {
			return err
		}
	}

	c := &client{
		key: secrets.APIKey, secret: secrets.APISecret,
		dataURL: "https://data.alpaca.markets", tradeURL: secrets.BaseURL,
		feed: cfg.MarketData.Feed, cacheDir: cacheDir,
		http: &http.Client{Timeout: 120 * time.Second},
	}

	fmt.Printf("Period      %s → %s\n", from.Format("2006-01-02"), to.Format("2006-01-02"))
	fmt.Printf("Feed        %s   Bars %s   Config %s\n\n", cfg.MarketData.Feed, timeframe, cfgPath)

	universe, err := c.tradableAssets()
	if err != nil {
		return err
	}
	sort.Strings(universe)
	if limit > 0 && limit < len(universe) {
		// Take an even spread rather than the first N. The list is alphabetical, so
		// a prefix would sample A-names and call it a market.
		step := len(universe) / limit
		sampled := make([]string, 0, limit)
		for i := 0; i < len(universe) && len(sampled) < limit; i += step {
			sampled = append(sampled, universe[i])
		}
		universe = sampled
	}
	fmt.Fprintf(os.Stderr, "universe: %d symbols\n", len(universe))

	cands, err := c.findCandidates(cfg, universe, from, to)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "pre-filtered candidate symbol-days: %d\n", len(cands))

	benchPrev, err := c.benchmarkPrevCloses(cfg, from, to)
	if err != nil {
		return err
	}

	byDate := map[string][]CandidateDay{}
	var dates []string
	for _, cd := range cands {
		if _, seen := byDate[cd.Date]; !seen {
			dates = append(dates, cd.Date)
		}
		byDate[cd.Date] = append(byDate[cd.Date], cd)
	}
	sort.Strings(dates)

	days := make([]*DayData, 0, len(dates))
	for i, d := range dates {
		fmt.Fprintf(os.Stderr, "\rintraday+news: %d/%d sessions", i+1, len(dates))
		day, err := c.loadDay(cfg, d, timeframe, byDate[d])
		if err != nil {
			return fmt.Errorf("%s: %w", d, err)
		}
		days = append(days, day)
	}
	fmt.Fprintln(os.Stderr)

	prepared := prepare(cfg, days, benchPrev)

	base := simulate(cfg, prepared, 0)
	reportFunnel(base, len(dates))
	reportTrades(base, "as configured, no slippage")

	fmt.Printf("\n## Slippage sensitivity\n\n")
	fmt.Printf("%-10s %7s %11s %9s %11s %7s\n",
		"per side", "trades", "mean/trade", "median", "end equity", "maxDD")
	for _, slip := range []float64{0, 0.10, 0.25, 0.50} {
		s := simulate(cfg, prepared, slip)
		rets := returns(s.Trades)
		fmt.Printf("%-10s %7d %10.2f%% %8.2f%% %11.0f %6.1f%%\n",
			fmt.Sprintf("%.2f%%", slip), len(rets), mean(rets), median(rets),
			s.EndEquity, s.MaxDrawdownPc)
	}

	reportExitQuality(base)

	if grid {
		reportSweep(cfg, prepared)
		reportRankings(cfg, prepared)
		reportMoveCaps(cfg, prepared)
	}
	return nil
}

// benchmarkPrevCloses gives the sentiment gate its denominators.
func (c *client) benchmarkPrevCloses(cfg *config.Config, from, to time.Time) (map[string]map[string]float64, error) {
	out := map[string]map[string]float64{}
	raw, err := c.bars(cfg.Sentiment.Symbols, "1Day", from.AddDate(0, 0, -10), to)
	if err != nil {
		return nil, err
	}
	for sym, bars := range raw {
		for i := 1; i < len(bars); i++ {
			d := bars[i].T.In(scheduler.ET).Format("2006-01-02")
			if out[d] == nil {
				out[d] = map[string]float64{}
			}
			out[d][sym] = bars[i-1].C
		}
	}
	return out, nil
}

// trimPct renders a configured percentage without inventing precision.
func trimPct(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64) + "%"
}

func reportFunnel(s Stats, sessions int) {
	fmt.Printf("## Candidate funnel\n\n")
	fmt.Printf("Sessions with at least one pre-filtered candidate   %d\n", sessions)
	fmt.Printf("Sessions traded (sentiment gate open)               %d\n", s.TradingDays-s.HaltedDays)
	fmt.Printf("Sessions halted bearish                             %d\n", s.HaltedDays)
	fmt.Printf("Sessions skipped (sentiment basket unreadable)       %d\n", s.SkippedNoData)
	f := s.Funnel
	fmt.Printf("Symbol-days surviving the daily pre-filter          %d\n", f.SymbolDays)
	fmt.Printf("  rejected by the tradability floors                %d\n", f.FailedTradable)
	fmt.Printf("  cleared the floors                                %d\n", f.PassedTradable)
	fmt.Printf("Of those, ever passed each criterion intraday:\n")
	fmt.Printf("  news catalyst                                     %d\n", f.EverPassedNews)
	fmt.Printf("  intraday move                                     %d\n", f.EverPassedMove)
	fmt.Printf("  relative volume                                   %d\n", f.EverPassedVol)
	fmt.Printf("  all three at once (qualified)                     %d\n", f.EverQualified)
	fmt.Printf("Entries taken                                       %d\n", len(s.Trades))
	fmt.Printf("Qualified but never bought, by first gate hit:\n")
	fmt.Printf("  concurrency cap                                   %d\n", s.BlockedByCap)
	fmt.Printf("  already holding it                                %d\n", s.BlockedHeld)
	fmt.Printf("  same-day re-entry                                 %d\n", s.BlockedSameDay)
	fmt.Printf("  unaffordable at %-4s of equity                     %d\n",
		trimPct(s.PositionSizePct), s.BlockedByCash)
	fmt.Printf("Worst rolling 5-session day-trade count             %d (PDT limit is 3)\n", s.MaxDayTrades5Bd)
}

func reportTrades(s Stats, label string) {
	rets := returns(s.Trades)
	fmt.Printf("\n## Trades — %s\n\n", label)
	if len(rets) == 0 {
		fmt.Println("No trades.")
		return
	}
	fmt.Printf("Trades          %d\n", len(rets))
	fmt.Printf("Win rate        %.1f%%\n", winRate(rets))
	fmt.Printf("Mean return     %+.2f%%\n", mean(rets))
	fmt.Printf("Median return   %+.2f%%\n", median(rets))
	fmt.Printf("Std deviation   %.2f%%\n", stdev(rets))
	fmt.Printf("t-statistic     %.2f  (|t| below ~2 is indistinguishable from zero)\n", tStat(rets))
	fmt.Printf("Best / worst    %+.1f%% / %+.1f%%\n", maxOf(rets), minOf(rets))
	fmt.Printf("Equity          %.0f → %.0f  (%+.1f%%, max drawdown %.1f%%)\n",
		s.StartEquity, s.EndEquity, (s.EndEquity/s.StartEquity-1)*100, s.MaxDrawdownPc)

	fmt.Printf("\n%-16s %7s %9s %9s\n", "exit", "share", "mean", "median")
	for _, r := range []domain.ExitReason{domain.ExitStopLoss, domain.ExitForcedEOD} {
		var sub []float64
		for _, t := range s.Trades {
			if t.Reason == r {
				sub = append(sub, t.ReturnPct)
			}
		}
		if len(sub) == 0 {
			continue
		}
		fmt.Printf("%-16s %6.1f%% %8.2f%% %8.2f%%\n",
			r, float64(len(sub))/float64(len(rets))*100, mean(sub), median(sub))
	}
}

// reportExitQuality is the diagnostic that says whether the exit rules, rather than
// the entries, are what is costing money.
func reportExitQuality(s Stats) {
	if len(s.Trades) == 0 {
		return
	}
	fmt.Printf("\n## Exit quality\n\n")
	fmt.Println("MFE/MAE are the best and worst the position reached, measured to the session's")
	fmt.Println("end regardless of when the rule closed it. \"left behind\" is how much higher it")
	fmt.Printf("went after the exit — money a different rule could have captured.\n\n")

	fmt.Printf("%-16s %6s %9s %9s %9s %11s %11s\n",
		"exit", "n", "mean ret", "mean MFE", "mean MAE", "left behind", "if held EOD")
	groups := []domain.ExitReason{domain.ExitStopLoss, domain.ExitForcedEOD}
	for _, r := range groups {
		var ret, mfe, mae, after, held []float64
		for _, t := range s.Trades {
			if t.Reason != r {
				continue
			}
			ret = append(ret, t.ReturnPct)
			mfe = append(mfe, t.MFEPct)
			mae = append(mae, t.MAEPct)
			after = append(after, math.Max(0, t.MFEAfterExit-t.ReturnPct))
			held = append(held, t.CloseIfHeldPct)
		}
		if len(ret) == 0 {
			continue
		}
		fmt.Printf("%-16s %6d %8.2f%% %8.2f%% %8.2f%% %10.2f%% %10.2f%%\n",
			r, len(ret), mean(ret), mean(mfe), mean(mae), mean(after), mean(held))
	}

	// How many trades were ever green by a given amount tells you what a
	// reachable profit target looks like.
	fmt.Printf("\n%-18s %8s\n", "ever reached", "share")
	for _, th := range []float64{2, 5, 8, 10, 15, 20, 30} {
		n := 0
		for _, t := range s.Trades {
			if t.MFEPct >= th {
				n++
			}
		}
		fmt.Printf("%-18s %7.1f%%\n", fmt.Sprintf("%+.0f%%", th),
			float64(n)/float64(len(s.Trades))*100)
	}

	// And how many dipped by a given amount, which is what the stop distance has
	// to survive.
	fmt.Printf("\n%-18s %8s\n", "ever dipped to", "share")
	for _, th := range []float64{-2, -4, -6, -8, -10} {
		n := 0
		for _, t := range s.Trades {
			if t.MAEPct <= th {
				n++
			}
		}
		fmt.Printf("%-18s %7.1f%%\n", fmt.Sprintf("%+.0f%%", th),
			float64(n)/float64(len(s.Trades))*100)
	}

	// Entry time matters: a momentum setup found at 15:00 has an hour to work.
	fmt.Printf("\n%-14s %6s %9s %9s\n", "entry hour ET", "n", "mean", "median")
	buckets := map[int][]float64{}
	for _, t := range s.Trades {
		h := t.EntryTime.In(scheduler.ET).Hour()
		buckets[h] = append(buckets[h], t.ReturnPct)
	}
	var hours []int
	for h := range buckets {
		hours = append(hours, h)
	}
	sort.Ints(hours)
	for _, h := range hours {
		fmt.Printf("%02d:00          %6d %8.2f%% %8.2f%%\n",
			h, len(buckets[h]), mean(buckets[h]), median(buckets[h]))
	}

	// Move-at-entry buckets. The strategy requires at least +10%; whether chasing
	// a bigger gap helps or hurts is a separate question from the threshold.
	fmt.Printf("\n%-14s %6s %9s %9s\n", "move at entry", "n", "mean", "median")
	moveEdges := []float64{15, 25, 50, 100, math.Inf(1)}
	prevEdge := 0.0
	for _, e := range moveEdges {
		var sub []float64
		for _, t := range s.Trades {
			if t.MovePct > prevEdge && t.MovePct <= e {
				sub = append(sub, t.ReturnPct)
			}
		}
		if len(sub) > 0 {
			fmt.Printf("%-14s %6d %8.2f%% %8.2f%%\n",
				fmt.Sprintf("+%.0f–%.0f%%", prevEdge, e), len(sub), mean(sub), median(sub))
		}
		prevEdge = e
	}

	// Holding time, which bounds how much the exit rules can matter at all.
	holds := make([]float64, len(s.Trades))
	for i, t := range s.Trades {
		holds[i] = t.HoldMins
	}
	fmt.Printf("\nHold time      mean %.0f min, median %.0f min\n", mean(holds), median(holds))

	// The most extreme entries, listed so data artifacts are visible rather than
	// averaged away.
	extreme := append([]Trade(nil), s.Trades...)
	sort.Slice(extreme, func(i, j int) bool { return extreme[i].MovePct > extreme[j].MovePct })
	fmt.Printf("\n%-8s %-12s %8s %8s %9s %8s %-16s\n",
		"symbol", "date", "move", "relvol", "entry $", "return", "exit")
	for i, t := range extreme {
		if i >= 8 {
			break
		}
		fmt.Printf("%-8s %-12s %7.0f%% %7.0fx %9.2f %7.1f%% %-16s\n",
			t.Symbol, t.Date, t.MovePct, t.VolMult, t.EntryPrice, t.ReturnPct, t.Reason)
	}

	// Relative-volume buckets, to re-test the ranking the strategy uses for
	// tie-breaks.
	fmt.Printf("\n%-14s %6s %9s %9s\n", "rel. volume", "n", "mean", "median")
	edges := []float64{5, 8, 12, 20, 50, math.Inf(1)}
	prev := 0.0
	for _, e := range edges {
		var sub []float64
		for _, t := range s.Trades {
			if t.VolMult > prev && t.VolMult <= e {
				sub = append(sub, t.ReturnPct)
			}
		}
		if len(sub) > 0 {
			fmt.Printf("%-14s %6d %8.2f%% %8.2f%%\n",
				fmt.Sprintf("%.0f–%.0fx", prev, e), len(sub), mean(sub), median(sub))
		}
		prev = e
	}
}

// reportSweep sweeps what is still tunable now that the profit target and trailing
// stop are gone: the stop distance and the position size.
//
// It cannot sweep cfg.Screening, because screening results are precomputed once per
// session; a screening change needs reportMoveCaps, which re-prepares.
func reportSweep(base *config.Config, days []*preparedDay) {
	fmt.Printf("\n## Stop distance x position size (0.25%% slippage per side)\n\n")
	fmt.Printf("%-8s", "stop")
	sizes := []float64{2, 5, 10, 20}
	for _, sz := range sizes {
		fmt.Printf(" %13s", fmt.Sprintf("%.0f%% size", sz))
	}
	fmt.Println()

	for _, stop := range []float64{5, 8, 10, 15, 20, 30} {
		fmt.Printf("%-8s", fmt.Sprintf("%.0f%%", stop))
		for _, sz := range sizes {
			cfg := *base
			cfg.Risk.StopLossPct = stop
			cfg.Risk.PositionSizePct = sz
			s := simulate(&cfg, days, 0.25)
			fmt.Printf(" %13s", fmt.Sprintf("$%.0f/%.0f%%dd", s.EndEquity, s.MaxDrawdownPc))
		}
		fmt.Println()
	}
	fmt.Println("\nCell is end equity from $10,000 and max drawdown. Per-trade mean barely")
	fmt.Println("moves with size; the account does, because a fixed fraction compounds.")
}

// reportMoveCaps measures what a ceiling on the entry move would do. There is no
// such setting in the config, because this is the sweep that argued against adding
// one — it is kept so the conclusion can be re-tested rather than trusted.
//
// The cap is applied by dropping qualifiers above it, which is equivalent to failing
// them on the move criterion and avoids a config knob that only exists for a sweep.
func reportMoveCaps(base *config.Config, days []*preparedDay) {
	fmt.Printf("\n## What a ceiling on the move at entry would do (0.25%% slippage per side)\n\n")
	fmt.Printf("%-12s %7s %9s %7s %9s %9s %7s\n",
		"max move", "trades", "mean", "t", "median", "equity", "maxDD")
	for _, cap := range []float64{1e6, 200, 100, 50, 35, 25, 15} {
		cfg := *base
		s := simulate(&cfg, capped(days, cap), 0.25)
		r := returns(s.Trades)
		if len(r) == 0 {
			continue
		}
		label := fmt.Sprintf("+%.0f%%", cap)
		if cap >= 1e6 {
			label = "none (current)"
		}
		fmt.Printf("%-12s %7d %8.2f%% %7.2f %8.2f%% %9.0f %6.1f%%\n",
			label, len(r), mean(r), tStat(r), median(r), s.EndEquity, s.MaxDrawdownPc)
	}
}

// reportRankings measures the tie-break that decides which candidates get the free
// slots when more qualify than there is room for.
//
// The ranking is only consulted when slots are scarce, so a large difference here
// would be surprising. It is measured rather than assumed because the per-bucket
// returns hinted the current ordering was backwards, and a hint is not a finding.
func reportRankings(base *config.Config, days []*preparedDay) {
	fmt.Printf("\n## Candidate tie-break (0.25%% slippage per side)\n\n")
	fmt.Printf("%-34s %7s %9s %7s %9s\n", "ranked by", "trades", "mean", "t", "equity")

	orders := []struct {
		name string
		less func(a, b qualifier) bool
	}{
		{"highest relative volume (current)", func(a, b qualifier) bool { return a.VolMult > b.VolMult }},
		{"lowest relative volume", func(a, b qualifier) bool { return a.VolMult < b.VolMult }},
		{"smallest move so far", func(a, b qualifier) bool { return a.MovePct < b.MovePct }},
		{"largest move so far", func(a, b qualifier) bool { return a.MovePct > b.MovePct }},
		{"highest price", func(a, b qualifier) bool { return a.Price > b.Price }},
	}
	for _, o := range orders {
		s := simulate(base, reranked(days, o.less), 0.25)
		r := returns(s.Trades)
		if len(r) == 0 {
			continue
		}
		fmt.Printf("%-34s %7d %8.2f%% %7.2f %9.0f\n",
			o.name, len(r), mean(r), tStat(r), s.EndEquity)
	}
	fmt.Println("\nThe tie-break only binds when more candidates qualify than there are free")
	fmt.Println("slots, so small differences here are expected and are not evidence.")
}

// tStat is the per-trade mean over its standard error. Below about 2 the mean is
// not distinguishable from zero, which matters more than its sign.
func tStat(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	sd := stdev(v)
	if sd == 0 {
		return 0
	}
	return mean(v) / (sd / math.Sqrt(float64(len(v))))
}

func returns(ts []Trade) []float64 {
	out := make([]float64, len(ts))
	for i, t := range ts {
		out[i] = t.ReturnPct
	}
	return out
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

func stdev(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	m := mean(v)
	var s float64
	for _, x := range v {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(v)-1))
}

func winRate(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	n := 0
	for _, x := range v {
		if x > 0 {
			n++
		}
	}
	return float64(n) / float64(len(v)) * 100
}

func maxOf(v []float64) float64 {
	m := math.Inf(-1)
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

func minOf(v []float64) float64 {
	m := math.Inf(1)
	for _, x := range v {
		m = math.Min(m, x)
	}
	return m
}
