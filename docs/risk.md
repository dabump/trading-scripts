# Risk management

**Status:** implemented. These numbers were explicitly committed to during the grill-me interview — treat them as settled, not placeholders.

## Position sizing

- **Sizing is from risk, not from portfolio fraction.** Every trade risks `risk.risk_per_trade_pct` (1%) of the account: `shares = (portfolio value × 1%) ÷ (entry − stop)`. The stop comes from the chart (see [`strategy.md`](./strategy.md) §3), so risk per share is known before the size is chosen.

  This replaced a fixed 10%-of-portfolio allocation, then a 5% one. The problem with both is that dollar risk then swings with the stock's volatility: a trade whose stop is 4% away loses eight times as much as one whose stop is 0.5% away, so the account's worst days are decided by which setups happened to be wide rather than by any deliberate choice.

- **Two caps bound the size:** `risk.max_position_pct` (33%) on one position's notional, and available cash. The notional cap does bind on tight stops, and when it does the trade risks *less* than 1% — conservative, and visible in the audit trail's `risk_dollars`. The cap must stay above `risk_per_trade_pct ÷ entry.max_stop_distance_pct`, or it binds on every trade and sizing silently reverts to a fixed fraction of the account. Config validation does not check that ratio; a test in `internal/config` does.

- **Concurrency is 3, not 5.** The approach being followed runs a small number of positions at a time rather than a basket. Three at 33% keeps maximum exposure just under fully invested, and total simultaneous risk at 3%.
- The size is clamped to available cash. Sizing is computed from equity, which includes the market value of positions already held, so on a heavily deployed account the share count the risk budget allows can still cost more than the cash left to spend; submitting an order the account cannot fund just earns a broker rejection mid-session.
- If the budget cannot buy a single share, the candidate is skipped with a logged reason rather than rounded up.
- If a qualifying candidate appears while `risk.max_concurrent_positions` are already open, it is skipped — there is no queueing, and nothing already held is displaced to make room.

## Per-trade stop-loss

- **The working stop is the chart stop**, from the setup's pullback low, capped at `entry.max_stop_distance_pct` (4%) away. A setup whose stop is wider is refused rather than sized around.
- **`risk.stop_loss_pct` (10%) is now a gap backstop**, not the working stop. It is only reachable when price jumps straight through the chart stop. Config validation requires it to be wider than `entry.max_stop_distance_pct`; otherwise it would fire first and the chart stop would never be reached, silently reverting the strategy to a fixed-percentage stop.
- A price sitting exactly on either threshold counts as a hit (`strategy.priceEpsilon`).
- **Once the first target is banked the stop moves to the entry price**, so a winner cannot become a loser.
- This is a hard floor, checked independently of — and ahead of — the profit target, so a bug or gap in the target/scale-out logic cannot turn into an unbounded loss on a single position. `strategy.EvaluateExit` checks the forced end-of-day exit, then the stop, then the target, in that order.

## Sentiment kill switch (live and reversible)

**Status: changed 2026-10-17.** This used to be a one-time decision made from a
short window of readings right after the open, after which a bearish verdict halted
the rest of the session outright. It is now checked continuously, every
`timing.sentiment_poll_interval` (5m), through the whole trading and extended-hours
day, and the verdict it produces is live: a bearish reading blocks new entries only
until a later reading is not bearish, and a bullish reading can be overturned by a
later bearish one just as readily. See [`strategy.md`](./strategy.md) §1 for the
classification itself and [`decisions.md`](./decisions.md) for what this replaced.

- Each new reading immediately decides whether a *new* position may be opened,
  right now. It is a pre-emptive gate on opening — distinct from the per-trade
  stop-loss, which protects a position already open — and it is re-evaluated on
  every reading rather than settled once for the day.
- Open positions are monitored regardless of the current verdict: the gate stops
  new entries, it does not abandon anything already held. A bearish reading never
  sells anything.
- Before the first reading of a session exists, new entries are blocked the same
  way a bearish reading blocks them — no data is not evidence that it is safe to
  trade — but this cannot outlast the next tick: a daemon started mid-session takes
  its first reading immediately rather than waiting for a blocking window that no
  longer exists. There is nothing here left to dismiss from the status page.
- **Pre-market, the regular session and post-market all read the same verdict.**
  Before this change the two extended-hours halves took their entry authority from
  different places — a live, unpersisted read before the bell standing in for a
  gate that had not run yet, and a deferral to the day's already-resolved verdict
  after the close. Now that the gate runs continuously rather than being confined to
  a window after the open, that asymmetry is gone: all three read the current,
  continuously-updated verdict the same way. A position opened pre-market is
  covered by everything else in this document from the moment it exists: the same
  stop, the same sizing, the same forced end-of-day exit.
- **The book is flat overnight regardless.** Post-market trading (2026-10-08)
  brought a second forced exit with it, `exit.eod_exit_offset_minutes` before
  `extended.end`, so a position opened at 17:00 is sold before 20:00 rather than
  carried. Without it, extending coverage past the close would have quietly
  converted this from a day-trading strategy into one holding overnight gap risk on
  microcaps — a different risk profile from the one anything in this document was
  measured against. The residual exposure is a limit order that does not fill in a
  thin 19:55 book; that is the same failure mode as an unfilled extended-hours stop,
  and the same `extended.limit_slip_pct` governs it.

## End-of-day exit

- Every position the strategy opened is force-closed `exit.eod_exit_offset_minutes` before market close (currently **5m**) — the strategy never holds overnight.
- **Positions opened by hand are force-closed too** (2026-10-02, on request; between 2026-09-30 and that date they were not, and were held overnight). A manual position is exempt from the *signal* exit rules — no gap backstop, no candle trail, no scale-out — but it is subject to its own stop and to the forced end-of-day exit, so the book really is flat after the close.

## The protective stop rests at the broker

- **Every position's stop is a real order at the broker, sent as part of the buy** (since 2026-10-03; manual positions since 2026-10-02). In the regular session the buy goes as an Alpaca `oto` order with a `stop_loss` leg at the stop that sized it, good-till-cancelled, so the stop is resting from the instant the buy fills. Its id is stored on the position (`stop_order_id`, migration 007). `engine.protectEntry` adopts the leg — or, after a short fill, replaces it with a stop for exactly what was bought.
- **Where it cannot be attached, it follows the fill.** The extended sessions accept only simple limit orders, so an extended-hours buy goes alone and the stop is placed straight after (`engine.placeProtectiveStop`); it is accepted there but cannot trigger outside the bells, so the engine holds the floor on the tick until it can. If the broker refuses the attachment, the buy is resent alone under the same client order id — a duplicate would be refused, so it cannot buy twice — and the stop is placed separately. The audit's `stop_order_note` says which.
- **Why an order rather than a tick check.** A stop that exists only inside a running process has the tick's latency at best and none at all during a restart. On 2026-10-02 all three automated stop-outs were evaluated on the tick, and on these names that latency is a large share of the loss.
- **Automated positions move their stop, and the order moves with it.** The candle trail and the breakeven move after the target raise `StopPrice`; `engine.moveProtectiveStop` cancels, confirms and re-places the order at the new level. A scale-out takes the order off first — it covers every share — and puts it back for the runner. If a move fails the position is never left worse off: the old order still rests at the lower stop, or the tick holds the new one.
- **The tick still evaluates an automated position's rules**, stop included. The two cannot both sell, because every sell takes the resting order off first and a filled order is recorded as the exit instead. A manual position's stop is evaluated on the tick only where the order cannot cover it (below).
- **The daemon checks a resting stop's status every 15s, or on every tick once price is at or near it** (`checkProtectiveStop`). The broker enforces the stop regardless; the lookup is only how the daemon learns it fired. Asking every 2-second tick would be 30 requests a minute per position.
- **Only a terminal order is an outcome.** A stop on a thin name fills in pieces, and the share count on a `partially_filled` order is a snapshot of a sale still in progress — the broker owns the remaining shares and goes on selling them. `pollProtectiveStop` reports a fill only once the order is `Done()` (filled, cancelled, expired, rejected), which is how `releaseProtectiveStop` has always read the same status. Reading a mid-fill count as the exit banks shares that are about to sell anyway and, because recording an exit clears `stop_order_id`, hands the floor to the engine while the order is still live — the one state the two are meant never to share. That is **QTEX on 2026-10-05**: a 1779-share stop reported 523 filled mid-fill, the daemon banked that and left 1256 "held", the order then filled all 1779, and for the next three hours every tick sent a sell for 1256 shares that did not exist — 2,862 orders, each refused with `cannot be sold short`. A genuine short fill still arrives, as `canceled` with a partial, and takes the `ReduceShares` branch. `closeOnProtectiveStop` refuses a working order outright rather than trusting its callers.
- **Every path that sells cancels it first**, and the cancel is confirmed by looking the order up rather than trusting the cancel (`engine.releaseProtectiveStop`). A resting sell order that outlives its holding sells shares that are not there, which is a **short** position rather than a flat one. If the cancel cannot be confirmed the exit is held off and faulted rather than raced: being late out of a position is recoverable, being short is not. If the lookup shows the stop filled in the meantime, that fill is recorded as the exit instead.
- **For a manual position, the engine keeps the floor where the order cannot.** Two cases, both reported: the order does not exist (placement was refused, or the broker ended it without a fill), or the clock is before the opening bell, where Alpaca accepts a stop order but will not trigger it. In both, `strategy.EvaluateManualExit` evaluates the same stop on the tick. The two are mutually exclusive by construction, so they cannot both sell the same shares.
- **A refused stop order does not unwind the buy.** Selling straight back across the spread is a certain loss taken to avoid an uncertain one. The position keeps its stop on the tick, the trail records a `FAULT`, and the page says plainly that the stop is not resting — because an operator who believes they have a resting stop and does not will size the next decision wrongly.
- Entry-side rules (the cap, one position per symbol, same-day re-entry, sizing from the stop) apply to opening one, and it occupies a slot until closed.

## Not yet decided / worth revisiting once paper trading is running

- Whether a *portfolio-level* daily drawdown limit (e.g. halt new entries if today's realized+unrealized P&L drops below some %) is needed in addition to the per-trade stop-loss. Currently there isn't one — the per-trade risk budget (1%, so at most 3% across three concurrent positions) plus the 99% max exposure cap are the only loss containment. Flag this as a gap to reassess after seeing real paper-trading behavior, not something to silently add or silently skip.
