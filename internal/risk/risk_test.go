package risk

import (
	"strings"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/config"
)

func cfg() *config.Config {
	c := &config.Config{}
	c.Risk.RiskPerTradePct = 1
	c.Risk.MaxPositionPct = 20
	c.Risk.MaxConcurrentPositions = 5
	c.Risk.StopLossPct = 10
	return c
}

// Five concurrent positions is the documented cap; the sixth must be refused.
func TestConcurrencyCap(t *testing.T) {
	c := cfg()
	for open := 0; open < 5; open++ {
		if !CanOpen(open, c) {
			t.Errorf("CanOpen(%d) = false, want true", open)
		}
	}
	if CanOpen(5, c) {
		t.Error("CanOpen(5) = true, want false at the cap")
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
