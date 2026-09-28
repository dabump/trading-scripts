package screener

import (
	"strings"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func cfg() *config.Config {
	c := &config.Config{}
	c.Screening.MinIntradayPct = 10
	c.Screening.MinVolumeMultiple = 5
	return c
}

// A candidate that clears every documented criterion.
func passing() Input {
	return Input{
		Symbol: "ABCD", Price: 4.20, IntradayPct: 14,
		TodayVolume: 6_100_000, AvgVolume: 1_000_000, NewsCount: 2,
	}
}

func TestEvaluateAllCriteriaMustPass(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Input)
		wantPass   bool
		wantFailed string
	}{
		{"all three criteria pass", func(*Input) {}, true, ""},
		{
			"no news fails",
			func(in *Input) { in.NewsCount = 0 },
			false, CriterionNews,
		},
		{
			"move below 10% fails",
			func(in *Input) { in.IntradayPct = 9.9 },
			false, CriterionMove,
		},
		{
			"move at exactly 10% passes",
			func(in *Input) { in.IntradayPct = 10 },
			true, "",
		},
		{
			// There is deliberately no ceiling. A cap was measured and removed: the
			// extreme movers are the right tail the strategy lives on once winners
			// are allowed to run.
			"an extreme move still passes",
			func(in *Input) { in.IntradayPct = 312 },
			true, "",
		},
		{
			"volume below 5x fails",
			func(in *Input) { in.TodayVolume = 4_900_000 },
			false, CriterionVolume,
		},
		{
			"volume at exactly 5x passes",
			func(in *Input) { in.TodayVolume = 5_000_000 },
			true, "",
		},
		{
			"missing average volume fails the volume criterion",
			func(in *Input) { in.AvgVolume = 0 },
			false, CriterionVolume,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := passing()
			tt.mutate(&in)
			got := Evaluate(in, cfg())

			if got.Qualifies != tt.wantPass {
				t.Fatalf("Qualifies = %v, want %v (%s)", got.Qualifies, tt.wantPass, got.FailReason)
			}
			// Every criterion is always reported so the UI can show the breakdown.
			if len(got.Criteria) != 3 {
				t.Errorf("got %d criteria, want all 3 reported regardless of outcome", len(got.Criteria))
			}
			if !tt.wantPass && !strings.Contains(got.FailReason, tt.wantFailed) {
				t.Errorf("FailReason = %q, want it to name %q", got.FailReason, tt.wantFailed)
			}
		})
	}
}

// Criteria carry their underlying value, not just a boolean, per docs/web-ui.md.
func TestEvaluateReportsValues(t *testing.T) {
	got := Evaluate(passing(), cfg())
	want := map[string]string{
		CriterionNews:   "2 today",
		CriterionMove:   "+14.0%",
		CriterionVolume: "6.1x",
	}
	for _, c := range got.Criteria {
		if w, ok := want[c.Name]; ok && c.Display != w {
			t.Errorf("%s display = %q, want %q", c.Name, c.Display, w)
		}
	}

}

func TestQualifyingRanksByRelativeVolume(t *testing.T) {
	mk := func(sym string, mult float64, ok bool) domain.Evaluation {
		return domain.Evaluation{Symbol: sym, Qualifies: ok, VolumeMultiple: mult}
	}
	in := []domain.Evaluation{
		mk("LOW", 5.2, true),
		mk("FAIL", 99, false),
		mk("HIGH", 18.4, true),
		mk("MID", 9.1, true),
	}

	got := Qualifying(in)
	if len(got) != 3 {
		t.Fatalf("got %d qualifying, want 3 (the failing one must be dropped)", len(got))
	}
	for i, want := range []string{"HIGH", "MID", "LOW"} {
		if got[i].Symbol != want {
			t.Errorf("rank %d = %s, want %s", i, got[i].Symbol, want)
		}
	}
}

func TestQualifyingEmpty(t *testing.T) {
	if got := Qualifying(nil); len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}

func TestVolumeMultiple(t *testing.T) {
	in := Input{TodayVolume: 3_000_000, AvgVolume: 600_000}
	if got := in.VolumeMultiple(); got != 5 {
		t.Errorf("VolumeMultiple = %v, want 5", got)
	}
	if got := (Input{TodayVolume: 100}).VolumeMultiple(); got != 0 {
		t.Errorf("VolumeMultiple with no average = %v, want 0", got)
	}
}

// The floors are a tradability gate applied before enrichment, not a momentum
// criterion: the backtest showed the screen otherwise admitting warrants at $0.07
// and SPAC units whose 20-session average volume was 15 shares.
func TestTradableFloors(t *testing.T) {
	c := cfg()
	c.Screening.MinPrice = 1
	c.Screening.MinDollarVolume = 1_000_000

	tests := []struct {
		name         string
		price        float64
		dollarVolume float64
		wantOK       bool
		wantReason   string
	}{
		{"clears both floors", 4.20, 25_000_000, true, ""},
		{"exactly at both floors", 1.00, 1_000_000, true, ""},
		{"a warrant priced at $0.07", 0.07, 50_000_000, false, "below the $1.00 floor"},
		{"a cent below the price floor", 0.99, 50_000_000, false, "below the $1.00 floor"},
		{"a SPAC unit trading 15 shares", 9.80, 147, false, "below the $1000000 floor"},
		{"a dollar below the liquidity floor", 4.20, 999_999, false, "below the $1000000 floor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := Tradable(tt.price, tt.dollarVolume, c)
			if ok != tt.wantOK {
				t.Fatalf("Tradable(%v, %v) = %v (%q), want %v",
					tt.price, tt.dollarVolume, ok, reason, tt.wantOK)
			}
			if tt.wantOK {
				if reason != "" {
					t.Errorf("a tradable symbol must carry no reason, got %q", reason)
				}
				return
			}
			// The reason is logged, so it has to name the value and the floor it missed.
			if !strings.Contains(strings.ReplaceAll(reason, ",", ""), tt.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", reason, tt.wantReason)
			}
		})
	}
}
