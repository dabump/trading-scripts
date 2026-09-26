# Status web page

**Status:** implemented. The page is served by the `web` package (see [`architecture.md`](./architecture.md)) and renders state from `store`. It never places orders and never changes what the agent will do — but it is no longer purely passive: two buttons trigger read-only checks on demand (see "Manual checks" below).

## Visual design

- **Rendering:** server-rendered Go `html/template`, styled with modern CSS (cards, clear typography/spacing, subtle color accents) — no JS frontend framework, no separate build pipeline. Keeps this a single Go binary; "modern and stylish" is a CSS/layout goal, not a reason to add a second toolchain.
- **Theme:** dark, fitting a trading-terminal feel and easier to glance at repeatedly through the trading day. No light-theme toggle in v1 — one theme, done well, rather than two done halfway.
- **Live updates:** lightweight JS polling every ~10–15s that patches the page in place (not a full reload, not a WebSocket). This resolves the "refresh behavior" question below — it's no longer open.

## Manual checks (two buttons)

Two header buttons run a check on demand and show the result in an in-page modal
overlay. Both are **read-only by contract**, and the contract is the point:

- **Check sentiment** runs the same classification as the scheduled poll and reports
  what it would mean. **Nothing is persisted.** The gate verdict is decided by the
  most recent *stored* reading, so writing a manual check would let a click at any
  hour overturn what the first hour concluded.
- **Screen candidates** runs the screening evaluation and lists what it found,
  ranked strongest first with failures kept visible — seeing what nearly qualified
  is the point of looking. It **works with the exchange closed**, which is the case
  it was asked for; when closed, the modal says the prices and percentages describe
  the last session rather than a live move.

Neither button can reach the order path. This is enforced in the engine, not the
handler: the manual screen shares the universe-gathering and evaluation code with
the automated scan but never calls the entry logic. That separation matters most
outside market hours, when the automated path's buy step would be exactly wrong.

The manual screen applies exactly the criteria the automated scan does, so a row
labelled "Qualifies" is one the agent really would act on during trading hours.

Both endpoints are POST-only, so a prefetch, crawler or pasted link cannot trigger
upstream work. Each action refuses to run concurrently with itself; overlapping
clicks get told it is already running rather than multiplying API calls.

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
| `EOD_WINDOW` | Amber | Final 30 minutes before the close. Flattening open positions; no new entries. |
| `HALTED_BEARISH` | Red | First-hour sentiment came back overwhelmingly bearish. No trades for the rest of the session; waiting for next trading day. |
| `ERROR` | Red (distinct label from `HALTED_BEARISH`, not just color) | The daemon hit an unhandled error (e.g. data source failure). Needs attention — this is not a normal trading-halt state. |

`EOD_WINDOW` was added during implementation. The original five states had no way to describe the window between the forced-exit mark and the close: the market is still open, so `MARKET_CLOSED` would be untrue, and `SCREENING` would imply entries were still possible. Rather than misreport either, the window got its own state.

`ERROR` must be visually distinguishable from `HALTED_BEARISH` beyond color alone (e.g. a different icon or label text) since both are red but mean very different things — one is a deliberate risk decision, the other is a fault.

## Screening section (visible while `SCREENING`)

A table of tickers currently being evaluated, with a per-criterion breakdown rather than a single pass/fail — this is what lets you see *why* a candidate did or didn't qualify:

| Ticker | News catalyst today | ≥10% intraday move | ≥5x avg volume | Overall |
|---|---|---|---|---|
| e.g. `ABCD` | ✅ 2 today | ✅ +14% | ✅ 6.1x | **Qualifies** |
| e.g. `WXYZ` | ❌ 0 today | ✅ +11% | ✅ 5.3x | Fails (no news) |

Each cell shows both the pass/fail and the underlying value, not just a checkmark, so the numbers are visible without cross-referencing anything else.

A final **Action** column says what the entry pass did with each qualifying
candidate — bought (with size and price), or why not: the position cap, already
holding it, already traded today, insufficient cash. Qualifying is not the same as
being bought, and without this the reason lived only in the log.

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
