package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/domain"
)

// startedLate reproduces the case the button exists for: the daemon comes up after
// the first hour, so no readings exist and resolveGate halts the day.
func (h *harness) startedLate(t *testing.T) {
	t.Helper()
	h.setBullish()
	h.at(11, 0)
	h.tick()
	h.wantState(domain.StateHaltedBearish)
	if readings, err := h.store.SentimentReadings(h.date); err != nil {
		t.Fatal(err)
	} else if len(readings) != 0 {
		t.Fatalf("expected no readings, got %d", len(readings))
	}
}

// The whole point: after dismissing it, the automated scan runs again.
func TestIgnoreHaltResumesScreening(t *testing.T) {
	h := newHarness(t)
	h.addCandidate("ABCD", 5.00)
	h.startedLate(t)

	if _, _, err := h.store.LatestScreenSnapshot(h.date); err != nil {
		t.Fatal(err)
	}
	if len(h.openPositions()) != 0 {
		t.Fatal("a halted session must not have traded")
	}

	if err := h.eng.IgnoreHalt(context.Background()); err != nil {
		t.Fatal(err)
	}

	h.at(11, 2)
	h.tick()
	h.wantState(domain.StateScreening)

	evals, _, err := h.store.LatestScreenSnapshot(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if len(evals) == 0 {
		t.Error("screening must run once the halt is dismissed")
	}
	if len(h.openPositions()) != 1 {
		t.Errorf("open positions = %d, want the candidate bought", len(h.openPositions()))
	}
}

// The verdict must not claim the gate passed — it never ran.
func TestIgnoreHaltRecordsAnOverrideNotAPass(t *testing.T) {
	h := newHarness(t)
	h.startedLate(t)

	if err := h.eng.IgnoreHalt(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, err := h.store.Session(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verdict != domain.VerdictOverridden {
		t.Errorf("verdict = %q, want GATE_OVERRIDDEN — the gate was stood down, not passed", rec.Verdict)
	}
	if rec.Halted {
		t.Error("the session must no longer be halted")
	}
	if rec.HaltReason != "" {
		t.Errorf("halt reason = %q, want it cleared", rec.HaltReason)
	}
}

// It survives a restart, or the next start would re-halt the day and the click would
// have bought nothing but a few minutes.
func TestIgnoreHaltSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.startedLate(t)
	if err := h.eng.IgnoreHalt(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A fresh engine over the same store is what a restart looks like.
	fresh := New(Deps{Config: h.cfg, Store: h.store, Data: h.fake, Trading: h.fake,
		Logger: discardLogger(), Now: func() time.Time { return h.now }})
	h.at(11, 5)
	fresh.Tick(context.Background())
	if state, _ := fresh.State(); state == domain.StateHaltedBearish {
		t.Error("a restart must not re-halt a session whose halt was dismissed")
	}
}

// The case it deliberately does not cover: a halt the gate actually reached. That is
// the kill switch working, and dismissing it is a different decision.
func TestIgnoreHaltRefusesAJudgedHalt(t *testing.T) {
	h := newHarness(t)
	h.setBearish()
	h.at(9, 30)
	h.tick()
	h.at(10, 31)
	h.tick()
	h.wantState(domain.StateHaltedBearish)

	err := h.eng.IgnoreHalt(context.Background())
	if !errors.Is(err, ErrHaltWasJudged) {
		t.Fatalf("dismissing a judged halt returned %v, want ErrHaltWasJudged", err)
	}
	rec, err := h.store.Session(h.date)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Halted || rec.Verdict != domain.VerdictBearish {
		t.Errorf("session = %+v, want it still halted on the bearish verdict", rec)
	}
}

// Nothing to dismiss on a healthy session.
func TestIgnoreHaltOnAnUnhaltedSession(t *testing.T) {
	h := newHarness(t)
	h.setBullish()
	h.at(9, 30)
	h.tick()
	h.at(10, 31)
	h.tick()

	if err := h.eng.IgnoreHalt(context.Background()); !errors.Is(err, ErrNotHalted) {
		t.Errorf("got %v, want ErrNotHalted", err)
	}
}

// It goes in the trail, and says it was a person.
func TestIgnoreHaltIsAudited(t *testing.T) {
	h := newHarness(t)
	rec := &audit.Memory{}
	h.eng.audit = rec
	h.startedLate(t)

	if err := h.eng.IgnoreHalt(context.Background()); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range rec.Events() {
		if ev.Kind == audit.GateResolved && ev.Detail["manual"] == true {
			found = true
			if ev.Detail["verdict"] != string(domain.VerdictOverridden) {
				t.Errorf("audited verdict = %v", ev.Detail["verdict"])
			}
		}
	}
	if !found {
		t.Error("dismissing a halt must be recorded in the audit trail")
	}
}
