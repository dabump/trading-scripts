// Package engine drives the daily loop from docs/architecture.md: sentiment
// gate, screening, position monitoring, and the forced end-of-day exit.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
	Floats  broker.FloatProvider
	Logger  *slog.Logger
	// Now is injectable so a test can drive a whole trading day deterministically.
	Now func() time.Time
}

type Engine struct {
	cfg     *config.Config
	store   *store.Store
	data    broker.MarketData
	trading broker.Trading
	floats  broker.FloatProvider
	log     *slog.Logger
	now     func() time.Time

	mu           sync.RWMutex
	state        domain.AgentState
	lastError    string
	session      scheduler.Session
	sessionKnown bool
	lastScan     time.Time
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
	floats := d.Floats
	if floats == nil {
		floats = broker.NoFloatProvider{}
	}
	return &Engine{
		cfg: d.Config, store: d.Store, data: d.Data, trading: d.Trading,
		floats: floats, log: logger, now: now, state: domain.StateMarketClosed,
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
	}
}

// fail puts the agent into ERROR, which docs/web-ui.md requires be shown
// distinctly from a deliberate bearish halt.
func (e *Engine) fail(op string, err error) {
	e.log.Error("engine error", "op", op, "err", err)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = domain.StateError
	e.lastError = fmt.Sprintf("%s: %v", op, err)
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
		e.log.Warn("no sentiment readings for this session; halting for the day",
			"session", sess.Date)
		if err := e.store.SetVerdict(sess.Date, domain.VerdictBearish,
			"no sentiment readings were taken during the first hour"); err != nil {
			return false, err
		}
		return true, nil
	}

	verdict := sentiment.GateVerdict(readings, e.cfg)
	reason := ""
	if verdict == domain.VerdictBearish {
		reason = fmt.Sprintf("first-hour sentiment %v", readings[len(readings)-1].Percentages)
	}
	e.log.Info("sentiment gate resolved", "session", sess.Date, "verdict", verdict)
	if err := e.store.SetVerdict(sess.Date, verdict, reason); err != nil {
		return false, err
	}
	return verdict == domain.VerdictBearish, nil
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
	if err := e.store.SaveScreenSnapshot(sess.Date, now, evals); err != nil {
		return err
	}
	return e.enterPositions(ctx, sess, evals)
}

// screen evaluates the candidate universe against the four entry criteria.
//
// The move criterion is checked first from the movers snapshot, and only symbols
// that clear it get the per-symbol float, news and average-volume lookups. At a
// one-minute scan cadence, enriching every mover would multiply API calls for
// names that are already disqualified on the cheapest criterion.
func (e *Engine) screen(ctx context.Context, sess scheduler.Session) ([]domain.Evaluation, error) {
	movers, err := e.data.Movers(ctx, e.cfg.Screening.UniverseSize)
	if err != nil {
		return nil, fmt.Errorf("movers: %w", err)
	}
	if len(movers) == 0 {
		return nil, nil
	}

	symbols := make([]string, 0, len(movers))
	for _, m := range movers {
		symbols = append(symbols, m.Symbol)
	}
	snaps, err := e.data.Snapshots(ctx, symbols)
	if err != nil {
		return nil, fmt.Errorf("snapshots: %w", err)
	}

	var shortlist []string
	for _, sym := range symbols {
		if snaps[sym].IntradayPct >= e.cfg.Screening.MinIntradayPct {
			shortlist = append(shortlist, sym)
		}
	}

	news := map[string]int{}
	if len(shortlist) > 0 {
		news, err = e.data.NewsCounts(ctx, shortlist, sess.Open)
		if err != nil {
			return nil, fmt.Errorf("news: %w", err)
		}
	}

	evals := make([]domain.Evaluation, 0, len(symbols))
	onShortlist := make(map[string]bool, len(shortlist))
	for _, s := range shortlist {
		onShortlist[s] = true
	}

	for _, sym := range symbols {
		snap := snaps[sym]
		in := screener.Input{
			Symbol:      sym,
			Price:       snap.Price,
			IntradayPct: snap.IntradayPct,
			TodayVolume: snap.TodayVolume,
		}
		if onShortlist[sym] {
			if avg, err := e.data.AverageDailyVolume(ctx, sym, e.cfg.Screening.AvgVolumeLookbackDays); err != nil {
				e.log.Warn("average volume unavailable", "symbol", sym, "err", err)
			} else {
				in.AvgVolume = avg
			}
			if shares, ok, err := e.floats.FloatShares(ctx, sym); err != nil {
				e.log.Warn("float unavailable", "symbol", sym, "err", err)
			} else {
				in.FloatShares, in.FloatKnown = shares, ok
			}
			in.NewsCount = news[sym]
		}
		evals = append(evals, screener.Evaluate(in, e.cfg))
	}
	return evals, nil
}

// enterPositions buys the strongest qualifying candidates that fit under the
// exposure cap.
func (e *Engine) enterPositions(ctx context.Context, sess scheduler.Session, evals []domain.Evaluation) error {
	qualifying := screener.Qualifying(evals)
	if len(qualifying) == 0 {
		return nil
	}

	open, err := e.store.OpenPositions()
	if err != nil {
		return err
	}
	tradedToday, err := e.store.SymbolsTradedOn(sess.Date)
	if err != nil {
		return err
	}
	openSymbols := make(map[string]bool, len(open))
	for _, p := range open {
		openSymbols[p.Symbol] = true
	}
	openCount := len(open)

	for _, cand := range qualifying {
		if !risk.CanOpen(openCount, e.cfg) {
			return nil
		}
		if allowed, reason, routine := risk.AllowEntry(cand.Symbol, openSymbols, tradedToday, openCount, e.cfg); !allowed {
			if routine {
				e.log.Debug("skipping candidate", "symbol", cand.Symbol, "reason", reason)
			} else {
				e.log.Info("skipping candidate", "symbol", cand.Symbol, "reason", reason)
			}
			continue
		}

		snaps, err := e.data.Snapshots(ctx, []string{cand.Symbol})
		if err != nil {
			return fmt.Errorf("price for %s: %w", cand.Symbol, err)
		}
		price := snaps[cand.Symbol].Price
		acct, err := e.trading.Account(ctx)
		if err != nil {
			return fmt.Errorf("account: %w", err)
		}
		sizing := risk.Size(acct, price, e.cfg)
		if !sizing.OK {
			e.log.Info("skipping candidate", "symbol", cand.Symbol, "reason", sizing.Reason)
			continue
		}

		if err := e.submit(ctx, sess, cand.Symbol, "buy", sizing.Shares, price); err != nil {
			return err
		}
		if _, err := e.store.InsertPosition(domain.Position{
			SessionDate: sess.Date, Symbol: cand.Symbol, Shares: sizing.Shares,
			EntryPrice: price, EntryTime: e.now(), PeakPrice: price,
		}); err != nil {
			if errors.Is(err, store.ErrDuplicateOpenPosition) {
				e.log.Warn("duplicate position rejected by store", "symbol", cand.Symbol)
				continue
			}
			return err
		}

		e.log.Info("entered position", "symbol", cand.Symbol, "shares", sizing.Shares,
			"price", price, "rel_volume", cand.VolumeMultiple)
		openSymbols[cand.Symbol] = true
		tradedToday[cand.Symbol] = true
		openCount++
	}
	return nil
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
