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

## Daily kill switch

- If the gate's sentiment read comes back overwhelmingly bearish, no trades are placed for the rest of that session. The agent waits for the next trading day. The verdict is persisted, so a restart does not re-open a halted day.
- Open positions are still monitored while halted: the kill switch stops new entries, it does not abandon anything already held. (In practice nothing can be open when the gate halts, since no entries happen during the gate window — but a restart that adopted a broker position, or a pre-market entry, are the cases where this matters.)
- **The kill switch does not cover pre-market, because it cannot.** Its readings are taken after the open, so anything before the bell necessarily predates the verdict. Pre-market screening is unaffected — it buys nothing by itself — but `premarket.allow_entry` trades with the switch nonexistent rather than merely off. A live sentiment read through the same classifier stands in for it (`docs/strategy.md` §1 and §2b); one sample is a weaker guarantee than a window of them, which is why `allow_entry` defaults to false. A position opened pre-market is then covered by everything else in this document from the moment it exists: the same stop, the same sizing, the same forced end-of-day exit.
- This is a pre-emptive halt (based on sentiment, before any position is opened) — distinct from the per-trade stop-loss, which protects an already-open position.
- **A halt is only dismissible when the gate never ran.** Starting the daemon after the gate window leaves no readings, and the resulting halt reports an absence of data rather than a bearish market; the status page can dismiss that one, recording the verdict as `GATE_OVERRIDDEN`. A halt the gate reached on real readings is the kill switch doing its job and cannot be dismissed from the page. The distinction is made on whether any readings exist, not on the halt's wording.

## End-of-day exit

- Every open position is force-closed `exit.eod_exit_offset_minutes` before market close (currently **5m**), no exceptions — the strategy never holds overnight.

## Not yet decided / worth revisiting once paper trading is running

- Whether a *portfolio-level* daily drawdown limit (e.g. halt new entries if today's realized+unrealized P&L drops below some %) is needed in addition to the per-trade stop-loss. Currently there isn't one — the per-trade risk budget (1%, so at most 3% across three concurrent positions) plus the 99% max exposure cap are the only loss containment. Flag this as a gap to reassess after seeing real paper-trading behavior, not something to silently add or silently skip.
