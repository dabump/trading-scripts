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

// Input is the per-symbol data the four criteria are evaluated against.
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
// The criteria are news, price move and relative volume only. Nothing here filters
// on company size, so the screen admits large caps as readily as small ones.
func Evaluate(in Input, cfg *config.Config) domain.Evaluation {
	mult := in.VolumeMultiple()

	volDisplay := "unknown"
	if mult > 0 {
		volDisplay = fmt.Sprintf("%.1fx", mult)
	}

	criteria := []domain.Criterion{
		{Name: CriterionNews, Pass: in.NewsCount > 0,
			Display: fmt.Sprintf("%d today", in.NewsCount)},
		{Name: CriterionMove, Pass: in.IntradayPct >= cfg.Screening.MinIntradayPct,
			Display: fmt.Sprintf("%+.1f%%", in.IntradayPct)},
		{Name: CriterionVolume, Pass: mult >= cfg.Screening.MinVolumeMultiple,
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
