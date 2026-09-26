# Architecture

**Status:** design only — nothing under this layout exists yet.

## Shape of the system

A single Go binary runs as one continuously-running daemon process (not a cron job) with two concurrent responsibilities:

1. **The trading loop** — sentiment gating, screening, entry/exit decisions, order placement.
2. **A status web server** — a read-only page showing current state (open positions, today's sentiment reading, recent decisions), backed directly by the same store the trading loop writes to.

There is one Alpaca account/API key set used for both market data and order execution, so there's a single `broker` client rather than separate data and execution integrations.

## Components (planned `internal/` packages)

| Package | Responsibility |
|---|---|
| `scheduler` | Knows US market hours (open/close, the first-hour window, the T-30min-before-close mark). Drives the daemon's daily loop: wait for open, run the sentiment loop, hand off to strategy, force-exit before close, idle until next session. |
| `sentiment` | Polls market data every 10 minutes during the first hour post-open and produces a bullish/overwhelmingly-bearish/neutral reading. See [`strategy.md`](./strategy.md) for what feeds this. |
| `screener` | Gets a candidate universe (Alpaca most-actives, or a dedicated screener API if that's not enough — open item) and scans it using config-driven thresholds: float, a news-catalyst presence check, intraday price move, and volume multiple (all four required — see [`strategy.md`](./strategy.md)). Depends on `broker` for price/volume, plus float and news-headline data, which may require a second provider if Alpaca doesn't cover them (open item). |
| `strategy` | Turns a screened candidate into a buy decision, and turns an open position + live price into a sell decision (profit target, trailing stop, stop-loss, forced EOD exit — see [`risk.md`](./risk.md)). |
| `risk` | Position sizing (10% of portfolio per trade), max concurrent positions (5), per-trade stop-loss, and the daily kill switch when sentiment reads overwhelmingly bearish. |
| `broker` | Alpaca client, wrapped behind an interface so market data and/or execution could be swapped to a different provider later without touching strategy/risk code. |
| `store` | SQLite persistence — open positions, trade history, sentiment history. The daemon must be able to restart mid-day and resume correctly from this. |
| `web` | HTTP handlers for the status page. Reads from `store`; does not itself drive trading decisions. Page content/layout is specified in [`web-ui.md`](./web-ui.md). |

## Data flow (one trading day)

```
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

`web` runs alongside this the whole time, independently reading `store` to render status — it never blocks or is blocked by the trading loop.

## Why this shape

- **One daemon, not cron-triggered scripts**: the first-hour polling and continuous position monitoring need persistent in-process state (or at minimum a tight polling loop) rather than being cleanly split into independently-scheduled invocations.
- **Interface-based `broker`**: Alpaca was the explicit choice for v1, but the interface exists so a future move to a different broker/data provider (or a backtest-mode implementation) doesn't require touching strategy or risk logic.
- **SQLite over a server DB**: this is a single-instance daemon; embedding the DB avoids running/operating a separate database process for no benefit at this scale.
