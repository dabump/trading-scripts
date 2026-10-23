# Architecture

**Status:** implemented. The package layout below is what exists; `go doc ./internal/...` is authoritative for signatures.

## Shape of the system

A single Go binary runs as one continuously-running daemon process (not a cron job) with up to four concurrent responsibilities:

1. **The trading loop** — sentiment gating, screening, entry/exit decisions, order placement.
2. **A status web server** — a page showing current state (open and closed positions, today's sentiment reading, recent decisions), backed directly by the same store the trading loop writes to. Mostly read-only: two manual checks that cannot trade, plus **Open** and **Close** controls that can. Open overrides the entry signal but not the risk rules; a position it opens keeps its 1R stop — left resting at the broker as a real order — and is force-closed at the bell, but no other exit rule touches it.
3. **The trade review worker** (`claude.review_failed_trades`, on by default) — finds closed, losing positions with no review yet and asks the Claude Code CLI why each one lost, fed `strategy.md`/`risk.md`/`decisions.md` plus the trade's own numbers. Entirely separate from the trading loop: it depends only on `store` and a function value for running Claude, never on `engine`, so a wedged or logged-out CLI cannot affect trading. Reviews run strictly one at a time on their own ticker (`claude.review_interval`, default 5m) — a CLI call can take tens of seconds, nothing about it is time-critical the way an order is. See `internal/tradereview`.
4. **The news-sentiment worker** (`claude.assess_news_sentiment`, on by default) — for a symbol on the latest screening pass whose news-catalyst criterion passed, asks the Claude Code CLI whether that news reads as positive, at most once per symbol per session, and the page stars it on the watchlist if so. Purely a watchlist annotation: it reads `store.LatestScreenSnapshot` — the same evaluations the page and the entry pass both already use, kept as the one evaluation path (see `CLAUDE.md`) — and writes to its own table, never back into screening or `engine`. See `internal/newssentiment`.

There is one Alpaca account/API key set used for both market data and order execution, so there's a single `broker` client rather than separate data and execution integrations. `broker` also ships a `Fake` implementation, which is what the tests and `-offline` mode run against; the daemon can therefore be exercised end to end with no credentials and no network.

## Components (`internal/` packages)

| Package | Responsibility |
|---|---|
| `scheduler` | Pure session arithmetic, including the countdowns the status page shows (`UntilOpen`, `UntilClose`, `FormatCountdown`): given the exchange's calendar day and config, reports which phase the clock is in (closed / extended hours / trading / EOD window) and where the boundaries fall. Two of those occur twice a day: extended hours before the open and after the close, and the EOD window before each of the two closes. There is no separate sentiment-gate phase any more — the gate is checked continuously within trading and extended hours rather than owning a blocking window of its own. Holds no state and does no I/O. |
| `engine` | Drives the daily loop that `scheduler` describes, plus restart reconciliation. One `Tick` decides what is due, and every order is sent from it; there are no per-phase goroutines and no direct `time.Now()` calls (the clock is injected, which is what makes a whole day testable). The one exception to running inline is the screen: under `Run` it runs on a background goroutine and publishes a watchlist, so a pass that takes most of a minute no longer holds up the 2-second position checks or the per-candle setup check. A test calling `Tick` directly gets the screen inline. |
| `config` | Loads and validates `config/config.yaml`, rejecting configurations that would only fail mid-session — for example sizing × concurrency exceeding 100% of the portfolio. Credentials are read from the environment here, never from YAML. |
| `domain` | Shared types (positions, snapshots, evaluations, agent states). Exists to keep `store`, `broker`, `strategy`, `risk` and `web` from importing each other. |
| `sentiment` | Polls market data every `timing.sentiment_poll_interval` (5m), continuously through the trading and extended-hours day, and classifies each reading bullish/overwhelmingly-bearish/neutral. The latest reading is a live, reversible verdict rather than a one-time decision for the day. See [`strategy.md`](./strategy.md) for what feeds this. |
| `screener` | Scans the candidate universe (every tradable US equity, cached per session) using config-driven thresholds: a news-catalyst presence check, intraday price move, and volume multiple (all three required — see [`strategy.md`](./strategy.md)). |
| `cmd/backtest` | Measures the configured strategy against real Alpaca history. Imports `screener`, `sentiment`, `risk` and `strategy` so the rules measured are the rules the daemon runs; GET-only, cannot place an order. See `docs/decisions.md` for the results. |
| `strategy` | The entry setup and the exit decisions. `FindSetup` looks for a pullback and resumption and returns the stop the pattern defines, which is what makes risk knowable before sizing; `EvaluateExit` returns the *first* action due, in priority order: forced EOD, then the stop, then the first profit target. Threshold comparisons carry a tiny epsilon because a threshold like `entry*0.9` is not exactly representable in binary floating point, and without it a price sitting exactly on a documented threshold would miss its exit. |
| `risk` | Position sizing from the stop (`SizeForRisk`: 1% of the account at risk per trade, capped at 33% notional), max concurrent positions (3), and per-trade stop-loss. The sentiment-driven block on new entries lives in `engine`/`sentiment` rather than here — see [`risk.md`](./risk.md). |
| `broker` | Alpaca client, wrapped behind an interface so market data and/or execution could be swapped to a different provider later without touching strategy/risk code. |
| `store` | SQLite persistence — positions, trade history, sentiment readings, session verdicts, screening snapshots and submitted orders. Migrations are embedded from `internal/store/migrations/` (not the repo root: `go:embed` cannot reach outside its package, and the deployment target is one self-contained binary). A partial unique index makes a second open position in the same symbol impossible at the schema level. |
| `audit` | Append-only JSON-lines trail of decisions and actions, one file per session date, flushed on every event. Deliberately independent of `store`: the database holds current state, this holds the narrative of how that state came to be. |
| `web` | HTTP handlers for the status page. Reads from `store`; does not itself drive trading decisions. Exposes the two read-only manual checks and the **Open**/**Close** actions behind an `Actions` interface — the handlers carry a symbol or a position id and nothing else, and the engine decides whether the trade is possible and on what terms. The result types live in `domain`, so `web` still does not import `engine`. Page content/layout is specified in [`web-ui.md`](./web-ui.md). |
| `claudelogin` | Bridges the Claude Code CLI's login flow through the status page's Claude card. OAuth's browser callback cannot complete inside a container, so the CLI falls back to printing a URL and expecting a pasted code back; this package runs it under a pty, capturing output for the page and relaying input back to it. One attempt at a time. Also offers `Logout`, which — unlike login — needs no pty: `claude auth logout` is a plain one-shot command. Independent of trading entirely — it exists so the container has an authenticated CLI session for `tradereview` and `newssentiment` (and whatever else) to use. |
| `tradereview` | Asks Claude why a losing trade lost and what, if anything, should change about the strategy, grounded in the strategy's own docs. `Worker.Run` polls `store.FailedTradesNeedingReview` on its own ticker and calls an `Analyze` function (the real one shells out to `claude -p` with every tool disabled, reading the prompt from stdin); a row is written to `store` exactly once per trade, win or lose on the call, so a restart mid-review just finds the same trade again rather than one stuck "in progress" forever. |
| `newssentiment` | Asks Claude whether a screened symbol's same-day news catalyst reads as positive, so the watchlist can star it. `Worker.Run` reads the latest screening pass (`store.LatestScreenSnapshot`), keeps the symbols whose news-catalyst criterion passed, and skips any already analyzed today (`store.AnalyzedNewsSentiment`) before fetching that symbol's actual headlines (`broker.NewsHeadlines` — unlike the scan path's `NewsCounts`, this needs the story text, not just a count) and running them past Claude. Unlike `tradereview`, it is deliberately *not* logged-out-tolerant the same way: while the CLI has no session, a sweep does nothing at all rather than recording anything, so a symbol is never stuck with a stale answer — and once Claude is actually asked, the day's one allowed analysis for that symbol is spent whether or not the call succeeds, which is what keeps token use to at most one call per symbol per session regardless of retries. |

## Data flow (one trading day)

```
scheduler: pre-market opens (extended.start, only when extended.enabled)
  -> sentiment: poll every 5min, write readings to store; verdict is live and reversible
  -> screener: find candidates, on extended thresholds and cadence
       -> store: record the pass; the page shows it with "why not bought" per row
       (extended.allow_entry only) current sentiment verdict -> not bearish
         -> strategy: setup on pre-market candles -> risk: size
              -> broker: extended-hours limit order -> store: record open position
  -> strategy: evaluate any pre-market position against the exit rules
scheduler: market opens
  -> sentiment: poll every 5min, write readings to store; verdict is live and reversible
  -> screener (background, ~1/min): publish a watchlist regardless of the verdict
     tick, once per closed candle per watchlist name:
       current sentiment verdict bearish -> enterPositions skipped; screening continues
       not bearish ->
         -> strategy: setup on closed candles -> live price re-checked
         -> risk: size from the live price, check exposure cap
              -> broker: place order -> store: record open position
       (repeat through the day as candidates, the verdict and exits change)
  -> strategy: continuously evaluate open positions against exit rules
       -> broker: place sell order -> store: close out position
  -> scheduler: force-exit any remaining open positions at T-30min-before-close
scheduler: market closes; post-market opens (to extended.end)
  -> sentiment: poll every 5min, write readings to store; same verdict as the regular session
  -> screener: find candidates, on the same extended thresholds and cadence
       -> store: record the pass; the page shows it with "why not bought" per row
       (extended.allow_entry only) current sentiment verdict -> not bearish
         -> strategy: setup on post-market candles -> risk: size
              -> broker: extended-hours limit order -> store: record open position
  -> strategy: evaluate any post-market position against the exit rules
  -> scheduler: force-exit again before extended.end; the book is flat overnight
```

`web` runs alongside this the whole time, independently reading `store` to render status — it never blocks or is blocked by the trading loop, and it makes no broker calls of its own: the loop persists each position's last mark, so the ~12s page poll costs nothing upstream. The account balance reaches the page the same way. The tick reads `broker.Account` at most every 10s (`accountRefresh`) and publishes the result on the engine (`engine.Account`, alongside `Session` and `NextSession`), so the figure is refreshed on a fixed cadence — bounded by the loop — rather than once per page poll by every open browser.

The page is not purely passive any more. `engine.CheckSentiment` and
`engine.ScreenNow` back the two manual buttons; both share code with the automated
path but deliberately stop short of it — neither persists anything and neither can
place an order, which is what makes them safe to run with the market closed.
`engine.ClosePosition` and `engine.OpenPosition` back the **Close** and **Open**
buttons and are the two controls that trade. Both go through the same `submit` path
the strategy uses. A manual open overrides the setup gate and nothing else on the way
in — sizing from a stop, the position cap and the same-day rules are all applied
exactly as they are for an automatic entry. Once open, the position is flagged
`manual` and **only two exit rules apply to it**: its own 1R stop, left resting at the
broker as a good-till-cancelled stop order (`engine.placeProtectiveStop`, id stored as
`stop_order_id`), and the forced end-of-day exit. The signal rules — gap backstop,
candle trail, scale-out — do not. Every path that sells it cancels the resting order
first, confirmed by looking the order up, because an order outliving its holding goes
short. See [`web-ui.md`](./web-ui.md) and [`risk.md`](./risk.md) for the reasoning.

Extended hours occupy time that was previously `PhaseClosed`, and only that time: everything between the bells is unchanged, and with `extended.enabled: false` the phase never occurs. **The three windows never overlap**, which matters because the extended thresholds are far looser than the regular ones (0.5x relative volume against 5x, a $100,000 turnover floor against $1,000,000) and the extended book takes only limit orders: an overlap in either direction would apply one session's rules to the other. `scheduler.Bounds` clamps each extended mark against the calendar's own open and close rather than a hardcoded 09:30/16:00, and drops a half whose configured clock would overlap rather than clamping it to the bell.

Post-market was added on request (2026-10-08) and brought the forced exit with it: **the EOD window runs twice**, once before the regular close and again `exit.eod_exit_offset_minutes` before `extended.end`, so a position opened at 17:00 is flattened rather than carried overnight. That is what keeps `docs/risk.md`'s flat-overnight rule true now that the agent trades after the close. A manual or automated position held during either extended session is the case where its stop is enforced on the tick rather than by the resting order: Alpaca accepts a stop order outside the bells but will not trigger one there.

On startup, before the loop begins, `engine.Reconcile` compares the broker's positions against the store: anything the broker holds that the store does not know about is adopted, anything the store thinks is open that the broker does not hold is closed as `RECONCILED` — unless it carried a resting stop order that filled, in which case that order's own fill price and `STOP_LOSS` are recorded instead of this morning's mark, which on a gap is a different number entirely — and orders recorded as submitted but never confirmed are resolved. This is what closes the crash-between-submit-and-confirm hole.

**Positions are recorded from fills, not quotes.** `engine.submit` places the order,
then polls `broker.Order` until the broker reports it done, for up to ten seconds
(paper fills took up to five). Whatever is still working then is cancelled, and the
caller gets what actually executed: the entry, exit and scale-out prices are the
broker's average fill, and the share counts are what filled. That can be less than
was asked for, and every caller handles it — a buy that filled nothing is not a
position, and an exit that sold short banks what sold and leaves the rest open for the
still-breached rule to sell on the next tick (`store.ReduceShares`, audited as
`ORDER_NOT_FILLED`). If the broker never says how an order ended, the old assumption
applies — filled in full at the quote — and the order is stored as `unconfirmed` for
`Reconcile`. The `orders` table keeps the final status, `filled_price` and
`filled_shares`; before this it kept Alpaca's `pending_new` acknowledgement forever,
and on 2026-09-29 the quoted prices put the day at −$541.80 when the fills said
−$632.15.

## Why this shape

- **One daemon, not cron-triggered scripts**: the gate's polling and continuous position monitoring need persistent in-process state (or at minimum a tight polling loop) rather than being cleanly split into independently-scheduled invocations.
- **Interface-based `broker`**: Alpaca was the explicit choice for v1, but the interface exists so a future move to a different broker/data provider (or a backtest-mode implementation) doesn't require touching strategy or risk logic.
- **SQLite over a server DB**: this is a single-instance daemon; embedding the DB avoids running/operating a separate database process for no benefit at this scale. The pure-Go driver (`modernc.org/sqlite`) keeps the build CGO-free, so the artefact stays a single portable binary.
- **`scheduler` split from `engine`**: session arithmetic is pure and heavily edge-cased (early closes can push the forced-exit mark inside the gate window), so it is worth testing in isolation from anything that does I/O.
