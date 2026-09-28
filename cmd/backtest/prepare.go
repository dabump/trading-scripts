package main

// Stage 1 of the backtest: reduce a year of daily bars to the symbol-days that
// could possibly have been bought, then fetch the intraday and news data those
// days need.
//
// The pre-filter conditions are deliberately *supersets* of the real criteria. At
// 10:30 the agent knows only a partial day, so anything it could see is bounded by
// the full day's figures: the price it reads cannot exceed the day's high, and the
// volume it has accumulated cannot exceed the day's total. A day the full-day
// figures cannot satisfy is therefore a day no intraday moment could satisfy, and
// can be discarded without fetching intraday bars for it.

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// CandidateDay is a symbol-day worth examining minute by minute.
type CandidateDay struct {
	Symbol    string  `json:"s"`
	Date      string  `json:"d"`
	PrevClose float64 `json:"pc"`
	AvgVolume float64 `json:"av"`
}

// dailySeries is one symbol's daily history, oldest first.
type dailySeries struct {
	dates  []string
	closes []float64
	highs  []float64
	vols   []float64
}

func (c *client) findCandidates(cfg *config.Config, universe []string, from, to time.Time) ([]CandidateDay, error) {
	// The cache key carries the period and every screening value the pre-filter
	// reads. Keying on the filename alone silently reuses one period's candidate
	// list for another, which is a mistake that produces a plausible-looking report
	// for the wrong dates.
	key := fmt.Sprintf("candidates-%s_%s-p%g-d%g-m%g-v%g-l%d-n%d.json",
		from.Format("20060102"), to.Format("20060102"),
		cfg.Screening.MinPrice, cfg.Screening.MinDollarVolume,
		cfg.Screening.MinIntradayPct, cfg.Screening.MinVolumeMultiple,
		cfg.Screening.AvgVolumeLookbackDays, len(universe))
	return cached(c, key, func() ([]CandidateDay, error) {
		// A lead-in is needed so the first tradeable day already has 20 prior
		// sessions behind it for the average-volume denominator.
		leadIn := from.AddDate(0, 0, -50)

		var (
			mu  sync.Mutex
			out []CandidateDay
			wg  sync.WaitGroup
			// Batches are independent, so a handful in flight shortens a run that
			// is otherwise dominated by round trips.
			sem     = make(chan struct{}, 6)
			firstEr error
			done    int
		)
		batches := chunk(universe, barsPerSymbolRequest)
		for _, batch := range batches {
			wg.Add(1)
			go func(batch []string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				raw, err := c.bars(batch, "1Day", leadIn, to)
				mu.Lock()
				defer mu.Unlock()
				done++
				fmt.Fprintf(os.Stderr, "\rdaily bars: batch %d/%d", done, len(batches))
				if err != nil {
					if firstEr == nil {
						firstEr = err
					}
					return
				}
				for sym, bars := range raw {
					out = append(out, scanSeries(cfg, sym, bars, from)...)
				}
			}(batch)
		}
		wg.Wait()
		fmt.Fprintln(os.Stderr)
		if firstEr != nil {
			return nil, firstEr
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Date != out[j].Date {
				return out[i].Date < out[j].Date
			}
			return out[i].Symbol < out[j].Symbol
		})
		return out, nil
	})
}

// scanSeries applies the superset pre-filter to one symbol's daily history.
func scanSeries(cfg *config.Config, symbol string, bars []Bar, from time.Time) []CandidateDay {
	if len(bars) < 22 {
		return nil
	}
	s := dailySeries{}
	for _, b := range bars {
		s.dates = append(s.dates, b.T.In(scheduler.ET).Format("2006-01-02"))
		s.closes = append(s.closes, b.C)
		s.highs = append(s.highs, b.H)
		s.vols = append(s.vols, b.V)
	}

	lookback := cfg.Screening.AvgVolumeLookbackDays
	fromDate := from.In(scheduler.ET).Format("2006-01-02")

	var out []CandidateDay
	for i := lookback; i < len(s.dates); i++ {
		if s.dates[i] < fromDate {
			continue
		}
		// The average excludes the day being judged, exactly as
		// broker.AverageVolumeExcludingToday does.
		var sum float64
		for j := i - lookback; j < i; j++ {
			sum += s.vols[j]
		}
		avg := sum / float64(lookback)
		if avg <= 0 {
			continue
		}
		prevClose := s.closes[i-1]
		if prevClose <= 0 {
			continue
		}

		// Superset of the intraday move: the agent can never read a price above
		// the day's high.
		if s.highs[i] < prevClose*(1+cfg.Screening.MinIntradayPct/100) {
			continue
		}
		// Superset of relative volume: accumulated volume cannot exceed the day's.
		if s.vols[i] < avg*cfg.Screening.MinVolumeMultiple {
			continue
		}
		// Supersets of both tradability floors.
		if s.highs[i] < cfg.Screening.MinPrice {
			continue
		}
		if s.highs[i]*s.vols[i] < cfg.Screening.MinDollarVolume {
			continue
		}
		out = append(out, CandidateDay{
			Symbol: symbol, Date: s.dates[i], PrevClose: prevClose, AvgVolume: avg,
		})
	}
	return out
}

// DayData is everything the simulator needs for one session.
type DayData struct {
	Date string `json:"date"`
	// Intraday holds regular-session intraday bars per symbol, oldest first,
	// including the sentiment basket.
	Intraday map[string][]Bar `json:"intraday"`
	// News holds story timestamps per symbol, covering the lookback window.
	News map[string][]time.Time `json:"news"`
	// Candidates are the pre-filtered symbol-days for this date.
	Candidates []CandidateDay `json:"candidates"`
}

// loadDay fetches (and caches) one session's intraday bars and news.
func (c *client) loadDay(cfg *config.Config, date, timeframe string, cands []CandidateDay) (*DayData, error) {
	d, err := cached(c, "days/"+timeframe+"-"+date+".json", func() (*DayData, error) {
		day, err := time.ParseInLocation("2006-01-02", date, scheduler.ET)
		if err != nil {
			return nil, err
		}
		symbols := make([]string, 0, len(cands)+len(cfg.Sentiment.Symbols))
		for _, cd := range cands {
			symbols = append(symbols, cd.Symbol)
		}
		symbols = append(symbols, cfg.Sentiment.Symbols...)

		// Regular session only: the agent's volume comparison is against daily
		// volume, and pre-market prints would inflate the accumulation.
		open := day.Add(9*time.Hour + 30*time.Minute)
		close := day.Add(16 * time.Hour)

		intraday, err := c.bars(symbols, timeframe, open, close)
		if err != nil {
			return nil, err
		}
		for sym, bs := range intraday {
			kept := bs[:0]
			for _, b := range bs {
				t := b.T.In(scheduler.ET)
				if !t.Before(open) && t.Before(close) {
					kept = append(kept, b)
				}
			}
			sort.Slice(kept, func(i, j int) bool { return kept[i].T.Before(kept[j].T) })
			intraday[sym] = kept
		}

		// The news window has to reach back past the open, so the earliest scan of
		// the day can still see an overnight catalyst.
		newsFrom := open.Add(-cfg.Screening.NewsLookback)
		news := map[string][]time.Time{}
		if len(cands) > 0 {
			syms := make([]string, 0, len(cands))
			for _, cd := range cands {
				syms = append(syms, cd.Symbol)
			}
			news, err = c.newsTimes(syms, newsFrom, close)
			if err != nil {
				return nil, err
			}
		}
		return &DayData{Date: date, Intraday: intraday, News: news}, nil
	})
	if err != nil {
		return nil, err
	}
	d.Candidates = cands
	return d, nil
}

func chunk[T any](in []T, size int) [][]T {
	var out [][]T
	for i := 0; i < len(in); i += size {
		out = append(out, in[i:min(i+size, len(in))])
	}
	return out
}
