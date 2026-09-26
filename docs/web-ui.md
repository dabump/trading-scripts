# Status web page

**Status:** design only. This is the read-only page served by the `web` package (see [`architecture.md`](./architecture.md)) — it renders state from `store`, it never drives trading decisions.

## Visual design

- **Rendering:** server-rendered Go `html/template`, styled with modern CSS (cards, clear typography/spacing, subtle color accents) — no JS frontend framework, no separate build pipeline. Keeps this a single Go binary; "modern and stylish" is a CSS/layout goal, not a reason to add a second toolchain.
- **Theme:** dark, fitting a trading-terminal feel and easier to glance at repeatedly through the trading day. No light-theme toggle in v1 — one theme, done well, rather than two done halfway.
- **Live updates:** lightweight JS polling every ~10–15s that patches the page in place (not a full reload, not a WebSocket). This resolves the "refresh behavior" question below — it's no longer open.

## Header: two separate indicators

These are distinct and shouldn't be visually merged, since the agent's status doesn't always match raw market-open/closed (e.g. market is open but the agent is in its no-trade first hour, or halted for the day on bearish sentiment):

1. **Market hours badge** — simple green/red: green while the US exchange is open, red while closed. Driven directly by `scheduler`'s market-hours knowledge, not by anything the agent itself decided.
2. **Agent status badge** — one of the states below (the "legend"), reflecting what the agent is actually doing right now.

### Agent status legend

| State | Color | Meaning |
|---|---|---|
| `MARKET_CLOSED` | Gray | Outside exchange hours. Daemon idle, waiting for next open. |
| `SENTIMENT_CHECK` | Amber | Market open, within the first hour. Polling every 10 minutes. No trades placed. |
| `SCREENING` | Green | First-hour sentiment came back bullish. Actively screening for candidates and may hold open positions. |
| `HALTED_BEARISH` | Red | First-hour sentiment came back overwhelmingly bearish. No trades for the rest of the session; waiting for next trading day. |
| `ERROR` | Red (distinct label from `HALTED_BEARISH`, not just color) | The daemon hit an unhandled error (e.g. data source failure). Needs attention — this is not a normal trading-halt state. |

`ERROR` must be visually distinguishable from `HALTED_BEARISH` beyond color alone (e.g. a different icon or label text) since both are red but mean very different things — one is a deliberate risk decision, the other is a fault.

## Screening section (visible while `SCREENING`)

A table of tickers currently being evaluated, with a per-criterion breakdown rather than a single pass/fail — this is what lets you see *why* a candidate did or didn't qualify:

| Ticker | Float < 10M | News catalyst today | ≥10% intraday move | ≥5x avg volume | Overall |
|---|---|---|---|---|---|
| e.g. `ABCD` | ✅ 4.2M | ✅ | ✅ +14% | ✅ 6.1x | **Qualifies** |
| e.g. `WXYZ` | ✅ 8.9M | ❌ | ✅ +11% | ✅ 5.3x | Fails (no news) |

Each cell shows both the pass/fail and the underlying value, not just a checkmark, so the numbers are visible without cross-referencing anything else.

## Open positions section

| Ticker | Purchase price | Shares | Current price | Unrealized P&L ($ and %) |
|---|---|---|---|---|

Current price and unrealized P&L were added beyond what was originally specified (purchase price + share count) because showing a position's cost basis without its live performance would mean checking somewhere else for the number that actually matters day-to-day.

## End-of-day section (appears after market close)

Scope for v1: **today's session only** — not a browsable multi-day history. This appears once the exchange has closed, summarizing what happened that day:

| Ticker | Shares | Opening (purchase) price | Closing (sale) price | P&L % |
|---|---|---|---|---|

Plus a totals row: net P&L for the day (aggregate $ and %) and a win/loss count, so the day's outcome is visible without adding up the rows by hand.

**Not yet decided:** whether a later version should support browsing prior days' sessions (would need `store` queries across days and a day-picker UI). Flagged as a possible v2 item in [`decisions.md`](./decisions.md), not to be built now.

## Refresh behavior

Settled — see "Visual design" above: lightweight JS polling every ~10–15s, patching the page in place.
