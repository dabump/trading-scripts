# Risk management

**Status:** implemented. These numbers were explicitly committed to during the grill-me interview — treat them as settled, not placeholders.

## Position sizing

- Each position: **10% of portfolio value** at time of purchase.
- The allocation is clamped to available cash. The target is a percentage of total portfolio *value*, which includes the market value of positions already held, so on a heavily deployed account the untouched percentage could still exceed the cash left to spend; submitting an order the account cannot fund just earns a broker rejection mid-session.
- If the allocation cannot buy a single share, the candidate is skipped with a logged reason rather than rounded up.
- Maximum **5 concurrent open positions** → maximum 50% of portfolio exposed at once, minimum 50% held in cash.
- If a qualifying candidate appears while 5 positions are already open, it is skipped — there is no queueing or displacing an existing position.

## Per-trade stop-loss

- Any open position down **10% from entry** is exited immediately, regardless of the trailing-stop/profit-target logic in [`strategy.md`](./strategy.md). A price sitting exactly on the threshold counts as a hit.
- This is a hard floor — it should be checked independently of (and take priority over) the momentum-based exit signal, so a bug or gap in the exit-signal logic can't turn into an unbounded loss on a single position.

## Daily kill switch

- If the first-hour sentiment read comes back overwhelmingly bearish, no trades are placed for the rest of that session. The agent waits for the next trading day. The verdict is persisted, so a restart does not re-open a halted day.
- Open positions are still monitored while halted: the kill switch stops new entries, it does not abandon anything already held. (In practice nothing can be open when the gate halts, since no entries happen during the first hour — but a restart that adopted a broker position is the case where this matters.)
- This is a pre-emptive halt (based on sentiment, before any position is opened) — distinct from the per-trade stop-loss, which protects an already-open position.

## End-of-day exit

- Every open position is force-closed 30 minutes before market close, no exceptions — the strategy never holds overnight.

## Not yet decided / worth revisiting once paper trading is running

- Whether a *portfolio-level* daily drawdown limit (e.g. halt new entries if today's realized+unrealized P&L drops below some %) is needed in addition to the per-trade stop-loss. Currently there isn't one — the per-trade 10% stop plus the 50% max exposure cap are the only loss containment. Flag this as a gap to reassess after seeing real paper-trading behavior, not something to silently add or silently skip.
