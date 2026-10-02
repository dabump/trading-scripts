package engine

import (
	"context"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
)

// The screen and the setup answer different questions at different speeds. Whether a
// name has news, a 10% move and 5x volume changes over minutes and costs ~130 requests
// to find out; whether its chart has just triggered changes every candle and costs one
// bar request per candidate. Run together, the setup waited behind the screen: on
// 2026-10-02 each entry was decided on a candle that had closed almost a minute
// earlier, and the stops on open positions went unchecked for most of every minute.
//
// So the screen publishes a watchlist and the tick acts on it. The screen runs in the
// background under Run (inline when a test calls Tick), and never trades: every order
// is still sent from Tick.

// setupBarGrace is how long after a candle closes the setup check keeps asking for it.
// The feed publishes a minute bar a few seconds after the minute ends, and a name that
// did not trade in that minute has no bar at all; past this it waits for the next one.
const setupBarGrace = 10 * time.Second

// watchlist is one screen's result: the candidates the setup check works from until
// the next screen replaces it.
type watchlist struct {
	date    string
	takenAt time.Time
	pass    scanPass
	evals   []domain.Evaluation
}

// startScreen runs one screening pass and publishes its result as the watchlist. The
// pass is built inside, because the pre-market one takes a sentiment reading and that
// is a request the tick should not wait on either.
func (e *Engine) startScreen(ctx context.Context, sess scheduler.Session, op string, build func(context.Context) scanPass) {
	run := func() {
		p := build(ctx)
		takenAt := e.now()
		evals, err := e.screen(ctx, p.thresholds, p.barsSince)
		if err != nil {
			// Shutdown cancels a screen part-way; that is not a fault.
			if ctx.Err() == nil {
				e.fail(op, err)
			}
			return
		}
		e.mu.Lock()
		e.watch = &watchlist{date: sess.Date, takenAt: takenAt, pass: p, evals: evals}
		e.watchSeq++
		e.mu.Unlock()
	}
	if !e.background {
		run()
		return
	}
	e.mu.Lock()
	e.screening = true
	e.mu.Unlock()
	e.screens.Add(1)
	go func() {
		defer e.screens.Done()
		defer func() {
			e.mu.Lock()
			e.screening = false
			e.mu.Unlock()
		}()
		run()
	}()
}

// checkSetups acts on the current watchlist: reads the chart of each qualifying
// candidate whose candle has closed since it was last read, buys the ones that have
// set up, and saves the candidate table with what happened to each.
//
// A watchlist from the other session is ignored rather than acted on: pre-market and
// the regular session judge candidates against different thresholds.
func (e *Engine) checkSetups(ctx context.Context, sess scheduler.Session, bounds scheduler.Boundaries, preMarket bool) error {
	e.mu.RLock()
	w, seq := e.watch, e.watchSeq
	e.mu.RUnlock()
	if w == nil || w.date != sess.Date || w.pass.preMarket != preMarket {
		return nil
	}
	e.setups.resetFor(sess.Date, preMarket)
	fresh := seq != e.setups.seq
	e.setups.seq = seq

	p := w.pass
	if !p.preMarket {
		// Re-judged on every tick rather than taken from the screen, so the window
		// closes on time even when the watchlist was published just before it.
		p.blocked = e.regularPass(e.now(), sess, bounds).blocked
	}

	var outcomes map[string]string
	var entryErr error
	if p.blocked != "" {
		outcomes = make(map[string]string, len(w.evals))
		for _, ev := range w.evals {
			if ev.Qualifies {
				outcomes[ev.Symbol] = p.blocked
			}
		}
		e.recordSkip("", p.blocked,
			map[string]any{"reason": p.blocked, "pre_market": p.preMarket})
	} else {
		outcomes, entryErr = e.enterPositions(ctx, sess, w.evals, p)
	}

	// Saved only when something on it changed: the check runs every tick.
	if !e.setups.remember(outcomes) && !fresh {
		return entryErr
	}
	evals := make([]domain.Evaluation, len(w.evals))
	copy(evals, w.evals)
	for i := range evals {
		if evals[i].Qualifies {
			evals[i].Outcome = e.setups.outcomes[evals[i].Symbol]
		}
	}
	// Saved even when entry failed: a pass that went wrong is exactly when seeing
	// the candidate table matters.
	if err := e.store.SaveScreenSnapshot(sess.Date, w.takenAt, evals); err != nil {
		return err
	}
	return entryErr
}

// setupState is what the setup check remembers between ticks, for one session.
type setupState struct {
	key string
	// seq is the watchlist publication last acted on.
	seq int
	// lastRead is the start of the newest completed candle read per symbol, and
	// fetchedFor the candle the last bar request was made for.
	lastRead   map[string]time.Time
	fetchedFor map[string]time.Time
	// outcomes is the Action column, per symbol: kept between reads, because a
	// candidate whose chart has not moved still has the outcome it was last given.
	outcomes map[string]string
}

// resetFor starts afresh when the session, or which of its two halves, changes.
func (s *setupState) resetFor(date string, preMarket bool) {
	key := date
	if preMarket {
		key += "/pre"
	}
	if s.key == key {
		return
	}
	s.key = key
	s.lastRead = map[string]time.Time{}
	s.fetchedFor = map[string]time.Time{}
	s.outcomes = map[string]string{}
}

// due reports whether symbol's chart is worth requesting: the candle that most
// recently closed has not been read, and either it has not been asked for or it is
// still young enough that it may simply not be published yet.
func (s *setupState) due(symbol string, now time.Time, interval time.Duration) bool {
	want := now.Truncate(interval).Add(-interval)
	if !s.lastRead[symbol].Before(want) {
		return false
	}
	return !s.fetchedFor[symbol].Equal(want) || now.Sub(want.Add(interval)) < setupBarGrace
}

func (s *setupState) fetched(symbol string, now time.Time, interval time.Duration) {
	s.fetchedFor[symbol] = now.Truncate(interval).Add(-interval)
}

// read records that the chart up to the candle starting at newest has been read, and
// reports false when that candle was already read.
func (s *setupState) read(symbol string, newest time.Time) bool {
	if !newest.After(s.lastRead[symbol]) {
		return false
	}
	s.lastRead[symbol] = newest
	return true
}

// remember merges outcomes into the Action column and reports whether any changed.
func (s *setupState) remember(outcomes map[string]string) bool {
	changed := false
	for sym, o := range outcomes {
		if s.outcomes[sym] != o {
			s.outcomes[sym] = o
			changed = true
		}
	}
	return changed
}

// completedBars drops the candle still forming, and any after it.
func completedBars(bars []domain.Bar, now time.Time, interval time.Duration) []domain.Bar {
	n := len(bars)
	for n > 0 && bars[n-1].Time.Add(interval).After(now) {
		n--
	}
	return bars[:n]
}
