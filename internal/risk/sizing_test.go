package risk

import (
	"math"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

func sizingCfg() *config.Config {
	c := &config.Config{}
	c.Risk.RiskPerTradePct = 1
	c.Risk.MaxPositionPct = 33
	return c
}

// The whole reason for sizing from the stop: two trades with very different stop
// distances must put the same number of dollars at risk. Sizing by a fixed slice of
// the portfolio instead makes the wide-stop trade cost several times the tight one.
func TestSizeForRiskHoldsDollarRiskConstant(t *testing.T) {
	cfg := sizingCfg()
	acct := domain.Account{PortfolioValue: 10_000, Cash: 10_000, Equity: 10_000}

	// Both stops are wide enough that the risk budget, not the notional cap, decides
	// the size — which is the rule under test.
	tight := SizeForRisk(acct, 10.00, 9.60, cfg) // 4% stop
	wide := SizeForRisk(acct, 10.00, 9.20, cfg)  // 8% stop
	if !tight.OK || !wide.OK {
		t.Fatalf("both should size: tight=%+v wide=%+v", tight, wide)
	}

	// $100 of risk on a $10,000 account at 1% per trade, either way.
	for name, got := range map[string]Sizing{"tight": tight, "wide": wide} {
		if math.Abs(got.RiskDollar-100) > 1 {
			t.Errorf("%s stop risks $%.2f, want $100 — the point is that it does not vary",
				name, got.RiskDollar)
		}
	}
	// And the share counts differ by the ratio of the stop distances.
	if tight.Shares <= wide.Shares {
		t.Errorf("a tighter stop must buy more shares: tight=%d wide=%d",
			tight.Shares, wide.Shares)
	}
	if got, want := tight.Shares, 250; got != want {
		t.Errorf("tight shares = %d, want %d ($100 risk ÷ $0.40 per share)", got, want)
	}
	if got, want := wide.Shares, 125; got != want {
		t.Errorf("wide shares = %d, want %d ($100 risk ÷ $0.80 per share)", got, want)
	}
}

// A very tight stop would otherwise size to a position bigger than the account.
func TestSizeForRiskCapsNotionalAndCash(t *testing.T) {
	cfg := sizingCfg()

	t.Run("the position cap binds before the risk budget runs out", func(t *testing.T) {
		acct := domain.Account{PortfolioValue: 10_000, Cash: 10_000, Equity: 10_000}
		// A 0.1% stop: the risk budget alone would buy 10,000 shares of a $10 stock,
		// which is $100,000 of a $10,000 account.
		got := SizeForRisk(acct, 10.00, 9.99, cfg)
		if !got.OK {
			t.Fatalf("should still size, capped: %s", got.Reason)
		}
		if got.Dollars > 10_000*cfg.Risk.MaxPositionPct/100+0.01 {
			t.Errorf("notional $%.2f exceeds the %.0f%% cap", got.Dollars, cfg.Risk.MaxPositionPct)
		}
		if got.Shares != 330 {
			t.Errorf("shares = %d, want 330 ($3,300 cap ÷ $10)", got.Shares)
		}
		// Capped means the trade risks less than the nominal 1%, not more. That is the
		// safe direction, and it is why the cap is allowed to bind.
		if got.RiskDollar > 100 {
			t.Errorf("a capped position risks $%.2f, more than the $100 budget", got.RiskDollar)
		}
	})

	t.Run("available cash binds on a deployed account", func(t *testing.T) {
		// Portfolio value counts holdings, so the untouched percentage can exceed the
		// cash left to spend.
		acct := domain.Account{PortfolioValue: 10_000, Cash: 500, Equity: 10_000}
		got := SizeForRisk(acct, 10.00, 9.99, cfg)
		if !got.OK {
			t.Fatalf("should size within cash: %s", got.Reason)
		}
		if got.Dollars > 500.01 {
			t.Errorf("spent $%.2f of $500 cash", got.Dollars)
		}
	})
}

func TestSizeForRiskRefusals(t *testing.T) {
	cfg := sizingCfg()
	acct := domain.Account{PortfolioValue: 10_000, Cash: 10_000, Equity: 10_000}

	tests := []struct {
		name        string
		entry, stop float64
		acct        domain.Account
	}{
		{"no entry price", 0, 9, acct},
		{"stop above entry", 10, 11, acct},
		{"stop equal to entry", 10, 10, acct},
		{"negative stop", 10, -1, acct},
		{"empty account", 10, 9, domain.Account{}},
		{"no cash", 10, 9, domain.Account{PortfolioValue: 10_000, Cash: 0}},
		{
			// $100 of risk cannot buy a share whose stop is $200 away.
			"one share costs more risk than the budget allows", 1000, 800, acct,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SizeForRisk(tt.acct, tt.entry, tt.stop, cfg)
			if got.OK {
				t.Errorf("SizeForRisk = %+v, want a refusal", got)
			}
			if got.Reason == "" {
				t.Error("a refusal must say why, so the skip is explicable in the log")
			}
		})
	}
}
