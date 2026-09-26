package risk

import (
	"strings"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func cfg() *config.Config {
	c := &config.Config{}
	c.Risk.PositionSizePct = 10
	c.Risk.MaxConcurrentPositions = 5
	c.Risk.StopLossPct = 10
	return c
}

func TestSize(t *testing.T) {
	tests := []struct {
		name       string
		acct       domain.Account
		price      float64
		wantOK     bool
		wantShares int
		wantReason string
	}{
		{
			name:       "10% of a 100k portfolio at $50",
			acct:       domain.Account{PortfolioValue: 100_000, Cash: 100_000},
			price:      50,
			wantOK:     true,
			wantShares: 200,
		},
		{
			name:       "fractional allocation rounds down to whole shares",
			acct:       domain.Account{PortfolioValue: 10_000, Cash: 10_000},
			price:      333,
			wantOK:     true,
			wantShares: 3, // $1000 / $333 = 3.003
		},
		{
			name: "clamped to cash when the account is mostly deployed",
			// 10% of 100k is 10k, but only 4k of cash remains.
			acct:       domain.Account{PortfolioValue: 100_000, Cash: 4_000},
			price:      100,
			wantOK:     true,
			wantShares: 40,
		},
		{
			name:       "share price above the whole allocation is refused",
			acct:       domain.Account{PortfolioValue: 10_000, Cash: 10_000},
			price:      1_500, // allocation is $1000
			wantReason: "below one share",
		},
		{
			name:       "no cash is refused",
			acct:       domain.Account{PortfolioValue: 100_000, Cash: 0},
			price:      10,
			wantReason: "no cash",
		},
		{
			name:       "zero price is refused",
			acct:       domain.Account{PortfolioValue: 100_000, Cash: 100_000},
			price:      0,
			wantReason: "no valid price",
		},
		{
			name:       "empty portfolio is refused",
			acct:       domain.Account{},
			price:      10,
			wantReason: "portfolio value is zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Size(tt.acct, tt.price, cfg())
			if got.OK != tt.wantOK {
				t.Fatalf("OK = %v, want %v (reason %q)", got.OK, tt.wantOK, got.Reason)
			}
			if tt.wantOK {
				if got.Shares != tt.wantShares {
					t.Errorf("Shares = %d, want %d", got.Shares, tt.wantShares)
				}
				if want := float64(tt.wantShares) * tt.price; got.Dollars != want {
					t.Errorf("Dollars = %.2f, want %.2f", got.Dollars, want)
				}
			} else if !strings.Contains(got.Reason, tt.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tt.wantReason)
			}
		})
	}
}

// Five positions at 10% each is the documented cap; the sixth must be refused.
func TestConcurrencyCap(t *testing.T) {
	c := cfg()
	for open := 0; open < 5; open++ {
		if !CanOpen(open, c) {
			t.Errorf("CanOpen(%d) = false, want true", open)
		}
		if got, want := FreeSlots(open, c), 5-open; got != want {
			t.Errorf("FreeSlots(%d) = %d, want %d", open, got, want)
		}
	}
	if CanOpen(5, c) {
		t.Error("CanOpen(5) = true, want false at the cap")
	}
	if got := FreeSlots(7, c); got != 0 {
		t.Errorf("FreeSlots(7) = %d, want 0 (never negative)", got)
	}
}

func TestAllowEntry(t *testing.T) {
	tests := []struct {
		name        string
		symbol      string
		openSymbols map[string]bool
		tradedToday map[string]bool
		openCount   int
		reentry     bool
		wantAllow   bool
		wantReason  string
		wantRoutine bool
	}{
		{
			name: "clean entry", symbol: "ABCD", openCount: 0, wantAllow: true,
		},
		{
			name: "at the cap", symbol: "ABCD", openCount: 5,
			wantReason: "position cap reached",
		},
		{
			name: "already holding it", symbol: "ABCD",
			openSymbols: map[string]bool{"ABCD": true}, openCount: 1,
			wantReason: "already holding", wantRoutine: true,
		},
		{
			name: "re-entry blocked by default", symbol: "ABCD",
			tradedToday: map[string]bool{"ABCD": true}, openCount: 0,
			wantReason: "same-day re-entry is disabled", wantRoutine: true,
		},
		{
			name: "re-entry allowed when enabled", symbol: "ABCD",
			tradedToday: map[string]bool{"ABCD": true}, openCount: 0,
			reentry: true, wantAllow: true,
		},
		{
			name: "a different symbol traded today is irrelevant", symbol: "ABCD",
			tradedToday: map[string]bool{"WXYZ": true}, openCount: 1,
			wantAllow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cfg()
			c.Risk.AllowSameDayReentry = tt.reentry
			allow, reason, routine := AllowEntry(tt.symbol, tt.openSymbols, tt.tradedToday, tt.openCount, c)
			if allow != tt.wantAllow {
				t.Fatalf("allow = %v, want %v (reason %q)", allow, tt.wantAllow, reason)
			}
			if !tt.wantAllow && !strings.Contains(reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", reason, tt.wantReason)
			}
			// Routine refusals repeat every scan and must not be logged at info level.
			if routine != tt.wantRoutine {
				t.Errorf("routine = %v, want %v", routine, tt.wantRoutine)
			}
		})
	}
}
