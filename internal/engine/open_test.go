package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// tradingSession drives the harness to a normal mid-session state with the gate
// resolved, so a manual open is judged on its own merits rather than the clock.
func (h *harness) tradingSession(t *testing.T) {
	t.Helper()
	h.setBullish()
	h.at(9, 30)
	h.tick()
	h.at(10, 31)
	h.tick()
}

// The point of the button: a candidate the setup gate refuses can still be bought,
// and it becomes an ordinary position with a real stop and a sized share count.
func TestOpenPositionOverridesTheSetupGate(t *testing.T) {
	h := newHarness(t)
	h.addScreenedOnly("NOSETUP", 5.00) // qualifies, never prints a setup
	h.tradingSession(t)

	if len(h.openPositions()) != 0 {
		t.Fatal("the automated path should not have bought a candidate with no setup")
	}

	res, err := h.eng.OpenPosition(context.Background(), "NOSETUP")
	if err != nil {
		t.Fatal(err)
	}
	if res.FromSetup {
		t.Error("there is no setup here, so the stop cannot have come from one")
	}
	// Without a chart stop the position is sized off the configured maximum, which
	// keeps the risk rule in charge rather than falling back to a fixed fraction.
	wantStop := res.Entry * (1 - h.cfg.Entry.MaxStopDistancePct/100)
	if diff := res.Stop - wantStop; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("stop = %.4f, want %.4f (%.1f%% below entry)",
			res.Stop, wantStop, h.cfg.Entry.MaxStopDistancePct)
	}

	open := h.openPositions()
	if len(open) != 1 {
		t.Fatalf("open positions = %d, want 1", len(open))
	}
	pos := open[0]
	if pos.StopPrice != res.Stop || pos.InitialRisk <= 0 {
		t.Errorf("stored position = %+v, want the stop and initial risk recorded", pos)
	}
	// The risk budget, not a fraction of the account.
	budget := 100_000 * h.cfg.Risk.RiskPerTradePct / 100
	if res.RiskDollar > budget+0.01 {
		t.Errorf("risk = $%.2f, above the $%.2f budget", res.RiskDollar, budget)
	}
}

// When the chart does show a completed setup, the manual path must produce exactly
// what the automated one would have — an operator clicking a moment early should not
// get a worse stop than the strategy would have taken.
func TestOpenPositionPrefersTheChartStop(t *testing.T) {
	h := newHarness(t)
	h.addCandidate("ABCD", 5.00) // prints a real pullback-and-resumption
	h.setBullish()
	h.at(9, 30)
	h.tick()

	res, err := h.eng.OpenPosition(context.Background(), "ABCD")
	if err != nil {
		t.Fatal(err)
	}
	if !res.FromSetup {
		t.Fatal("a completed setup must supply the stop")
	}
	maxStop := res.Entry * (1 - h.cfg.Entry.MaxStopDistancePct/100)
	if res.Stop <= maxStop {
		t.Errorf("chart stop %.4f is no tighter than the configured maximum %.4f",
			res.Stop, maxStop)
	}
}

// The rules that are not about the signal still apply. Holding it already is the one
// an operator is most likely to trip over by double-clicking.
func TestOpenPositionKeepsTheRiskRules(t *testing.T) {
	h := newHarness(t)
	h.addScreenedOnly("NOSETUP", 5.00)
	h.tradingSession(t)

	if _, err := h.eng.OpenPosition(context.Background(), "NOSETUP"); err != nil {
		t.Fatal(err)
	}
	orders := len(h.fake.Placed())

	_, err := h.eng.OpenPosition(context.Background(), "NOSETUP")
	if !errors.Is(err, ErrCannotEnter) {
		t.Errorf("second open returned %v, want ErrCannotEnter", err)
	}
	if len(h.fake.Placed()) != orders {
		t.Error("a second open must not place a second buy order")
	}
	if len(h.openPositions()) != 1 {
		t.Errorf("open positions = %d, want 1", len(h.openPositions()))
	}
}

// The position cap is an account-integrity rule, not a signal one, so it holds.
func TestOpenPositionRespectsThePositionCap(t *testing.T) {
	h := newHarness(t)
	h.cfg.Risk.MaxConcurrentPositions = 1
	h.addScreenedOnly("AAA", 5.00)
	h.addScreenedOnly("BBB", 5.00)
	h.tradingSession(t)

	if _, err := h.eng.OpenPosition(context.Background(), "AAA"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.OpenPosition(context.Background(), "BBB"); !errors.Is(err, ErrCannotEnter) {
		t.Errorf("opening past the cap returned %v, want ErrCannotEnter", err)
	}
}

// Buying inside the forced-exit window means buying something about to be sold, so
// it is refused — a guaranteed round trip across the spread and nothing else.
func TestOpenPositionRefusedInTheEODWindow(t *testing.T) {
	h := newHarness(t)
	h.addScreenedOnly("NOSETUP", 5.00)
	h.tradingSession(t)

	h.at(15, 45)
	h.tick()
	_, err := h.eng.OpenPosition(context.Background(), "NOSETUP")
	if !errors.Is(err, ErrCannotEnter) {
		t.Errorf("open in the EOD window returned %v, want ErrCannotEnter", err)
	}
	if len(h.openPositions()) != 0 {
		t.Error("nothing may be opened in the forced-exit window")
	}
}

// And nothing at all with the exchange shut.
func TestOpenPositionRefusedWhenExchangeIsShut(t *testing.T) {
	h := newHarness(t)
	h.addScreenedOnly("NOSETUP", 5.00)
	h.tradingSession(t)

	h.at(18, 0)
	h.tick()
	if _, err := h.eng.OpenPosition(context.Background(), "NOSETUP"); !errors.Is(err, ErrExchangeClosed) {
		t.Errorf("open after hours returned %v, want ErrExchangeClosed", err)
	}
}

// Overriding the kill switch is allowed — an instruction about one named symbol is
// not what it exists to stop — but the override has to be on the record.
func TestOpenPositionRecordsAHaltOverride(t *testing.T) {
	h := newHarness(t)
	rec := &audit.Memory{}
	h.eng.audit = rec
	h.addScreenedOnly("NOSETUP", 5.00)
	h.setBearish()
	h.at(9, 30)
	h.tick()
	h.at(10, 31)
	h.tick()
	h.wantState(domain.StateHaltedBearish)

	res, err := h.eng.OpenPosition(context.Background(), "NOSETUP")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Halted {
		t.Error("the result must say the kill switch was on")
	}
	var found bool
	for _, ev := range rec.Events() {
		if ev.Kind == audit.PositionOpened && ev.Detail["manual"] == true {
			found = true
			if ev.Detail["overrode_halt"] != true {
				t.Error("the audit event must record that it overrode the halt")
			}
			if ev.Detail["stop_from_setup"] != false {
				t.Error("the audit event must say the stop did not come from a setup")
			}
		}
	}
	if !found {
		t.Error("a manual open must be recorded in the audit trail")
	}
}
