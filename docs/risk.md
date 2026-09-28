# Risk management

**Status:** implemented. These numbers were explicitly committed to during the grill-me interview — treat them as settled, not placeholders.

## Position sizing

- **Sizing is from risk, not from portfolio fraction.** Every trade risks `risk.risk_per_trade_pct` (1%) of the account: `shares = (portfolio value × 1%) ÷ (entry − stop)`. The stop comes from the chart (see [`strategy.md`](./strategy.md) §3), so risk per share is known before the size is chosen.

  This replaced a fixed 10%-of-portfolio allocation, then a 5% one. The problem with both is that dollar risk then swings with the stock's volatility: a trade whose stop is 4% away loses eight times as much as one whose stop is 0.5% away, so the account's worst days are decided by which setups happened to be wide rather than by any deliberate choice.

- **Two caps bound the size:** `risk.max_position_pct` (33%) on one position's notional, and available cash. The notional cap does bind on tight stops, and when it does the trade risks *less* than 1% — conservative, and visible in the audit trail's `risk_dollars`. The cap must stay above `risk_per_trade_pct ÷ entry.max_stop_distance_pct`, or it binds on every trade and sizing silently reverts to a fixed fraction of the account. Config validation does not check that ratio; a test in `internal/config` does.

- **Concurrency is 3, not 5.** The approach being followed runs a small number of positions at a time rather than a basket. Three at 33% keeps maximum exposure just under fully invested, and total simultaneous risk at 3%.
- The allocation is clamped to available cash. The target is a percentage of total portfolio *value*, which includes the market value of positions already held, so on a heavily deployed account the untouched percentage could still exceed the cash left to spend; submitting an order the account cannot fund just earns a broker rejection mid-session.
- If the allocation cannot buy a single share, the candidate is skipped with a logged reason rather than rounded up.
- Maximum **5 concurrent open positions** → maximum 50% of portfolio exposed at once, minimum 50% held in cash.
- If a qualifying candidate appears while 5 positions are already open, it is skipped — there is no queueing or displacing an existing position.

## Per-trade stop-loss

- **The working stop is the chart stop**, from the setup's pullback low, capped at `entry.max_stop_distance_pct` (4%) away. A setup whose stop is wider is refused rather than sized around.
- **`risk.stop_loss_pct` (10%) is now a gap backstop**, not the working stop. It is only reachable when price jumps straight through the chart stop. Config validation requires it to be wider than `entry.max_stop_distance_pct`; otherwise it would fire first and the chart stop would never be reached, silently reverting the strategy to a fixed-percentage stop.
- A price sitting exactly on either threshold counts as a hit (`strategy.priceEpsilon`).
- **Once the first target is banked the stop moves to the entry price**, so a winner cannot become a loser.
- This is a hard floor — it should be checked independently of (and take priority over) the momentum-based exit signal, so a bug or gap in the exit-signal logic can't turn into an unbounded loss on a single position.

## Daily kill switch

- If the first-hour sentiment read comes back overwhelmingly bearish, no trades are placed for the rest of that session. The agent waits for the next trading day. The verdict is persisted, so a restart does not re-open a halted day.
- Open positions are still monitored while halted: the kill switch stops new entries, it does not abandon anything already held. (In practice nothing can be open when the gate halts, since no entries happen during the first hour — but a restart that adopted a broker position is the case where this matters.)
- This is a pre-emptive halt (based on sentiment, before any position is opened) — distinct from the per-trade stop-loss, which protects an already-open position.

## End-of-day exit

- Every open position is force-closed 30 minutes before market close, no exceptions — the strategy never holds overnight.

## Not yet decided / worth revisiting once paper trading is running

- Whether a *portfolio-level* daily drawdown limit (e.g. halt new entries if today's realized+unrealized P&L drops below some %) is needed in addition to the per-trade stop-loss. Currently there isn't one — the per-trade risk budget (1%, so at most 3% across three concurrent positions) plus the 99% max exposure cap are the only loss containment. Flag this as a gap to reassess after seeing real paper-trading behavior, not something to silently add or silently skip.
