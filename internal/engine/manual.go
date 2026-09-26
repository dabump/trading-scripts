package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/screener"
	"github.com/martincoetzee/trading-agent/internal/sentiment"
)

// ErrBusy is returned when a manual action is already running. Both actions fan
// out to several API calls, so letting clicks queue up would turn a impatient
// user into a rate-limit problem.
var ErrBusy = errors.New("that check is already running")

// manualGuards serialises each manual action independently of the other.
type manualGuards struct {
	sentiment sync.Mutex
	screen    sync.Mutex
}

// CheckSentiment runs the sentiment classification on demand and returns the
// result without storing it.
//
// Nothing is persisted on purpose: the gate verdict is decided by the most recent
// stored reading, so writing a manual check would let a click at any hour
// overturn what the first hour concluded.
func (e *Engine) CheckSentiment(ctx context.Context) (domain.SentimentCheck, error) {
	if !e.manual.sentiment.TryLock() {
		return domain.SentimentCheck{}, ErrBusy
	}
	defer e.manual.sentiment.Unlock()

	snaps, err := e.data.Snapshots(ctx, e.cfg.Sentiment.Symbols)
	if err != nil {
		return domain.SentimentCheck{}, fmt.Errorf("snapshots: %w", err)
	}

	pcts := make(map[string]float64, len(snaps))
	var missing []string
	for _, sym := range e.cfg.Sentiment.Symbols {
		snap, ok := snaps[sym]
		if !ok {
			missing = append(missing, sym)
			continue
		}
		pct, ok := sentiment.PercentChange(snap.Price, snap.PrevClose)
		if !ok {
			missing = append(missing, sym)
			continue
		}
		pcts[sym] = pct
	}

	check := domain.SentimentCheck{
		TakenAt:        e.now(),
		Percentages:    pcts,
		Classification: sentiment.Classify(pcts, e.cfg),
		Missing:        missing,
	}
	e.log.Info("manual sentiment check", "classification", check.Classification,
		"percentages", pcts, "persisted", false)
	return check, nil
}

// ScreenNow evaluates the candidate universe on demand and returns what it found.
//
// This path cannot place an order: it shares the gathering and evaluation code
// with the automated scan but never reaches the entry logic, which matters most
// when it is run outside market hours. Nothing is persisted either, so the status
// page's screening table still reflects the automated loop rather than the last
// button press.
//
// It applies exactly the same criteria as the automated scan, so a row reported as
// qualifying here is one the agent would genuinely act on during trading hours.
func (e *Engine) ScreenNow(ctx context.Context) (domain.ScreenPreview, error) {
	if !e.manual.screen.TryLock() {
		return domain.ScreenPreview{}, ErrBusy
	}
	defer e.manual.screen.Unlock()

	now := e.now()
	sess, tradingDay := e.Session()

	// The same catalyst window as the automated scan, which is relative to now and
	// therefore works just as well with the exchange closed.
	inputs, universe, err := e.gatherCandidates(ctx, e.newsSince())
	if err != nil {
		return domain.ScreenPreview{}, err
	}

	evals := make([]domain.Evaluation, 0, len(inputs))
	for _, in := range inputs {
		evals = append(evals, screener.Evaluate(in, e.cfg))
	}

	marketOpen := tradingDay && !now.Before(sess.Open) && now.Before(sess.Close)
	preview := domain.ScreenPreview{
		TakenAt:      now,
		UniverseSize: universe,
		Evaluations:  screener.Rank(evals),
		MarketOpen:   marketOpen,
	}
	e.log.Info("manual screening pass", "universe_scanned", universe,
		"evaluated", len(evals), "market_open", marketOpen, "orders_placed", 0)
	return preview, nil
}
