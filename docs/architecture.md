# Architecture

**Status:** implemented. The package layout below is what exists; `go doc ./internal/...` is authoritative for signatures.

## Shape of the system

A single Go binary runs as one continuously-running daemon process (not a cron job) with two concurrent responsibilities:

1. **The trading loop** — sentiment gating, screening, entry/exit decisions, order placement.
2. **A status web server** — a read-only page showing current state (open positions, today's sentiment reading, recent decisions), backed directly by the same store the trading loop writes to.

There is one Alpaca account/API key set used for both market data and order execution, so there's a single `broker` client rather than separate data and execution integrations. `broker` also ships a `Fake` implementation, which is what the tests and `-offline` mode run against; the daemon can therefore be exercised end to end with no credentials and no network.

## Components (`internal/` packages)

| Package | Responsibility |
|---|---|
| `scheduler` | Pure session arithmetic, including the countdowns the status page shows (`UntilOpen`, `UntilClose`, `FormatCountdown`): given the exchange's calendar day and config, reports which phase the clock is in (closed / first hour / trading / EOD window) and where the boundaries fall. Holds no state and does no I/O. |
| `engine` | Drives the daily loop that `scheduler` describes, plus restart reconciliation. One `Tick` decides what is due; there are no per-phase goroutines and no direct `time.Now()` calls (the clock is injected, which is what makes a whole day testable). |
| `config` | Loads and validates `config/config.yaml`, rejecting configurations that would only fail mid-session — for example sizing × concurrency exceeding 100% of the portfolio. Credentials are read from the environment here, never from YAML. |
| `domain` | Shared types (positions, snapshots, evaluations, agent states). Exists to keep `store`, `broker`, `strategy`, `risk` and `web` from importing each other. |
| `sentiment` | Polls market data every 10 minutes during the first hour post-open and produces a bullish/overwhelmingly-bearish/neutral reading. See [`strategy.md`](./strategy.md) for what feeds this. |
| `screener` | Scans the candidate universe (every tradable US equity, cached per session) using config-driven thresholds: a news-catalyst presence check, intraday price move, and volume multiple (all three required — see [`strategy.md`](./strategy.md)). |
| `cmd/backtest` | Measures the configured strategy against real Alpaca history. Imports `screener`, `sentiment`, `risk` and `strategy` so the rules measured are the rules the daemon runs; GET-only, cannot place an order. See `docs/decisions.md` for the results. |
| `strategy` | The entry setup and the exit decisions. `FindSetup` looks for a pullback and resumption and returns the stop the pattern defines, which is what makes risk knowable before sizing; `EvaluateExit` returns the *first* action due, in priority order: forced EOD, then the stop, then the first profit target. Threshold comparisons carry a tiny epsilon because a threshold like `entry*0.9` is not exactly representable in binary floating point, and without it a price sitting exactly on a documented threshold would miss its exit. |
| `risk` | Position sizing from the stop (`SizeForRisk`: 1% of the account at risk per trade, capped at 33% notional), max concurrent positions (3), per-trade stop-loss, and the daily kill switch when sentiment reads overwhelmingly bearish. |
| `broker` | Alpaca client, wrapped behind an interface so market data and/or execution could be swapped to a different provider later without touching strategy/risk code. |
| `store` | SQLite persistence — positions, trade history, sentiment readings, session verdicts, screening snapshots and submitted orders. Migrations are embedded from `internal/store/migrations/` (not the repo root: `go:embed` cannot reach outside its package, and the deployment target is one self-contained binary). A partial unique index makes a second open position in the same symbol impossible at the schema level. |
| `audit` | Append-only JSON-lines trail of decisions and actions, one file per session date, flushed on every event. Deliberately independent of `store`: the database holds current state, this holds the narrative of how that state came to be. |
| `web` | HTTP handlers for the status page. Reads from `store`; does not itself drive trading decisions. Also exposes the two manual-check buttons, which call read-only engine methods behind an `Actions` interface — the handlers have no access to the order path. Page content/layout is specified in [`web-ui.md`](./web-ui.md). |

## Data flow (one trading day)

```
scheduler: pre-market opens (premarket.start, only when premarket.enabled)
  -> screener: find candidates, on pre-market thresholds and cadence
       -> store: record the pass; the page shows it with "why not bought" per row
       (premarket.allow_entry only) live sentiment read -> not bearish
         -> strategy: setup on pre-market candles -> risk: size
              -> broker: extended-hours limit order -> store: record open position
  -> strategy: evaluate any pre-market position against the exit rules
scheduler: market opens
  -> sentiment: poll every 10min for 1 hour, write readings to store
  -> scheduler: read sentiment history from store, decide bullish/bearish
       bearish -> risk: trigger daily kill switch, done for the day
       bullish -> screener: find candidates
                    -> strategy: decide buy -> risk: size position, check exposure cap
                         -> broker: place order -> store: record open position
                    (repeat through the day as candidates and exits occur)
  -> strategy: continuously evaluate open positions against exit rules
       -> broker: place sell order -> store: close out position
  -> scheduler: force-exit any remaining open positions at T-30min-before-close
```

`web` runs alongside this the whole time, independently reading `store` to render status — it never blocks or is blocked by the trading loop, and it makes no broker calls of its own: the loop persists each position's last mark, so the ~12s page poll costs nothing upstream. The account balance reaches the page the same way. Every tick reads `broker.Account` and publishes the result on the engine (`engine.Account`, alongside `Session` and `NextSession`), so the figure is refreshed once per tick — bounded by the loop — rather than once per page poll by every open browser.

The page is not purely passive any more: `engine.CheckSentiment` and
`engine.ScreenNow` back the two manual buttons. Both share code with the automated
path but deliberately stop short of it — neither persists anything and neither can
place an order, which is what makes them safe to run with the market closed. See
[`web-ui.md`](./web-ui.md) for the reasoning.

Pre-market occupies time that was previously `PhaseClosed`, and only that time: everything from the opening bell onwards is unchanged, and with `premarket.enabled: false` the phase never occurs. Post-market is deliberately not covered — the forced exit has already flattened the book, and this strategy holds nothing overnight.

On startup, before the loop begins, `engine.Reconcile` compares the broker's positions against the store: anything the broker holds that the store does not know about is adopted, anything the store thinks is open that the broker does not hold is closed as `RECONCILED`, and orders recorded as submitted but never confirmed are resolved. This is what closes the crash-between-submit-and-confirm hole.

## Why this shape

- **One daemon, not cron-triggered scripts**: the first-hour polling and continuous position monitoring need persistent in-process state (or at minimum a tight polling loop) rather than being cleanly split into independently-scheduled invocations.
- **Interface-based `broker`**: Alpaca was the explicit choice for v1, but the interface exists so a future move to a different broker/data provider (or a backtest-mode implementation) doesn't require touching strategy or risk logic.
- **SQLite over a server DB**: this is a single-instance daemon; embedding the DB avoids running/operating a separate database process for no benefit at this scale. The pure-Go driver (`modernc.org/sqlite`) keeps the build CGO-free, so the artefact stays a single portable binary.
- **`scheduler` split from `engine`**: session arithmetic is pure and heavily edge-cased (early closes can push the forced-exit mark inside the first hour), so it is worth testing in isolation from anything that does I/O.
