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
	// nextSession is the first session after today, for the status page's countdown
	// to the next open. After the close — or on a weekend or holiday — today's
	// calendar entry says nothing about when trading resumes.
	nextSession      scheduler.Session
	nextSessionKnown bool
	lastScan         time.Time
	// account is the last balance read from the broker, for the status page. The web
	// layer cannot ask the broker itself, so the tick publishes it here.
	account domain.AccountSnapshot

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

	// trailedTo is the start of the last completed candle the candle trail has read,
	// per position, so the bar request is made once per candle rather than on every
	// position poll. Only managePositions touches it, and only from Tick.
	trailedTo map[int64]time.Time
	// faultedStops latches the positions whose exit is being held off because their
	// resting stop could not be cancelled, so that fault is audited once rather than
	// on every tick — and timing.position_poll_interval is 2s, so "every tick" is
	// over a thousand identical rows an hour. Cleared when the stop does come off,
	// so a condition that recurs later is recorded again.
	faultedStops map[int64]bool
	// fillWait bounds how long submit waits for an order to finish filling before
	// cancelling what is left, and fillPoll is how often it asks in the meantime.
	// Fields rather than config because they describe the broker, not the strategy;
	// tests shorten them.
	fillWait time.Duration
	fillPoll time.Duration
}

// Paper fills on 2026-09-29 took up to five seconds from submission, market orders
// included, so the wait is twice that.
const (
	defaultFillWait = 10 * time.Second
	defaultFillPoll = 250 * time.Millisecond
)

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
		fillWait:     defaultFillWait, fillPoll: defaultFillPoll,
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

// NextSession returns the first session after today, and whether it is known.
// Unknown is a normal outcome — the calendar lookup may have failed — and callers
// render nothing rather than guessing.
func (e *Engine) NextSession() (scheduler.Session, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.nextSession, e.nextSessionKnown
}

// Account returns the last balance read from the broker, for the status page.
//
// Not known is a normal outcome — before the first tick, or while the account
// endpoint is failing — and callers render nothing rather than a zero balance.
func (e *Engine) Account() domain.AccountSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.account
}

// publishAccount caches a balance the tick has just read.
func (e *Engine) publishAccount(acct domain.Account) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.account = domain.AccountSnapshot{Account: acct, At: e.now(), Known: true}
}

// refreshAccount updates the balance the page shows.
//
// It runs on every tick, including outside market hours: one request per tick is
// negligible beside the scan's ~130, and it is what keeps the figure current when
// the market is closed and nothing else talks to the broker.
//
// A failure is display-only and never faults the agent — the trading path reads the
// account for itself before it sizes anything, and faults there. The previous
// snapshot is kept with its own timestamp, so the page reports an aging balance
// rather than losing it. Logged at debug because it would otherwise repeat on every
// tick for as long as the endpoint is unhappy.
func (e *Engine) refreshAccount(ctx context.Context) {
	acct, err := e.trading.Account(ctx)
	if err != nil {
		e.log.Debug("account balance unavailable", "err", err)
		return
	}
	e.publishAccount(acct)
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
		// A manual position with a resting stop has an obvious suspect, and it carries
		// the real exit: the broker's own fill price and time. Asking it first is what
		// keeps a stop that fired overnight from being recorded as a reconciliation at
		// whatever the symbol happens to be worth this morning, which on a gap is a
		// different number entirely.
		if lp.StopOrderID != "" {
			res, err := e.trading.Order(ctx, lp.StopOrderID)
			if err != nil {
				e.log.Warn("could not read the protective stop while reconciling",
					"symbol", sym, "err", err)
			} else if res.FilledShares > 0 {
				e.log.Info("protective stop had fired while the agent was not running",
					"symbol", sym, "shares", res.FilledShares, "fill", res.FilledPrice)
				e.record(audit.Reconciled, sym,
					fmt.Sprintf("the resting stop order sold %d shares at $%.2f while the agent was "+
						"not running", res.FilledShares, res.FilledPrice),
					map[string]any{"action": "stop_filled", "shares": res.FilledShares,
						"entry_price": lp.EntryPrice, "exit_price": res.FilledPrice,
						"stop_price": lp.StopPrice})
				if err := e.closeOnProtectiveStop(lp, res); err != nil {
					return fmt.Errorf("reconcile-close %s: %w", sym, err)
				}
				continue
			}
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

	// Fetched in the same pass, so it costs one extra call per day rather than one
	// per tick. A failure here is not fatal: the countdown simply reads "unknown"
	// while trading continues.
	next, nextKnown := scheduler.Session{}, false
	if day, err := e.trading.NextSession(ctx, date); err != nil {
		e.log.Warn("next session unavailable; the page cannot count down to the next open",
			"err", err)
	} else if !day.Open.IsZero() && !day.Close.IsZero() {
		next = scheduler.Session{Date: day.Date, Open: day.Open, Close: day.Close}
		nextKnown = true
	}

	e.mu.Lock()
	e.session, e.sessionKnown = sess, tradingDay
	e.nextSession, e.nextSessionKnown = next, nextKnown
	e.mu.Unlock()
	return sess, tradingDay, nil
}

// Tick performs one iteration: it works out the phase and does whatever is due.
func (e *Engine) Tick(ctx context.Context) {
	// Ahead of the session load, so a failing calendar lookup does not also take the
	// balance off the page.
	e.refreshAccount(ctx)

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

	case domain.PhasePreMarket:
		e.setState(domain.StatePreMarket)
		// Exits run before the bell too. Normally there is nothing held — the previous
		// session was flattened at its forced exit — but with premarket.allow_entry on,
		// a position opened at 07:00 has to be able to stop out at 08:00 rather than
		// waiting three hours for the regular loop to notice.
		if err := e.managePositions(ctx, sess, bounds, false); err != nil {
			e.fail("manage positions", err)
			return
		}
		if !e.claimScan(now, e.cfg.PreMarket.ScanInterval) {
			return
		}
		if err := e.runScan(ctx, sess, e.preMarketPass(ctx, bounds)); err != nil {
			e.fail("pre-market screen", err)
		}

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
		if !e.claimScan(e.now(), e.cfg.Timing.ScreenerScanInterval) {
			return
		}
		if err := e.runScan(ctx, sess, e.regularPass(e.now(), sess, bounds)); err != nil {
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

// scanPass is everything one screening pass needs to know about the session it runs
// in.
//
// There are two of them now. The regular session and pre-market share the screening
// and entry machinery but agree on almost nothing else: a different cadence,
// different volume thresholds, a different start for the setup's chart, a different
// order routing, and different reasons to screen without buying. Bundling that into
// a value built once in Tick keeps the decision in one place instead of re-deriving
// it from the clock inside every step.
type scanPass struct {
	// preMarket labels the pass in the log and the audit trail.
	preMarket bool
	// thresholds are the screening numbers in force, resolved once so the trail can
	// say what the candidates were judged against.
	thresholds screener.Thresholds
	// barsSince is where the setup detector's chart begins.
	barsSince time.Time
	// extendedHours routes any order to the pre-market book.
	extendedHours bool
	// blocked, when non-empty, is why this pass may screen but not buy. It becomes the
	// Action column's text against every qualifying candidate, which is the whole
	// reason screening continues when entry does not.
	blocked string
}

// claimScan reports whether the scan interval has elapsed, claiming the slot when it
// has.
//
// The interval check lives here, called from Tick, rather than inside the pass:
// pre-market runs on its own cadence (premarket.scan_interval) and a single scan
// that consulted timing.screener_scan_interval for both would either flood the
// pre-market with requests or starve the regular session of them.
func (e *Engine) claimScan(now time.Time, interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.lastScan.IsZero() && now.Sub(e.lastScan) < interval {
		return false
	}
	e.lastScan = now
	return true
}

// regularPass is the scan for the open market.
//
// After the entry window closes it keeps screening but stops buying. The page is the
// reason: an operator watching the afternoon should still see what is setting up and
// why it was not taken, rather than an empty table that looks like a broken scanner.
func (e *Engine) regularPass(now time.Time, sess scheduler.Session, bounds scheduler.Boundaries) scanPass {
	p := scanPass{
		thresholds: screener.ThresholdsFor(e.cfg, false),
		barsSince:  sess.Open,
	}
	if !now.Before(bounds.EntryWindowEnd) {
		p.blocked = "entry window closed"
	}
	return p
}

// preMarketPass is the scan for the 04:00-09:30 session, including whether it may buy.
//
// The first-hour sentiment gate has not run and cannot have: its readings are taken
// after the open. So pre-market entry has no kill switch to inherit. Rather than
// trade without one, this takes a live reading of the same basket through the same
// classifier and withholds entry on an overwhelmingly bearish tape — and withholds it
// just as readily when the reading is unavailable, because no answer is not a
// passing answer.
//
// The reading is deliberately not persisted. resolveGate decides the session from the
// stored readings, and a 06:00 sample must not be able to settle the day before the
// market has opened.
func (e *Engine) preMarketPass(ctx context.Context, bounds scheduler.Boundaries) scanPass {
	p := scanPass{
		preMarket:     true,
		thresholds:    screener.ThresholdsFor(e.cfg, true),
		barsSince:     bounds.PreMarketOpen,
		extendedHours: true,
	}
	if !e.cfg.PreMarket.AllowEntry {
		p.blocked = "pre-market entry is disabled"
		return p
	}

	check, err := e.readSentiment(ctx)
	switch {
	case err != nil:
		e.log.Warn("pre-market sentiment unavailable; screening only", "err", err)
		p.blocked = "pre-market sentiment unavailable"
	case check.Classification == domain.VerdictBearish:
		e.log.Info("pre-market sentiment is bearish; screening only",
			"percentages", check.Percentages)
		p.blocked = "pre-market sentiment is overwhelmingly bearish"
	}
	return p
}

// runScan performs one screening pass and, unless the pass is blocked, acts on it.
func (e *Engine) runScan(ctx context.Context, sess scheduler.Session, p scanPass) error {
	now := e.now()

	evals, err := e.screen(ctx, p.thresholds, p.barsSince)
	if err != nil {
		return err
	}

	// Entry runs before the snapshot is stored so the page can show what actually
	// happened to each qualifying candidate, not just that it qualified.
	var outcomes map[string]string
	var entryErr error
	if p.blocked != "" {
		outcomes = make(map[string]string, len(evals))
		for _, ev := range evals {
			if ev.Qualifies {
				outcomes[ev.Symbol] = p.blocked
			}
		}
		e.recordSkip("", p.blocked,
			map[string]any{"reason": p.blocked, "pre_market": p.preMarket})
	} else {
		outcomes, entryErr = e.enterPositions(ctx, sess, evals, p)
	}
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
func (e *Engine) screen(ctx context.Context, th screener.Thresholds, sessionStart time.Time) ([]domain.Evaluation, error) {
	inputs, _, err := e.gatherCandidates(ctx, e.newsSince(), th, sessionStart)
	if err != nil {
		return nil, err
	}
	evals := make([]domain.Evaluation, 0, len(inputs))
	for _, in := range inputs {
		evals = append(evals, screener.Evaluate(in, th))
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
func (e *Engine) gatherCandidates(ctx context.Context, newsSince time.Time, th screener.Thresholds, sessionStart time.Time) ([]screener.Input, int, error) {
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
		volume float64
		dollar float64
	}
	var movers []mover
	var rejected int
	for _, sym := range universe {
		snap, ok := snaps[sym]
		if !ok || snap.IntradayPct < th.MinIntradayPct {
			continue
		}
		// The price band is answerable from the snapshot in either session, so it is
		// applied first and narrows the list before anything is fetched.
		if ok, reason := screener.TradablePrice(snap.Price, th); !ok {
			e.log.Debug("not tradable", "symbol", sym, "reason", reason)
			rejected++
			continue
		}
		volume, dollar := snap.TodayVolume, snap.Price*snap.TodayVolume
		if !th.PreMarket {
			// Regular session: the snapshot already carries volume, so the turnover
			// floor is applied here too and rejects before any per-symbol call.
			if ok, reason := screener.TradableLiquidity(dollar, th); !ok {
				e.log.Debug("not tradable", "symbol", sym, "reason", reason)
				rejected++
				continue
			}
		}
		movers = append(movers, mover{symbol: sym, snap: snap, volume: volume, dollar: dollar})
	}

	// Pre-market the snapshot's volume is not merely stale, it does not exist: no
	// daily bar for today has been created yet, so every symbol reads as zero shares
	// traded. Verified against a live account — see docs/decisions.md. The session's
	// real volume therefore has to be fetched, and it is fetched batched, for exactly
	// the names that cleared the move and the price band. Doing it here rather than
	// during enrichment is what keeps the dollar-volume ranking below meaningful:
	// ranking on zeros would hand the enrichment budget to whichever symbols happened
	// to sort first.
	if th.PreMarket && len(movers) > 0 {
		symbols := make([]string, 0, len(movers))
		for _, m := range movers {
			symbols = append(symbols, m.symbol)
		}
		volumes, err := e.data.SessionVolumes(ctx, symbols, sessionStart)
		if err != nil {
			return nil, 0, fmt.Errorf("pre-market volumes: %w", err)
		}
		kept := movers[:0]
		for _, m := range movers {
			m.volume = volumes[m.symbol]
			m.dollar = m.snap.Price * m.volume
			if ok, reason := screener.TradableLiquidity(m.dollar, th); !ok {
				e.log.Debug("not tradable", "symbol", m.symbol, "reason", reason)
				rejected++
				continue
			}
			kept = append(kept, m)
		}
		movers = kept
	}

	if rejected > 0 {
		e.log.Info("movers rejected by the tradability floors",
			"rejected", rejected, "remaining", len(movers),
			"min_price", th.MinPrice,
			"min_dollar_volume", th.MinDollarVolume,
			"pre_market", th.PreMarket)
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
			// The session's volume, which pre-market came from SessionVolumes rather
			// than the snapshot. This is the numerator of the relative-volume
			// criterion, so taking the snapshot's zero here would fail every
			// pre-market candidate on a criterion it was never measured against.
			TodayVolume: m.volume,
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
func (e *Engine) enterPositions(ctx context.Context, sess scheduler.Session, evals []domain.Evaluation, p scanPass) (map[string]string, error) {
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

		// The setup gate. Screening said this name is interesting; the chart has to
		// say that now is the moment and where the risk sits. This is the only place
		// a per-symbol bar request is made, and it is made only for candidates that
		// already passed all three criteria.
		// The chart starts where the pass's session does, so a pre-market setup is read
		// on pre-market candles rather than on an empty regular session.
		bars, err := e.data.IntradayBars(ctx, cand.Symbol, e.cfg.Entry.PatternInterval, p.barsSince)
		if err != nil {
			e.log.Warn("skipping candidate: bars unavailable", "symbol", cand.Symbol, "err", err)
			outcomes[cand.Symbol] = "bars unavailable"
			e.recordSkip(cand.Symbol, "bars unavailable",
				map[string]any{"reason": "bars unavailable", "error": err.Error()})
			continue
		}
		setup := strategy.FindSetup(bars, e.cfg)
		if !setup.Triggered {
			// Not a failure: most of the time a screened candidate simply has not set
			// up yet. It stays a candidate and is re-examined on the next scan, so
			// this is logged at Debug and de-duplicated in the audit trail.
			e.log.Debug("no setup", "symbol", cand.Symbol, "reason", setup.Reason)
			outcomes[cand.Symbol] = "no setup: " + setup.Reason
			e.recordSkip(cand.Symbol, "no setup",
				map[string]any{"reason": setup.Reason})
			continue
		}
		price := setup.Entry

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
		// The page gets this read for free. It is taken after any earlier candidate in
		// the same pass has been filled, so on a multi-buy pass the balance moves as
		// the cash goes rather than only on the next tick.
		e.publishAccount(acct)

		sizing := risk.SizeForRisk(acct, price, setup.Stop, e.cfg)
		if !sizing.OK {
			e.log.Info("skipping candidate", "symbol", cand.Symbol, "reason", sizing.Reason)
			outcomes[cand.Symbol] = sizing.Reason
			e.recordSkip(cand.Symbol, sizing.Reason,
				map[string]any{"reason": sizing.Reason, "price": price, "cash": acct.Cash})
			continue
		}

		f, err := e.submit(ctx, sess, cand.Symbol, "buy", sizing.Shares, price, p.extendedHours)
		if err != nil {
			e.log.Warn("skipping candidate: order rejected", "symbol", cand.Symbol, "err", err)
			outcomes[cand.Symbol] = "order rejected"
			e.recordSkip(cand.Symbol, "order rejected",
				map[string]any{"reason": "order rejected", "shares": sizing.Shares,
					"price": price, "error": err.Error()})
			continue
		}
		if f.Shares == 0 {
			// Nothing bought, and the remainder is cancelled: not a position.
			e.log.Warn("skipping candidate: order did not fill", "symbol", cand.Symbol,
				"status", f.Status)
			outcomes[cand.Symbol] = "order did not fill"
			e.recordSkip(cand.Symbol, "order did not fill",
				map[string]any{"reason": "order did not fill", "shares": sizing.Shares,
					"price": price, "status": f.Status})
			continue
		}
		// The risk is measured from the fill, not the quote: that is the risk actually
		// taken on, and the target is a multiple of it. A fill at or under the stop has
		// no risk to measure; the stop sells it on the next tick either way.
		entry := f.Price
		riskPerShare := entry - setup.Stop
		if riskPerShare <= 0 {
			riskPerShare = setup.RiskPerShare
		}
		if _, err := e.store.InsertPosition(domain.Position{
			SessionDate: sess.Date, Symbol: cand.Symbol, Shares: f.Shares,
			EntryPrice: entry, EntryTime: e.now(), PeakPrice: entry,
			StopPrice: setup.Stop, InitialRisk: riskPerShare,
		}); err != nil {
			if errors.Is(err, store.ErrDuplicateOpenPosition) {
				e.log.Warn("duplicate position rejected by store", "symbol", cand.Symbol)
				outcomes[cand.Symbol] = "already holding this symbol"
				continue
			}
			return outcomes, err
		}

		e.log.Info("entered position", "symbol", cand.Symbol, "shares", f.Shares,
			"price", entry, "quoted", price, "stop", setup.Stop,
			"risk", riskPerShare*float64(f.Shares), "rel_volume", cand.VolumeMultiple)
		outcomes[cand.Symbol] = fmt.Sprintf("bought %d @ $%.2f, stop $%.2f",
			f.Shares, entry, setup.Stop)
		// Everything needed to reconstruct the decision later: the criteria that were
		// met, the size and why it was that size, and the account state behind it.
		e.record(audit.PositionOpened, cand.Symbol,
			fmt.Sprintf("bought %d shares at $%.2f", f.Shares, entry),
			fillDetail(map[string]any{
				"shares":               f.Shares,
				"price":                entry,
				"dollars":              entry * float64(f.Shares),
				"relative_volume":      cand.VolumeMultiple,
				"criteria":             criteriaDetail(cand),
				"portfolio_value":      acct.PortfolioValue,
				"cash_before":          acct.Cash,
				"risk_per_trade_pct":   e.cfg.Risk.RiskPerTradePct,
				"risk_dollars":         riskPerShare * float64(f.Shares),
				"stop_price":           setup.Stop,
				"stop_distance_pct":    riskPerShare / entry * 100,
				"setup_pause_high":     setup.PauseHigh,
				"setup_pause_bars":     setup.PauseBars,
				"open_positions_after": openCount + 1,
				"pre_market":           p.preMarket,
			}, f, price, sizing.Shares))
		openSymbols[cand.Symbol] = true
		tradedToday[cand.Symbol] = true
		openCount++
	}
	return outcomes, nil
}

// managePositions updates high-water marks and applies the exit rules. When
// forceEOD is set, every open position is closed regardless of the other rules.
func (e *Engine) managePositions(ctx context.Context, sess scheduler.Session, bounds scheduler.Boundaries, forceEOD bool) error {
	// An exit before the bell has to be routed to the pre-market book, or the broker
	// rejects it and the position sits through its stop. Derived from the clock rather
	// than passed in, because every caller would otherwise have to remember it and the
	// one that forgot would be the one holding the loser.
	extendedHours := scheduler.PhaseAt(e.now(), bounds) == domain.PhasePreMarket

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
		if p.Manual {
			// Opened by hand, and governed by two rules rather than none: its own 1R
			// stop, which is what sized it, and the forced exit. The signal rules —
			// backstop, candle trail, scale-out — still do not apply.
			if price > 0 {
				if err := e.store.UpdateMark(p.ID, price, max(p.PeakPrice, price)); err != nil {
					return err
				}
			}
			if err := e.manageManual(ctx, sess, bounds, p, price, forceEOD); err != nil {
				return err
			}
			continue
		}
		if price <= 0 && !forceEOD {
			// Without a price the exit rules cannot be evaluated; skip rather than
			// acting on a zero that would read as a catastrophic loss.
			e.log.Warn("no price for open position", "symbol", p.Symbol)
			continue
		}

		if price > p.PeakPrice {
			p.PeakPrice = price
		}
		if err := e.store.UpdateMark(p.ID, price, p.PeakPrice); err != nil {
			return err
		}

		if !forceEOD {
			p.StopPrice = e.trailStop(ctx, p)
		}

		decision := strategy.EvaluateExit(strategy.ExitInput{
			Position: p, Price: price, EODReached: forceEOD,
		}, e.cfg)

		// A partial sale at the first target. The remainder stays open, which is the
		// whole point: the runner is what pays for the losing trades.
		if decision.Scale {
			if err := e.scaleOut(ctx, sess, p, price, decision, extendedHours); err != nil {
				return err
			}
			continue
		}
		if !decision.Exit {
			continue
		}

		quoted := price
		if quoted <= 0 {
			quoted = p.EntryPrice
		}
		f, err := e.submit(ctx, sess, p.Symbol, "sell", p.SharesOpen, quoted, extendedHours)
		if err != nil {
			return err
		}
		if short, err := e.shortExit(p, f, quoted, decision.Reason); short || err != nil {
			if err != nil {
				return err
			}
			continue
		}
		exitPrice := f.Price
		if err := e.store.ClosePosition(p.ID, exitPrice, e.now(), decision.Reason); err != nil {
			return err
		}
		// Total P&L, which for a scaled position is the banked profit plus the final
		// leg — not the price move from entry to this exit.
		pnl := p.UnrealizedDollars(exitPrice)
		e.log.Info("exited position", "symbol", p.Symbol, "reason", decision.Reason,
			"entry", p.EntryPrice, "exit", exitPrice, "shares", p.SharesOpen,
			"banked", p.BankedDollars, "pnl", fmt.Sprintf("%+.2f", pnl))
		e.record(audit.PositionClosed, p.Symbol,
			fmt.Sprintf("sold %d shares at $%.2f (%s), %+.2f%% on the trade",
				p.SharesOpen, exitPrice, decision.Reason,
				pnl/(p.EntryPrice*float64(p.Shares))*100),
			fillDetail(map[string]any{
				"reason":         string(decision.Reason),
				"shares_sold":    p.SharesOpen,
				"shares_bought":  p.Shares,
				"entry_price":    p.EntryPrice,
				"exit_price":     exitPrice,
				"stop_price":     p.StopPrice,
				"initial_stop":   p.EntryPrice - p.InitialRisk,
				"candle_trail":   e.cfg.Exit.CandleTrail,
				"peak_price":     p.PeakPrice,
				"banked_earlier": p.BankedDollars,
				"pnl_dollars":    pnl,
				"r_multiple":     p.RMultiple(exitPrice),
				"held_for":       e.now().Sub(p.EntryTime).String(),
			}, f, quoted, p.SharesOpen))
	}
	return nil
}

// manageManual applies the two rules a manual position has, and does it around a
// stop that lives at the broker rather than here.
//
// The order of business is fixed. First ask whether the resting stop has already sold
// the position, because everything else is wrong if it has. Then act only where that
// order cannot: the end of the day, which an order has no concept of, and the windows
// where the stop is not actually being enforced. In the ordinary case — regular
// session, order resting, price above the stop — this does nothing but the lookup,
// which is the point: the floor is the broker's to hold.
func (e *Engine) manageManual(ctx context.Context, sess scheduler.Session,
	bounds scheduler.Boundaries, p domain.Position, price float64, forceEOD bool) error {
	if res, err := e.pollProtectiveStop(ctx, &p); err != nil {
		// A lookup that failed says nothing about the order, which is still the
		// broker's floor under the position. Selling on a failed read would be acting
		// on no information at all, so leave it and ask again next tick.
		e.log.Warn("could not read the protective stop; leaving the position alone",
			"symbol", p.Symbol, "err", err)
		return nil
	} else if res.FilledShares > 0 {
		return e.closeOnProtectiveStop(p, res)
	}

	decision := strategy.ExitDecision{}
	switch {
	case forceEOD:
		decision = strategy.EvaluateManualExit(strategy.ExitInput{
			Position: p, Price: price, EODReached: true,
		})
	case e.stopIsUnenforced(p, bounds):
		// No working order, or before the bell where a stop order cannot trigger.
		// The stop is the engine's to hold until that changes.
		decision = strategy.EvaluateManualExit(strategy.ExitInput{Position: p, Price: price})
	}
	if !decision.Exit {
		return nil
	}

	quoted := price
	if quoted <= 0 {
		quoted = p.EntryPrice
	}

	// Off the book before anything is sold. A resting sell order that outlives its
	// holding is a short position, so a failure here stops the exit rather than
	// racing it — and if the stop filled in the meantime, that fill is the exit.
	res, err := e.releaseProtectiveStop(ctx, &p)
	if err != nil {
		e.log.Error("not selling: the protective stop could not be taken off the book",
			"symbol", p.Symbol, "reason", decision.Reason, "err", err)
		if !e.faultedStops[p.ID] {
			if e.faultedStops == nil {
				e.faultedStops = map[int64]bool{}
			}
			e.faultedStops[p.ID] = true
			e.record(audit.Fault, p.Symbol,
				fmt.Sprintf("held off a %s exit for %s: its resting stop order could not be cancelled, "+
					"and selling with that order still working could go short", decision.Reason, p.Symbol),
				map[string]any{
					"reason":          string(decision.Reason),
					"protective_stop": "still working",
					"stop_order_id":   p.StopOrderID,
					"err":             err.Error(),
				})
		}
		return nil
	}
	delete(e.faultedStops, p.ID)
	if res.FilledShares > 0 {
		return e.closeOnProtectiveStop(p, res)
	}

	extendedHours := scheduler.PhaseAt(e.now(), bounds) == domain.PhasePreMarket
	f, err := e.submit(ctx, sess, p.Symbol, "sell", p.SharesOpen, quoted, extendedHours)
	if err != nil {
		return err
	}
	if short, err := e.shortExit(p, f, quoted, decision.Reason); short || err != nil {
		// Whatever is still held now has no resting stop, so the engine keeps its
		// floor — stopIsUnenforced is true with the id cleared — and the exit is
		// tried again next tick.
		return err
	}
	exitPrice := f.Price
	if err := e.store.ClosePosition(p.ID, exitPrice, e.now(), decision.Reason); err != nil {
		return err
	}

	pnl := p.UnrealizedDollars(exitPrice)
	e.log.Info("exited manual position", "symbol", p.Symbol, "reason", decision.Reason,
		"entry", p.EntryPrice, "exit", exitPrice, "shares", p.SharesOpen,
		"pnl", fmt.Sprintf("%+.2f", pnl))
	e.record(audit.PositionClosed, p.Symbol,
		fmt.Sprintf("sold %d shares of a position opened by hand at $%.2f (%s), %+.2f%% on the trade",
			p.SharesOpen, exitPrice, decision.Reason, pnl/(p.EntryPrice*float64(p.Shares))*100),
		fillDetail(map[string]any{
			"manual":          true,
			"reason":          string(decision.Reason),
			"shares_sold":     p.SharesOpen,
			"shares_bought":   p.Shares,
			"entry_price":     p.EntryPrice,
			"exit_price":      exitPrice,
			"stop_price":      p.StopPrice,
			"initial_stop":    p.EntryPrice - p.InitialRisk,
			"protective_stop": "cancelled before selling",
			"peak_price":      p.PeakPrice,
			"pnl_dollars":     pnl,
			"r_multiple":      p.RMultiple(exitPrice),
			"held_for":        e.now().Sub(p.EntryTime).String(),
		}, f, quoted, p.SharesOpen))
	return nil
}

// closeOnProtectiveStop records an exit the broker executed on its own, from the
// resting stop order rather than from anything the daemon did.
//
// The fill price is the broker's, not a mark the daemon read: the whole reason the
// order rests there is that it can fire between ticks, or while this process is not
// running, so the only honest exit price is the one that executed. The reason is
// ExitStopLoss, because that is what happened — the position was stopped out, and
// nothing about it being enforced off-process makes it a different exit.
func (e *Engine) closeOnProtectiveStop(p domain.Position, res broker.OrderResult) error {
	if err := e.store.SetStopOrderID(p.ID, ""); err != nil {
		return err
	}
	price := res.FilledPrice
	if price <= 0 {
		price = p.StopPrice
	}

	// A stop that filled short left shares behind. Bank what sold and leave the rest
	// held with no working order, so the engine's own stop picks it up next tick.
	if res.FilledShares < p.SharesOpen {
		if err := e.store.ReduceShares(p.ID, res.FilledShares, price); err != nil {
			return err
		}
		left := p.SharesOpen - res.FilledShares
		e.log.Warn("protective stop filled short; the rest is still held",
			"symbol", p.Symbol, "sold", res.FilledShares, "held", left)
		e.record(audit.OrderNotFilled, p.Symbol,
			fmt.Sprintf("the resting stop order sold %d of %d shares; %d is still held",
				res.FilledShares, p.SharesOpen, left),
			map[string]any{
				"protective_stop": "filled short",
				"shares_sold":     res.FilledShares,
				"shares_left":     left,
				"fill_price":      price,
				"stop_price":      p.StopPrice,
			})
		return nil
	}

	if err := e.store.ClosePosition(p.ID, price, e.now(), domain.ExitStopLoss); err != nil {
		return err
	}
	pnl := p.UnrealizedDollars(price)
	e.log.Info("protective stop filled at the broker", "symbol", p.Symbol,
		"shares", res.FilledShares, "stop", p.StopPrice, "fill", price,
		"pnl", fmt.Sprintf("%+.2f", pnl))
	e.record(audit.PositionClosed, p.Symbol,
		fmt.Sprintf("the resting stop order sold %d shares at $%.2f, %+.2f%% on the trade",
			res.FilledShares, price, pnl/(p.EntryPrice*float64(p.Shares))*100),
		map[string]any{
			"manual":          true,
			"reason":          string(domain.ExitStopLoss),
			"protective_stop": "filled",
			"shares_sold":     res.FilledShares,
			"shares_bought":   p.Shares,
			"entry_price":     p.EntryPrice,
			"exit_price":      price,
			"stop_price":      p.StopPrice,
			"slippage":        price - p.StopPrice,
			"pnl_dollars":     pnl,
			"r_multiple":      p.RMultiple(price),
			"held_for":        e.now().Sub(p.EntryTime).String(),
			"order_status":    res.Status,
		})
	return nil
}

// shortExit handles an exit order that sold less than the position held, and reports
// whether it did. What sold is banked and the rest stays open, so the exit rule — still
// true on the next tick — sells it then. Closing the whole row on a short fill is how
// the store came to think a position was flat while the broker still held it.
func (e *Engine) shortExit(p domain.Position, f fill, quoted float64, reason domain.ExitReason) (bool, error) {
	if f.Shares >= p.SharesOpen {
		return false, nil
	}
	if f.Shares > 0 {
		if err := e.store.ReduceShares(p.ID, f.Shares, f.Price); err != nil {
			return true, err
		}
	}
	left := p.SharesOpen - f.Shares
	e.log.Warn("exit order filled short; the rest stays held", "symbol", p.Symbol,
		"reason", reason, "sold", f.Shares, "held", left, "status", f.Status)
	e.record(audit.OrderNotFilled, p.Symbol,
		fmt.Sprintf("%s exit sold %d of %d shares; %d still held", reason, f.Shares, p.SharesOpen, left),
		fillDetail(map[string]any{
			"reason":      string(reason),
			"shares_sold": f.Shares,
			"shares_left": left,
			"fill_price":  f.Price,
			"entry_price": p.EntryPrice,
		}, f, quoted, p.SharesOpen))
	return true, nil
}

// trailStop applies the candle trail (exit.candle_trail) to p and returns the stop to
// evaluate it against: raised to the low of the last completed candle when the trail
// applies and that is higher, otherwise unchanged.
//
// A failure to read bars leaves the stop where it was. The trail only ever tightens a
// stop that already protects the position, so missing one candle costs a little
// profit, never the floor under the trade.
func (e *Engine) trailStop(ctx context.Context, p domain.Position) float64 {
	switch e.cfg.Exit.CandleTrail {
	case config.CandleTrailAlways:
	case config.CandleTrailAfterTarget:
		if !p.TargetHit {
			return p.StopPrice
		}
	default:
		return p.StopPrice
	}

	interval := e.cfg.Entry.PatternInterval
	now := e.now()
	// Already read the candle that closed most recently: nothing new until the next
	// one does. A bar published late is simply picked up on a later poll.
	if e.trailedTo[p.ID].Equal(now.Truncate(interval).Add(-interval)) {
		return p.StopPrice
	}
	bars, err := e.data.IntradayBars(ctx, p.Symbol, interval, p.EntryTime.Truncate(interval))
	if err != nil {
		e.log.Warn("candle trail: bars unavailable", "symbol", p.Symbol, "err", err)
		return p.StopPrice
	}
	last, ok := strategy.LastCompletedBar(bars, now, interval)
	if !ok {
		return p.StopPrice
	}
	if e.trailedTo == nil {
		e.trailedTo = map[int64]time.Time{}
	}
	e.trailedTo[p.ID] = last.Time

	lvl := strategy.CandleTrailStop(p, last, e.cfg)
	if lvl <= p.StopPrice {
		return p.StopPrice
	}
	if err := e.store.MoveStop(p.ID, lvl); err != nil {
		e.log.Warn("candle trail: stop not saved", "symbol", p.Symbol, "err", err)
		return p.StopPrice
	}
	// Logged, not audited: it moves once a minute per position. The close event
	// records the stop it sold at alongside the initial one.
	e.log.Debug("candle trail raised stop", "symbol", p.Symbol,
		"from", p.StopPrice, "to", lvl, "candle", last.Time)
	return lvl
}

// scaleOut banks part of a winning position and, when configured, lifts the stop on
// what is left to the entry price.
//
// The sell is submitted before the store is updated, and a store refusal afterwards
// is treated as benign rather than fatal: ErrScaleOutNotApplicable means another tick
// already banked this target, which is exactly what the latch exists to prevent.
func (e *Engine) scaleOut(ctx context.Context, sess scheduler.Session, p domain.Position,
	price float64, decision strategy.ExitDecision, extendedHours bool) error {

	quoted := price
	f, err := e.submit(ctx, sess, p.Symbol, "sell", decision.ScaleShares, quoted, extendedHours)
	if err != nil {
		// One symbol failing to scale must not abort the pass over the others; the
		// position simply stays whole and the target is re-tested next tick.
		e.log.Warn("scale-out order rejected", "symbol", p.Symbol, "err", err)
		return nil
	}
	if f.Shares == 0 {
		e.log.Warn("scale-out order did not fill", "symbol", p.Symbol, "status", f.Status)
		return nil
	}
	// A short fill banks what sold and still latches the target: the runner is
	// larger than planned, which is the side of the error this strategy prefers.
	price = f.Price
	if err := e.store.ScaleOut(p.ID, f.Shares, price, decision.NewStop); err != nil {
		if errors.Is(err, store.ErrScaleOutNotApplicable) {
			e.log.Warn("scale-out already recorded", "symbol", p.Symbol)
			return nil
		}
		return err
	}

	banked := (price - p.EntryPrice) * float64(f.Shares)
	remaining := p.SharesOpen - f.Shares
	e.log.Info("scaled out", "symbol", p.Symbol, "shares", f.Shares,
		"price", price, "remaining", remaining, "banked", fmt.Sprintf("%+.2f", banked),
		"r", fmt.Sprintf("%.2f", p.RMultiple(price)))
	e.record(audit.PositionScaledOut, p.Symbol,
		fmt.Sprintf("sold %d of %d shares at $%.2f (%.1fR), %d left running",
			f.Shares, p.SharesOpen, price, p.RMultiple(price), remaining),
		fillDetail(map[string]any{
			"reason":          string(domain.ExitScaleOut),
			"shares_sold":     f.Shares,
			"shares_left":     remaining,
			"price":           price,
			"entry_price":     p.EntryPrice,
			"banked_dollars":  banked,
			"r_multiple":      p.RMultiple(price),
			"stop_moved_to":   decision.NewStop,
			"initial_risk_ps": p.InitialRisk,
		}, f, quoted, decision.ScaleShares))
	return nil
}

// fill is what an order actually executed, as the broker reported it after the fact.
// Positions are recorded from this, not from the price the daemon read when it decided
// to trade: on 2026-09-29 those differed on 11 of 15 trades, and the quoted prices put
// the day $90 better than it was.
type fill struct {
	Price  float64
	Shares int
	Status string
	// Confirmed is false when the broker never said how the order ended — lookups
	// failing, or shutdown interrupting the wait. Price and Shares are then the
	// quote and the full order, which is what the daemon assumed before it read
	// fills at all; the order is left for Reconcile, and the audit says so.
	Confirmed bool
}

// submit records an order's intent before sending it, so a crash between the two
// leaves something for Reconcile to find, then waits for the broker to report what
// executed.
//
// An order still working after fillWait has its remainder cancelled rather than left
// on the book: a pre-market limit that has not filled in ten seconds has been passed
// by, and a remainder filling later would hold shares the store does not know about.
// The result can therefore be a partial fill or none at all, and every caller has to
// handle both.
func (e *Engine) submit(ctx context.Context, sess scheduler.Session, symbol, side string, shares int, price float64, extendedHours bool) (fill, error) {
	clientOrderID := fmt.Sprintf("%s-%s-%s-%d", sess.Date, symbol, side, e.now().UnixNano())

	rec := store.OrderRecord{
		ClientOrderID: clientOrderID, SessionDate: sess.Date, Symbol: symbol,
		Side: side, Shares: shares, SubmittedAt: e.now(), Status: "submitted",
	}
	if err := e.store.RecordOrder(rec); err != nil {
		return fill{}, err
	}

	orderType, slipPct := e.cfg.Execution.OrderType, e.cfg.Execution.LimitSlipPct
	if extendedHours {
		// Not a preference. Alpaca accepts only a day limit order for the extended
		// session, so execution.order_type does not apply before the bell — it keeps
		// governing the regular session, and pre-market sends a limit order whatever
		// it says. Coupling the two would have made enabling pre-market entry change
		// how the rest of the day trades.
		orderType = "limit"
		if e.cfg.PreMarket.LimitSlipPct > 0 {
			slipPct = e.cfg.PreMarket.LimitSlipPct
		}
	}

	req := broker.OrderRequest{
		Symbol: symbol, Shares: shares, Side: side,
		Type: orderType, ClientOrderID: clientOrderID,
		ExtendedHours: extendedHours,
	}
	if req.Type == "limit" {
		slip := 1 + slipPct/100
		if side == "sell" {
			slip = 1 - slipPct/100
		}
		req.LimitPrice = price * slip
	}

	res, err := e.trading.PlaceOrder(ctx, req)
	if err != nil {
		if updateErr := e.store.UpdateOrderStatus(clientOrderID, "failed", ""); updateErr != nil {
			e.log.Error("failed to record order failure", "err", updateErr)
		}
		return fill{}, fmt.Errorf("place %s order for %s: %w", side, symbol, err)
	}
	if err := e.store.UpdateOrderStatus(clientOrderID, res.Status, res.BrokerOrderID); err != nil {
		return fill{}, err
	}

	res, confirmed := e.awaitFill(ctx, res)
	status := res.Status
	f := fill{Price: res.FilledPrice, Shares: res.FilledShares, Status: res.Status, Confirmed: confirmed}
	if !confirmed {
		e.log.Warn("order outcome unknown; assuming it filled as sent", "symbol", symbol,
			"side", side, "shares", shares, "last_status", res.Status)
		f.Price, f.Shares = price, shares
		// Not 'submitted': the broker did acknowledge it. Reconcile picks both up.
		status = "unconfirmed"
	}
	if f.Shares > 0 && f.Price <= 0 {
		f.Price = price
	}
	if err := e.store.RecordFill(clientOrderID, status, res.BrokerOrderID,
		res.FilledPrice, res.FilledShares); err != nil {
		return fill{}, err
	}
	return f, nil
}

// awaitFill polls a placed order until it can no longer fill, cancelling whatever is
// still working once fillWait has passed. It reports false only when the broker never
// gave a final answer.
//
// The wait runs on real timers, not e.now: it is waiting on the broker, not deciding
// anything about the session.
func (e *Engine) awaitFill(ctx context.Context, res broker.OrderResult) (broker.OrderResult, bool) {
	if res.Done() {
		return res, true
	}
	res = e.pollOrder(ctx, res)
	if res.Done() {
		return res, true
	}
	if err := e.trading.CancelOrder(ctx, res.BrokerOrderID); err != nil {
		// Commonly because it filled between the last poll and the cancel; the
		// lookups below find out either way.
		e.log.Warn("cancel of unfilled remainder failed", "order", res.BrokerOrderID, "err", err)
	}
	res = e.pollOrder(ctx, res)
	return res, res.Done()
}

// pollOrder asks the broker for an order's state every fillPoll until it is done or
// fillWait has passed, and returns the last state it was told.
func (e *Engine) pollOrder(ctx context.Context, res broker.OrderResult) broker.OrderResult {
	wait := time.NewTimer(e.fillWait)
	defer wait.Stop()
	poll := time.NewTicker(e.fillPoll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return res
		case <-wait.C:
			return res
		case <-poll.C:
			got, err := e.trading.Order(ctx, res.BrokerOrderID)
			if err != nil {
				e.log.Warn("order lookup failed", "order", res.BrokerOrderID, "err", err)
				continue
			}
			res = got
			if res.Done() {
				return res
			}
		}
	}
}

// fillDetail adds what the broker executed to an audit event, next to what was
// asked for, so slippage and short fills can be read straight off the trail.
func fillDetail(d map[string]any, f fill, quoted float64, ordered int) map[string]any {
	d["quoted_price"] = quoted
	d["shares_ordered"] = ordered
	d["order_status"] = f.Status
	d["fill_confirmed"] = f.Confirmed
	return d
}
