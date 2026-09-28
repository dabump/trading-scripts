package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// withAudit rebuilds the harness engine around a capturing recorder.
func withAudit(t *testing.T, h *harness) *audit.Memory {
	t.Helper()
	var mem audit.Memory
	h.eng = New(Deps{
		Config: h.cfg, Store: h.store, Data: h.fake, Trading: h.fake,
		Logger: discardLogger(), Audit: &mem,
		Now: func() time.Time { return h.now },
	})
	return &mem
}

func kindsOf(events []audit.Event) []audit.Kind {
	out := make([]audit.Kind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func findEvent(events []audit.Event, kind audit.Kind, symbol string) (audit.Event, bool) {
	for _, e := range events {
		if e.Kind == kind && (symbol == "" || e.Symbol == symbol) {
			return e, true
		}
	}
	return audit.Event{}, false
}

// A full day must leave a trail that explains itself end to end.
func TestAuditTrailCoversATradingDay(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	// Run it up and hold it: with no profit target or trailing stop left, the
	// forced end-of-day exit is what closes a winner.
	h.at(11, 0)
	h.fake.SetPrice("ABCD", 6.00)
	h.tick()
	h.at(15, 30)
	h.fake.SetPrice("ABCD", 5.70)
	h.tick()

	events := mem.Events()
	for _, want := range []audit.Kind{
		audit.SentimentRead, audit.GateResolved, audit.PositionOpened, audit.PositionClosed,
	} {
		if _, ok := findEvent(events, want, ""); !ok {
			t.Errorf("trail is missing %s; got %v", want, kindsOf(events))
		}
	}

	// The buy record must carry enough to reconstruct the decision.
	opened, _ := findEvent(events, audit.PositionOpened, "ABCD")
	if opened.Detail["shares"] != 2000 {
		t.Errorf("shares = %v, want 2000", opened.Detail["shares"])
	}
	for _, key := range []string{
		"price", "dollars", "relative_volume", "criteria",
		"portfolio_value", "cash_before", "position_size_pct",
	} {
		if _, ok := opened.Detail[key]; !ok {
			t.Errorf("buy record is missing %q, so the decision cannot be reconstructed", key)
		}
	}
	// The criteria that were true at the time, not just that it qualified.
	criteria, ok := opened.Detail["criteria"].(map[string]any)
	if !ok || len(criteria) != 3 {
		t.Errorf("criteria = %v, want all three recorded", opened.Detail["criteria"])
	}

	// The sell record must say why and what it cost or made.
	closed, _ := findEvent(events, audit.PositionClosed, "ABCD")
	if closed.Detail["reason"] != string(domain.ExitForcedEOD) {
		t.Errorf("exit reason = %v, want FORCED_EOD", closed.Detail["reason"])
	}
	if pnl, ok := closed.Detail["pnl_dollars"].(float64); !ok || pnl < 1399 || pnl > 1401 {
		t.Errorf("pnl_dollars = %v, want ~1400", closed.Detail["pnl_dollars"])
	}
	if _, ok := closed.Detail["peak_price"]; !ok {
		t.Error("sell record should carry the high-water mark")
	}
	if _, ok := closed.Detail["held_for"]; !ok {
		t.Error("sell record should say how long the position was held")
	}
	if !strings.Contains(closed.Summary, "FORCED_EOD") {
		t.Errorf("summary = %q, should name the exit trigger", closed.Summary)
	}
}

// A halt is the most consequential decision of the day and must be recorded with
// the evidence behind it.
func TestAuditRecordsBearishHalt(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	h.setBearish()
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	events := mem.Events()
	halted, ok := findEvent(events, audit.TradingHalted, "")
	if !ok {
		t.Fatalf("no halt recorded; got %v", kindsOf(events))
	}
	if halted.Detail["verdict"] != string(domain.VerdictBearish) {
		t.Errorf("verdict = %v, want bearish", halted.Detail["verdict"])
	}
	if _, ok := halted.Detail["percentages"]; !ok {
		t.Error("the halt record should carry the readings that justified it")
	}
	// Nothing was bought, so nothing should claim to have been.
	if _, ok := findEvent(events, audit.PositionOpened, ""); ok {
		t.Error("a position was recorded as opened on a halted day")
	}
}

// Skips that are a real decision get recorded; the ones that repeat every scan do
// not, or the trail would be unreadable.
func TestAuditRecordsMeaningfulSkipsOnly(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	h.setBullish()
	h.cfg.Risk.MaxConcurrentPositions = 1

	for _, c := range []struct {
		symbol string
		avgVol float64
	}{{"AAA", 300_000}, {"BBB", 400_000}} {
		h.addCandidate(c.symbol, 5.00)
		h.fake.SetAverageVolume(c.symbol, c.avgVol)
	}

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	// Several more scans: the "already holding" skip repeats every minute and must
	// not be recorded each time.
	for _, m := range []int{40, 45, 50} {
		h.at(10, m)
		h.tick()
	}

	var skips int
	for _, e := range mem.Events() {
		if e.Kind == audit.EntrySkipped {
			skips++
			if strings.Contains(e.Summary, "already holding") {
				t.Errorf("a routine repeating skip was audited: %q", e.Summary)
			}
		}
	}
	if skips == 0 {
		t.Error("the candidate turned away by the position cap should be audited")
	}
	if skips > 3 {
		t.Errorf("%d skip events across four scans suggests routine skips are being recorded", skips)
	}
}

// A persistent outage must not swamp the trail, but a change of fault must still
// register, and so must a recurrence after recovery.
func TestAuditDeduplicatesRepeatingFaults(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	h.fake.SetError(errors.New("upstream 503"))

	for _, m := range []int{35, 40, 45} {
		h.at(9, m)
		h.eng.Tick(context.Background())
	}
	if got := countKind(mem.Events(), audit.Fault); got != 1 {
		t.Errorf("recorded %d faults for one repeating outage, want 1", got)
	}

	// A different fault is new information.
	h.fake.SetError(errors.New("upstream 500 different"))
	h.at(9, 50)
	h.eng.Tick(context.Background())
	if got := countKind(mem.Events(), audit.Fault); got != 2 {
		t.Errorf("recorded %d faults, want 2 once the fault changed", got)
	}

	// Recovery then a recurrence of the original must be recorded again, not
	// swallowed as a repeat.
	h.fake.SetError(nil)
	h.setBullish()
	h.at(9, 55)
	h.tick()
	h.fake.SetError(errors.New("upstream 503"))
	// Far enough past the 09:55 reading that the next poll is actually due;
	// otherwise the tick does no work and nothing can fail.
	h.at(10, 10)
	h.eng.Tick(context.Background())
	if got := countKind(mem.Events(), audit.Fault); got != 3 {
		t.Errorf("recorded %d faults, want 3: a fault after recovery is new", got)
	}
}

func countKind(events []audit.Event, kind audit.Kind) int {
	var n int
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// Restart reconciliation changes real positions, so it belongs in the trail.
func TestAuditRecordsReconciliation(t *testing.T) {
	h := newHarness(t)
	mem := withAudit(t, h)
	h.at(11, 0)
	h.fake.SeedPosition(broker.BrokerPosition{
		Symbol: "ORPH", Shares: 500, AvgEntry: 3.00, CurrentPrice: 3.20})
	h.fake.SetSnapshot("ORPH", 3.20, 3.00, 500_000)

	if err := h.eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	ev, ok := findEvent(mem.Events(), audit.Reconciled, "ORPH")
	if !ok {
		t.Fatalf("adoption was not audited; got %v", kindsOf(mem.Events()))
	}
	if ev.Detail["action"] != "adopted" || ev.Detail["shares"] != 500 {
		t.Errorf("reconciliation detail = %v", ev.Detail)
	}
}

// A failing audit sink must not stop the agent managing open positions: losing the
// exits to preserve a record would be the wrong trade.
func TestAuditFailureDoesNotStopTrading(t *testing.T) {
	h := newHarness(t)
	h.eng = New(Deps{
		Config: h.cfg, Store: h.store, Data: h.fake, Trading: h.fake,
		Logger: discardLogger(), Audit: failingRecorder{},
		Now: func() time.Time { return h.now },
	})
	h.setBullish()
	h.addCandidate("ABCD", 5.00)

	h.at(9, 30)
	h.tick()
	h.at(10, 25)
	h.tick()
	h.at(10, 35)
	h.tick()

	if got := len(h.openPositions()); got != 1 {
		t.Errorf("got %d positions, want 1: a broken audit sink must not block trading", got)
	}
	if state, _ := h.eng.State(); state == domain.StateError {
		t.Error("a failed audit write should not fault the agent")
	}
}

type failingRecorder struct{}

func (failingRecorder) Record(audit.Event) error { return errors.New("disk full") }
