package main

import (
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

func testCfg() *config.Config {
	c := &config.Config{}
	c.Risk.StopLossPct = 10
	c.Exit.EODExitOffsetMins = 30
	c.Screening.MinPrice = 1
	c.Screening.MinDollarVolume = 1_000_000
	c.Screening.MinIntradayPct = 10
	c.Screening.MinVolumeMultiple = 5
	c.Screening.AvgVolumeLookbackDays = 20
	return c
}

// How one bar is resolved decides whether the backtest flatters the strategy: a bar
// whose low broke the stop must book the stop, at the stop's price, however high the
// same bar traded afterwards.
func TestCheckExitResolvesABarAgainstTheTrader(t *testing.T) {
	cfg := testCfg()

	t.Run("a bar that traded through the stop stops out", func(t *testing.T) {
		pos := &domain.Position{EntryPrice: 100, PeakPrice: 100, Shares: 10}
		reason, fill := checkExit(pos, Bar{O: 100, H: 120, L: 89, C: 118}, false, cfg)
		if reason != domain.ExitStopLoss {
			t.Fatalf("reason = %q, want %q", reason, domain.ExitStopLoss)
		}
		if fill != 90 {
			t.Errorf("fill = %v, want the 90 stop threshold", fill)
		}
	})

	t.Run("a big pullback from the peak is held", func(t *testing.T) {
		// Once a trailing-stop exit at 114; now only the 90 stop can close it.
		pos := &domain.Position{EntryPrice: 100, PeakPrice: 120, Shares: 10}
		if reason, _ := checkExit(pos, Bar{O: 118, H: 120, L: 113, C: 115}, false, cfg); reason != "" {
			t.Fatalf("reason = %q, want the position held", reason)
		}
	})

	t.Run("a bar that gapped below the threshold cannot fill at it", func(t *testing.T) {
		pos := &domain.Position{EntryPrice: 100, PeakPrice: 100, Shares: 10}
		_, fill := checkExit(pos, Bar{O: 85, H: 86, L: 80, C: 82}, false, cfg)
		if fill != 85 {
			t.Errorf("fill = %v, want the 85 open — the 90 stop was already gapped through", fill)
		}
	})

	t.Run("a quiet bar leaves the position open and raises the peak", func(t *testing.T) {
		pos := &domain.Position{EntryPrice: 100, PeakPrice: 100, Shares: 10}
		if reason, _ := checkExit(pos, Bar{O: 101, H: 106, L: 99, C: 104}, false, cfg); reason != "" {
			t.Fatalf("reason = %q, want no exit", reason)
		}
		// The peak no longer drives an exit, but it is still recorded and shown.
		if pos.PeakPrice != 106 {
			t.Errorf("peak = %v, want 106", pos.PeakPrice)
		}
	})

	t.Run("the forced exit ignores price and fills at the bar open", func(t *testing.T) {
		pos := &domain.Position{EntryPrice: 100, PeakPrice: 140, Shares: 10}
		reason, fill := checkExit(pos, Bar{O: 137, H: 138, L: 130, C: 131}, true, cfg)
		if reason != domain.ExitForcedEOD || fill != 137 {
			t.Errorf("got %q at %v, want FORCED_EOD at 137", reason, fill)
		}
	})
}

// The news window moves with the clock, so a story has to be invisible to a scan
// that ran before it was published. Counting the whole day's stories at every scan
// would let an 15:00 headline justify an 11:00 entry.
func TestNewsCountIsRelativeToTheScanInstant(t *testing.T) {
	at := time.Date(2026, 9, 28, 11, 0, 0, 0, scheduler.ET)
	stories := []time.Time{
		at.Add(-30 * time.Hour), // older than the lookback
		at.Add(-17 * time.Hour), // inside an 18h lookback
		at.Add(-1 * time.Minute),
		at,                    // exactly now counts
		at.Add(1 * time.Hour), // the future must not
	}
	if got := newsCount(stories, at, 18*time.Hour); got != 3 {
		t.Errorf("newsCount = %d, want 3", got)
	}
	// A shorter window starting after the open is what the news-lookback setting
	// exists to avoid; it must genuinely see less.
	if got := newsCount(stories, at, time.Hour); got != 2 {
		t.Errorf("newsCount over 1h = %d, want 2", got)
	}
}

func TestExcursionsSplitAtTheExit(t *testing.T) {
	day := time.Date(2026, 9, 28, 10, 0, 0, 0, scheduler.ET)
	bar := func(mins int, h, l, c float64) Bar {
		return Bar{T: day.Add(time.Duration(mins) * time.Minute), H: h, L: l, C: c}
	}
	bars := []Bar{
		bar(0, 102, 99, 101),   // before entry
		bar(5, 106, 100, 105),  // after entry: +6% high
		bar(10, 104, 94, 95),   // -6% low
		bar(15, 130, 95, 128),  // after the exit: +30%
		bar(20, 132, 120, 121), // last close +21%
	}
	pos := &domain.Position{EntryPrice: 100, PeakPrice: 100, EntryTime: day.Add(5 * time.Minute)}
	exitAt := day.Add(15 * time.Minute)

	mfe, mae, before, after, held := excursions(bars, pos, exitAt)
	if mfe != 32 {
		t.Errorf("MFE = %v, want 32", mfe)
	}
	if mae != -6 {
		t.Errorf("MAE = %v, want -6", mae)
	}
	if before != 6 {
		t.Errorf("MFE before exit = %v, want 6 (the pre-entry bar must be excluded)", before)
	}
	if after != 32 {
		t.Errorf("MFE after exit = %v, want 32", after)
	}
	if held != 21 {
		t.Errorf("close if held = %v, want 21", held)
	}
}

// Every trade in this strategy opens and closes in one session, so each is a day
// trade. The rolling five-session count is what the PDT rule caps at three.
func TestMaxDayTradesInWindow(t *testing.T) {
	// Two trading weeks.
	week := []string{
		"2026-09-14", "2026-09-15", "2026-09-16", "2026-09-17", "2026-09-18",
		"2026-09-21", "2026-09-22", "2026-09-23", "2026-09-24", "2026-09-25",
	}
	tests := []struct {
		name  string
		trade map[string]int
		want  int
	}{
		{"none", nil, 0},
		{"one a day fills the window", map[string]int{
			"2026-09-21": 1, "2026-09-22": 1, "2026-09-23": 1, "2026-09-24": 1,
			"2026-09-25": 1}, 5},
		{"three in one session", map[string]int{"2026-09-21": 3}, 3},
		{"the window does not reach a sixth session", map[string]int{
			"2026-09-14": 1, "2026-09-21": 1, "2026-09-22": 1, "2026-09-23": 1,
			"2026-09-24": 1, "2026-09-25": 1}, 5},
		{
			// The point of sliding over sessions rather than over trade days: these
			// four trades span two calendar weeks, so no five-session window holds
			// more than two of them.
			name: "sparse trading is not a breach",
			trade: map[string]int{
				"2026-09-14": 1, "2026-09-16": 1, "2026-09-22": 1, "2026-09-24": 1},
			want: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maxDayTradesInWindow(tt.trade, week); got != tt.want {
				t.Errorf("maxDayTradesInWindow = %d, want %d", got, tt.want)
			}
		})
	}
}

// The daily pre-filter decides which symbol-days are examined intraday at all, so a
// condition that is not a true superset silently discards real trades and biases
// every number downstream.
func TestPreFilterAdmitsOnlyDaysAnIntradayMomentCouldPass(t *testing.T) {
	cfg := testCfg()
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, scheduler.ET)

	// 21 quiet sessions, then the day under test.
	build := func(high, vol float64) []Bar {
		var bars []Bar
		day := time.Date(2026, 8, 3, 0, 0, 0, 0, scheduler.ET)
		for i := 0; i < 21; i++ {
			bars = append(bars, Bar{
				T: day.AddDate(0, 0, i), O: 10, H: 10.1, L: 9.9, C: 10, V: 200_000,
			})
		}
		bars = append(bars, Bar{
			T: time.Date(2026, 9, 2, 0, 0, 0, 0, scheduler.ET),
			O: 10, H: high, L: 9.9, C: high, V: vol,
		})
		return bars
	}

	tests := []struct {
		name  string
		high  float64
		vol   float64
		admit bool
	}{
		{"clears every superset condition", 11.5, 1_200_000, true},
		{"day high never reached +10%", 10.9, 1_200_000, false},
		{"exactly +10% at the high is admitted", 11.0, 1_200_000, true},
		{"full-day volume below 5x cannot become 5x intraday", 11.5, 900_000, false},
		{"exactly 5x is admitted", 11.5, 1_000_000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanSeries(cfg, "TEST", build(tt.high, tt.vol), from)
			if (len(got) == 1) != tt.admit {
				t.Fatalf("admitted %d days, want admit=%v", len(got), tt.admit)
			}
			if tt.admit {
				if got[0].PrevClose != 10 {
					t.Errorf("PrevClose = %v, want 10", got[0].PrevClose)
				}
				if got[0].AvgVolume != 200_000 {
					t.Errorf("AvgVolume = %v, want 200000 (today excluded)", got[0].AvgVolume)
				}
			}
		})
	}

	// A thin name can clear the move and the volume multiple and still not trade
	// enough dollars to be worth a position. With a $1.20 high on 500 shares, no
	// intraday moment could reach the $1m floor.
	var thin []Bar
	base := time.Date(2026, 8, 3, 0, 0, 0, 0, scheduler.ET)
	for i := 0; i < 21; i++ {
		thin = append(thin, Bar{T: base.AddDate(0, 0, i), O: 1, H: 1.01, L: 0.99, C: 1, V: 100})
	}
	thin = append(thin, Bar{
		T: time.Date(2026, 9, 2, 0, 0, 0, 0, scheduler.ET),
		O: 1, H: 1.2, L: 1, C: 1.2, V: 500,
	})
	if got := scanSeries(cfg, "THIN", thin, from); len(got) != 0 {
		t.Errorf("$600 of turnover must fail the dollar-volume floor, got %+v", got)
	}

	// A penny stock is excluded by price even when its move and volume are extreme.
	var penny []Bar
	day := time.Date(2026, 8, 3, 0, 0, 0, 0, scheduler.ET)
	for i := 0; i < 21; i++ {
		penny = append(penny, Bar{T: day.AddDate(0, 0, i), O: 0.5, H: 0.51, L: 0.49, C: 0.5, V: 400_000})
	}
	penny = append(penny, Bar{
		T: time.Date(2026, 9, 2, 0, 0, 0, 0, scheduler.ET),
		O: 0.5, H: 0.9, L: 0.5, C: 0.9, V: 4_000_000,
	})
	if got := scanSeries(cfg, "PENNY", penny, from); len(got) != 0 {
		t.Errorf("a $0.90 high must fail the price floor, got %+v", got)
	}
}
