package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/risk"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/screener"
	"github.com/martincoetzee/trading-agent/internal/sentiment"
	"github.com/martincoetzee/trading-agent/internal/strategy"
)

// ErrBusy is returned when a manual action is already running. Both actions fan
// out to several API calls, so letting clicks queue up would turn a impatient
// user into a rate-limit problem.
var ErrBusy = errors.New("that check is already running")

// manualGuards serialises each manual action independently of the other.
type manualGuards struct {
	sentiment sync.Mutex
	screen    sync.Mutex

	// opening holds the symbols with an open in flight, for the same reason as
	// closing: a double-clicked button must not submit two buy orders.
	opening map[string]bool
	// closing holds the position ids with a close in flight. Keyed per position
	// rather than a single lock so two positions can be closed at once, while a
	// double-clicked button cannot submit two sell orders for the same one.
	mu      sync.Mutex
	closing map[int64]bool
}

func (g *manualGuards) claimClose(id int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing[id] {
		return false
	}
	if g.closing == nil {
		g.closing = map[int64]bool{}
	}
	g.closing[id] = true
	return true
}

func (g *manualGuards) releaseClose(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.closing, id)
}

func (g *manualGuards) claimOpen(symbol string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.opening[symbol] {
		return false
	}
	if g.opening == nil {
		g.opening = map[string]bool{}
	}
	g.opening[symbol] = true
	return true
}

func (g *manualGuards) releaseOpen(symbol string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.opening, symbol)
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

	check, err := e.readSentiment(ctx)
	if err != nil {
		return domain.SentimentCheck{}, err
	}
	e.log.Info("manual sentiment check", "classification", check.Classification,
		"percentages", check.Percentages, "persisted", false)
	return check, nil
}

// readSentiment classifies the basket as it stands right now, without storing
// anything.
//
// Two callers share it and both depend on it not persisting: the page's button,
// which must not be able to overturn the session's gate with a click, and the
// pre-market pass, which stands this in for a gate that has not run yet. Storing
// either would change what resolveGate judges the session on.
func (e *Engine) readSentiment(ctx context.Context) (domain.SentimentCheck, error) {
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

	return domain.SentimentCheck{
		TakenAt:        e.now(),
		Percentages:    pcts,
		Classification: sentiment.Classify(pcts, e.cfg),
		Missing:        missing,
	}, nil
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

	// The button reports what the agent would find at this moment, so it has to judge
	// by the numbers that are in force at this moment: pre-market thresholds while
	// pre-market, regular ones otherwise. Deriving it from the phase rather than a
	// separate rule keeps the preview honest about which screen it just ran — the
	// whole promise of this path is that a row it calls qualifying is one the
	// automated scan would act on.
	bounds := scheduler.Bounds(sess, e.cfg)
	preMarket := tradingDay && scheduler.PhaseAt(now, bounds) == domain.PhasePreMarket
	th := screener.ThresholdsFor(e.cfg, preMarket)

	// The same catalyst window as the automated scan, which is relative to now and
	// therefore works just as well with the exchange closed. The session start is
	// where a pre-market pass sums its volume from, and is unused otherwise.
	inputs, universe, err := e.gatherCandidates(ctx, e.newsSince(), th, bounds.PreMarketOpen)
	if err != nil {
		return domain.ScreenPreview{}, err
	}

	evals := make([]domain.Evaluation, 0, len(inputs))
	for _, in := range inputs {
		evals = append(evals, screener.Evaluate(in, th))
	}

	marketOpen := tradingDay && !now.Before(sess.Open) && now.Before(sess.Close)
	preview := domain.ScreenPreview{
		TakenAt:      now,
		UniverseSize: universe,
		Evaluations:  screener.Rank(evals),
		MarketOpen:   marketOpen,
	}
	e.log.Info("manual screening pass", "universe_scanned", universe,
		"evaluated", len(evals), "market_open", marketOpen, "pre_market", preMarket,
		"orders_placed", 0)
	return preview, nil
}

// ErrPositionNotOpen means the position the page asked to close is no longer open —
// the trading loop exited it, or the button was pressed twice.
var ErrPositionNotOpen = errors.New("that position is no longer open")

// ErrNotFilled means the broker executed less of a manual order than was asked for
// within the fill wait, and the remainder was cancelled.
var ErrNotFilled = errors.New("the order did not fill")

// ErrExchangeClosed means there is no session to sell into right now.
var ErrExchangeClosed = errors.New(
	"the exchange is closed, so a sell cannot be filled now — the order would sit until the next open " +
		"while the agent recorded the position as closed")

// ClosePosition sells whatever is still held in one position, on an operator's
// instruction from the status page.
//
// This is the one action on the page that trades, and it is deliberately the only
// one. The two read-only buttons stop short of the entry path precisely so they can
// never place an order; this one exists because the opposite need is real — an
// operator watching a position go wrong should not have to kill the daemon or open
// the broker's own UI to get out of it.
//
// It goes through the same submit-then-record path as a strategy exit rather than a
// shortcut of its own, so the order record, the audit trail and the realised P&L are
// built the same way. The exit reason is ExitManual, kept distinct so nothing later
// reads this as a rule having fired.
//
// It refuses when the exchange is shut. A sell submitted then would be queued to the
// next open while the store had already marked the position closed, and the two would
// disagree until the next restart reconciled them — the page reporting a flat book
// that the broker does not have is worse than refusing.
func (e *Engine) ClosePosition(ctx context.Context, id int64) (domain.Position, error) {
	if !e.manual.claimClose(id) {
		return domain.Position{}, ErrBusy
	}
	defer e.manual.releaseClose(id)

	// Read the row rather than trusting the id the browser sent: the page it came
	// from may be a poll interval old and describe a position the loop has since
	// exited.
	pos, err := e.store.PositionByID(id)
	if err != nil {
		return domain.Position{}, err
	}
	if !pos.Open || pos.SharesOpen <= 0 {
		return domain.Position{}, ErrPositionNotOpen
	}

	sess, tradingDay := e.Session()
	if !tradingDay {
		return domain.Position{}, ErrExchangeClosed
	}
	now := e.now()
	phase := scheduler.PhaseAt(now, scheduler.Bounds(sess, e.cfg))
	if phase == domain.PhaseClosed {
		return domain.Position{}, ErrExchangeClosed
	}
	extendedHours := phase == domain.PhasePreMarket

	// A fresh mark, because this one is being priced into an order. Falling back to
	// the stored mark and then the entry price keeps a data blip from stranding an
	// operator who is trying to get out.
	price := pos.LastPrice
	if snaps, err := e.data.Snapshots(ctx, []string{pos.Symbol}); err == nil {
		if snap, ok := snaps[pos.Symbol]; ok && snap.Price > 0 {
			price = snap.Price
		}
	}
	if price <= 0 {
		price = pos.EntryPrice
	}

	quoted := price
	f, err := e.submit(ctx, sess, pos.Symbol, "sell", pos.SharesOpen, quoted, extendedHours)
	if err != nil {
		return domain.Position{}, err
	}
	if short, err := e.shortExit(pos, f, quoted, domain.ExitManual); short || err != nil {
		if err != nil {
			return domain.Position{}, err
		}
		return domain.Position{}, fmt.Errorf("%w: sold %d of %d shares, and the rest is still held",
			ErrNotFilled, f.Shares, pos.SharesOpen)
	}
	price = f.Price
	if err := e.store.ClosePosition(pos.ID, price, now, domain.ExitManual); err != nil {
		return domain.Position{}, err
	}

	pnl := pos.UnrealizedDollars(price)
	e.log.Info("position closed manually", "symbol", pos.Symbol, "shares", pos.SharesOpen,
		"price", price, "pnl", fmt.Sprintf("%+.2f", pnl))
	e.record(audit.PositionClosed, pos.Symbol,
		fmt.Sprintf("closed by hand from the status page: sold %d shares at $%.2f", pos.SharesOpen, price),
		fillDetail(map[string]any{
			"reason":         string(domain.ExitManual),
			"shares_sold":    pos.SharesOpen,
			"shares_bought":  pos.Shares,
			"entry_price":    pos.EntryPrice,
			"exit_price":     price,
			"stop_price":     pos.StopPrice,
			"banked_earlier": pos.BankedDollars,
			"pnl_dollars":    pnl,
			"r_multiple":     pos.RMultiple(price),
			"held_for":       now.Sub(pos.EntryTime).String(),
			"pre_market":     extendedHours,
		}, f, quoted, pos.SharesOpen))

	// The closed row, for the confirmation the page shows.
	closed, err := e.store.PositionByID(pos.ID)
	if err != nil {
		return domain.Position{}, err
	}
	return closed, nil
}

// ErrAlreadyHeld and friends explain a refused manual open in terms the page can
// show without translation.
var (
	ErrCannotEnter = errors.New("entry is not allowed right now")
)

// OpenPosition buys a screened candidate on an operator's instruction, overriding
// the setup gate.
//
// This is the counterpart to ClosePosition and it is a bigger departure: the setup
// gate is the strategy's single most load-bearing rule, and the whole measured
// difference between this version and the one with no edge is that it refuses to buy
// without a pullback to size the risk from. Clicking this button is a decision to
// skip that. It is supported because a human watching a chart can read a pattern the
// mechanical detector cannot — see the KNRX case in docs/decisions.md — not because
// the gate is thought to be wrong.
//
// Everything that is not the signal still applies: the position cap, one position per
// symbol, the same-day re-entry rule, and sizing from a stop. The kill switch is the
// exception — an explicit instruction about one named symbol is not the thing it
// exists to stop — but the override is recorded.
//
// The stop is the part worth understanding. If the chart happens to show a completed
// setup, its stop is used and the result is identical to what the automated path
// would have done. If it does not, the stop is placed at entry.max_stop_distance_pct
// below the entry — the widest risk this strategy will accept — which keeps
// risk.SizeForRisk in charge of the share count and makes the position the smallest
// the risk budget allows rather than an arbitrary fraction of the account.
func (e *Engine) OpenPosition(ctx context.Context, symbol string) (domain.ManualOpen, error) {
	if !e.manual.claimOpen(symbol) {
		return domain.ManualOpen{}, ErrBusy
	}
	defer e.manual.releaseOpen(symbol)

	sess, tradingDay := e.Session()
	if !tradingDay {
		return domain.ManualOpen{}, ErrExchangeClosed
	}
	now := e.now()
	bounds := scheduler.Bounds(sess, e.cfg)
	phase := scheduler.PhaseAt(now, bounds)
	switch phase {
	case domain.PhaseClosed:
		return domain.ManualOpen{}, ErrExchangeClosed
	case domain.PhaseEODWindow:
		// Buying inside the forced-exit window means buying something the agent is
		// about to be required to sell, which is a guaranteed round trip across the
		// spread and nothing else.
		return domain.ManualOpen{}, fmt.Errorf(
			"%w: the end-of-day window has started, and anything bought now would be force-sold within minutes",
			ErrCannotEnter)
	}
	extendedHours := phase == domain.PhasePreMarket

	// The risk rules that are not about the signal still hold.
	open, err := e.store.OpenPositions()
	if err != nil {
		return domain.ManualOpen{}, err
	}
	openSymbols := make(map[string]bool, len(open))
	for _, p := range open {
		openSymbols[p.Symbol] = true
	}
	tradedToday, err := e.store.SymbolsTradedOn(sess.Date)
	if err != nil {
		return domain.ManualOpen{}, err
	}
	if allowed, reason, _ := risk.AllowEntry(symbol, openSymbols, tradedToday, len(open), e.cfg); !allowed {
		return domain.ManualOpen{}, fmt.Errorf("%w: %s", ErrCannotEnter, reason)
	}

	snaps, err := e.data.Snapshots(ctx, []string{symbol})
	if err != nil {
		return domain.ManualOpen{}, fmt.Errorf("price for %s: %w", symbol, err)
	}
	snap, ok := snaps[symbol]
	if !ok || snap.Price <= 0 {
		return domain.ManualOpen{}, fmt.Errorf("no price available for %s", symbol)
	}

	res := domain.ManualOpen{Entry: snap.Price, ScreenReason: "not on the latest screen"}
	// Recorded rather than required: the page offers Open on failing rows too, and the
	// trail should say which kind of override this was.
	if evals, _, err := e.store.LatestScreenSnapshot(sess.Date); err == nil {
		for _, ev := range evals {
			if ev.Symbol == symbol {
				res.Qualified, res.ScreenReason = ev.Qualifies, ev.FailReason
				break
			}
		}
	}

	// Prefer the chart's own stop when the pattern is actually there: an operator who
	// clicks a fraction early should still get the stop the strategy would have used.
	barsSince := sess.Open
	if extendedHours {
		barsSince = bounds.PreMarketOpen
	}
	if bars, err := e.data.IntradayBars(ctx, symbol, e.cfg.Entry.PatternInterval, barsSince); err == nil {
		if setup := strategy.FindSetup(bars, e.cfg); setup.Triggered {
			res.Entry, res.Stop, res.FromSetup = setup.Entry, setup.Stop, true
		} else {
			res.SetupReason = setup.Reason
		}
	} else {
		res.SetupReason = "bars unavailable: " + err.Error()
	}
	if !res.FromSetup {
		res.Stop = res.Entry * (1 - e.cfg.Entry.MaxStopDistancePct/100)
	}

	acct, err := e.trading.Account(ctx)
	if err != nil {
		return domain.ManualOpen{}, fmt.Errorf("account: %w", err)
	}
	e.publishAccount(acct)

	sizing := risk.SizeForRisk(acct, res.Entry, res.Stop, e.cfg)
	if !sizing.OK {
		return domain.ManualOpen{}, fmt.Errorf("%w: %s", ErrCannotEnter, sizing.Reason)
	}
	res.Shares, res.RiskDollar = sizing.Shares, sizing.RiskDollar

	if rec, err := e.store.Session(sess.Date); err == nil {
		res.Halted = rec.Halted
	}

	quoted := res.Entry
	f, err := e.submit(ctx, sess, symbol, "buy", sizing.Shares, quoted, extendedHours)
	if err != nil {
		return domain.ManualOpen{}, err
	}
	if f.Shares == 0 {
		return domain.ManualOpen{}, fmt.Errorf("%w: nothing was bought, and the order was cancelled", ErrNotFilled)
	}
	// What was bought, at the price it was bought at; the stop stays where it was set,
	// so the risk is re-measured from the fill as the automated entry does.
	res.Entry, res.Shares = f.Price, f.Shares
	riskPerShare := res.Entry - res.Stop
	if riskPerShare <= 0 {
		riskPerShare = quoted - res.Stop
	}
	res.RiskDollar = riskPerShare * float64(res.Shares)
	id, err := e.store.InsertPosition(domain.Position{
		SessionDate: sess.Date, Symbol: symbol, Shares: res.Shares,
		EntryPrice: res.Entry, EntryTime: now, PeakPrice: res.Entry,
		StopPrice: res.Stop, InitialRisk: riskPerShare, Manual: true,
	})
	if err != nil {
		return domain.ManualOpen{}, err
	}

	e.log.Info("position opened by hand", "symbol", symbol, "shares", res.Shares,
		"price", res.Entry, "quoted", quoted, "stop", res.Stop, "from_setup", res.FromSetup,
		"risk", res.RiskDollar, "halted", res.Halted)
	e.record(audit.PositionOpened, symbol,
		fmt.Sprintf("opened by hand from the status page: bought %d shares at $%.2f",
			res.Shares, res.Entry),
		fillDetail(map[string]any{
			"manual":             true,
			"shares":             res.Shares,
			"price":              res.Entry,
			"dollars":            res.Entry * float64(res.Shares),
			"stop_price":         res.Stop,
			"stop_from_setup":    res.FromSetup,
			"setup_reason":       res.SetupReason,
			"risk_dollars":       res.RiskDollar,
			"risk_per_trade_pct": e.cfg.Risk.RiskPerTradePct,
			"portfolio_value":    acct.PortfolioValue,
			"cash_before":        acct.Cash,
			"overrode_halt":      res.Halted,
			"pre_market":         extendedHours,
			"exit_rules":         "none: closed only by hand",
			"screen_qualified":   res.Qualified,
			"screen_reason":      res.ScreenReason,
		}, f, quoted, sizing.Shares))

	stored, err := e.store.PositionByID(id)
	if err != nil {
		return domain.ManualOpen{}, err
	}
	res.Position = stored
	return res, nil
}

// ErrNotHalted and ErrHaltWasJudged explain a refused override.
var (
	ErrNotHalted = errors.New("this session is not halted")
	// ErrHaltWasJudged marks the case this button deliberately does not cover: a halt
	// the gate reached on real readings.
	ErrHaltWasJudged = errors.New(
		"this halt came from actual sentiment readings, not from missing ones — it is the kill " +
			"switch working, and it is not dismissed from here")
)

// IgnoreHalt dismisses a halt that exists only because the gate had nothing to judge.
//
// resolveGate halts the day when no first-hour readings exist, which happens whenever
// the daemon is started after the first hour. That is the right default — trading
// without the safety check ever having run is worse than sitting out — but it is not a
// risk decision, it is an absence of data, and a restart at 11:00 should not
// automatically cost the rest of the day.
//
// It deliberately does not dismiss a halt the gate actually reached. A bearish verdict
// on real readings is the kill switch doing its job; overriding that is a different
// decision with different consequences, and it is not this button. The two are told
// apart structurally — by whether any readings exist — rather than by matching the
// halt's wording, which would break the moment the message was reworded.
//
// The verdict becomes GATE_OVERRIDDEN rather than PROCEED, so neither the page nor the
// trail can later claim the gate passed. It is persisted, so a further restart does not
// re-halt the day.
func (e *Engine) IgnoreHalt(ctx context.Context) error {
	sess, tradingDay := e.Session()
	if !tradingDay {
		return ErrNotHalted
	}

	rec, err := e.store.Session(sess.Date)
	if err != nil {
		return err
	}
	if !rec.Halted {
		return ErrNotHalted
	}
	readings, err := e.store.SentimentReadings(sess.Date)
	if err != nil {
		return err
	}
	if len(readings) > 0 {
		return ErrHaltWasJudged
	}

	if err := e.store.SetVerdict(sess.Date, domain.VerdictOverridden, ""); err != nil {
		return err
	}
	e.log.Warn("session halt dismissed by hand; screening resumes without a sentiment gate",
		"session", sess.Date, "previous_reason", rec.HaltReason)
	e.record(audit.GateResolved, "",
		"halt dismissed by hand: no first-hour readings existed, so the gate had nothing to judge",
		map[string]any{
			"verdict":         string(domain.VerdictOverridden),
			"manual":          true,
			"readings":        0,
			"previous_reason": rec.HaltReason,
		})
	return nil
}
