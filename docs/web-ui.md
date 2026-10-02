# Status web page

**Status:** implemented. The page is served by the `web` package (see [`architecture.md`](./architecture.md)) and renders state from `store`.

It is not read-only. Two buttons trigger **read-only** checks on demand (see "Manual checks"), and two controls trade: **Close** on an open position, and **Open** on any screening row not already held — qualifying or not.

The rule that replaced "the page cannot trade" is narrower and more useful: **a manual action overrides the signal, never the risk rules.** An operator can decide *that* a trade happens; they cannot decide how big it is, whether it fits under the position cap, or that it goes without a stop. Those are computed identically for a hand-placed trade and an automatic one.

## Visual design

- **Rendering:** server-rendered Go `html/template`, styled with modern CSS (cards, clear typography/spacing, subtle color accents) — no JS frontend framework, no separate build pipeline. Keeps this a single Go binary; "modern and stylish" is a CSS/layout goal, not a reason to add a second toolchain.
- **Theme:** dark, fitting a trading-terminal feel and easier to glance at repeatedly through the trading day. No light-theme toggle in v1 — one theme, done well, rather than two done halfway.
- **Live updates:** lightweight JS polling every ~10–15s that patches the page in place (not a full reload, not a WebSocket). This resolves the "refresh behavior" question below — it's no longer open.
- **Every symbol is a link to its TradingView chart**, opened in a new tab (`tradingview.com/chart/?symbol=…`). The page shows why the agent acted but no chart, and reading the tape is the first thing an operator does with a ticker they have just seen. It is one shared template (`symbolCell`), so positions, the screening table and both modals link identically; a plain anchor rather than JS, so the existing click handlers — which all match on `button` — cannot swallow it, and so it survives the fragment swap.

## Manual checks (two buttons)

Two header buttons run a check on demand and show the result in an in-page modal
overlay. Both are **read-only by contract**, and the contract is the point:

- **Check sentiment** runs the same classification as the scheduled poll and reports
  what it would mean. **Nothing is persisted.** The gate verdict is decided by the
  most recent *stored* reading, so writing a manual check would let a click at any
  hour overturn what the gate concluded.
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

These are distinct and shouldn't be visually merged, since the agent's status doesn't always match raw market-open/closed (e.g. market is open but the agent is in its no-trade opening window, or halted for the day on bearish sentiment):

1. **Market hours badge** — green while the US exchange is open, **orange** while the pre-market session is running (`PRE-MARKET`), red while closed. Driven directly by `scheduler`'s market-hours knowledge, not by anything the agent itself decided. It also carries a **countdown**: `closes in 4h 12m` while open, `opens in 15h 42m` while closed, with the exact target moment in the badge's tooltip.

   Two things about the countdown are worth knowing. First, the "next open" is often *not* today's session — after the close, or on a weekend or holiday, it is the following session, which the engine looks up from the exchange calendar once per day and caches (`NextSession`). If that lookup fails the countdown is simply absent, because a blank is better than a wrong number. Second, the resolution stops at **minutes**: the page refreshes on a ~12s poll, so a seconds figure would advance in 12-second jumps and read as broken, and none of these boundaries is actionable to the second since the agent handles them itself.
2. **Agent status badge** — one of the states below (the "legend"), reflecting what the agent is actually doing right now.

### Agent status legend

| State | Color | Meaning |
|---|---|---|
| `MARKET_CLOSED` | Gray | Outside exchange hours. Daemon idle, waiting for next open. |
| `PRE_MARKET` | Orange | The pre-market session (`premarket.start` → 09:30). Screening the early tape; the sentiment gate has not run yet, and nothing is bought unless `premarket.allow_entry` is set. |
| `SENTIMENT_CHECK` | Amber | Market open, inside the gate window (`timing.sentiment_window`). Polling every `timing.sentiment_poll_interval`. No trades placed. |
| `SCREENING` | Green | First-hour sentiment came back bullish. Actively screening for candidates, every `timing.screener_scan_interval`, and may hold open positions. |
| `EOD_WINDOW` | Amber | The last `exit.eod_exit_offset_minutes` before the close. Flattening open positions; no new entries. |
| `HALTED_BEARISH` | Red | The gate's sentiment came back overwhelmingly bearish, **or** the daemon started after the gate window and had no readings to judge. No trades for the rest of the session. The second case can be dismissed from the page — see "Dismissing a halt the gate could not judge". |
| `ERROR` | Red (distinct label from `HALTED_BEARISH`, not just color) | The daemon hit an unhandled error (e.g. data source failure). Needs attention — this is not a normal trading-halt state. |

`PRE_MARKET` was added when pre-market coverage was, for the same reason `EOD_WINDOW` exists: neither of the neighbouring states is true. `MARKET_CLOSED` would report a scanning agent as idle, and `SCREENING` would imply entries are being taken when by default they are not. It gets its own orange, sitting between the grey of a closed market and the green of a trading one — and a warmer orange than the amber `SENTIMENT_CHECK`/`EOD_WINDOW` use, so the two are not read as variations of each other. With `premarket.enabled: false` the state never appears and the morning is `MARKET_CLOSED` exactly as before.

The market-hours badge stays on the *regular* session throughout: during pre-market it reads `PRE-MARKET` and still counts down to 09:30, because that bell is when the sentiment gate and ordinary entries begin.

`EOD_WINDOW` was added during implementation. The original five states had no way to describe the window between the forced-exit mark and the close: the market is still open, so `MARKET_CLOSED` would be untrue, and `SCREENING` would imply entries were still possible. Rather than misreport either, the window got its own state.

The legend's own text is built from config (`legendFor` in `internal/web/view.go`),
not written out: the gate window, its poll cadence, the scan cadence and the
forced-exit offset are all tunable, and the legend shipped once with the originally
specified hour / 10-minute / 30-minute wording long after all three had changed. Keys
are named in the table above because this document is read against the config; the
page renders the values, because its reader wants the minutes.

`ERROR` must be visually distinguishable from `HALTED_BEARISH` beyond color alone (e.g. a different icon or label text) since both are red but mean very different things — one is a deliberate risk decision, the other is a fault.

## Dismissing a halt the gate could not judge

When the daemon starts *after* the gate window, no sentiment readings exist, so `resolveGate` has nothing to judge and halts the day:

> ■ Halted for the session: no sentiment readings were taken during the first hour

(The wording is quoted as the engine prints it; it predates `timing.sentiment_window` being shortened from the hour originally specified.)

That is the right default — trading with the safety check never having run is worse than sitting out — but it is an **absence of data, not a risk decision**, and a restart at 11:00 should not automatically cost the rest of the session. The halt banner therefore carries an **Ignore** button, which resumes screening for the rest of the day.

Two things about it:

- **It only appears on that halt.** A halt the gate actually *reached*, on real readings, is the kill switch working, and it is not dismissible from the page. The two are told apart structurally — halted with zero readings — rather than by matching the message text, which would break the moment the wording changed.
- **The verdict becomes `GATE_OVERRIDDEN`, not `PROCEED`.** The gate did not pass, it was stood down, and neither the page nor the audit trail should later claim otherwise. It renders amber rather than green for the same reason. The decision is persisted, so a further restart does not re-halt the day.

The confirmation says plainly that the session then trades with no sentiment kill switch behind it. Every other rule — sizing, the stop, the position cap, the exits — is unchanged.

## Positions

One card with two tabs, **Open** and **Closed**, so the day's outcome sits beside what is still running rather than in a separate place.

**Open** carries the live marks — entry, working stop, current, peak, R multiple, P&L in dollars and percent — plus a **Close** button per row.

### The Close button

It sells whatever is still held in that position, immediately, at the market's current mark. It exists because the opposite need is real: an operator watching a position go wrong should not have to kill the daemon or open the broker's own UI to get out of it.

What it does *not* do matters as much:

- **It cannot open anything.** The page has no path to `enterPositions`. 
- **It does not ask first.** The click sends the sell; there is no browser confirmation (removed 2026-09-30, on request). The result modal reports what filled.
- **It goes through the same path as a strategy exit** — `engine.submit` then `store.ClosePosition` — so the order record, the audit trail and the realised P&L are built exactly the way a rule-driven exit builds them. It is not a shortcut with its own accounting.
- **It can take up to ten seconds, and it can fail after sending.** Like every order it waits for the broker to report the fill, and cancels whatever is still working after that. A sell that filled nothing, or only part, is refused with the count that sold: the position stays open with whatever is still held, rather than the page reporting a close the broker did not make. The Open button behaves the same way — a buy that filled nothing opens nothing, and a partial fill opens the shares that filled.
- **It records the exit reason as `MANUAL`**, kept distinct for the same reason `RECONCILED` is: nothing reading the trail or the closed-positions table later should attribute an operator's decision to a strategy rule.
- **It is refused when the exchange is shut.** A sell submitted then would be queued to the next open while the store had already marked the position closed, and the two would disagree until a restart reconciled them. A page reporting a flat book the broker does not have is worse than a refusal. Pre-market it is allowed, routed to the extended-hours book like any other pre-market order.
- **A second press cannot send a second order.** The engine holds a per-position guard while one is in flight, and re-reads the row before acting — the id came from a page that may be a poll interval old and may describe a position the trading loop has since exited.

**Closed** lists everything closed on one day — selected by when it closed, not when it opened, since a manual position may have been opened in an earlier session — symbol, shares, opened, closed, P&L in dollars and percent, and which rule (or `MANUAL`) closed it — with that day's net P&L and win/loss count underneath.

Each row has a **▸ arrow** before the symbol. Clicking it folds out a line underneath with the exact time the position was **opened** and **closed** (ET, to the second, with the date — a manual position can close sessions after it opened) and how long it was **held**. Which rows are open is kept in the browser, keyed by position id, and re-applied after every poll; like the shown day, it is not remembered across a reload.

It is visible from the moment something closes, not only after the bell. It used to be an "End of day" card hidden until the forced-exit mark, which was reasonable while a strategy rule was the only thing that could close a position; once the page can close one by hand, an operator who has just sold something cannot be made to wait four hours to see what it made. After the close it reads the same way the end-of-day summary did.

### Stepping back through earlier days

The list is headed by **‹ date ›** arrows, so the whole trade history is reachable from the page rather than only the current session. A few things about them:

- **The totals stay one day's.** Net P&L and the win/loss count always describe the day shown above them, never a running total — the daily figure is the one an operator compares against the day before.
- **The arrows step to days that have closes**, not to the next calendar day, so there is no weekend or quiet Tuesday to click past. An arrow with nothing in its direction renders disabled. Forward always reaches **today**, even when nothing has closed today, so stepping back is never a one-way trip out of the live session.
- **Which day is showing is scoped to that one block.** Everything else on the page — state, positions, screening, the account — describes the live session regardless of where the closed list is parked.
- **It travels on the fragment poll's query string** (`?closed=YYYY-MM-DD`), because the rows are rendered by the server and the poll replaces the whole content block; without it the list would snap back to today every twelve seconds. The arrows carry the date they lead to, so the calendar arithmetic — and the question of which days have anything to show — stays in Go rather than being re-derived in JavaScript. An unusable or future date shows today rather than erroring: it is one browser's navigation, not an instruction the agent has to honour.
- **It is deliberately not remembered** across a reload, unlike the tab choice. Reopening the page should land on the live session, not where someone was browsing yesterday.

The selected tab is remembered per browser, because the page swaps its whole content block on every poll and would otherwise snap back to **Open** every twelve seconds.

## Screening section (visible while `SCREENING`)

Every row not already held carries an **Open** button — on a qualifying row it overrides the setup gate; on a row that failed the screen (drawn dashed) it overrides the screen as well, and the result modal says the symbol did not qualify and why. The audit event records `screen_qualified` and `screen_reason` either way. Offered on failing rows on request (2026-09-30); before that only qualifying rows had it.

### The Open button

It buys that candidate now, **overriding the setup gate** — the rule the whole strategy turns on. It exists because a human reading a chart can see a pattern the mechanical detector cannot: `FindSetup` wants a surge, a 1–2 candle pause and a close above the pause candle's high on the very next candle. A name can move in a way no rule captures, or set up a candle before the scan sees it. See the KNRX case in [`decisions.md`](./decisions.md).

What it overrides is the **signal**, and only the signal:

- **Size is still computed, never chosen.** `risk.SizeForRisk` runs exactly as it does for an automatic entry: shares = risk budget ÷ distance to the stop.
- **There is always a stop.** If the chart happens to show a completed setup, its stop is used and the trade is identical to the automatic one. If not, the stop goes at `entry.max_stop_distance_pct` below entry — the widest risk the strategy accepts, which makes the position the *smallest* the risk budget allows. The confirmation says which of the two you got, and quotes the detector's reason when there was no setup.
- **The position cap, one-position-per-symbol and same-day re-entry all still apply.**
- **The kill switch does not block it**, because an instruction about one named symbol is not the thing it exists to stop — but the confirmation warns, and the audit event carries `overrode_halt`.
- **Refused inside the end-of-day window**, where anything bought is about to be force-sold, and refused with the exchange shut.

**Once open, its stop and the bell sell it — nothing else does.** A position opened here has no gap backstop, no candle trail and no scale-out at a target, but it does have the stop that sized it, and it is force-closed before the close like any other position (changed 2026-10-02, on request; between 2026-09-30 and that date it had no exit rules at all and was held overnight).

**The stop is a real order at the broker**, placed right after the entry fill: a good-till-cancelled sell stop at the 1R price, so it is enforced between scans and while the agent is not running. That is the point of it — a stop checked once per scan is a stop with a tick of latency, and on the kind of mover this screen finds that latency is most of the loss it was supposed to cap. The confirmation says it is resting. Every way of selling the position — Close, the forced exit — cancels that order first.

If the order could not be placed, the confirmation says so in an **alert**, not a footnote: the agent then checks that stop on each scan instead, which is a weaker guarantee, and an operator who believes they have a resting stop and does not will size the next decision wrongly. The same is true before the bell, where a stop order is accepted but cannot trigger until 09:30.

In the positions table a manual row shows its stop as a price, with `on tick` beside it when no order is resting, and the cell's tooltip says which rules apply. A position carried from an earlier session says `held since <date>` under its share count.

A table of tickers currently being evaluated, with a per-criterion breakdown rather than a single pass/fail — this is what lets you see *why* a candidate did or didn't qualify:

| Ticker | News catalyst today | ≥10% intraday move | ≥5x avg volume | Overall |
|---|---|---|---|---|
| e.g. `ABCD` | ✅ 2 today | ✅ +14% | ✅ 6.1x | **Qualifies** |
| e.g. `WXYZ` | ❌ 0 today | ✅ +11% | ✅ 5.3x | Fails (no news) |

Each cell shows both the pass/fail and the underlying value, not just a checkmark, so the numbers are visible without cross-referencing anything else.

The Action column carries the setup's verdict too, which is usually why a qualifying
candidate was not bought: "no setup: close 9.93 has not cleared the 10.02 pullback
high" is the common case, and seeing it is what distinguishes a quiet screen from a
broken one.

The open-positions table shows each position's **stop** and its **R multiple**, and a
position that has scaled out shows what is still held with the banked profit beside
it ("100 of 200, +$20.00"). Showing only the remaining share count would misreport
both the exposure and any P&L a reader works out in their head.

A final **Action** column says what the entry pass did with each qualifying
candidate — bought (with size and price), or why not: the position cap, already
holding it, already traded today, insufficient cash. Qualifying is not the same as
being bought, and without this the reason lived only in the log.

## Account section

A strip above the open positions showing the broker account as the trading loop last
read it: **available cash**, **equity** and **portfolio value**, with the time of the
reading beside the heading. Cash is labelled "available" and called out as what
sizing spends, because `risk.SizeForRisk` caps a position against `cash` — not
against buying power, which on a margin account is the larger and more flattering
number.

Two properties of this panel are deliberate:

- **The figure comes from the engine, not from the handler.** The web layer makes no
  broker calls (see [`architecture.md`](./architecture.md)); the tick reads the
  account and publishes a snapshot the page renders. So the refresh rate is one per
  tick regardless of how many browsers are open, and a page poll still costs nothing
  upstream. The entry path's per-candidate account read publishes too — it is taken
  after any earlier fill in the same pass, so on a pass that buys more than once the
  balance moves with the cash instead of only on the next tick.
- **Missing and stale are distinguished from zero.** Before the first successful read
  the panel says so instead of rendering `$0.00`, which would read as a drained
  account. After three scan intervals without a new reading — the tick refreshes every
  loop, so that is the account endpoint failing, not slow polling — the last known
  balance stays visible with a warning that it is no longer being refreshed. A failed
  read never faults the agent: the balance is display-only, and the trading path reads
  the account for itself before it sizes anything.

Balances are printed with thousands separators, unlike the per-share prices
elsewhere on the page: these are the only figures large enough to be misread by a
factor of ten at a glance.

## Scope of the two position views

Both views live in the Positions card described above. What is worth recording is where
they depart from the original spec, and where v1 stops:

- **Current price and unrealized P&L go beyond what was originally specified** (purchase
  price + share count). A cost basis without live performance would mean looking
  somewhere else for the number that matters day-to-day.
- **The closed view covers today's session only**, with a totals row for net P&L and a
  win/loss count so the day's outcome needs no mental arithmetic. It is no longer gated
  on the closing bell — see "Positions" above for why that changed once the page could
  close a position by hand.
- **Not yet decided:** whether a later version should support browsing prior days'
  sessions (would need `store` queries across days and a day-picker UI). Flagged as a
  possible v2 item in [`decisions.md`](./decisions.md), not to be built now.

## Active strategy section (bottom of the page)

A four-part panel at the foot of the page describing the running strategy: the
sentiment gate, the screening criteria, the entry setup and sizing, and the exits.
The exits section lists the chart stop, the gap backstop behind it and the scale-out
target separately, because which of the two stops fired is the difference between a
normal loss and a gap.

**Every value is read from the loaded configuration**, never written into the
template. That is the whole point of the section: a hardcoded panel would pass a
"does it render" test and start lying the first time a threshold was tuned — the same
drift this project has repeatedly had to correct in its own docs. Tuning
`config/config.yaml` and restarting changes what the page says.

Details that follow from that goal:

- **Derived figures are computed the way the engine computes them.** Maximum exposure
  is size × concurrency (the product config validation caps at 100%) — so the panel
  cannot disagree with behaviour.
- **Numbers are printed exactly, with only trailing zeros trimmed** (the dollar-volume
  floor additionally gets thousands separators, which insert characters without rounding). Fixed precision
  would round a tuned `3.25` to `3.2` (Go rounds half to even), and someone who had
  just set 3.25 would reasonably conclude their change had not applied.
- **Exits are listed in the order `strategy.EvaluateExit` checks them**, because the
  first match wins; any other order would misrepresent which rule takes effect. The
  heading says so rather than leaving the order to be inferred.
- **Conditional settings appear only when they apply.** A limit order's slippage
  allowance is shown for `order_type: limit` and hidden for `market`, where the value
  is set but inert. The all-negative sentiment requirement changes how the halt
  threshold is described.
- It lives **inside the polled fragment**, so it is never stale relative to the rest
  of the page.

## Refresh behavior

Settled — see "Visual design" above: lightweight JS polling every ~10–15s, patching the page in place.
