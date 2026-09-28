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

## Open items (not yet decided)

- **Measure realised slippage in paper trading, then decide on limit orders.** It is now the single largest unknown: the strategy is profitable at 0.10% per side and unprofitable at 0.50%. `execution.order_type` supports `limit` with `limit_slip_pct` already.
- **Still no demonstrated edge, and still not a candidate for live capital.** t = 1.20 is not significance. The survivorship bias in the universe and the optimistic stop fills both push the true figure down, not up.

- **Recommended, in order of measured value, none applied:** (1) remove `exit.profit_target_pct` and `exit.trailing_stop_pct` entirely, leaving the hard stop and the forced EOD exit — moves t from −9.76 to +0.28; (2) cut `risk.position_size_pct` to 2–5%, which changes max drawdown from 79% to 23% at the same per-trade mean; (3) widen `risk.stop_loss_pct` toward 15–20%, or at least do not tighten it, since 5% is measurably worse than 10%; (4) invert or drop the relative-volume tie-break; (5) add a ceiling on the intraday move at entry, because gaps over +50% are the worst bucket measured.
- **Even the best structure has no edge.** t ≈ +0.3 is indistinguishable from zero: the recommendations above remove a proven loss, they do not produce a proven profit. Nothing measured here justifies live capital.
- **The PDT rule is not a future problem, it is a blocker.** The worst rolling five-session day-trade count is **64** against a limit of 3 for a sub-$25k margin account.

- **The strategy has no demonstrated edge, and this is the load-bearing open item.** Screening floors and the MACD removal address composition and dead weight; neither creates an edge. Before any real money: measure realised slippage in paper trading, since that single number decides the outcome, and decide whether entries must be limit orders.

- **Whether the strategy should still target small caps at all.** There is no size criterion, so the screen admits large caps while the docs still describe a small-cap strategy. Either the naming changes, or a size criterion returns — which needs a data source Alpaca does not provide.
- **PDT rule resolution before going live** — fund above $25k, reduce trade frequency, or switch to a cash account; none chosen yet (`docs/operations.md`).
- Review the claude-proposed values above — especially the profit target, trailing stop and sentiment thresholds, none of which rest on evidence.
- Whether `news_lookback` should be widened, or derived from the previous session's close, so Monday gaps driven by weekend news are not missed (`docs/strategy.md`).
- Whether market orders are acceptable for entries on thinly traded names, or entries should use limit orders (the config supports both; `market` is the current default).
- Whether a portfolio-level daily drawdown limit is needed alongside the per-trade stop-loss (`docs/risk.md`). Still not implemented.
- Hosting/deployment target for the running daemon (`docs/operations.md`).
- Whether the status page needs a browsable multi-day session history (currently scoped to today-only) (`docs/web-ui.md`).
