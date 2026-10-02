package main

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/strategy"
)

// The research report asks a narrower question than simulate: once a setup has fired,
// where does the price go, and in what order? simulate answers "what did these rules
// make"; this answers "what could any stop and target have made", so a rule is chosen
// from the shape of the paths rather than by sweeping until something looks good.
//
// It follows the first setup each symbol prints per session, entered at the next
// bar's open exactly as simulate enters it (including strategy.CheckEntryPrice), and
// ignores the position cap and account size: every setup is one equal-weight trade.
// Results are split into the two halves of the period so anything chosen on one half
// can be checked on the other.

// setupPath is one setup's price path from its entry bar to the forced exit.
type setupPath struct {
	date      string
	entryTime time.Time
	entry     float64
	stopPct   float64 // the chart stop's distance below entry, in percent
	volMult   float64
	movePct   float64
	bars      []Bar
	eodPrice  float64
}

func collectPaths(cfg *config.Config, days []*preparedDay) []setupPath {
	var out []setupPath
	for _, day := range days {
		if day.GateBearish || day.NoGateData {
			continue
		}
		seen := map[string]bool{}
		for i, t := range day.Boundaries {
			if !t.Before(day.EntryEndAt) {
				break
			}
			for _, q := range day.qualifiers[i] {
				if seen[q.Symbol] || q.BuyStop > 0 {
					continue
				}
				entryBar, ok := day.barAt(q.Symbol, t)
				if !ok {
					continue
				}
				setup := strategy.Setup{Entry: q.SetupClose, Stop: q.Stop, PauseHigh: q.PauseHigh}
				if strategy.CheckEntryPrice(setup, entryBar.O, cfg) != "" {
					continue
				}
				seen[q.Symbol] = true
				p := setupPath{
					date: day.Date, entryTime: t, entry: entryBar.O,
					stopPct: (entryBar.O - q.Stop) / entryBar.O * 100,
					volMult: q.VolMult, movePct: q.MovePct,
				}
				for _, b := range day.series[q.Symbol] {
					if b.T.Before(t) {
						continue
					}
					if !b.T.Before(day.EODAt) {
						p.eodPrice = b.O
						break
					}
					p.bars = append(p.bars, b)
				}
				if len(p.bars) == 0 {
					continue
				}
				if p.eodPrice == 0 {
					p.eodPrice = p.bars[len(p.bars)-1].C
				}
				out = append(out, p)
			}
		}
	}
	return out
}

// bracket is the return, in percent before costs, of a plain stop and target on one
// path: out at the stop (or the open, if a bar gapped through it), out at the target
// (likewise), else at the forced exit. A bar that reaches both is read stop-first, the
// pessimistic order. stopPct <= 0 means the chart stop; targetPct <= 0 means none.
func bracket(p setupPath, stopPct, targetPct float64) float64 {
	if stopPct <= 0 {
		stopPct = p.stopPct
	}
	stop := p.entry * (1 - stopPct/100)
	target := math.Inf(1)
	if targetPct > 0 {
		target = p.entry * (1 + targetPct/100)
	}
	ret := func(px float64) float64 { return (px/p.entry - 1) * 100 }
	for _, b := range p.bars {
		if b.L <= stop {
			return ret(math.Min(b.O, stop))
		}
		if b.H >= target {
			return ret(math.Max(b.O, target))
		}
	}
	return ret(p.eodPrice)
}

// cost is the round trip at 0.25% a side, the slippage the other reports use.
const researchCost = 0.5

type pathStats struct {
	n           int
	mean, t     float64
	win         float64
	meanPerRisk float64
	// exTop5 is the mean without the five best trades, and med the median. A heavy
	// right tail makes the mean depend on a handful of names; these say whether
	// anything is left without them.
	exTop5, med float64
}

func statsOf(rets []float64, riskPct []float64) pathStats {
	s := pathStats{n: len(rets)}
	if s.n == 0 {
		return s
	}
	s.mean, s.t, s.win, s.med = mean(rets), tStat(rets), winRate(rets), median(rets)
	sorted := append([]float64(nil), rets...)
	sort.Float64s(sorted)
	if len(sorted) > 5 {
		s.exTop5 = mean(sorted[:len(sorted)-5])
	}
	var r float64
	for i, v := range rets {
		r += v / riskPct[i]
	}
	s.meanPerRisk = r / float64(len(rets))
	return s
}

func reportResearch(cfg *config.Config, days []*preparedDay) {
	// One split date for every study, so each rule is judged on the same two halves.
	var dates []string
	for _, d := range days {
		if !d.GateBearish && !d.NoGateData {
			dates = append(dates, d.Date)
		}
	}
	if len(dates) == 0 {
		return
	}
	mid := dates[len(dates)/2]
	fmt.Printf("\n# Research: the paths after an entry\n\n")
	fmt.Printf("Halves split at %s. Equal-weight per entry, first per symbol per session, no\n", mid)
	fmt.Printf("position cap. Returns are net of %.2f%% round-trip cost.\n", researchCost)

	studyPaths("the micro pullback setup (as configured)", collectPaths(cfg, days), mid)
	for _, rule := range entryRules {
		studyPaths(rule.name, collectRule(days, rule.find), mid)
	}
}

// studyPaths reports how one entry rule's paths behave under a grid of stops and
// targets, judged on each half separately.
func studyPaths(name string, paths []setupPath, mid string) {
	var h1, h2 []setupPath
	for _, p := range paths {
		if p.date < mid {
			h1 = append(h1, p)
		} else {
			h2 = append(h2, p)
		}
	}
	fmt.Printf("\n\n# Entry: %s — %d entries (%d / %d by half)\n", name, len(paths), len(h1), len(h2))
	if len(h1) < 10 || len(h2) < 10 {
		fmt.Println("too few to study")
		return
	}

	reportPathShape(paths)

	stops := []float64{0, 2, 3, 4, 6, 8, 10, 15}
	targets := []float64{3, 5, 8, 10, 15, 20, 30, 0}
	stopLabel := func(s float64) string {
		if s <= 0 {
			return "chart"
		}
		return fmt.Sprintf("-%g%%", s)
	}
	targetLabel := func(t float64) string {
		if t <= 0 {
			return "none"
		}
		return fmt.Sprintf("+%g%%", t)
	}

	grid := func(title string, set []setupPath) {
		fmt.Printf("\n## %s (%d setups): mean %% per setup after costs, by stop × target\n\n", title, len(set))
		fmt.Printf("%-8s", "stop")
		for _, t := range targets {
			fmt.Printf(" %8s", targetLabel(t))
		}
		fmt.Println()
		for _, s := range stops {
			fmt.Printf("%-8s", stopLabel(s))
			for _, t := range targets {
				var rets []float64
				for _, p := range set {
					rets = append(rets, bracket(p, s, t)-researchCost)
				}
				fmt.Printf(" %+7.2f%%", mean(rets))
			}
			fmt.Println()
		}
	}
	grid("First half", h1)
	grid("Second half", h2)

	// The cells that held up in both halves: positive after costs in each, ranked by
	// the weaker half, which is the honest estimate of a rule picked from a grid.
	type cell struct {
		stop, target float64
		a, b, all    pathStats
	}
	var cells []cell
	for _, s := range stops {
		for _, t := range targets {
			run := func(set []setupPath) pathStats {
				var rets, risk []float64
				for _, p := range set {
					rets = append(rets, bracket(p, s, t)-researchCost)
					r := s
					if r <= 0 {
						r = p.stopPct
					}
					risk = append(risk, r)
				}
				return statsOf(rets, risk)
			}
			cells = append(cells, cell{s, t, run(h1), run(h2), run(paths)})
		}
	}
	sort.Slice(cells, func(i, j int) bool {
		return math.Min(cells[i].a.mean, cells[i].b.mean) > math.Min(cells[j].a.mean, cells[j].b.mean)
	})
	fmt.Printf("\n## Best stop × target by the weaker half\n\n")
	fmt.Printf("%-8s %-7s %9s %9s %9s %7s %7s %9s %9s %9s\n", "stop", "target", "half 1", "half 2", "all", "t",
		"win", "mean R", "ex-top-5", "median")
	for _, c := range cells[:10] {
		fmt.Printf("%-8s %-7s %+8.2f%% %+8.2f%% %+8.2f%% %7.2f %6.0f%% %+9.2f %+8.2f%% %+8.2f%%\n",
			stopLabel(c.stop), targetLabel(c.target), c.a.mean, c.b.mean, c.all.mean, c.all.t,
			c.all.win, c.all.meanPerRisk, c.all.exTop5, c.all.med)
	}

	// Where a rule works on some setups and not others, the split is worth finding.
	// Only for the best cell, and only on features fixed at entry.
	best := cells[0]
	fmt.Printf("\n## %s stop, %s target, split by what was known at entry\n\n",
		stopLabel(best.stop), targetLabel(best.target))
	split := func(name string, bucket func(setupPath) string, order []string) {
		fmt.Printf("%-16s %6s %9s %6s %9s %6s %9s\n", name, "n h1", "half 1", "n h2", "half 2", "n", "all")
		for _, k := range order {
			var r1, r2, ra []float64
			for _, p := range paths {
				if bucket(p) != k {
					continue
				}
				v := bracket(p, best.stop, best.target) - researchCost
				ra = append(ra, v)
				if p.date < mid {
					r1 = append(r1, v)
				} else {
					r2 = append(r2, v)
				}
			}
			if len(ra) == 0 {
				continue
			}
			fmt.Printf("%-16s %6d %+8.2f%% %6d %+8.2f%% %6d %+8.2f%%\n",
				k, len(r1), mean(r1), len(r2), mean(r2), len(ra), mean(ra))
		}
		fmt.Println()
	}
	split("entry hour ET", func(p setupPath) string {
		return fmt.Sprintf("%02d:00", p.entryTime.In(scheduler.ET).Hour())
	}, []string{"09:00", "10:00", "11:00", "12:00", "13:00", "14:00", "15:00"})
	split("chart stop", func(p setupPath) string {
		switch {
		case p.stopPct < 1:
			return "< 1%"
		case p.stopPct < 2:
			return "1–2%"
		case p.stopPct < 3:
			return "2–3%"
		default:
			return "3–4%"
		}
	}, []string{"< 1%", "1–2%", "2–3%", "3–4%"})
	split("rel. volume", func(p setupPath) string {
		switch {
		case p.volMult < 8:
			return "5–8x"
		case p.volMult < 20:
			return "8–20x"
		case p.volMult < 50:
			return "20–50x"
		default:
			return "50x+"
		}
	}, []string{"5–8x", "8–20x", "20–50x", "50x+"})
	split("move at entry", func(p setupPath) string {
		switch {
		case p.movePct < 25:
			return "< 25%"
		case p.movePct < 50:
			return "25–50%"
		case p.movePct < 100:
			return "50–100%"
		default:
			return "100%+"
		}
	}, []string{"< 25%", "25–50%", "50–100%", "100%+"})
	split("price", func(p setupPath) string {
		switch {
		case p.entry < 2:
			return "< $2"
		case p.entry < 5:
			return "$2–5"
		case p.entry < 10:
			return "$5–10"
		default:
			return "$10+"
		}
	}, []string{"< $2", "$2–5", "$5–10", "$10+"})
}

// reportPathShape describes the order things happen in: how far a setup tends to dip
// before it makes its high, and how long the high takes.
func reportPathShape(paths []setupPath) {
	var mfe, ddBefore, minsToHigh []float64
	reach := map[float64]int{}
	reachBeforeDip := map[[2]float64]int{}
	ups := []float64{5, 10, 20}
	dips := []float64{2, 4, 6, 10}
	for _, p := range paths {
		hi, lo, hiAt := p.entry, p.entry, p.entryTime
		loBeforeHi := p.entry
		for _, b := range p.bars {
			if b.H > hi {
				hi, hiAt, loBeforeHi = b.H, b.T, lo
			}
			lo = math.Min(lo, b.L)
		}
		mfe = append(mfe, (hi/p.entry-1)*100)
		ddBefore = append(ddBefore, (loBeforeHi/p.entry-1)*100)
		minsToHigh = append(minsToHigh, hiAt.Sub(p.entryTime).Minutes())
		for _, u := range ups {
			if hi >= p.entry*(1+u/100) {
				reach[u]++
			}
			for _, d := range dips {
				// Did +u print before the first dip of -d?
				up, down := p.entry*(1+u/100), p.entry*(1-d/100)
				for _, b := range p.bars {
					if b.L <= down {
						break
					}
					if b.H >= up {
						reachBeforeDip[[2]float64{u, d}]++
						break
					}
				}
			}
		}
	}
	n := float64(len(paths))
	fmt.Printf("\n## The shape of the path to the session's high\n\n")
	fmt.Printf("median high reached       %+.1f%%   (mean %+.1f%%)\n", median(mfe), mean(mfe))
	fmt.Printf("median dip before it      %+.1f%%   (mean %+.1f%%)\n", median(ddBefore), mean(ddBefore))
	fmt.Printf("median minutes to it      %.0f\n\n", median(minsToHigh))
	fmt.Printf("%-10s %9s", "reaches", "ever")
	for _, d := range dips {
		fmt.Printf(" %13s", fmt.Sprintf("before -%g%%", d))
	}
	fmt.Println()
	for _, u := range ups {
		fmt.Printf("%-10s %8.0f%%", fmt.Sprintf("+%g%%", u), float64(reach[u])/n*100)
		for _, d := range dips {
			fmt.Printf(" %12.0f%%", float64(reachBeforeDip[[2]float64{u, d}])/n*100)
		}
		fmt.Println()
	}
}

// entryRule finds one entry on a screened symbol's session, after it first passed the
// screen and before the entry window closes: when, at what price, and from which bar
// of the series the path starts (the fill bar itself, read stop-first).
type entryRule struct {
	name string
	find func(day *preparedDay, bars []Bar, fq firstQualification) (at time.Time, price float64, from int, ok bool)
}

var entryRules = []entryRule{
	{"at first qualification (next bar's open)", func(day *preparedDay, bars []Bar, fq firstQualification) (time.Time, float64, int, bool) {
		for i, b := range bars {
			if b.T.After(fq.at) && b.T.Before(day.EntryEndAt) {
				return b.T, b.O, i, true
			}
		}
		return time.Time{}, 0, 0, false
	}},
	{"new high of day after qualifying (buy-stop at the old high)", func(day *preparedDay, bars []Bar, fq firstQualification) (time.Time, float64, int, bool) {
		hod := 0.0
		for i, b := range bars {
			if b.T.After(fq.at) && b.T.Before(day.EntryEndAt) && hod > 0 && b.H > hod {
				return b.T, math.Max(b.O, hod), i, true
			}
			hod = math.Max(hod, b.H)
		}
		return time.Time{}, 0, 0, false
	}},
	{"15-minute opening-range breakout after qualifying", func(day *preparedDay, bars []Bar, fq firstQualification) (time.Time, float64, int, bool) {
		if len(bars) == 0 {
			return time.Time{}, 0, 0, false
		}
		open := bars[0].T.In(scheduler.ET)
		open = time.Date(open.Year(), open.Month(), open.Day(), 9, 30, 0, 0, scheduler.ET)
		orEnd := open.Add(15 * time.Minute)
		orh := 0.0
		for i, b := range bars {
			if b.T.Before(orEnd) {
				orh = math.Max(orh, b.H)
				continue
			}
			if orh > 0 && b.T.After(fq.at) && b.T.Before(day.EntryEndAt) && b.H > orh {
				return b.T, math.Max(b.O, orh), i, true
			}
		}
		return time.Time{}, 0, 0, false
	}},
	{"VWAP reclaim after qualifying (next bar's open)", func(day *preparedDay, bars []Bar, fq firstQualification) (time.Time, float64, int, bool) {
		var pv, vol float64
		dipped := false
		for i, b := range bars {
			pv += (b.H + b.L + b.C) / 3 * b.V
			vol += b.V
			if vol == 0 || !b.T.After(fq.at) {
				continue
			}
			vwap := pv / vol
			if b.L < vwap {
				dipped = true
			}
			if dipped && b.C > vwap && i+1 < len(bars) && bars[i+1].T.Before(day.EntryEndAt) {
				return bars[i+1].T, bars[i+1].O, i + 1, true
			}
		}
		return time.Time{}, 0, 0, false
	}},
}

// collectRule applies one entry rule to every screened symbol-day.
func collectRule(days []*preparedDay, find func(*preparedDay, []Bar, firstQualification) (time.Time, float64, int, bool)) []setupPath {
	var out []setupPath
	for _, day := range days {
		if day.GateBearish || day.NoGateData {
			continue
		}
		for sym, fq := range day.firstQualified {
			bars := day.series[sym]
			at, price, from, ok := find(day, bars, fq)
			if !ok || price <= 0 {
				continue
			}
			p := setupPath{date: day.Date, entryTime: at, entry: price,
				stopPct: 4, volMult: fq.volMult, movePct: fq.movePct}
			for _, b := range bars[from:] {
				if !b.T.Before(day.EODAt) {
					p.eodPrice = b.O
					break
				}
				p.bars = append(p.bars, b)
			}
			if len(p.bars) == 0 {
				continue
			}
			if p.eodPrice == 0 {
				p.eodPrice = p.bars[len(p.bars)-1].C
			}
			out = append(out, p)
		}
	}
	return out
}
