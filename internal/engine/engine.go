// Package engine drives the daily loop from docs/architecture.md: sentiment
// gate, screening, position monitoring, and the forced end-of-day exit.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/risk"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/screener"
	"github.com/martincoetzee/trading-agent/internal/sentiment"
	"github.com/martincoetzee/trading-agent/internal/store"
	"github.com/martincoetzee/trading-agent/internal/strategy"
)

// Deps are the engine's collaborators.
type Deps struct {
	Config  *config.Config
	Store   *store.Store
	Data    broker.MarketData
	Trading broker.Trading
	Logger  *slog.Logger
	// Audit records decisions and actions durably. A nil recorder is tolerated so a
	// misconfiguration can never panic a daemon holding open positions.
	Audit audit.Recorder
	// Now is injectable so a test can drive a whole trading day deterministically.
	Now func() time.Time
}

type Engine struct {
	cfg     *config.Config
	store   *store.Store
	data    broker.MarketData
	trading broker.Trading
	log     *slog.Logger
	audit   audit.Recorder
	now     func() time.Time

	mu           sync.RWMutex
	state        domain.AgentState
	lastError    string
	session      scheduler.Session
	sessionKnown bool
	lastScan     time.Time

	// The tradable universe barely changes within a day, so it is fetched once per
	// session rather than on every one-minute scan.
	universe     []string
	universeDate string
	// lastAuditedFault de-duplicates a repeating fault so the trail is not swamped.
	lastAuditedFault string
	// auditedSkips remembers the last skip reason audited per symbol, so a reason
	// that recurs on every scan — being at the position cap, most of all — is
	// recorded once rather than several hundred times a day.
	auditedSkips     map[string]string
	auditedSkipsDate string

	manual manualGuards
}

func New(d Deps) *Engine {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	recorder := d.Audit
	if recorder == nil {
		recorder = audit.Discard{}
	}
	return &Engine{
		cfg: d.Config, store: d.Store, data: d.Data, trading: d.Trading,
		log: logger, audit: recorder, now: now, state: domain.StateMarketClosed,
		auditedSkips: map[string]string{},
	}
}

// State is the current agent status for the web page's legend.
func (e *Engine) State() (domain.AgentState, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state, e.lastError
}

// Session returns the session currently loaded, and whether the exchange trades
// at all today.
func (e *Engine) Session() (scheduler.Session, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.session, e.sessionKnown
}

func (e *Engine) setState(s domain.AgentState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = s
	if s != domain.StateError {
		e.lastError = ""
		// Recovery resets the de-duplication, so a fault that comes back later is
		// recorded again rather than being swallowed as a repeat.
		e.lastAuditedFault = ""
	}
}

// record writes an audit event.
//
// A failed audit write is logged but does not fault the agent or abort the tick:
// the daemon may be holding open positions, and abandoning their exits to preserve
// a record would be the wrong trade. The error is logged at error level so the gap
// is visible rather than silent.
func (e *Engine) record(kind audit.Kind, symbol, summary string, detail map[string]any) {
	ev := audit.Event{
		At:          e.now(),
		SessionDate: scheduler.SessionDate(e.now()),
		Kind:        kind,
		Symbol:      symbol,
		Summary:     summary,
		Detail:      detail,
	}
	if err := e.audit.Record(ev); err != nil {
		e.log.Error("audit write failed; this decision is not in the trail",
			"kind", kind, "symbol", symbol, "err", err)
	}
}

// recordSkip audits a candidate that qualified but was not bought, at most once per
// symbol-and-reason per session.
//
// The reasons that matter recur: once the position cap is reached, every remaining
// qualifier is turned away on every scan for the rest of the day. Recording each
// occurrence would bury the trail, and recording none would lose the fact that a
// qualifying candidate was passed over at all.
func (e *Engine) recordSkip(symbol, reason string, detail map[string]any) {
	date := scheduler.SessionDate(e.now())

	e.mu.Lock()
	if e.auditedSkipsDate != date {
		e.auditedSkips = map[string]string{}
		e.auditedSkipsDate = date
	}
	already := e.auditedSkips[symbol] == reason
	e.auditedSkips[symbol] = reason
	e.mu.Unlock()

	if already {
		return
	}
	e.record(audit.EntrySkipped, symbol, "not entered: "+reason, detail)
}

// fail puts the agent into ERROR, which docs/web-ui.md requires be shown
// distinctly from a deliberate bearish halt.
func (e *Engine) fail(op string, err error) {
	e.log.Error("engine error", "op", op, "err", err)

	message := fmt.Sprintf("%s: %v", op, err)

	e.mu.Lock()
	repeat := e.lastAuditedFault == message
	e.lastAuditedFault = message
	e.state = domain.StateError
	e.lastError = message
	e.mu.Unlock()

	// A persistent upstream outage would otherwise write a fault every tick and
	// bury the trail; only a change of fault is worth recording.
	if !repeat {
		e.record(audit.Fault, "", message, map[string]any{"op": op})
	}
}

// Run ticks until the context is cancelled. The tick interval is the shortest of
// the configured cadences, and each tick decides for itself what is actually due.
func (e *Engine) Run(ctx context.Context) error {
	interval := e.cfg.Timing.PositionPollInterval
	if e.cfg.Timing.ScreenerScanInterval < interval {
		interval = e.cfg.Timing.ScreenerScanInterval
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}

	if err := e.Reconcile(ctx); err != nil {
		// A failed reconciliation is not fatal: it leaves the agent in ERROR so the
		// page shows it, and the next tick retries. Exiting would take the status
		// page down with it, which is the opposite of useful when state is unclear.
		e.fail("reconcile", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	e.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			e.Tick(ctx)
		}
	}
}

// Reconcile aligns local state with the broker after a restart.
//
// docs/decisions.md flags an unconfirmed order as a real hole: the daemon could
// have submitted an order and died before recording the fill. The broker's
// position list is the authority, so anything it holds that the store does not
// know about is adopted, and anything the store thinks is open but the broker
// does not hold is closed out as reconciled rather than left to be "sold" later.
func (e *Engine) Reconcile(ctx context.Context) error {
	brokerPositions, err := e.trading.Positions(ctx)
	if err != nil {
		return fmt.Errorf("list broker positions: %w", err)
	}
	local, err := e.store.OpenPositions()
	if err != nil {
		return fmt.Errorf("list local positions: %w", err)
	}

	byBroker := make(map[string]broker.BrokerPosition, len(brokerPositions))
	for _, p := range brokerPositions {
		byBroker[p.Symbol] = p
	}
	byLocal := make(map[string]domain.Position, len(local))
	for _, p := range local {
		byLocal[p.Symbol] = p
	}

	now := e.now()
	date := scheduler.SessionDate(now)

	for sym, bp := range byBroker {
		if _, known := byLocal[sym]; known {
			continue
		}
		e.log.Warn("adopting broker position absent from local state",
			"symbol", sym, "shares", bp.Shares, "avg_entry", bp.AvgEntry)
		e.record(audit.Reconciled, sym,
			fmt.Sprintf("adopted a broker position of %d shares the local record did not know about", bp.Shares),
			map[string]any{"action": "adopted", "shares": bp.Shares, "avg_entry": bp.AvgEntry})
		if _, err := e.store.InsertPosition(domain.Position{
			SessionDate: date, Symbol: sym, Shares: bp.Shares,
			EntryPrice: bp.AvgEntry, EntryTime: now, PeakPrice: bp.CurrentPrice,
		}); err != nil && !errors.Is(err, store.ErrDuplicateOpenPosition) {
			return fmt.Errorf("adopt %s: %w", sym, err)
		}
	}

	for sym, lp := range byLocal {
		if _, held := byBroker[sym]; held {
			continue
		}
		e.log.Warn("closing local position the broker no longer holds", "symbol", sym)
		e.record(audit.Reconciled, sym,
			"closed a local position the broker no longer holds",
			map[string]any{"action": "closed", "shares": lp.Shares, "entry_price": lp.EntryPrice})
		price := lp.EntryPrice
		if snaps, err := e.data.Snapshots(ctx, []string{sym}); err == nil {
			if s, ok := snaps[sym]; ok && s.Price > 0 {
				price = s.Price
			}
		}
		if err := e.store.ClosePosition(lp.ID, price, now, domain.ExitReconciled); err != nil {
			return fmt.Errorf("reconcile-close %s: %w", sym, err)
		}
	}

	unresolved, err := e.store.UnresolvedOrders()
	if err != nil {
		return fmt.Errorf("list unresolved orders: %w", err)
	}
	for _, o := range unresolved {
		e.log.Warn("order was never confirmed; resolved against broker positions",
			"client_order_id", o.ClientOrderID, "symbol", o.Symbol, "side", o.Side)
		e.record(audit.Reconciled, o.Symbol,
			fmt.Sprintf("resolved an unconfirmed %s order against the broker's positions", o.Side),
			map[string]any{"action": "order_resolved", "side": o.Side,
				"shares": o.Shares, "client_order_id": o.ClientOrderID})
		if err := e.store.UpdateOrderStatus(o.ClientOrderID, "reconciled", o.BrokerOrderID); err != nil {
			return fmt.Errorf("mark order reconciled: %w", err)
		}
	}
	return nil
}

// loadSession fetches the exchange calendar for the current date, caching it for
// the rest of the day.
func (e *Engine) loadSession(ctx context.Context) (scheduler.Session, bool, error) {
	now := e.now()
	date := scheduler.SessionDate(now)

	e.mu.RLock()
	cached, known := e.session, e.sessionKnown
	e.mu.RUnlock()
	if cached.Date == date {
		return cached, known, nil
	}

	day, err := e.trading.Calendar(ctx, date)
	if err != nil {
		return scheduler.Session{}, false, fmt.Errorf("calendar: %w", err)
	}

	sess := scheduler.Session{Date: date, Open: day.Open, Close: day.Close}
	tradingDay := !day.Open.IsZero() && !day.Close.IsZero()
	if !tradingDay {
		// Cache the fact that today is closed so the calendar is not re-fetched on
		// every tick through a weekend.
		sess = scheduler.Session{Date: date}
	}

	e.mu.Lock()
	e.session, e.sessionKnown = sess, tradingDay
	e.mu.Unlock()
	return sess, tradingDay, nil
}

// Tick performs one iteration: it works out the phase and does whatever is due.
func (e *Engine) Tick(ctx context.Context) {
	sess, tradingDay, err := e.loadSession(ctx)
	if err != nil {
		e.fail("load session", err)
		return
	}
	if !tradingDay {
		e.setState(domain.StateMarketClosed)
		return
	}

	bounds := scheduler.Bounds(sess, e.cfg)
	now := e.now()

	switch scheduler.PhaseAt(now, bounds) {
	case domain.PhaseClosed:
		e.setState(domain.StateMarketClosed)

	case domain.PhaseFirstHour:
		e.setState(domain.StateSentimentCheck)
		if err := e.pollSentiment(ctx, sess); err != nil {
			e.fail("poll sentiment", err)
		}

	case domain.PhaseTrading:
		halted, err := e.resolveGate(ctx, sess)
		if err != nil {
			e.fail("resolve sentiment gate", err)
			return
		}
		// Positions are monitored even when halted: the kill switch stops new
		// entries, it does not abandon anything already held.
		if err := e.managePositions(ctx, sess, bounds, false); err != nil {
			e.fail("manage positions", err)
			return
		}
		if halted {
			e.setState(domain.StateHaltedBearish)
			return
		}
		e.setState(domain.StateScreening)
		if err := e.maybeScreen(ctx, sess); err != nil {
			e.fail("screen", err)
		}

	case domain.PhaseEODWindow:
		e.setState(domain.StateEODWindow)
		if err := e.managePositions(ctx, sess, bounds, true); err != nil {
			e.fail("force exit", err)
		}
	}
}

// pollSentiment records a reading if one is due.
func (e *Engine) pollSentiment(ctx context.Context, sess scheduler.Session) error {
	readings, err := e.store.SentimentReadings(sess.Date)
	if err != nil {
		return err
	}
	now := e.now()
	// Derived from stored readings rather than an in-memory timestamp so a restart
	// mid-hour does not double-poll or skip the remaining polls.
	if len(readings) > 0 {
		last := readings[len(readings)-1].TakenAt
		if now.Sub(last) < e.cfg.Timing.SentimentPollInterval {
			return nil
		}
	}

	snaps, err := e.data.Snapshots(ctx, e.cfg.Sentiment.Symbols)
	if err != nil {
		return err
	}
	pcts := make(map[string]float64, len(snaps))
	for sym, s := range snaps {
		if pct, ok := sentiment.PercentChange(s.Price, s.PrevClose); ok {
			pcts[sym] = pct
		}
	}

	reading := domain.SentimentReading{
		TakenAt: now, SessionDate: sess.Date, Percentages: pcts,
		Classification: sentiment.Classify(pcts, e.cfg),
	}
	e.log.Info("sentiment reading", "session", sess.Date,
		"classification", reading.Classification, "percentages", pcts)
	e.record(audit.SentimentRead, "", fmt.Sprintf("sentiment reads %s", reading.Classification),
		map[string]any{"classification": string(reading.Classification), "percentages": pcts})
	return e.store.AddSentimentReading(reading)
}

// resolveGate decides the session once the first hour is over, and reports
// whether trading is halted.
func (e *Engine) resolveGate(ctx context.Context, sess scheduler.Session) (bool, error) {
	rec, err := e.store.Session(sess.Date)
	if err != nil {
		return false, err
	}
	if rec.Verdict != domain.VerdictPending {
		return rec.Halted, nil
	}

	readings, err := e.store.SentimentReadings(sess.Date)
	if err != nil {
		return false, err
	}
	if len(readings) == 0 {
		// The daemon was started after the first hour, so the gate has no data to
		// judge. Trading requires a sentiment check to have happened, so stay out
		// for the day rather than assuming conditions are fine.
		reason := "no sentiment readings were taken during the first hour"
		e.log.Warn("no sentiment readings for this session; halting for the day",
			"session", sess.Date)
		if err := e.store.SetVerdict(sess.Date, domain.VerdictBearish, reason); err != nil {
			return false, err
		}
		e.record(audit.TradingHalted, "", "halted for the session: "+reason,
			map[string]any{"reason": reason, "readings": 0})
		return true, nil
	}

	verdict := sentiment.GateVerdict(readings, e.cfg)
	reason := ""
	if verdict == domain.VerdictBearish {
		reason = fmt.Sprintf("first-hour sentiment %v", readings[len(readings)-1].Percentages)
	}
	e.log.Info("sentiment gate resolved", "session", sess.Date, "verdict", verdict)
	last := readings[len(readings)-1]
	detail := map[string]any{
		"verdict":     string(verdict),
		"readings":    len(readings),
		"percentages": last.Percentages,
	}
	if err := e.store.SetVerdict(sess.Date, verdict, reason); err != nil {
		return false, err
	}

	halted := verdict == domain.VerdictBearish
	if halted {
		e.record(audit.TradingHalted, "",
			"halted for the session: first-hour sentiment was overwhelmingly bearish", detail)
	} else {
		e.record(audit.GateResolved, "",
			"gate passed; screening may begin", detail)
	}
	return halted, nil
}

// maybeScreen runs a screening pass if the scan interval has elapsed.
func (e *Engine) maybeScreen(ctx context.Context, sess scheduler.Session) error {
	now := e.now()
	e.mu.RLock()
	last := e.lastScan
	e.mu.RUnlock()
	if !last.IsZero() && now.Sub(last) < e.cfg.Timing.ScreenerScanInterval {
		return nil
	}
	e.mu.Lock()
	e.lastScan = now
	e.mu.Unlock()

	evals, err := e.screen(ctx, sess)
	if err != nil {
		return err
	}

	// Entry runs before the snapshot is stored so the page can show what actually
	// happened to each qualifying candidate, not just that it qualified.
	outcomes, entryErr := e.enterPositions(ctx, sess, evals)
	for i := range evals {
		if outcome, ok := outcomes[evals[i].Symbol]; ok {
			evals[i].Outcome = outcome
		}
	}
	// Saved even when entry failed: a pass that went wrong is exactly when seeing
	// the candidate table matters.
	if err := e.store.SaveScreenSnapshot(sess.Date, now, evals); err != nil {
		return err
	}
	return entryErr
}

// screen evaluates the candidate universe against the four entry criteria.
func (e *Engine) screen(ctx context.Context, sess scheduler.Session) ([]domain.Evaluation, error) {
	inputs, _, err := e.gatherCandidates(ctx, e.newsSince())
	if err != nil {
		return nil, err
	}
	evals := make([]domain.Evaluation, 0, len(inputs))
	for _, in := range inputs {
		evals = append(evals, screener.Evaluate(in, e.cfg))
	}
	return evals, nil
}

// newsSince is the start of the window searched for a catalyst.
//
// It deliberately reaches back before the market open rather than starting at it: a
// gap-up's catalyst usually breaks overnight or pre-market, so a window beginning at
// 09:30 would miss the story that caused the move and report no news for precisely
// the candidates worth trading.
func (e *Engine) newsSince() time.Time {
	return e.now().Add(-e.cfg.Screening.NewsLookback)
}

// criteriaDetail flattens an evaluation's criteria for the audit trail, so the
// record shows what was true at the moment of the decision rather than requiring
// the reader to trust that the thresholds were the same.
func criteriaDetail(eval domain.Evaluation) map[string]any {
	out := make(map[string]any, len(eval.Criteria))
	for _, c := range eval.Criteria {
		out[c.Name] = c.Display
	}
	return out
}

// tradableUniverse returns the day's symbol list, fetching it at most once per
// session.
func (e *Engine) tradableUniverse(ctx context.Context) ([]string, error) {
	date := scheduler.SessionDate(e.now())

	e.mu.RLock()
	cached, cachedDate := e.universe, e.universeDate
	e.mu.RUnlock()
	if cachedDate == date && len(cached) > 0 {
		return cached, nil
	}

	symbols, err := e.data.TradableAssets(ctx)
	if err != nil {
		return nil, fmt.Errorf("tradable assets: %w", err)
	}
	e.log.Info("loaded tradable universe", "symbols", len(symbols), "session", date)

	e.mu.Lock()
	e.universe, e.universeDate = symbols, date
	e.mu.Unlock()
	return symbols, nil
}

// gatherCandidates scans the whole tradable universe and assembles the per-symbol
// screening inputs.
//
// The order of work is what makes a full-market scan affordable. The price-move
// criterion is evaluated first, from batched snapshots that cost a couple of dozen
// requests for the entire market; that alone cuts thousands of symbols down to a
// handful. Only those survivors get the per-symbol news and average-volume calls.
//
// When more names clear the move threshold than MaxEnriched allows, the busiest by
// dollar volume are kept. Dollar volume is the closest cheap stand-in for the
// relative-volume criterion the strategy actually ranks on — sorting by percentage
// change instead (which is what Alpaca's movers endpoint did) would discard a
// heavily traded +11% name in favour of a thin +40% one, the opposite of the
// intended preference.
func (e *Engine) gatherCandidates(ctx context.Context, newsSince time.Time) ([]screener.Input, int, error) {
	universe, err := e.tradableUniverse(ctx)
	if err != nil {
		return nil, 0, err
	}
	if len(universe) == 0 {
		return nil, 0, nil
	}

	snaps, err := e.data.Snapshots(ctx, universe)
	if err != nil {
		return nil, 0, fmt.Errorf("snapshots: %w", err)
	}

	type mover struct {
		symbol string
		snap   domain.Snapshot
		dollar float64
	}
	var movers []mover
	for _, sym := range universe {
		snap, ok := snaps[sym]
		if !ok || snap.IntradayPct < e.cfg.Screening.MinIntradayPct {
			continue
		}
		movers = append(movers, mover{
			symbol: sym, snap: snap, dollar: snap.Price * snap.TodayVolume,
		})
	}

	sort.SliceStable(movers, func(i, j int) bool { return movers[i].dollar > movers[j].dollar })
	moved := len(movers)
	if moved > e.cfg.Screening.MaxEnriched {
		movers = movers[:e.cfg.Screening.MaxEnriched]
		e.log.Info("more movers than the enrichment budget; keeping the busiest",
			"cleared_threshold", moved, "enriched", len(movers))
	}
	if len(movers) == 0 {
		return nil, len(universe), nil
	}

	shortlist := make([]string, 0, len(movers))
	for _, m := range movers {
		shortlist = append(shortlist, m.symbol)
	}
	news, err := e.data.NewsCounts(ctx, shortlist, newsSince)
	if err != nil {
		return nil, 0, fmt.Errorf("news: %w", err)
	}

	inputs := make([]screener.Input, 0, len(movers))
	for _, m := range movers {
		in := screener.Input{
			Symbol:      m.symbol,
			Price:       m.snap.Price,
			IntradayPct: m.snap.IntradayPct,
			TodayVolume: m.snap.TodayVolume,
			NewsCount:   news[m.symbol],
		}
		if avg, err := e.data.AverageDailyVolume(ctx, m.symbol, e.cfg.Screening.AvgVolumeLookbackDays); err != nil {
			e.log.Warn("average volume unavailable", "symbol", m.symbol, "err", err)
		} else {
			in.AvgVolume = avg
		}
		inputs = append(inputs, in)
	}
	return inputs, len(universe), nil
}

// enterPositions buys the strongest qualifying candidates that fit under the
// exposure cap, and reports what happened to each one.
//
// A failure affecting a single candidate — an unavailable price, say — skips that
// candidate rather than abandoning the pass. Aborting would discard the remaining
// qualifiers, which on a one-minute cadence means a transient blip on one symbol
// silently costs the others their entry. Failures that are not specific to a
// candidate still stop the pass and surface as an error.
func (e *Engine) enterPositions(ctx context.Context, sess scheduler.Session, evals []domain.Evaluation) (map[string]string, error) {
	outcomes := map[string]string{}

	qualifying := screener.Qualifying(evals)
	if len(qualifying) == 0 {
		return outcomes, nil
	}

	open, err := e.store.OpenPositions()
	if err != nil {
		return outcomes, err
	}
	tradedToday, err := e.store.SymbolsTradedOn(sess.Date)
	if err != nil {
		return outcomes, err
	}
	openSymbols := make(map[string]bool, len(open))
	for _, p := range open {
		openSymbols[p.Symbol] = true
	}
	openCount := len(open)

	for i, cand := range qualifying {
		if !risk.CanOpen(openCount, e.cfg) {
			// Explain every remaining candidate rather than leaving them blank.
			reason := fmt.Sprintf("position cap reached (%d)", e.cfg.Risk.MaxConcurrentPositions)
			for _, rest := range qualifying[i:] {
				outcomes[rest.Symbol] = reason
				e.recordSkip(rest.Symbol, reason, map[string]any{
					"reason":          reason,
					"relative_volume": rest.VolumeMultiple,
					"open_positions":  openCount,
				})
			}
			return outcomes, nil
		}
		if allowed, reason, routine := risk.AllowEntry(cand.Symbol, openSymbols, tradedToday, openCount, e.cfg); !allowed {
			if routine {
				e.log.Debug("skipping candidate", "symbol", cand.Symbol, "reason", reason)
			} else {
				e.log.Info("skipping candidate", "symbol", cand.Symbol, "reason", reason)
			}
			outcomes[cand.Symbol] = reason
			// Routine reasons (already holding it, already traded today) are implied by
			// the position events themselves, so they add nothing to the trail.
			if !routine {
				e.recordSkip(cand.Symbol, reason,
					map[string]any{"reason": reason, "relative_volume": cand.VolumeMultiple})
			}
			continue
		}

		snaps, err := e.data.Snapshots(ctx, []string{cand.Symbol})
		if err != nil {
			e.log.Warn("skipping candidate: price unavailable", "symbol", cand.Symbol, "err", err)
			outcomes[cand.Symbol] = "price unavailable"
			e.recordSkip(cand.Symbol, "price unavailable",
				map[string]any{"reason": "price unavailable", "error": err.Error()})
			continue
		}
		price := snaps[cand.Symbol].Price

		// Re-read per candidate rather than once per pass: each fill consumes cash,
		// so sizing the next position from a stale balance would over-commit.
		acct, err := e.trading.Account(ctx)
		if err != nil {
			e.log.Warn("skipping candidate: account unavailable", "symbol", cand.Symbol, "err", err)
			outcomes[cand.Symbol] = "account unavailable"
			e.recordSkip(cand.Symbol, "account unavailable",
				map[string]any{"reason": "account unavailable", "error": err.Error()})
			continue
		}

		sizing := risk.Size(acct, price, e.cfg)
		if !sizing.OK {
			e.log.Info("skipping candidate", "symbol", cand.Symbol, "reason", sizing.Reason)
			outcomes[cand.Symbol] = sizing.Reason
			e.recordSkip(cand.Symbol, sizing.Reason,
				map[string]any{"reason": sizing.Reason, "price": price, "cash": acct.Cash})
			continue
		}

		if err := e.submit(ctx, sess, cand.Symbol, "buy", sizing.Shares, price); err != nil {
			e.log.Warn("skipping candidate: order rejected", "symbol", cand.Symbol, "err", err)
			outcomes[cand.Symbol] = "order rejected"
			e.recordSkip(cand.Symbol, "order rejected",
				map[string]any{"reason": "order rejected", "shares": sizing.Shares,
					"price": price, "error": err.Error()})
			continue
		}
		if _, err := e.store.InsertPosition(domain.Position{
			SessionDate: sess.Date, Symbol: cand.Symbol, Shares: sizing.Shares,
			EntryPrice: price, EntryTime: e.now(), PeakPrice: price,
		}); err != nil {
			if errors.Is(err, store.ErrDuplicateOpenPosition) {
				e.log.Warn("duplicate position rejected by store", "symbol", cand.Symbol)
				outcomes[cand.Symbol] = "already holding this symbol"
				continue
			}
			return outcomes, err
		}

		e.log.Info("entered position", "symbol", cand.Symbol, "shares", sizing.Shares,
			"price", price, "rel_volume", cand.VolumeMultiple)
		outcomes[cand.Symbol] = fmt.Sprintf("bought %d @ $%.2f", sizing.Shares, price)
		// Everything needed to reconstruct the decision later: the criteria that were
		// met, the size and why it was that size, and the account state behind it.
		e.record(audit.PositionOpened, cand.Symbol,
			fmt.Sprintf("bought %d shares at $%.2f", sizing.Shares, price),
			map[string]any{
				"shares":               sizing.Shares,
				"price":                price,
				"dollars":              sizing.Dollars,
				"relative_volume":      cand.VolumeMultiple,
				"criteria":             criteriaDetail(cand),
				"portfolio_value":      acct.PortfolioValue,
				"cash_before":          acct.Cash,
				"position_size_pct":    e.cfg.Risk.PositionSizePct,
				"open_positions_after": openCount + 1,
			})
		openSymbols[cand.Symbol] = true
		tradedToday[cand.Symbol] = true
		openCount++
	}
	return outcomes, nil
}

// managePositions updates high-water marks and applies the exit rules. When
// forceEOD is set, every open position is closed regardless of the other rules.
func (e *Engine) managePositions(ctx context.Context, sess scheduler.Session, bounds scheduler.Boundaries, forceEOD bool) error {
	open, err := e.store.OpenPositions()
	if err != nil {
		return err
	}
	if len(open) == 0 {
		return nil
	}

	symbols := make([]string, 0, len(open))
	for _, p := range open {
		symbols = append(symbols, p.Symbol)
	}
	snaps, err := e.data.Snapshots(ctx, symbols)
	if err != nil {
		return fmt.Errorf("snapshots: %w", err)
	}

	for _, p := range open {
		price := snaps[p.Symbol].Price
		if price <= 0 && !forceEOD {
			// Without a price the exit rules cannot be evaluated; skip rather than
			// acting on a zero that would read as a catastrophic loss.
			e.log.Warn("no price for open position", "symbol", p.Symbol)
			continue
		}

		if price > p.PeakPrice {
			p.PeakPrice = price
		}
		armed := strategy.TrailArmed(p, e.cfg)
		if err := e.store.UpdateMark(p.ID, price, p.PeakPrice, armed); err != nil {
			return err
		}
		p.TrailArmed = armed

		var closes []float64
		if !forceEOD {
			bars, err := e.data.IntradayBars(ctx, p.Symbol, e.cfg.Exit.MACDIntervalMins, sess.Open)
			if err != nil {
				e.log.Warn("intraday bars unavailable; MACD exit cannot be evaluated",
					"symbol", p.Symbol, "err", err)
			} else {
				closes = make([]float64, 0, len(bars))
				for _, b := range bars {
					closes = append(closes, b.Close)
				}
			}
		}

		decision := strategy.EvaluateExit(strategy.ExitInput{
			Position: p, Price: price, Closes: closes, EODReached: forceEOD,
		}, e.cfg)
		if !decision.Exit {
			continue
		}

		exitPrice := price
		if exitPrice <= 0 {
			exitPrice = p.EntryPrice
		}
		if err := e.submit(ctx, sess, p.Symbol, "sell", p.Shares, exitPrice); err != nil {
			return err
		}
		if err := e.store.ClosePosition(p.ID, exitPrice, e.now(), decision.Reason); err != nil {
			return err
		}
		e.log.Info("exited position", "symbol", p.Symbol, "reason", decision.Reason,
			"entry", p.EntryPrice, "exit", exitPrice,
			"pct", fmt.Sprintf("%+.2f", p.UnrealizedPct(exitPrice)))
		e.record(audit.PositionClosed, p.Symbol,
			fmt.Sprintf("sold %d shares at $%.2f (%s), %+.2f%%",
				p.Shares, exitPrice, decision.Reason, p.UnrealizedPct(exitPrice)),
			map[string]any{
				"reason":      string(decision.Reason),
				"shares":      p.Shares,
				"entry_price": p.EntryPrice,
				"exit_price":  exitPrice,
				"peak_price":  p.PeakPrice,
				"trail_armed": p.TrailArmed,
				"pnl_dollars": p.UnrealizedDollars(exitPrice),
				"pnl_pct":     p.UnrealizedPct(exitPrice),
				"held_for":    e.now().Sub(p.EntryTime).String(),
			})
	}
	return nil
}

// submit records an order's intent before sending it, so a crash between the two
// leaves something for Reconcile to find.
func (e *Engine) submit(ctx context.Context, sess scheduler.Session, symbol, side string, shares int, price float64) error {
	clientOrderID := fmt.Sprintf("%s-%s-%s-%d", sess.Date, symbol, side, e.now().UnixNano())

	rec := store.OrderRecord{
		ClientOrderID: clientOrderID, SessionDate: sess.Date, Symbol: symbol,
		Side: side, Shares: shares, SubmittedAt: e.now(), Status: "submitted",
	}
	if err := e.store.RecordOrder(rec); err != nil {
		return err
	}

	req := broker.OrderRequest{
		Symbol: symbol, Shares: shares, Side: side,
		Type: e.cfg.Execution.OrderType, ClientOrderID: clientOrderID,
	}
	if req.Type == "limit" {
		slip := 1 + e.cfg.Execution.LimitSlipPct/100
		if side == "sell" {
			slip = 1 - e.cfg.Execution.LimitSlipPct/100
		}
		req.LimitPrice = price * slip
	}

	res, err := e.trading.PlaceOrder(ctx, req)
	if err != nil {
		if updateErr := e.store.UpdateOrderStatus(clientOrderID, "failed", ""); updateErr != nil {
			e.log.Error("failed to record order failure", "err", updateErr)
		}
		return fmt.Errorf("place %s order for %s: %w", side, symbol, err)
	}
	return e.store.UpdateOrderStatus(clientOrderID, res.Status, res.BrokerOrderID)
}
