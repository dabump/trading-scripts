// Package screener applies the four entry criteria from docs/strategy.md §2.
package screener

import (
	"fmt"
	"sort"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// Criterion names, also used as the web page's column headers.
const (
	CriterionNews   = "News catalyst"
	CriterionMove   = "Intraday move"
	CriterionVolume = "Rel. volume"
)

// Thresholds are the screening numbers in force for one pass.
//
// They exist because the same three criteria are applied to two different markets.
// A pre-market pass compares against a tape carrying a few percent of regular
// session volume, so its liquidity floor and its relative-volume multiple have to be
// different numbers or the screen returns nothing at all. Resolving them once, into
// a value the pass carries, means there is a single answer to "which numbers were
// these candidates judged against" — including in the audit trail.
//
// The price band and the move threshold are deliberately not split: a name is in the
// strategy's band, or moving enough, regardless of which session is printing it.
type Thresholds struct {
	// PreMarket says which session these came from. Nothing in screening branches on
	// it; it is carried so callers can label a pass without re-deriving it.
	PreMarket         bool
	MinPrice          float64
	MaxPrice          float64
	MinIntradayPct    float64
	MinDollarVolume   float64
	MinVolumeMultiple float64
}

// ThresholdsFor resolves the numbers a pass runs with.
func ThresholdsFor(cfg *config.Config, preMarket bool) Thresholds {
	th := Thresholds{
		PreMarket:         preMarket,
		MinPrice:          cfg.Screening.MinPrice,
		MaxPrice:          cfg.Screening.MaxPrice,
		MinIntradayPct:    cfg.Screening.MinIntradayPct,
		MinDollarVolume:   cfg.Screening.MinDollarVolume,
		MinVolumeMultiple: cfg.Screening.MinVolumeMultiple,
	}
	if preMarket {
		th.MinDollarVolume = cfg.PreMarket.MinDollarVolume
		th.MinVolumeMultiple = cfg.PreMarket.MinVolumeMultiple
	}
	return th
}

// Tradable reports whether a symbol is worth evaluating at all, given its price and
// the dollar volume it has traded so far today.
//
// This is a band on executability, deliberately separate from the momentum criteria.
// Without it the screen surfaces instruments the strategy was never meant to hold: a
// 2024-2026 backtest found warrants at $0.07, SPAC units whose 20-session average
// volume was 15 shares, and sub-$1 names trading a few hundred shares a day. A position
// of a few hundred dollars in those cannot be filled near the quoted price, and sub-$1
// names were also the worst-performing price bucket measured.
//
// The upper price bound is the band the strategy trades rather than an executability
// limit: above roughly $20 a 10% move on 5x volume is a different kind of event, and
// the pullback entry the setup looks for is not what drives it.
//
// Both inputs come from the snapshot already fetched for the move filter, so rejecting
// here costs nothing and saves the per-symbol news and average-volume lookups.
func Tradable(price, dollarVolume float64, th Thresholds) (bool, string) {
	if ok, reason := TradablePrice(price, th); !ok {
		return false, reason
	}
	return TradableLiquidity(dollarVolume, th)
}

// TradablePrice is the price-band half of Tradable, and TradableLiquidity the
// turnover half.
//
// They are separable because the two halves stop being answerable at the same moment.
// In the regular session one snapshot carries both, so Tradable applies them together
// and rejects before any per-symbol call. Pre-market there is no volume in the
// snapshot at all — no daily bar for today exists yet — so the price band is applied
// first to narrow the list, the session's volume is fetched for what survives, and
// only then can turnover be judged. Splitting the check is what keeps that ordering
// honest instead of comparing against a zero that means "unknown".
func TradablePrice(price float64, th Thresholds) (bool, string) {
	if price < th.MinPrice {
		return false, fmt.Sprintf("price $%.2f is below the $%.2f floor", price, th.MinPrice)
	}
	if price > th.MaxPrice {
		return false, fmt.Sprintf("price $%.2f is above the $%.2f ceiling", price, th.MaxPrice)
	}
	return true, ""
}

func TradableLiquidity(dollarVolume float64, th Thresholds) (bool, string) {
	if dollarVolume < th.MinDollarVolume {
		return false, fmt.Sprintf("only $%.0f traded so far, below the $%.0f floor",
			dollarVolume, th.MinDollarVolume)
	}
	return true, ""
}

// Input is the per-symbol data the three criteria are evaluated against.
type Input struct {
	Symbol      string
	Price       float64
	IntradayPct float64
	TodayVolume float64
	AvgVolume   float64
	NewsCount   int
}

// VolumeMultiple is today's volume relative to the average, or 0 when the
// average is unavailable.
func (in Input) VolumeMultiple() float64 {
	if in.AvgVolume <= 0 {
		return 0
	}
	return in.TodayVolume / in.AvgVolume
}

// Evaluate applies the three entry criteria. Every criterion is always reported,
// pass or fail, with its underlying value, because docs/web-ui.md requires the
// page to show why a candidate did or did not qualify.
//
// The move criterion is a floor with no ceiling, and that is a measured choice
// rather than an omission: capping the move was tested and made things worse once
// winners were allowed to run. See config.Screening.MinIntradayPct.
//
// The criteria are news, price move and relative volume only. Nothing here filters
// on company size, so the screen admits large caps as readily as small ones.
//
// The thresholds arrive as a value rather than being read from config, because a
// pre-market pass judges the same criteria against different numbers — see
// ThresholdsFor.
func Evaluate(in Input, th Thresholds) domain.Evaluation {
	mult := in.VolumeMultiple()

	volDisplay := "unknown"
	if mult > 0 {
		volDisplay = fmt.Sprintf("%.1fx", mult)
	}

	criteria := []domain.Criterion{
		{Name: CriterionNews, Pass: in.NewsCount > 0,
			Display: fmt.Sprintf("%d today", in.NewsCount)},
		{Name: CriterionMove, Pass: in.IntradayPct >= th.MinIntradayPct,
			Display: fmt.Sprintf("%+.1f%%", in.IntradayPct)},
		{Name: CriterionVolume, Pass: mult >= th.MinVolumeMultiple,
			Display: volDisplay},
	}

	eval := domain.Evaluation{
		Symbol:         in.Symbol,
		Criteria:       criteria,
		Qualifies:      true,
		VolumeMultiple: mult,
	}
	var failed []string
	for _, c := range criteria {
		if !c.Pass {
			eval.Qualifies = false
			failed = append(failed, c.Name)
		}
	}
	if !eval.Qualifies {
		eval.FailReason = "fails: " + join(failed)
	}
	return eval
}

func join(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// Rank orders evaluations strongest first without discarding failures, which is
// what the manual screening view needs: seeing what nearly qualified is the point
// of looking.
func Rank(evals []domain.Evaluation) []domain.Evaluation {
	out := append([]domain.Evaluation(nil), evals...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Qualifies != out[j].Qualifies {
			return out[i].Qualifies
		}
		return out[i].VolumeMultiple > out[j].VolumeMultiple
	})
	return out
}

// Qualifying filters to the candidates that passed every criterion, ranked
// strongest first.
//
// Ranking matters when more candidates qualify than there are free position
// slots. Relative volume is the tie-break: it is the criterion that best
// separates a genuine, liquid move from a thin drift, and buying the thinnest of
// several qualifying names is the worst of the available choices.
func Qualifying(evals []domain.Evaluation) []domain.Evaluation {
	out := make([]domain.Evaluation, 0, len(evals))
	for _, e := range evals {
		if e.Qualifies {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].VolumeMultiple > out[j].VolumeMultiple
	})
	return out
}
