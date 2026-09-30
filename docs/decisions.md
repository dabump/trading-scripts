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
| Screening criteria: a same-day news catalyst, ≥10% intraday move, ≥5x average volume | User-specified directly, replacing the earlier open "market cap range" placeholder. Note that nothing in the final set filters on company size. |
| All criteria required simultaneously (AND), not scored/weighted | Matches how the user described the requirement ("before purchasing... already up by 10%... and 5x above average volume") — a hard gate, not a ranking. |
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
| Screening table shows a per-criterion breakdown (news / %-move / volume), not just overall pass/fail | User chose this explicitly — makes it possible to see *why* a candidate qualified or didn't, not just whether it did. |
| Open positions show current price + unrealized P&L, in addition to the originally-specified purchase price + share count | User confirmed adding this — a position's cost basis without live performance would require checking elsewhere for the number that matters day-to-day. |
| End-of-day summary scope is today's session only, not a browsable multi-day history | User chose the simpler v1 option; multi-day browsing flagged as a possible later addition below. |

## 2026-09-26 — Gap review (via `grill-me`)

A full read-through of the spec surfaced several gaps. Resolutions below; unresolved items moved into "Open items."

| Decision | Rationale |
|---|---|
| Neutral/ambiguous first-hour sentiment is treated the same as bullish (proceeds to screening), not the same as bearish | User's explicit choice. Only a clear overwhelmingly-bearish reading halts trading now — `strategy.md`'s three-way classification collapsed to a two-way one (overwhelmingly bearish vs. not). |
| Candidate universe: try Alpaca's most-actives/screener data first, before adding a dedicated screener API | Consistent with the "try Alpaca first" pattern already used for news data. Superseded on 2026-09-26 — see below. |
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
| Screener re-scans the candidate universe every 1 minute once the sentiment gate opens | User's choice over reusing the 10-minute sentiment-poll cadence or a full streaming/continuous approach — tight enough to catch fast-developing moves without needing a real-time data stream. |

## 2026-09-26 — Implementation

Built in Go against the design above. Decisions marked **claude-proposed** filled
gaps the design left open so the agent could run; they are working defaults in
`config/config.yaml`, not settled positions, and are the first thing to review.

### Values filled in (claude-proposed, awaiting review)

| Value | Chosen | Reasoning |
|---|---|---|
| Profit target / trailing stop | +15% / 5% | Plausible for a momentum day-trade; no backtest supports these numbers. |
| Average-volume lookback | 20 sessions, excluding today | 20 is the conventional window. Today is excluded because today's volume is the number being compared against the average. |
| Sentiment classification | Avg of SPY/QQQ/IWM ≤ −0.8% **and** none positive | Needed a concrete rule to implement the gate at all. IWM is in the basket because the strategy trades small caps, so a small-cap index belongs in the read. |
| Order type | Market | Guarantees the exits actually happen, which matters more than slippage while paper trading. The slippage risk on thinly traded names is real and unaddressed. |
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

## 2026-09-26 — Manual check buttons (via `grill-me`)

| Decision | Rationale |
|---|---|
| Two header buttons: manual sentiment check and manual candidate screen, results in an in-page modal | User request. Modal over a real popup window: `window.open` is blocked by default in most browsers and would duplicate the theme. |
| Both are strictly evaluate-and-display; neither can place an order | User's explicit choice. Enforced in the engine rather than the handler: the manual screen reuses the gathering and evaluation code but never calls the entry logic. The automated screening path *does* buy, so wiring a button straight to it would have placed orders — and the button was asked to work with the market closed, which is when that would be worst. |
| A manual sentiment check is never persisted | User's explicit choice. The gate verdict is decided by the newest stored reading, so a persisted manual check could overturn what the first hour concluded. |
| Manual screening works with the exchange closed, with a stale-data warning | Explicitly requested. Prices and volumes then describe the last session, which the modal says rather than presenting them as live. |
| POST-only endpoints, one-at-a-time per action | A GET would let a prefetch or crawler trigger upstream work; the in-flight guard stops impatient clicking from becoming a rate-limit problem. |
| A failed manual check does not put the agent into `ERROR` | A failed button press is the clicker's problem, not a fault in the trading loop; the error is shown in the modal where the user is looking. |

This is a change to a documented contract: `web-ui.md` and `architecture.md`
previously described the page as read-only and said it never drives decisions. It
still never trades, but it is now an actuator for read-only checks, and both docs
say so.

## 2026-09-26 — Screening reduced to three criteria (via `grill-me`)

| Decision | Rationale |
|---|---|
| Screening is a news catalyst, a ≥10% intraday move and ≥5x average volume — three criteria, with no size or liquidity filter | The size criterion originally specified could not be sourced from Alpaca, and the user chose to drop it rather than take on a second data provider. |
| Nothing replaces it — no price ceiling, no market-cap proxy | User's explicit choice when offered substitutes. |
| The removed configuration, interfaces and evaluation variants were deleted rather than left dormant | A disabled-but-present criterion is worse than an absent one: it implies a check that is not happening. |
| The manual screening modal says "Qualifies" | It applies the same three criteria as the automated scan, so any hedged wording would understate what it means. |

**This changes what the strategy is.** The size criterion was what made this a
small-cap strategy. The screen is now "any stock up ≥10% on ≥5x volume with a
same-day news catalyst", which admits large caps — and since the universe is
Alpaca's top gainers across all market caps, nothing restricts it to small names
despite the project still being described that way in places.

**It also unblocks trading.** The unsourceable criterion was the only thing making
every candidate fail, so the agent could not previously open a position at all. It
now can, which raises the stakes on the two things still unverified: the Alpaca
endpoint paths/payloads (never exercised against a real account) and the unresolved
PDT constraint.

## 2026-09-26 — Full-market scanning (via `grill-me`)

The user asked why the scanner only looked at 50 stocks. It was a fair challenge:
50 was a `PROPOSED` bound Claude picked to limit API cost, and it turned out to be
both unraisable and the wrong selection rule.

| Decision | Rationale |
|---|---|
| Scan every active, tradable US equity instead of Alpaca's market-movers endpoint | Verified against Alpaca's docs: `top` on the movers endpoint is hard-capped at 50, so the cap could not be raised at all. On an active day far more than 50 names clear +10%, so the list was being silently truncated. |
| Exclude OTC venues from the universe | The strategy needs liquid intraday momentum; OTC combines poor data quality with spreads that make a market order a bad idea. |
| Cache the universe once per session | The tradable asset list barely changes within a day, so re-fetching it on every one-minute scan would be pure waste. |
| Snapshots are batched internally (500 symbols per request) | The symbols travel in the query string; a full-universe call in one request would build a URL tens of kilobytes long and risk rejection by any proxy in between. |
| When more names clear the move filter than `max_enriched`, keep the busiest by **dollar volume** | This is the user's chosen fix for the ranking mismatch. The movers endpoint ordered by percentage change while the strategy ranks entries by relative volume, so the old behaviour would discard a heavily traded +11% name in favour of a thin +40% one. A test verifies this: it fails under the old %-change sort. |
| `market_data.feed` added, set to `sip` | The account is on Algo Trader Plus. This is load-bearing rather than cosmetic: Alpaca's free tier is IEX-only, carrying a few percent of consolidated volume, and a relative-volume criterion computed from it compares one exchange's activity against a market-wide average. The daemon warns loudly if configured to `iex`. |
| `universe_size` replaced by `max_enriched`, and `Movers` deleted from the broker interface | The old key described a cap that no longer exists; leaving the movers code dormant would imply a discovery path that is not used. |

**Cost profile of the change.** The scan is affordable because the cheap filter runs
first: batched snapshots cover the whole market in a couple of dozen requests, and
only the handful clearing the move threshold incur per-symbol news and
average-volume calls. A test asserts that symbols failing the move filter are never
enriched.

## 2026-09-26 — News catalyst was broken in two ways (via `grill-me`)

The user reported that every candidate showed no news catalyst. Both causes were
real bugs, each independently capable of producing that symptom.

| Bug | Cause | Fix |
|---|---|---|
| Most symbols reported zero news | The news endpoint caps a page at **50 stories** (verified against Alpaca's docs) and the implementation requested a single unpaginated page for up to 100 symbols. Sorted newest-first, the 50-story budget was consumed by whichever few names are heavily covered, so every other symbol looked newsless. | Symbols are now batched 25 per request and each batch is paginated via `next_page_token`, with a page cap and a repeated-token guard so a malformed token cannot spin forever. |
| Pre-market catalysts were invisible | The search window started at the session open (09:30 ET). The catalyst behind a gap-up almost always breaks overnight or pre-market, so the story that *caused* the move fell outside the window — the criterion was structurally unable to see the thing it was testing for. | New `screening.news_lookback` (default 18h, measured from now, validated ≥7h). Both the automated scan and the manual button use it, so they cannot disagree. |

Both fixes carry tests that were confirmed to **fail against the old code** rather
than merely passing against the new: the pagination test showed 10 of 60 symbols
wrongly reporting no news, and the window test showed the search starting exactly at
09:30.

**Still unverified:** this was all proven against a stub server shaped like Alpaca's
documented responses. Whether real Alpaca news coverage is dense enough for the
criterion to be useful in practice — and whether 18h is the right window — can only
be judged against a live account.

## 2026-09-26 — Entry pass robustness and visibility (via `grill-me`)

The user asked whether the screening logic and the buy decision are the same logic.
They are — one `screener.Evaluate` pass per scan feeds both the displayed table and
`enterPositions`, and a test asserts the manual and automated paths agree. Checking
it surfaced two things worth changing.

| Decision | Rationale |
|---|---|
| A per-candidate failure skips that candidate instead of aborting the entry pass | The handling was inconsistent: a sizing failure skipped, but an unavailable price or account fetch returned an error that abandoned every remaining qualifier and faulted the agent. At a one-minute cadence a transient blip on one symbol silently cost the others their entry. Failures that are not candidate-specific still surface as `ERROR`. |
| Each qualifying candidate's entry outcome is recorded and shown on the page | Qualifying is not the same as being bought — the cap, the re-entry rule and available cash all still apply — and the reason was previously log-only. Candidates turned away by the cap are now explained individually rather than left blank. |
| The screening snapshot is saved *after* the entry pass | So it can carry the outcome. It is saved even when entry failed, since a pass that went wrong is exactly when the candidate table matters. |

## 2026-09-26 — Audit trail (via `grill-me`)

| Decision | Rationale |
|---|---|
| Append-only JSONL under `logs/`, one file per session date | User's choice over a SQLite table. Portable, greppable, easy to ship or archive, and it closes the gap where `operations.md` described a `logs/` directory nothing wrote to. Per-day files give rotation without any extra machinery. |
| State changes and actions only, not every evaluation | User's choice. The screen runs every minute; auditing each evaluation would bury the events that matter. The screening snapshot in the database already answers "what did the screen see just now". |
| Flushed to disk on every event | A trail that loses its last entries in a crash fails at the moment it is most needed. At a few dozen events a day the cost is irrelevant. |
| `AGENT_STARTED` records the full effective configuration, minus credentials | Without it, a reader has to assume the thresholds at the time matched today's config. Credentials are excluded so the trail is safe to share. |
| A failed audit write logs loudly but does not stop the agent | The daemon may be holding open positions, and abandoning their exits to preserve a record would be the wrong trade. Considered and rejected: halting on audit failure, which is the stricter convention but the worse outcome here. |
| Repeating events are de-duplicated rather than dropped | A persistent fault records once until it changes or the agent recovers; a candidate skipped for the same reason records once per session. Being at the position cap turns away every remaining qualifier on every scan, which would otherwise mean hundreds of identical rows. |

Found while writing the tests: candidates turned away by the position cap were
setting a page outcome but were not being audited at all — a real gap, since "we saw
this qualify at 11:03 but were full" is exactly what a post-mortem wants.

## 2026-09-26 — Containerisation

| Decision | Rationale |
|---|---|
| Multi-stage build on `alpine`, not `scratch`/distroless | The binary is static so `scratch` would work, but a shell and `wget` make the container debuggable and let the healthcheck hit `/api/status` without adding a health subcommand to the binary. ~8MB for that is a good trade. |
| The timezone database is embedded in the binary | A minimal image carries no tzdata, and `mustLoadET` silently falls back to a fixed EST offset — which puts every session boundary an hour out from March to November, most of the trading year. This is the single worst class of container bug for a clock-driven system: it passes on a developer machine and is wrong in production. A DST test now guards it. |
| `go vet` and the full test suite run inside the build stage | A daemon that places orders should not be packageable while its tests fail. |
| `data/` and `logs/` are named volumes, and the demo profile uses separate ones | The database makes a restart recoverable and the audit trail is evidence; both must outlive the container, and a demo run must never write into either. |
| Config bind-mounted read-only, with a copy baked in | Thresholds change with a restart instead of a rebuild, while the image still runs unmounted. |
| Port published to `127.0.0.1` only | The page exposes positions and two manual action buttons and has no authentication. |
| 30s stop grace period, exec-form `ENTRYPOINT` | SIGTERM reaches the process and triggers the graceful shutdown already implemented; being killed mid-order-submission is recoverable via reconciliation but better avoided. |

**Not verified:** the image was never actually built. Docker is installed on this
machine but the daemon is inactive and the socket is `root:docker` while the user is
in `wheel`, so the build could not run. Verified instead, offline: the exact build
context `.dockerignore` produces was reproduced and `go vet`, the full test suite and
the static build all pass inside it; `file` confirms the binary is statically linked;
the default config's relative paths resolve to precisely the declared volume mount
points (including SQLite's WAL sidecars); `/` and `/api/status` both answer 200; and
SIGTERM produces a clean shutdown. The first `docker compose up --build` is still the
real test.

## 2026-09-27 — Code review of the containerisation change

`/code-review` on commit `0656217` returned seven findings. All seven were independently
verified and all stood. The three that mattered:

| Finding | Reality | Fix |
|---|---|---|
| Both compose services published host port 8081 | The documented demo command could never run: the main agent is detached under `restart: unless-stopped`, so `docker compose --profile demo up agent-offline` always failed with "port is already allocated". | `agent-offline` moved to 8082. The main agent stays on 8081 (a deliberate local change); the docs were wrong, not the compose file. |
| SIGTERM did not shut down gracefully, despite three places claiming it did | `main` did `err = <-errs` across two goroutines and returned on whichever finished first. `Serve` swallows `http.ErrServerClosed` and returns almost instantly, so `run()` returned — firing the deferred store and audit `Close` — while the engine could still be mid-tick submitting an order. The `stop_grace_period: 30s` was never used. It also explains why "shutting down" appeared in one manual test but was never guaranteed: it only logged when the engine won the race. | New `waitForShutdown`, extracted so it could be unit-tested. Waits for every component, triggers cancellation after the first finishes so a lone failure does not orphan the other, and bounds the wait at 15s. |
| Two tests could not fail, and one was cited in two docs as a guarantee | Verified against Go's source: `LoadLocation` consults `$ZONEINFO`, the system zoneinfo directory and `$GOROOT/lib/time/zoneinfo.zip` *before* the `time/tzdata` embed, and that zip ships with every toolchain — so removing the import broke nothing. Separately, `TestSessionBoundariesAreHostTimezoneIndependent` asserted `open.In(loc).In(ET)`, an identity operation, so it passed for every input including the degraded fixed-zone fallback it existed to detect. | `tzdata` dropped from the runtime image so the embed is genuinely the only source; the DST test's comment corrected to say what it actually guards; the tautology replaced with a test that overrides `time.Local` and asserts the real property. The container check is now documented as the only real guard on the embed. |

The remaining four (healthcheck reporting healthy while faulted, healthcheck port
hardcoded against a bind-mounted config, `.dockerignore` matching only `.env`, and the
false claims in `CLAUDE.md`/`docs/operations.md`) were fixed in the same pass: `/healthz`
added returning 503 only on `StateError`, a `-healthcheck` flag that reads the port from
config, `.env*` with `!.env.example`, and the docs corrected.

**Worth recording plainly:** two of the seven were tests of mine that could not fail,
written in the same session where the standard "a test that cannot fail proves nothing"
was applied repeatedly to the implementation. Every fix in this pass was verified by
reverting the fix and watching the new test fail.

## 2026-09-28 — Market-hours countdown on the status page

| Decision | Rationale |
|---|---|
| The market-hours badge carries a countdown: time to the close while open, time to the next open while closed | User request. |
| The engine looks up and caches the *next* session once per day (`broker.NextSession`) | Today's calendar entry says nothing about when trading resumes, so after the close — or on a weekend or holiday, which is most of the time anyone would want this — the countdown had no target. The web layer cannot call the broker itself, so the engine caches it alongside today's session: one extra call per day, not per tick. |
| A failed next-session lookup renders no countdown rather than a guess | A blank is honest; a wrong "opens in" is worse than nothing. The lookup failing also must not fault the agent — trading is unaffected. |
| Minute resolution, not seconds | The page polls every ~12s, so a seconds figure would advance in 12-second jumps and read as broken. None of these boundaries is actionable to the second, since the agent handles them itself. |
| The countdown lives in the polled fragment | Otherwise it would freeze between manual reloads. A test asserts it is present in `/fragment`, not just `/`. |
| A 10-day calendar window for the next session | Has to clear the longest closure: a holiday beside a weekend (Thanksgiving, Christmas/New Year) can leave four consecutive non-trading days. |

Not added: countdowns to the *first-hour gate* or the *forced EOD exit*, which are
arguably the more operationally meaningful deadlines for this system. Both were left out
as scope the user did not ask for; easy to add if wanted.

## 2026-09-28 — Active strategy panel on the status page

| Decision | Rationale |
|---|---|
| A four-section panel at the foot of the page, built entirely from the loaded config | User request, with the explicit requirement that tuning the config be reflected. A hardcoded panel would pass a render test and start lying the first time a number changed — the exact drift this project has had to correct in its own docs more than once. |
| Derived values recomputed the way the engine computes them | Maximum exposure (size × concurrency) and the MACD warm-up (`slow + signal` candles × interval) are shown as derived, so the panel cannot disagree with behaviour. |
| Numbers printed exactly, trailing zeros trimmed, rather than fixed precision | Found while testing: `%.1f` renders a tuned `3.25` as `3.2`, because Go rounds half to even. Someone who had just set 3.25 would see 3.2 and reasonably conclude the change had not applied — which defeats the purpose of the panel. |
| Exits listed in `EvaluateExit`'s check order, labelled as priority | The first match wins, so any other order misrepresents which rule takes effect. |
| Conditional settings shown only when they apply | `limit_slip_pct` is inert under `order_type: market`, and `require_all_negative` changes what the bearish threshold means. |
| Rendered inside the polled fragment | Otherwise it could sit stale beside live state that had refreshed. |

Verified by mutating config and observing the page follow: move 10→12.75%, size
10→6.5%, concurrency 5→8 (exposure recomputing to 52%), stop 10→4%, market→limit (the
slippage note appearing). A test table asserts each field independently against the
JSON view, so a label/value pair cannot pass by coincidental substring match.

## 2026-09-28 — Backtest, then: screening floors added, MACD exit removed

The strategy had been assembled from elicited preferences with no evidence behind it.
The backtest that followed is the first measurement of whether it makes money, and the
honest summary is **no demonstrated edge**: 1,200 symbols sampled from the 13,192 the
agent's own filter admits, 2024-09-01 → 2026-09-20, 510,231 daily bars, candidates
confirmed against 15-minute bars at the true 10:30 ET decision point with no
look-ahead. The raw premise (buy 10:30, sell 15:30) returned a mean of **−2.72%** per
trade. With the agent's real exit rules the mean turns **+0.54%** — but the *median*
trade loses **4.48%**, so the whole result rests on the 24.6% of trades that reach the
trailing stop. And 0.25% of slippage per side takes the two-year equity curve to
**−2.5%**, which is not a conservative assumption for market orders on stocks that just
moved 10% on 5× volume. Full method, tables and limitations are in the plan file the
backtest produced; the limitations (9.1% sample, no news/sentiment gate applied,
survivorship bias, stop fills at exactly their trigger price) all push the real result
*worse*, not better.

Two changes follow from it. The other two recommendations — model execution before going
further, and do not go live — are not code changes and remain open.

| Decision | Rationale |
|---|---|
| Added `screening.min_price` ($1) and `screening.min_dollar_volume` ($1M today) as a **gate ahead of enrichment**, not a fourth criterion | The candidate list was dominated by instruments the strategy was never meant to hold: warrants (`GIBOW` $0.025, `LVWR.WS`), SPAC units (`QETAU`, 20-session average volume of **15 shares**) and sub-$1 names (`ASBP`, 371 shares/day). `broker.TradableAssets` excludes OTC but not warrants, units or penny stocks, and removing the float criterion left nothing else keeping them out. |
| Untradable symbols are dropped silently rather than shown as failing candidates | Tradability is not a momentum judgement. A $0.07 warrant is not a candidate that failed the screen; it is not a candidate. Showing it would bury the real names under noise the strategy will never act on. |
| The gate runs before the news and average-volume lookups | Same reason the move filter does: those are the per-symbol calls that make a full-market scan expensive. Ordering it after would pay for data on names already excluded. |
| Removed the MACD bearish-crossover exit, and with it `internal/strategy/macd.go` and `MarketData.IntradayBars` | It fired on **24.9% of trades for a mean of −0.08%** — statistically indistinguishable from not having it — while costing a bar request per open position per tick and carrying a 13-candle warm-up that left it unavailable before ~12:45pm ET. Retuning was rejected: nothing in the data suggested better periods existed, and the warm-up dead zone is structural to computing MACD inside one session. `IntradayBars` had no other caller. |
| Exit priority is now three triggers: forced EOD, stop-loss, trailing stop | Unchanged relative order; only the removal. `risk.md`'s requirement that the stop-loss outrank the momentum signals still holds. |

A correction to something said earlier in the session: I claimed the research
"confirmed" that ranking candidates by *highest* relative volume is inverted. On the raw
premise it looked that way (the 25–100× bucket returned −7.2%), but with the exits
modelled the effect largely disappears and is not monotonic across buckets. The
inversion is **not** supported once the full strategy is simulated, and the ranking was
left alone.

Verified by removing the floors from `gatherCandidates` and watching
`TestUntradableMoversAreRejectedBeforeEnrichment` fail on all four assertions — including
an order actually placed for the $0.07 warrant.

## 2026-09-28 — One-year backtest of the current strategy set

Measured with `cmd/backtest`, which imports `screener`, `sentiment`, `risk` and
`strategy` rather than reimplementing them, so the numbers describe the rules
`cmd/agent` actually runs.

**Method.** 2025-09-28 → 2026-09-26, 250 sessions, the **whole** tradable universe
(13,192 symbols), real Alpaca SIP data, split-adjusted, 5-minute bars. A superset
pre-filter on daily bars reduced the period to 7,246 candidate symbol-days; each was
then replayed at every 5-minute boundary from 10:30 to the forced exit, with volume
accumulated only from bars already closed, news counted only from stories already
published, and the sentiment gate evaluated from SPY/QQQ/IWM at 10:30. No look-ahead.
Unlike the earlier 2024-2026 run this one models the agent's **all-day scan cadence**
rather than a single 10:30 decision, and applies the **news filter and the sentiment
gate**, neither of which the earlier run included.

**Result: the strategy as configured loses money, and not marginally.**

| | as configured | with 0.25% slippage/side |
|---|---|---|
| Trades | 1,697 | 1,686 |
| Win rate | 42.2% | 38.2% |
| Mean / trade | **−1.70%** | **−2.13%** |
| Median | −1.55% | −2.11% |
| t-statistic | **−7.80** | **−9.76** |
| Equity from $10k | **$564** (−94.4%) | $289 |
| Max drawdown | 95.1% | 97.5% |

At t ≈ −10 over 1,697 trades this is not noise. 33 sessions were halted by the
bearish gate; 1,242 symbol-days were turned away by the tradability floors added
earlier the same day.

**The exits are the problem, and specifically the profit target and trailing stop.**

| exit | share | mean | mean MFE | exits at | left on the table |
|---|---|---|---|---|---|
| Stop-loss | 37.4% | −10.69% | +13.73% | −10% | +18.95% |
| Trailing stop | 23.2% | +9.31% | **+53.43%** | +9.31% | **+44.05%** |
| Forced EOD | 39.5% | +0.34% | +6.08% | — | +3.09% |

A trailing stop armed at +15% and trailing 5% cannot exit above +9.25%, and in
practice exits at +9.31% — while those same positions go on to average **+53%**. The
rule caps the right tail at +9% and leaves the left tail at −10.7%. At a 42% win rate
that payoff ratio cannot be profitable, and the measurement says it is not.

Removing it is the single largest available improvement. Every structure that keeps a
profit target scores t ≈ −10; every structure without one scores t ≈ 0:

| structure | trades | mean | t | equity |
|---|---|---|---|---|
| as configured (10 / 15 / 5) | 1,686 | −2.13% | −9.76 | $289 |
| stop 20% + forced EOD | 1,319 | +0.33% | +0.36 | $8,719 |
| **stop 10% + forced EOD** | 1,601 | **+0.18%** | **+0.28** | **$8,483** |
| stop 15% + forced EOD | 1,431 | +0.11% | +0.15 | $7,726 |
| forced EOD only | 1,064 | +0.10% | +0.08 | $6,064 |
| stop 5% + forced EOD | 1,727 | −0.11% | −0.23 | $6,065 |
| take-profit +10% + 10% stop | 1,715 | −2.04% | −9.95 | $314 |
| tight: 3% / +8% / 2% | 1,760 | −1.54% | −10.05 | $697 |

A 150-cell sweep of stop × target × trail (3–25% / 5–20% / 0–8%) was run as well.
**Every cell loses money**; the best ends at $727. No tuning of the current exit
structure rescues it — the structure itself has to change.

**Position size, not the exits, decides the account.** On stop 10% + forced EOD the
per-trade mean barely moves with size, but the equity curve does, because 10% of a
falling balance compounds:

| size | mean | equity | max drawdown |
|---|---|---|---|
| 2% | +0.29% | $10,695 | 23.1% |
| 5% | +0.26% | $11,237 | 48.5% |
| 10% (current) | +0.18% | $8,483 | 79.2% |
| 20% | +0.28% | $4,842 | 96.4% |

**Two entry findings, incidental but load-bearing.** The relative-volume tie-break
ranks backwards: 5–8x returns −1.46% and 12–20x returns −2.81%, so preferring the
highest relative volume systematically picks the worse name. And the bigger the gap,
the worse the trade: +15–25% returns −0.90%, over +100% returns −3.13% with a median
of −10.00%. The largest movers in the sample — `DSY` +732%, `CPHI` +729%, `SKK` +597%
— all stopped out.

| Decision | Rationale |
|---|---|
| Nothing changed in the strategy yet; this entry records the measurement only | The user asked what the strategy would do and how to improve the exits, not for the changes to be applied. The recommendations are listed under open items below. |
| `cmd/backtest` added to the repo rather than kept as a throwaway script | The plan this came from requires re-measuring after any `screener`/`strategy` change, and a measurement tool that drifts from the daemon reports on a strategy nobody runs. It imports the production packages and has its own tests for the pre-filter's superset property and the ambiguous-bar exit ordering. |

A correction to the earlier 2024-2026 backtest recorded above: it reported mean
**+0.54%** per trade with the exits applied and concluded the exits were what saved a
losing premise. With the full scan cadence modelled — the agent screens every minute
all day, not once at 10:30 — that reverses. The earlier figure was an artifact of
testing a single entry moment per session. The exits do not save the strategy; the
profit target and trailing stop are its largest single loss.

## 2026-09-28 — Recommendations applied, and one reverted on measurement

Acting on the backtest recorded above. Three of the five recommendations survived
re-measurement, one was a no-op, and one was **implemented and then backed out**
because measuring it after the other changes showed it did harm.

| Decision | Rationale |
|---|---|
| **Removed `exit.profit_target_pct` and `exit.trailing_stop_pct`**, and with them `strategy.TrailArmed`, `domain.ExitTrailingStop`, `Position.TrailArmed`, the `trail_armed` column and the status page's "trailing armed" pill | The measured cost of the rule. Armed at +15% and trailing 5% it could not exit above +9.25% and exited at +9.31%, while the same positions went on to average +53%. Over 1,697 trades removing it moved the per-trade mean from −2.13% to +0.18% and the t-statistic from −9.76 to +0.28. `EvaluateExit` is now two triggers: forced EOD, then stop-loss. |
| **`risk.position_size_pct` 10% → 5%** | Per-trade expectancy is nearly independent of size; the equity curve is not, because a fixed fraction of a moving balance compounds. At 0.25% slippage, 10% ended the year at $8,483 with a 79% max drawdown against 5% at $11,237 with 48%. 2% was better still on drawdown ($10,695 / 23%) — the user chose 5%, accepting the deeper drawdown for the higher terminal equity. Maximum exposure is now 25%, not 50%. |
| **`risk.stop_loss_pct` left at 10%** | The recommendation was "do not tighten", and that is satisfied by changing nothing. Widening to 15–20% measured slightly better ($10,641 and $12,620 at 5% size against $11,237) but the differences are well inside noise on a t-statistic near 0.3, and `risk.md` treats 10% as settled. Not worth churning a settled risk parameter for an insignificant gain. |
| **Candidate tie-break left alone** | Measured, not assumed. Ranking by highest relative volume (current), lowest relative volume, smallest move, largest move and highest price all land between −0.57% and −0.67% mean, t between −1.2 and −1.5. No ordering is distinguishable from another. The earlier per-bucket hint that the ranking was inverted does not survive a direct test — which is the second time that particular claim has failed to replicate, and it should now be considered settled as *no effect*. The tie-break only binds when slots are scarce, so this is the expected result. |
| **A ceiling on the entry move was added and then removed** | This is the important one. Under the old exits the widest gaps were clearly the worst trades: above +100% returned −3.13% against −0.90% for +15–25%, roughly three standard errors apart. `screening.max_intraday_pct` was implemented on that basis. Re-measured *after* the profit target and trailing stop came out, every ceiling was worse than none: none → +0.18% mean and $8,483, +50% → −0.59% and $4,262, +100% → −0.58% and $3,687. The reason is the reason the exits changed — once winners are allowed to run, the extreme movers **are** the right tail; the best trade in the year is +324%. The setting, its validation, its panel row and its tests were all backed out. |

**The generalisable lesson, which is worth more than the changes:** the move-ceiling
finding was real, well-powered and correctly derived — and wrong, because it was
derived under a rule set that then changed. Screening and exits are not independent.
Any future entry-side finding has to be re-measured against the exits in force, and
`cmd/backtest` keeps `reportMoveCaps` specifically so this conclusion can be re-tested
rather than trusted.

**Where the strategy now stands**, one year, full universe, 1,591 trades:

| slippage/side | mean | median | equity from $10k | max drawdown |
|---|---|---|---|---|
| 0.00% | **+0.76%** | −3.91% | **$16,453** | 38.4% |
| 0.10% | +0.55% | −4.20% | $13,611 | 45.3% |
| **0.25%** | **+0.26%** | −4.49% | **$11,237** | 48.5% |
| 0.50% | −0.32% | −5.10% | $6,962 | 59.3% |

Win rate 35.1%, best trade +324%, worst −42%. The profile inverted: it now loses on
most trades and makes its money on a minority of large winners, which is what the
removed rules were preventing.

**It still has no statistically significant edge.** t = 1.20 with no slippage and
lower with any, against a threshold of about 2. The changes removed a proven loss
(t = −9.76) and left something indistinguishable from zero. Execution quality is now
the whole question: the difference between 0.10% and 0.50% slippage is the difference
between +$3,611 and −$3,038. Measuring realised slippage in paper trading is the next
step, and limit orders are the obvious lever — `execution.order_type` already supports
them.

**The PDT blocker is unchanged.** The worst rolling five-session day-trade count is
**63** against a limit of 3 for a sub-$25k margin account.

## 2026-09-28 — Aligned with the discretionary small-cap momentum approach

The strategy's screen was always modelled on the well-known small-cap momentum
approach, but its *execution* was not: it bought extension at whatever price the scan
read, stopped out at a fixed percentage, sized by a fixed fraction of the portfolio
and sat out the first hour. Those are the parts where the approach's claimed edge would
live, and a one-year backtest of the screen-only version measured no edge at all.
Float is excluded throughout — Alpaca has no float data, and it stays an open item.

| Decision | Rationale |
|---|---|
| **A setup gate before any entry** (`strategy.FindSetup`): a 1–5 bar pullback whose high the next bar closes above, with price over the 9-EMA and the session VWAP | This is the change everything else hangs off. Screening says a name is interesting; the pattern says whether now is the moment. Its real output is not the entry price but **the stop** — the flag's low — which is what makes risk per share knowable before sizing. |
| **The stop comes from the chart**, capped at `entry.max_stop_distance_pct` (4%), and a wider setup is **refused** rather than sized around | Refusing is part of the approach, not a safety rail: with risk that wide the size has to shrink until a winner cannot pay for the losers. `risk.stop_loss_pct` (10%) demotes to a gap backstop, and validation requires it to stay the wider of the two or it would fire first and silently restore a fixed-percentage stop. |
| **Sizing from the stop** (`risk.SizeForRisk`): `shares = (equity × 1%) ÷ (entry − stop)` | Fixed-fraction sizing makes dollar risk swing with volatility, so the account's worst days are decided by which setups happened to be wide. The notional cap had to move to 33% — below `risk_per_trade ÷ max_stop_distance` (25%) it binds on every trade and sizing quietly reverts to a fixed fraction. A test in `internal/config` guards that ratio. |
| **Concurrency 5 → 3** | The approach runs a few positions at a time, not a basket. Also what makes the 33% notional cap fit under 100% exposure. |
| **Scale out at 2R, keep the runner**, stop to breakeven | The approach takes partial profits into strength. Stated in multiples of the trade's own risk rather than a fixed percentage, because a trade risking 2% and one risking 4% should not take profit at the same price move. Deliberately *not* the whole-position target that measured t = −9.76: a position too small to split is left to run for the same reason. |
| **Sentiment window 1h → 5m**, and a new `timing.entry_window` (2h) | An hour of kill switch meant sitting out the single most important hour: these moves begin in the first fifteen minutes. 09:30–09:35 is still not tradeable, which is a real and deliberate cost. The window then *stops buying but not screening*, so the afternoon page still shows what is setting up. |
| **A price ceiling** (`screening.max_price`, $20) | The band the approach trades. Above it a 10% move on 5× volume is a different event that a pullback entry does not describe. |
| **`MarketData.IntradayBars` re-added** | Deleted when MACD went; the setup needs a chart, and a chart cannot be batched across symbols. It runs only on candidates that already cleared all three criteria — a handful of names, not the market. |
| **Not automated, and not automatable here:** the Level 2 and time-and-sales reading this pattern is normally traded with | If a meaningful part of the edge lives in tape reading, this implementation cannot capture it. That is a limitation of automating the approach, not something tuning fixes, and it is the most likely explanation for any gap between published discretionary results and what follows. |

### What it measures

Same method as the run above, but replayed on **1-minute** bars because that is the
candle the setup is defined on — replaying at any other size measures a different
strategy, so `cmd/backtest` now defaults its bar size to `entry.pattern_interval`.

**The setup gate is brutally selective, and that is the headline.** Of 4,569
symbol-days clearing the tradability band, 2,144 passed all three screening criteria
at some point — and **55** printed a setup inside the entry window. The gate rejects
97.4% of screen-qualifying candidates, taking the year from 1,591 trades to 55. The
conjunction is what does it: three criteria true at the same minute, *and* a 1–5 bar
flag reclaiming the high of day, *and* price over EMA and VWAP, *and* a stop inside
4%, *and* all of it before 11:30.

| | before (screen-only) | after (aligned) |
|---|---|---|
| Trades | 1,591 | **55** |
| Mean / trade | +0.76% | **−0.28%** |
| t-statistic | 1.20 | **−0.53** |
| Median | −3.91% | −1.59% |
| Win rate | 35.1% | 36.4% |
| Worst trade | −42.2% | **−4.7%** |
| Max drawdown | 38.4% | **11.0%** |
| Equity from $10k | $16,453 | $9,623 |
| Worst 5-session day-trade count | 63 | **4** (limit 3) |

**Risk control improved enormously; expectancy did not.** Stop-losses now average
−1.19% instead of −10.65%, the worst trade in the year is −4.7% instead of −42%, and
drawdown fell by three quarters. The PDT problem went from structurally impossible (63
day trades per five sessions) to nearly compliant (4 against a limit of 3). But the
per-trade mean is slightly *negative*, and at 55 trades nothing here is
distinguishable from zero in either direction.

**The entry window is supported.** Concentrating early is measurably better: 2h ends
at $8,871 against 4h at $7,162 and all-day at $5,582 (t = −4.24). Trading later in the
day is the clearly worse choice.

**No configuration tested is profitable.** Stop width 2–8%, risk 0.5–3%, target
1.5–5R, scale-out fraction 25–90%, five entry windows, five tie-breaks, seven move
ceilings — the best cell ends at $9,741. Nothing was tuned toward those numbers.

| Decision | Rationale |
|---|---|
| Settings left at their stated values despite better-looking cells | A 3% stop ($9,631) and a +35% move ceiling ($9,741) both beat the current configuration, and both are inside noise at n = 55. Tuning to them would repeat exactly the mistake recorded above, where a well-powered finding derived under one rule set reversed when the rules changed. |
| The scale-out fraction stays at 50% even though selling more measured better | Selling 90% at the target ends at $9,217 against 50% at $8,871 — the *opposite* of the earlier finding that truncating gains is fatal, and for a coherent reason: with a 3–4% chart stop and a breakeven stop behind it, the runner is usually stopped before a tail develops, so there is less tail to protect. Interesting, not significant, and recorded rather than acted on. |

## 2026-09-28 — Pre-market session added

The US market trades from 04:00 ET and the agent ignored it entirely, which is
awkward for a strategy built on gap-and-go setups: the catalyst breaks overnight,
the name is already up 40% by 08:00, and 09:30 is where the move is *revealed*
rather than where it starts. `news_lookback` already reached back past the open for
exactly that reason — the agent was reading pre-market news while refusing to look
at the pre-market tape.

**Scope.** Pre-market only. Post-market was considered and deliberately left out:
the forced end-of-day exit flattens the book before the close, so after 16:00 there
is nothing to manage, and opening a position into the 16:00-20:00 session would mean
holding overnight, which this strategy never does. Adding an `AFTER_HOURS` state
would have been UI for a phase with no behaviour behind it.

**What was decided, and why.**

- **A `premarket` config section, not more fields on `screening`.** Two of the
  screening numbers have to differ before the bell, and the reason is structural
  rather than a matter of taste: a 5x test against a 20-session *daily* average is
  unreachable at 06:00, and the $1,000,000 dollar-volume floor rejects essentially
  the whole early tape. `screener.Thresholds`, resolved once per pass by
  `ThresholdsFor`, is what makes "which numbers were these candidates judged
  against" a question with one answer. The price band and the move threshold are
  deliberately *not* split — a name is in the strategy's band, or moving enough,
  regardless of which session prints it.
- **Its own cadence.** One scan is ~130 mostly-serial requests. A 04:00 start at the
  regular one-minute cadence adds ~330 passes before the bell for a tape that barely
  moves between prints, so `premarket.scan_interval` defaults to 5m and
  `premarket.start` to 07:00 rather than 04:00.
- **Screening and entry are separate switches, and entry defaults off.** With
  `allow_entry: false` the agent scans, the page fills, and every qualifying row
  carries `pre-market entry is disabled` in the Action column — the same reasoning as
  the closed entry window, where an empty table reads as a broken scanner rather than
  a deliberate stand-down.
- **Pre-market entry needs extended-hours limit orders.** Alpaca rejects a market
  order outside 09:30-16:00 outright, so `allow_entry` with
  `execution.order_type: market` would fail on every attempt and show up only as a
  rejected order in the trail. Config validation refuses the combination at start-up
  instead. Exits carry the same flag while the phase is pre-market, so a position
  opened at 07:00 can stop out at 08:00 rather than waiting three hours for the bell.
- **A live sentiment read stands in for the gate.** The first-hour gate's readings
  are taken after the open, so pre-market entry cannot inherit the kill switch.
  Rather than trade without one, a pass that may buy reads the same basket through
  the same classifier and withholds entry on an overwhelmingly bearish tape — and
  equally when the read fails, because no answer is not a passing answer. It is *not*
  persisted: `resolveGate` judges the session on the stored readings, and a 07:30
  sample must not settle the day. This is a weaker guarantee than the gate, and one
  of the reasons `allow_entry` defaults to false.
- **The snapshot decoder now reads the daily bar's date instead of trusting its
  name.** Which bar is "today" changes before the bell, and the old code took
  `prevDailyBar` as yesterday unconditionally. If Alpaca's `dailyBar` has not rolled
  over by 07:00, that computes every pre-market move against the close from *two*
  sessions ago — and that percentage is the entire basis of the screen. Reading the
  bar's own date is correct under either rollover behaviour, which is the point,
  because the behaviour could not be verified against a live account.

**Not measured.** `cmd/backtest` fetches 09:30-16:00 bars, so it has no pre-market
data to sweep and every `premarket.*` value is a reasoned default rather than a
measured one. This is the opposite of how the regular-session thresholds were
arrived at, and it is why the recommendation is to run with `allow_entry: false` and
read the candidate table for a while first.

### Three things the first real pre-market run corrected

Run against a live account at 06:15 ET, the feature did not work, in two separate
ways. Both are worth recording because both were listed above as unknowns and both
turned out to matter.

**1. `premarket.start` shipped at 07:00, so the agent sat in `MARKET_CLOSED` while
the market was in pre-market.** The reasoning for 07:00 was API cost, and it was the
wrong thing to spend. `PRE_MARKET` is a claim about what the exchange is doing, so a
start later than the exchange's own (04:00 ET) makes the agent report something
false — and from the outside it is indistinguishable from the feature being broken.
The default is now 04:00, the cost is absorbed by the 5m cadence instead (~66 passes
across the session, ~26 requests a minute against Alpaca's 200/min), and a test on
the shipped config fails if the start drifts past 04:00 again.

**2. The snapshot endpoint carries no pre-market volume at all**, which was the open
question flagged above, now answered from live data:

```
TSLA at 06:16 ET on Mon 2026-09-28
  latestTrade   p=369.36  t=2026-09-28T10:16Z
  dailyBar      c=372.11  v=46230933  t=2026-09-25   <- Friday, not today
  prevDailyBar  c=377.94  v=26410149  t=2026-09-24   <- Thursday
GET /v2/stocks/bars?timeframe=1Day&start=2026-09-28  -> {"bars":{}}
```

So `dailyBar` does **not** roll over before the bell, and no daily bar for today
exists to roll into. Two consequences:

- The date-aware decoder was the right call and is doing real work: `PrevClose` comes
  from `dailyBar` (Friday's 372.11), not `prevDailyBar` (Thursday's 377.94). The
  original code would have measured every pre-market move against a close one session
  too old.
- `TodayVolume` is **0** for every symbol before the bell. That zero failed the
  dollar-volume floor and the relative-volume criterion, so the first live scan found
  66 movers and rejected all 66 — an empty screen regardless of how the thresholds
  were set. The thresholds were never the problem.

The fix is `broker.SessionVolumes`, which sums each symbol's volume from **batched
hourly bars** since the pre-market open. Hourly rather than minute because the answer
wanted is one number per symbol and a pre-market session is at most six hourly candles
against ~330 minute ones. It runs on the names that cleared the move and the price
band — which is why `screener.Tradable` split into `TradablePrice` and
`TradableLiquidity`: the price band is answerable from the snapshot in either session,
turnover is not, so pre-market applies the band first, fetches volume for the
survivors, and only then judges turnover. Fetching before the ranking is deliberate:
the enrichment budget is handed out by dollar volume, and ranking on zeros would have
handed it to whichever symbols happened to sort first.

**3. `allow_entry` required `execution.order_type: limit`, which was the wrong
coupling.** Turning pre-market entry on failed start-up with a validation error
demanding the *regular* session switch to limit orders — an unrelated change to how
the rest of the day trades, and one the open items still list as undecided.

The constraint is narrower than the rule expressed. Alpaca accepts only a **day limit
order** for the extended session, so before the bell there is no choice to configure:
`engine.submit` now sends a limit order whenever the extended-hours flag is set,
whatever `execution.order_type` says, and `execution.order_type` governs the regular
session alone. Validation asks for the one thing a limit order genuinely cannot do
without — a price allowance — via `premarket.limit_slip_pct`, falling back to
`execution.limit_slip_pct`.

That allowance is now the setting most likely to make the feature look broken while
working: pre-market spreads on these names are several times wider than
regular-session ones, so 0.5% may never fill at 06:00. It defaults to 1.0% and is
PROPOSED like the rest of §2b. The **exit** side is the one that matters — an
unfilled pre-market stop leaves the position open until the bell, which is a real
consequence of `allow_entry` and part of why it defaults off.

**First observed pre-market screen** (06:21 ET, 13,191 symbols scanned, 66 cleared
+10%, 11 survived the floors):

| Symbol | Move | Rel. volume | Verdict |
|---|---|---|---|
| CLRO | +79.1% | 123.9x | qualifies |
| GYGY | +94.8% | 3.3x | qualifies |
| WBUY | +27.4% | 9.4x | qualifies |
| MIMI | +29.7% | 0.9x | qualifies |
| SDEV | +10.7% | 3.2x | qualifies |
| ACET | +10.8% | 0.2x | fails relative volume |
| MEDS | +18.2% | 0.1x | fails news, relative volume |

That spread is the first evidence that `min_volume_multiple: 0.5` discriminates
rather than admitting everything: genuine runners land between 3x and 124x, noise
between 0.1x and 0.3x. It is one morning's observation, not a measurement, and it
says nothing about whether any of these would have been profitable.

## 2026-09-28 — The status page can close a position

Requested directly. It breaks a stated invariant — the page was "read-only by
contract" — so the replacement invariant is written down rather than left implied:
**the page can close a position, and can never open one.** There is no path from a
handler to `enterPositions`, and any future page action belongs on the exit side for
the same reason. That is what keeps the strategy the only thing deciding what to buy,
which was the actual point of the original rule; "read-only" was the means, not the
end.

The need is real in the other direction. An operator watching a position go wrong had
no way out except killing the daemon — which leaves the position open at the broker
and relies on `Reconcile` at the next start — or going to Alpaca's own UI, which puts
the store and the broker out of step until the same reconciliation. Both are worse
than a button.

Decisions worth keeping:

- **It reuses the strategy's exit path** (`engine.submit` → `store.ClosePosition`)
  rather than a shortcut of its own, so the order record, the audit event and the
  realised P&L — which for a scaled-out position is not the entry-to-exit move — are
  built exactly the way a rule-driven exit builds them.
- **`domain.ExitManual`**, distinct like `ExitReconciled`, for the same reason: the
  closed-positions table and anything later measuring the rules must not read an
  operator's decision as a rule having fired.
- **Refused when the exchange is shut.** A sell submitted then is queued to the next
  open while the store has already marked the position closed, and the two disagree
  until a restart reconciles them. A page reporting a flat book the broker does not
  have is worse than a refusal. Pre-market is allowed and routed to the
  extended-hours book.
- **A per-position guard plus a re-read before acting.** The id comes from a page
  that may be a poll interval old and may name a position the loop has since exited,
  so the row is re-read under the guard and a second press gets `ErrPositionNotOpen`
  rather than a second sell order.
- **Sell-then-record, like the strategy path.** Neither ordering is safe against a
  crash in between; this one matches what `Reconcile` is already designed to repair.

The end-of-day card became the **Closed** tab of a two-tab Positions card and is now
visible for the whole session. Hiding it until the forced-exit mark was reasonable
while a rule was the only thing that could close a position; once the page can close
one by hand, the result has to appear when it happens.

## 2026-09-28 — KNRX, and the Open button that followed

**The observation.** KNRX screened at **+357.2% on 4846x relative volume with 7
catalysts** — comfortably the strongest candidate the screen has produced — and was
never bought. It was not the position cap (one position all day, three slots), not
the entry window, and not an outage. It was the setup gate, twice:

```
09:59 ET   no setup: pullback is 0 bars, need at least 1
14:13 ET   no setup: pullback is 7 bars, more than the 5 allowed
```

**Why that is structural rather than bad luck.** `FindSetup` takes the pole as the
session's highest high and the flag as the bars between it and the current one, and
needs 1–5 flag bars plus a close back above the pole. On a vertical mover the pole
keeps moving to the newest bar, so the flag is 0 for as long as the trend is clean —
the *better* the move, the more certainly it reads "0 bars". Once it tops, the flag
grows a bar a minute and the reclaim has to land 3–7 bars later: a window of about
five minutes, once, per high of day. Miss it and the setup is gone until a new high
resets the pole.

So the screen's best candidates are systematically the ones the gate is least able to
take. That is the "55 trades a year" open item, observed rather than inferred.

**What was done about it.** Not a change to the gate — nothing here measures whether
a looser gate would make money, and the gate is the single difference between this
version and the one with no edge. Instead the page grew an **Open** button on
qualifying rows, so a human can take the trade the detector cannot see.

The line drawn: **a manual action overrides the signal, never the risk rules.** An
operator decides *that* a trade happens; the machine still decides how big, where the
stop is, and whether it fits. Concretely:

- `risk.SizeForRisk` runs unchanged, so the trade risks the same 1% as any other.
- There is always a stop. The chart's own if a setup happens to be there — so clicking
  early gives the automatic trade — otherwise `entry.max_stop_distance_pct` below
  entry. That is the widest risk the strategy accepts, which makes the hand-placed
  position the *smallest* the budget allows rather than an arbitrary fraction of the
  account. Sizing by fraction is exactly what the no-edge version did.
- The position cap, one-position-per-symbol and same-day re-entry all still hold.
- The kill switch does not block it — an instruction about one named symbol is not what
  it exists to stop — but `overrode_halt` goes into the audit event.
- Refused in the end-of-day window and with the exchange shut.

**Also recorded.** This reverses the invariant written the same day ("the page can
close a position but can never open one"). It was reversed on request, deliberately,
and the replacement invariant is the signal/risk split above. The earlier one was
protecting the wrong thing: what matters is not that the page cannot trade, it is that
nothing can size a trade outside the risk rule.

**Observability gap found while investigating, still open.** The trail could not say
whether KNRX ever had a valid window, because `recordSkip` de-duplicates on symbol +
the bare reason `"no setup"` — so only the first reason is ever written and the
changing detail is lost — and `SaveScreenSnapshot` keeps only the newest pass. Two
data points for a whole session. Recording the setup reason when it *changes* is the
smaller fix and would make this class of question answerable.

## 2026-09-28 — A late restart no longer costs the session

`resolveGate` halts the day when no first-hour readings exist, which is what happens
every time the daemon is started after the first hour. The reasoning was sound —
trading with the safety check never having run is worse than sitting out — but the
consequence was not: a restart at 11:00 wrote off the rest of the day, and there was
no way back short of editing the database.

The distinction that fixes it: **that halt reports an absence of data, not a bearish
market.** So the halt banner now carries an Ignore button, and it is scoped to exactly
that case.

- **A halt the gate actually reached is not dismissible.** A bearish verdict on real
  readings is the kill switch working, and standing it down from a small button on a
  status page is a different decision with different consequences. Asked for as "the
  screening will fail with *no sentiment readings were taken during the first hour*",
  and implemented to that case rather than to halts in general.
- **The two are told apart structurally, by whether any readings exist** — not by
  matching the halt's wording. Matching the message would silently stop working the
  first time anyone reworded it, and it would fail open, which is the wrong direction
  for a safety check.
- **The verdict becomes `GATE_OVERRIDDEN`, not `PROCEED`.** Same reasoning as
  `ExitManual` and `ExitReconciled`: the gate did not pass, it was stood down, and
  neither the page nor the trail should let a later reader conclude otherwise. It
  renders amber rather than green.
- **It persists**, so a further restart does not re-halt the day — otherwise the click
  would buy a few minutes and nothing more.

The confirmation states plainly that the session then trades with no sentiment kill
switch behind it. That is true and worth reading before clicking: the gate cannot be
reconstructed after the fact, because its inputs were intraday percentage moves during
a window that has passed.

**Not done, and worth considering:** taking a *live* sentiment reading at the moment
of the restart and judging on that, instead of ignoring the gate entirely. It would be
a weaker check than the real one — one sample rather than a window — but it is not
nothing, and it is the same substitution `preMarketPass` already makes for pre-market
entry. Left out because it was not what was asked, and because a live read at 14:00
says something quite different from a first-hour one.

## 2026-09-29 — Symbols link to TradingView, and a documentation drift pass

| Decision | Rationale |
|---|---|
| Every symbol on the status page is a link to `tradingview.com/chart/?symbol=<SYM>`, opened in a new tab | The page explains *why* the agent acted but shows no chart, and reading the tape is the first thing an operator does with a ticker they have just seen. One shared template (`symbolCell`) so positions, screening and both modals link identically. |
| A plain anchor rather than a JS handler | The page's click handlers all match on `button`, so an anchor cannot be swallowed by them, and it survives the ~12s fragment swap with no extra code. Link colour is pinned to the body text colour in every state, since a default blue is unreadable on the dark theme. |
| TradingView remains **not** a data source | Unchanged from 2026-09-26: this is a link out to a chart a human reads, not an integration. Nothing is fetched from it. |

Then a read-through of every doc against the code and `config/config.yaml`, which found
drift in three groups. All of it is now fixed in the docs; no behaviour changed.

- **The sentiment gate was still documented as "every 10 minutes for one hour"** in `strategy.md` §1, `architecture.md`, `web-ui.md`'s legend and `operations.md`, although `timing.sentiment_window` was shortened to **5m** and the poll to **2m** — and `strategy.md` §3 already recorded that change, so the same document contradicted itself. Docs now name the config keys instead of restating an interval.
- **Numbers left over from the pre-rewrite sizing model.** `operations.md` tabled `max_concurrent_positions` as 5, and `risk.md` still had "maximum 5 concurrent positions → 50% exposure, 50% cash" three lines below the paragraph explaining why it is 3 at 33%. `risk.md` also described sizing as a percentage of portfolio value and gave the stop priority over "the momentum-based exit signal", which was the MACD exit removed on 2026-09-28.
- **`strategy.md` still listed backtesting as out of scope for v1**, which `cmd/backtest` superseded on 2026-09-28. Marked superseded rather than deleted.

Two of the findings are **config against doc, not doc against itself**, and the docs were
updated to describe the shipped values while keeping the recommendation visible:

| Key | Shipped | Documented | Note |
|---|---|---|---|
| `premarket.allow_entry` | `true` | `false` | The open item below asks for this to stay off until the PDT count is resolved: a pre-market entry that exits the same session is still a day trade. |
| `premarket.scan_interval` | `1m` | `5m` | The affordability arithmetic in both the docs and the key's own comment ("~66 passes, ~26 req/min") only holds at 5m. At 1m the morning runs ~330 passes at roughly the regular session's ~130 req/min against Alpaca's 200/min, and nothing measures pass duration. |

Also fixed: a broken markdown table in `web-ui.md` whose delimiter row had drifted three
paragraphs away from its header, two sections there still describing the pre-tabs
positions layout, and `docs/README.md` still introducing the directory as a spec written
before any code existed.

## 2026-09-29 — Two exit/re-entry settings kept at their shipped values

The documentation drift pass above changed `config/config.yaml` in the same commit it
was written in, and missed the two keys it changed. `TestLoadShippedConfig` caught one
of them; nothing guards the other, so it drifted silently. Both are kept as shipped and
the docs now describe them.

| Key | Was | Now | Note |
|---|---|---|---|
| `exit.eod_exit_offset_minutes` | 30 | **5** | The forced exit moves from 15:30 to 15:55. `timing.entry_window` still puts the last entry at 15:00, so it binds 55 minutes ahead of the exit and `entry_cutoff_buffer` (30m → 15:25) no longer binds at all. Unmeasured: `cmd/backtest` exits at the configured mark, so the last 25 minutes of a position's life is now held rather than closed, and nothing has been run to say whether that is better. |
| `risk.allow_same_day_reentry` | `false` | **`true`** | A symbol may be bought again once today's position in it has closed. One-position-per-symbol is unaffected — it is not configurable and still refuses the second buy while the first is open. |

Neither value is measured, and `allow_same_day_reentry` is still marked **PROPOSED** in
both the config and `operations.md`. The reason for recording them here is the one the
pass above makes: a shipped value that contradicts its documentation should be settled
deliberately, not left to whichever of the two a reader happens to trust.

The forced-exit interval is no longer restated as a number in `risk.md` or the
`EOD_WINDOW` legend; both name the key, as the sentiment-window fix did for the same
reason.

## 2026-09-29 — The micro pullback replaces the flag as the only entry pattern

**Why.** `FindSetup` did not describe the pattern the strategy is modelled on. It waited
for a close above the *session* high after a 1–5 candle pullback, a bull-flag breakout.
The micro pullback enters above the *previous candle's* high after a 1–2 candle pause
in a stock that is surging right now. It also counts a candle that ticks a marginal new
high and closes red as a pause, which is exactly the candle that made the flag read
KNRX as "pullback is 0 bars".

**What changed.** `strategy.FindSetup` is now the micro pullback, and the flag detector,
`entry.pattern` and `entry.min_pullback_bars` are gone. New settings:
`entry.surge_bars` (3), `min_surge_pct` (3.0), `max_retrace_pct` (50), and
`max_pullback_bars`, which now means the pause, at 2. There are two optional filters,
off by default: `require_macd` and `require_volume_decline`. The trend filters, the
stop rules and sizing are unchanged. The daemon still enters on the trigger candle's
*close*, because it scans closed candles; it places no buy-stop orders.

**The comparison** (one year, 2025-09-28 → 2026-09-26, 0.25% slippage per side, same
data). The flag row was measured before its code was removed:

```
entry                              setups  trades   mean     t     equity   maxDD
flag: close above session high       169     201   -0.94%  -4.21    5434   47.1%
micro: close above pause high        143     152   -0.70%  -1.62    7413   26.6%
micro: buy-stop at pause high        503     455   -0.79%  -3.50    3223   68.1%
```

The micro pullback on the close is better than the flag on every line: smaller loss
per trade, a much shallower drawdown, and $2k more equity at the end. **It is still
not profitable**: −0.30R a trade, and a t of −1.62 means the loss itself is not
clearly different from zero. It loses less; it has not shown an edge.

**The last two months** (2026-07-29 → 2026-09-28): 16 trades, 38% winners, −0.83% a
trade (−0.41R) at 0.25% slippage, equity 10,000 → 9,602, max drawdown 5.7%. Sixteen
trades are far too few to conclude anything (t = −1.18).

**What the sweep says about the settings** (one year, mean R per trade): as configured
−0.30. MACD −0.27 and pause up to 3 bars −0.33 are within noise of that. Every other
change is clearly worse: lighter pause volume −0.45, a 1-bar-only pause −0.46, a 6%+
surge −0.49, no minimum surge −1.16. **The shipped values are the best of those
tried, and none was tuned.**

**The buy-stop entry is not worth building on this evidence.** It fires three times as
often and loses at least as much per trade. The first run appeared to favour it, but
that came from a simulator bug: a bar that gapped over the trigger filled at its open
with the stop far beyond the 4% limit (YJ: fill 13.18, stop 10.90). The buy-stop is now
modelled as a stop-limit capped at that limit. It is still one of the report's rows.

**Two measurement fixes came with this, and they move earlier numbers.**
- `simulate` now checks the entry bar for a stop or target hit straight after the
  fill. Before, the bar a position was bought on was never examined. On the year's
  flag baseline this moved the no-slippage mean from −0.53% to −0.47%: small, but
  every earlier figure in this file was measured without it.
- The entry-pattern report adds **mean R** (P&L over dollars risked). With risk-based
  sizing, a percentage mean can be positive while the account falls, because a small
  wide-stop winner counts as much as a large tight-stop loser.

**Still open.** Both headline runs lose money after slippage, and the worst rolling
five-session day-trade count is 9 for the year, against a PDT limit of 3. Nothing
here changes the case for staying on paper.

## 2026-09-29 — The micro pullback's exit: the candle trail

**Why.** The exits already matched the micro pullback in three respects: the stop
under the pause low, half sold at 2R, and the rest of the position moved to
breakeven. What was missing was the rule for what is left: *sell on the first candle
to make a new low*, a trade below the previous candle's low. The discretionary
version applies it from entry, so a trade that makes a new low before paying is
abandoned rather than held to the stop.

**What changed.** `exit.candle_trail` (`off` / `after_target` / `always`), set to
`always`. `strategy.CandleTrailStop` raises the stop to the low of each completed
candle held through. The engine applies it through `store.MoveStop`, which never
lowers a stop, and makes one bar request per held position per completed candle. The
backtest calls the same function on every bar it holds through. The close event gains
`initial_stop` and `candle_trail`, so a trailed exit can be told apart from the chart
stop.

**The measurement** (0.25% slippage per side, the micro-pullback entry):

```
                    ── one year (2025-09-28 → 2026-09-26) ──   ── two months (2026-07-29 → 2026-09-28) ──
exit.candle_trail   trades   mean   mean R   equity   maxDD     trades   mean   mean R   equity   maxDD
off                   152   -0.70%   -0.30    7413    26.6%       16   -0.83%   -0.41    9602    5.7%
after_target          160   -0.76%   -0.29    7141    30.6%       16   -0.08%   -0.10    9954    3.4%
always                172   -0.82%   -0.31    6676    34.5%       17   -0.36%   -0.21    9828    3.7%
```

Per trade in R the three are the same within noise over the year. The trail frees
positions sooner, so more trades are taken, and over the year that turns into a lower
ending equity and a deeper drawdown. The last two months favour it, on 16–17 trades.
**It is on because it is the approach's exit, not because it measured better.** Over
the year `off` was the best of the three and `after_target` the least harmful way to
keep the rule. This is consistent with the earlier finding that this strategy's return
lives in a thin right tail that trailing exits cut into.

## 2026-09-30 — The first paper day: the trail read the entry candle, and fills were never read

The first session with real paper orders (2026-09-29) opened 15 positions and closed
14 on `STOP_LOSS`, with the stop apparently about 1% under entry. Two causes, both
found by comparing the audit trail and the database on the server against Alpaca's
own order history.

**The candle trail trailed the candle the buy landed in.** `CandleTrailStop` counted
the entry candle as "the first one held through". The backtest fills at a bar's open,
where that is true; the daemon buys whenever its scan decides, part-way through a
candle whose low was mostly printed *before* the fill. The stop went there within the
first minute. 10 of the 11 trailed positions were sold by it — SANG twice, YMT and
TURB with the stop set exactly at the recorded entry. The initial stops were not
tight: 4% for the manual opens, 0.9–2.9% from the chart. **Change:** a candle counts
only if it opened at or after the fill. For the backtest's default fill nothing
changes, which also means its earlier `candle_trail` figures describe the corrected
rule rather than what ran live; its buy-stop mode, which fills mid-bar, now skips the
entry bar too.

**Positions were recorded at the quote, not the fill.** `submit` saved Alpaca's
acknowledgement (`pending_new` on all 30 orders) and never asked again, so every
entry and exit price was what the daemon *read* when it decided. Against Alpaca's
fills:

```
recorded P&L  -$541.80
actual P&L    -$632.15   (all 30 orders filled in full)
```

The quote was wrong on 11 of 15 trades. The four "breakeven" trail exits were real
losses (−$52.59 together): each buy filled a cent above the quote the breakeven stop
was set at. NAUT at 12:20 ET planned $91 of risk and lost $154, because the stop is
polled rather than resting and a fast drop goes through it. **Change:** `submit`
waits up to 10s for the broker to report the order done, cancels any remainder, and
returns what executed. Positions are recorded at the fill price and filled share
count; the initial risk (and so the target) is re-measured from the fill; the
`orders` table keeps the final status and fill (migration 005). Short fills are
handled rather than assumed away — see [`architecture.md`](./architecture.md).

**Not changed, and worth knowing when reading that day.** 11 of the 15 trades were
manual pre-market opens that overrode the setup gate (the recorded reasons include
"below VWAP" and "no pullback"). Only the four NAUT trades were the strategy's own
entries, so the day says little about the strategy either way.

## 2026-09-30 — Positions opened by hand are closed only by hand

**On request.** A position opened from the page's Open button is now exempt from every
exit rule: the chart stop, the `risk.stop_loss_pct` backstop, the candle trail, the
scale-out at the first target, and the forced end-of-day exit. It is flagged `manual`
(migration 006), `managePositions` keeps its mark current and does nothing else, and
only its Close button sells it — overnight and across sessions if it is not pressed.
The first question asked was whether the forced exit should stay; the answer was that
it should not.

**What this gives up, stated so it is not rediscovered.** The page's invariant had been
"a manual action overrides the signal, never the risk rules". It now holds on the way
in only: sizing still comes from a stop, but the stop is not enforced afterwards, so a
manual position's loss is not bounded by `risk.risk_per_trade_pct`, and it can carry
overnight gap risk the rest of the strategy excludes. The automated positions are
unaffected, and so is the backtest, which never opens a position by hand.

**Still refused inside the end-of-day window.** Opening there was refused because the
position would be force-sold within minutes, which no longer happens to a manual
position. Left as it was; whether to lift it is open.

## Open items (not yet decided)

- **(Superseded 2026-09-29: the micro pullback trades ~150 times a year and still measured no edge; see above.)** **55 trades a year is the thing to resolve first.** It is too few to measure and probably too few to be worth running. Either the setup definition is stricter than the discretionary version it models — a human reads a flag more loosely than "1–5 bars reclaiming the high of day" — or the screening criteria and the setup rarely coincide. Loosening `entry.max_pullback_bars`, allowing a reclaim of a recent swing high rather than the session high, or reading the pattern on 2-minute candles are the obvious things to measure, one at a time, against this baseline.
- **Float is still absent and still matters.** Low float is the mechanism that makes these moves extend, and it is the one part of the approach that could not be aligned. It needs a fundamentals provider Alpaca does not offer; no provider has been chosen.
- **Tape reading is not automated and cannot be.** Any comparison against published discretionary results has to carry that caveat.
- **Still no demonstrated edge, and still not a candidate for live capital.** t = −0.53. The risk profile is now defensible where it was not; the expectancy is not.
- **Measure realised slippage in paper trading.** At 55 trades a year the slippage sensitivity is mild in absolute terms, but `execution.order_type` supports limit orders and the entry is now a defined price rather than "whatever the scan read", which makes a limit order far more usable than before.

- **Measure realised slippage in paper trading, then decide on limit orders.** (2026-09-30: now measurable — every trade's audit event carries the fill next to `quoted_price`.) It is now the single largest unknown: the strategy is profitable at 0.10% per side and unprofitable at 0.50%. `execution.order_type` supports `limit` with `limit_slip_pct` already.
- **Still no demonstrated edge, and still not a candidate for live capital.** t = 1.20 is not significance. The survivorship bias in the universe and the optimistic stop fills both push the true figure down, not up.

- **Recommended, in order of measured value, none applied:** (1) remove `exit.profit_target_pct` and `exit.trailing_stop_pct` entirely, leaving the hard stop and the forced EOD exit — moves t from −9.76 to +0.28; (2) cut `risk.position_size_pct` to 2–5%, which changes max drawdown from 79% to 23% at the same per-trade mean; (3) widen `risk.stop_loss_pct` toward 15–20%, or at least do not tighten it, since 5% is measurably worse than 10%; (4) invert or drop the relative-volume tie-break; (5) add a ceiling on the intraday move at entry, because gaps over +50% are the worst bucket measured.
- **Even the best structure has no edge.** t ≈ +0.3 is indistinguishable from zero: the recommendations above remove a proven loss, they do not produce a proven profit. Nothing measured here justifies live capital.
- **The PDT rule is not a future problem, it is a blocker.** The worst rolling five-session day-trade count is **64** against a limit of 3 for a sub-$25k margin account.

- **The strategy has no demonstrated edge, and this is the load-bearing open item.** Screening floors and the MACD removal address composition and dead weight; neither creates an edge. Before any real money: measure realised slippage in paper trading, since that single number decides the outcome, and decide whether entries must be limit orders.

- **Every `premarket.*` threshold is unmeasured.** `cmd/backtest` has no pre-market bars, so the start time, scan interval, dollar-volume floor and volume multiple are reasoned rather than measured. Extending the backtest to fetch extended-hours bars is what would close this; until then the pre-market screen should be read, not traded.
- **Pre-market entries make the PDT problem worse, not better.** A pre-market entry that exits the same session is still a day trade, and the rolling five-session count is already **64** against a limit of 3. `premarket.allow_entry` must stay off until the PDT constraint is resolved, and the live-trading guard does not cover it — that guard is about real money, and this is about trade count on paper too. **As of 2026-09-29 the shipped config has it on**, which contradicts this item: either the switch goes back off, or this item is retired deliberately rather than by drift.
- **`premarket.scan_interval` is set to the regular session's 1m**, so the pre-market morning costs ~330 passes at roughly ~130 requests a minute against Alpaca's 200/min, not the ~26 the key's own comment describes. Nothing measures pass duration or warns on an overrun, so the decision is whether to slow the key back to 5m or to add that measurement first.
- **Whether a dismissed gate should be replaced by a live sentiment read** rather than by nothing. The machinery exists (`readSentiment`, already used for pre-market entry); the question is whether a mid-session reading is meaningful enough to gate on, given the rule was specified against the first hour.
- **Whether the setup gate is too strict, now that there is a case.** KNRX is one observation, not a measurement, and the manual Open button is an escape hatch rather than an answer. The things worth sweeping in `cmd/backtest` — one at a time, against the current baseline — are `entry.max_pullback_bars`, reading the pattern on 2-minute candles, and allowing a reclaim of a recent swing high rather than the session high.
- **The setup reason is only audited once per symbol per session.** `recordSkip` de-duplicates on symbol + `"no setup"`, so a candidate whose reason changes through the day records only the first. Combined with `SaveScreenSnapshot` keeping just the latest pass, there is no way to ask afterwards whether a candidate ever had a valid entry window.
- **Whether the strategy should still target small caps at all.** There is no size criterion, so the screen admits large caps while the docs still describe a small-cap strategy. Either the naming changes, or a size criterion returns — which needs a data source Alpaca does not provide.
- **PDT rule resolution before going live** — fund above $25k, reduce trade frequency, or switch to a cash account; none chosen yet (`docs/operations.md`).
- Review the claude-proposed values above — especially the profit target, trailing stop and sentiment thresholds, none of which rest on evidence.
- Whether `news_lookback` should be widened, or derived from the previous session's close, so Monday gaps driven by weekend news are not missed (`docs/strategy.md`).
- Whether market orders are acceptable for entries on thinly traded names, or entries should use limit orders (the config supports both; `market` is the current default).
- Whether a portfolio-level daily drawdown limit is needed alongside the per-trade stop-loss (`docs/risk.md`). Still not implemented.
- Hosting/deployment target for the running daemon (`docs/operations.md`).
- Whether the status page needs a browsable multi-day session history (currently scoped to today-only) (`docs/web-ui.md`).
