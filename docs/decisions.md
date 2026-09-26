# Decision log

A dated record of decisions made during design, and why — so the reasoning behind a choice is still visible after the choice itself has been implemented and the conversation that produced it is gone. Add a new entry when a design decision changes; don't edit history, append to it.

## 2026-09-26 — Initial design (via `grill-me` interview)

| Decision | Rationale |
|---|---|
| Go, single continuously-running daemon + status web page | User specified this directly; fits the need for persistent in-process state across the trading day. |
| Alpaca for both market data and order execution | One account/API integration instead of two; official Go-friendly API; supports paper and live modes. |
| TradingView considered and rejected as the data source | No official API for pulling data out of a retail account for automated use — would require unofficial/scraping methods with ToS and reliability risk, unacceptable for a system placing real (even if initially paper) trades. |
| Paper trading first | Explicit choice to validate the sentiment gate, screening, and exit logic before any live capital is at risk. |
| SQLite for persistence | Single-instance daemon; avoids operating a separate DB server for no benefit at this scale. |
| 10% of portfolio per position, max 5 concurrent (50% max exposure) | User-specified sizing and exposure cap. |
| Hard 10% per-trade stop-loss | User-specified; a floor independent of the momentum-based exit signal, so a bug in that logic can't produce an unbounded loss on one position. |
| Daily kill switch on overwhelmingly bearish first-hour sentiment | User-specified; halts new entries for the rest of the session rather than trading through a clearly bad tape. |
| Profit target + trailing stop (exact % not yet set) chosen over pure reversal-signal exit | User picked this as simpler to implement and reason about than a technical-reversal-based exit; exact numbers deferred pending a proposal review. |
| Forced exit 30 minutes before close, no exceptions | User-specified; strategy is day-trade only, never holds overnight. |
| No backtesting module in v1 | Not requested; paper trading against live data is the stated validation step instead. |
| Env vars + gitignored `.env` for secrets (not a secrets manager) | Appropriate for a single-instance v1 daemon; a secrets manager was explicitly deferred as only worth it if this later runs in a multi-instance/team cloud setup. |
| Status web page + local logs only for observability in v1 | User chose the minimum viable option; external alerting explicitly deferred, not built speculatively. |

## 2026-09-26 — Screening criteria finalized

| Decision | Rationale |
|---|---|
| Screening criteria: float < 10,000,000 shares, a same-day news catalyst, ≥10% intraday move, ≥5x average volume | User-specified directly; replaces the earlier open "market cap range" placeholder with a float-based criterion instead. |
| All four criteria required simultaneously (AND), not scored/weighted | Matches how the user described the requirement ("before purchasing... already up by 10%... and 5x above average volume") — a hard gate, not a ranking. |
| News catalyst = presence of a same-day headline only, no sentiment/NLP scoring | User chose the simpler v1 option; the price/volume move itself is treated as confirmation the news is what's moving the stock, not the headline's tone. |
| Screening starts only after the first-hour sentiment gate passes; no background screening during the no-trade hour | User chose the simpler option over running screening during hour 1 and queuing candidates. |

## 2026-09-26 — MACD exit added

| Decision | Rationale |
|---|---|
| Added a 4th exit trigger: MACD bearish crossover (MACD line crosses below signal line) on the 30-minute chart | User-specified as another exit condition; checked continuously alongside the other three, whichever fires first wins. |
| Non-standard MACD periods: fast=5, slow=10, signal=3 (not the conventional 12/26/9) | A single trading day has only ~13 30-min candles, too few for the slow EMA(26) in the standard settings to stabilize without reaching into prior days. The user explicitly chose to give up the standard periods in favor of an indicator that's valid within a single day's data, over the alternative of pulling in multi-day history. Values proposed by Claude and accepted by the user — tunable, not battle-tested. |
| MACD indicator uses only the current day's candles from market open, no multi-day lookback | Follows from the above — this was the whole point of moving off the standard periods. |

## 2026-09-26 — Status page spec

| Decision | Rationale |
|---|---|
| Two separate header indicators: market-hours badge (green/red) and agent-status badge (5-state legend) | These can diverge (e.g. market open but agent in its no-trade first hour) — merging them into one indicator would hide that distinction. |
| Agent status states: `MARKET_CLOSED`, `SENTIMENT_CHECK`, `SCREENING`, `HALTED_BEARISH`, `ERROR` | Proposed by Claude, accepted by user. `ERROR` is deliberately kept visually distinct from `HALTED_BEARISH` (both red) since one is a fault and the other a deliberate risk decision. |
| Screening table shows a per-criterion breakdown (float/news/%-move/volume), not just overall pass/fail | User chose this explicitly — makes it possible to see *why* a candidate qualified or didn't, not just whether it did. |
| Open positions show current price + unrealized P&L, in addition to the originally-specified purchase price + share count | User confirmed adding this — a position's cost basis without live performance would require checking elsewhere for the number that matters day-to-day. |
| End-of-day summary scope is today's session only, not a browsable multi-day history | User chose the simpler v1 option; multi-day browsing flagged as a possible later addition below. |

## 2026-09-26 — Gap review (via `grill-me`)

A full read-through of the spec surfaced several gaps. Resolutions below; unresolved items moved into "Open items."

| Decision | Rationale |
|---|---|
| Neutral/ambiguous first-hour sentiment is treated the same as bullish (proceeds to screening), not the same as bearish | User's explicit choice. Only a clear overwhelmingly-bearish reading halts trading now — `strategy.md`'s three-way classification collapsed to a two-way one (overwhelmingly bearish vs. not). |
| Candidate universe: try Alpaca's most-actives/screener data first, before adding a dedicated screener API | Consistent with the "try Alpaca first" pattern already used for float/news data. Not yet confirmed sufficient — see open items. |
| MACD exit moved from the 30-minute chart to the 15-minute chart (periods unchanged: fast=5, slow=10, signal=3) | The 30-minute version needed 5 hours of same-day data to compute at all, active only in roughly the last hour before the forced EOD exit. Moving to 15-minute candles halves that to 2.5 hours. User chose this fix over shrinking the periods further or reverting to multi-day lookback. Explicitly a mitigation, not a full fix — positions closed before ~12:30pm still can't trigger this exit. |
| PDT rule flagged as a hard blocker for live trading, not for paper trading | Account is under $25k (or funding undecided) as of this date; the strategy's up-to-5-day-trades-per-day design would trip FINRA's PDT restriction almost immediately if run live as-is. No fix chosen yet — see open items. |

## 2026-09-26 — Status page visual design

| Decision | Rationale |
|---|---|
| Server-rendered Go `html/template` + modern CSS, no JS framework | User wants "modern and stylish" but this is achievable with styling alone; a JS frontend framework would add a build pipeline and second toolchain to what's otherwise a single Go binary, for a page that's read-only anyway. |
| Dark theme, no light/dark toggle in v1 | User's choice — fits a trading-terminal feel; one theme done well over two done halfway. |
| Live updates via JS polling every ~10–15s, patching in place | Resolves the previously-open "refresh behavior" question — user chose this over WebSocket push (more live but adds a persistent-connection component) or manual-reload-only (simplest but not "modern"). |

## 2026-09-26 — Screener re-scan frequency settled

| Decision | Rationale |
|---|---|
| Screener re-scans the candidate universe every 1 minute once the sentiment gate opens | User's choice over reusing the 10-minute sentiment-poll cadence or a full streaming/continuous approach — tight enough to catch fast-developing low-float moves without needing a real-time data stream. |

## 2026-09-26 — Implementation

Built in Go against the design above. Decisions marked **claude-proposed** filled
gaps the design left open so the agent could run; they are working defaults in
`config/config.yaml`, not settled positions, and are the first thing to review.

### Values filled in (claude-proposed, awaiting review)

| Value | Chosen | Reasoning |
|---|---|---|
| Profit target / trailing stop | +15% / 5% | Plausible for a low-float momentum day-trade; no backtest supports these numbers. |
| Average-volume lookback | 20 sessions, excluding today | 20 is the conventional window. Today is excluded because today's volume is the number being compared against the average. |
| Sentiment classification | Avg of SPY/QQQ/IWM ≤ −0.8% **and** none positive | Needed a concrete rule to implement the gate at all. IWM is in the basket because the strategy trades small caps, so a small-cap index belongs in the read. |
| Order type | Market | Guarantees the exits actually happen, which matters more than slippage while paper trading. The low-float slippage risk noted in the design is real and unaddressed. |
| Tie-break when candidates exceed free slots | Highest relative volume first | Best separates a genuine liquid move from a thin drift. |
| Same-day re-entry | Disabled | Avoids repeatedly buying back into a name that already stopped out. |
| Universe size | Top 50 movers per scan | Bounded API cost at a one-minute cadence. |
| Position re-mark interval | 15s | Exit rules need a reasonably fresh price without hammering the API. |

### Gaps closed during implementation

| Decision | Rationale |
|---|---|
| Market holidays and early closes come from Alpaca's calendar endpoint, not a hardcoded list | A hardcoded holiday table silently rots every year. This also resolved the holiday/early-close open item properly. |
| Restart reconciliation implemented: broker positions are the authority | Closes the flagged crash-between-submit-and-confirm hole. Adopts unknown broker positions, closes vanished local ones as `RECONCILED`, resolves unconfirmed orders. |
| New `RECONCILED` exit reason, distinct from the four strategy exits | So the end-of-day summary never attributes an operational disappearance to a strategy rule. |
| New `EOD_WINDOW` agent state (a sixth) | The five documented states could not describe the window between the forced-exit mark and the close without either claiming the market was closed or implying entries were still possible. |
| Float fails **closed** when unavailable | An unverifiable float is a failed criterion, never a skipped one; the alternative silently drops a documented entry requirement. `float_provider` accepts only `none`, so today nothing can qualify — and the status page says so. |
| Starting after the first hour halts the day | With no sentiment readings the gate cannot be judged, and trading without the safety check having run was never the intent. |
| Live trading needs `-allow-live-trading` in addition to the live base URL | The PDT constraint is unresolved; a base-URL typo should not be able to start real trading. |
| Config is validated at startup, including exposure > 100% | These failures are much cheaper before positions are open than during a session. |

### Corrections to the design

| Correction | Detail |
|---|---|
| MACD warm-up is 13 bars (≈3h15m), not 10 (2.5h) | The slow EMA is seeded with a simple average, the signal line is an EMA *of* the MACD line, and detecting a crossing needs the previous bar: warm-up is `slow + signal`. The trigger first becomes available around 12:45pm ET, so the dead zone the 15-minute switch was meant to shrink is larger than estimated. `strategy.md` §4 updated. |
| Price thresholds needed an epsilon | Thresholds are products like `peak*0.95`; `6.00*0.95` evaluates to 5.699999999999999, so a price of exactly 5.70 failed an "at or below 5% off the peak" test. Found by a test that expected the documented boundary behaviour. Comparisons now treat an exact-threshold price as triggering. |
| Migrations live in `internal/store/migrations/`, not the repo root | `go:embed` cannot reach outside its package, and the single-binary deployment decision outranks the directory's location. |

### Verification performed

`go build`, `go vet` and `gofmt` clean; full test suite passes (config, scheduler,
strategy/MACD, risk, sentiment, screener, store, broker, engine, web — including a
simulated full trading day and both restart-reconciliation directions). The daemon
was then run for real via `-offline`: it progressed `SENTIMENT_CHECK` → gate
`PROCEED` → `SCREENING` → entry sized at exactly 10% of the portfolio →
`EOD_WINDOW` forced exit, with the status page and `/api/status` reflecting each
step; restarting it on the existing database re-applied no migrations, kept prior
rows, and logged no errors.

**Not verified:** every Alpaca endpoint path and payload shape. No credentials were
available, so the client was tested against a stub server that returns
Alpaca-shaped JSON. The first real paper-trading run is the actual verification of
the endpoints themselves.

## Open items (not yet decided)

- **Float data source — the blocker.** Alpaca does not expose share float, so the float criterion fails closed and **no candidate can currently qualify for entry**. A provider has to be chosen and implemented before the agent can trade at all. News headlines are covered by Alpaca's news endpoint and are working.
- **Screening candidate universe** — the movers endpoint is implemented, but whether it is available on this Alpaca plan and whether the top 50 gainers cover enough of the market is unverified against a real account (`docs/strategy.md`).
- **PDT rule resolution before going live** — fund above $25k, reduce trade frequency, or switch to a cash account; none chosen yet (`docs/operations.md`).
- Review the claude-proposed values above — especially the profit target, trailing stop and sentiment thresholds, none of which rest on evidence.
- Whether market orders are acceptable for low-float entries, or entries should use limit orders (the config supports both; `market` is the current default).
- Whether a portfolio-level daily drawdown limit is needed alongside the per-trade stop-loss (`docs/risk.md`). Still not implemented.
- Hosting/deployment target for the running daemon (`docs/operations.md`).
- Whether the status page needs a browsable multi-day session history (currently scoped to today-only) (`docs/web-ui.md`).
