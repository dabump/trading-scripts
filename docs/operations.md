# Operations

**Status:** implemented.

## Running modes

- **Paper trading** is the v1 target — Alpaca's paper account, same API shape as live. This is explicitly the validation step before any real capital is involved.
- **Live trading** is a later, deliberate switch (different Alpaca base URL/keys), not a default or something to enable casually. Treat any change from paper to live as requiring explicit user sign-off, not an automatic config flip during normal development.

### Pattern Day Trader (PDT) constraint — hard blocker before going live

FINRA's Pattern Day Trader rule restricts margin accounts under $25,000 equity to 3 day-trades per rolling 5 business days. This strategy is designed to open and close up to `risk.max_concurrent_positions` (3) day-trades *per day*, and a one-year backtest measured a worst rolling five-session count of **64** — as of 2026-09-26 the account is under $25k (or funding level undecided), so running this live as designed would very quickly get the account flagged/restricted by Alpaca.

This does not block paper trading (Alpaca's paper environment doesn't carry real regulatory consequences either way), but it **must** be resolved before any live-trading switch. Options, none yet chosen:
- Fund the account above $25k before going live.
- Reduce live-trading frequency to fit within 3 day-trades per 5 business days (a significant change to the strategy as currently specified).
- Use a cash account instead of margin — not subject to PDT, but subject to T+1 settlement, which limits reusing the same cash for a new trade until the prior trade settles; this could conflict with wanting multiple same-day entries funded from exited positions' proceeds.

Do not silently pick one of these while implementing — surface it for an explicit decision when live trading is actually being scoped.

## Secrets

- Alpaca API key + secret are loaded from environment variables.
- Local development uses a gitignored `.env` file; `.env.example` documents the required variable names with no real values, so the shape of required config is visible without exposing anything.
- Never commit a populated `.env`, API keys, or account credentials to the repo.

## Configuration (`config/config.yaml`)

Tunable strategy parameters live here, not hardcoded — distinct from secrets, which go through env vars (below). This list should stay in sync with the values decided in `strategy.md`/`risk.md`; if you change a number in code without updating this list (or vice versa), one of the two has gone stale.

| Key | Value | Notes |
|---|---|---|
| `market_data.feed` | `sip` | Full consolidated tape. `iex` is accepted but the relative-volume criterion is not meaningful on it |
| `screening.min_price` | 1.0 | `strategy.md` §2 — tradability floor, not a momentum criterion; validated > 0 |
| `screening.max_price` | 20.0 | `strategy.md` §2 — the band the strategy trades; validated > `min_price` |
| `screening.min_dollar_volume` | 1000000 | `strategy.md` §2 — dollar volume traded today; validated > 0 |
| `screening.min_intraday_pct` | 10.0 | `strategy.md` §2 |
| `screening.min_volume_multiple` | 5.0 | `strategy.md` §2 |
| `screening.avg_volume_lookback_days` | 20 | **PROPOSED** — excludes today |
| `screening.max_enriched` | 100 | **PROPOSED** — caps the per-symbol lookups after the move filter, not the scan itself |
| `screening.news_lookback` | 18h | **PROPOSED** — must reach back past the open so pre-market catalysts are visible; validated to be ≥7h |
| `premarket.enabled` | `true` | `strategy.md` §2b — screening and scanning before the bell. Off means the morning is `MARKET_CLOSED` and no requests are made |
| `premarket.start` | `04:00` | HH:MM exchange time, matching the exchange's own pre-market open; validated to parse and to fall before the 09:30 open. A later value is supported to trim API calls, but reports `MARKET_CLOSED` while the market is in pre-market |
| `premarket.scan_interval` | 1m | **PROPOSED** — a separate key from `timing.screener_scan_interval` so pre-market can be slowed down independently, but currently set to the same 1m. That is ~330 passes of ~130 requests across 04:00–09:30, so the pre-market rate is the same ~130 req/min the regular session runs at, against Alpaca's 200/min. 5m would be ~66 passes at ~26 req/min; nothing measures pass duration, so a slower value is the cheap way to buy headroom |
| `premarket.min_dollar_volume` | 100000 | **PROPOSED** — replaces `screening.min_dollar_volume` while pre-market; the regular floor rejects the whole early tape |
| `premarket.min_volume_multiple` | 0.5 | **PROPOSED** — replaces `screening.min_volume_multiple` while pre-market; still measured against the 20-session **daily** average, so half a day's volume before the bell is an extreme reading |
| `premarket.allow_entry` | `true` | `strategy.md` §2b — buying before the bell, and **on** in the shipped config against the recommendation in `decisions.md` (pre-market entries are day trades, and the PDT count is the open blocker). Validated to require `premarket.enabled`, and a non-zero limit allowance from either `premarket.limit_slip_pct` or `execution.limit_slip_pct` |
| `premarket.limit_slip_pct` | 1.0 | **PROPOSED** — limit allowance on pre-market orders. Extended-hours orders are always limit orders, so `execution.order_type` does not apply before the bell. Wider than the regular 0.5% because pre-market spreads are wider; 0 falls back to `execution.limit_slip_pct` |
| `entry.pattern_interval` | 1m | `strategy.md` §3 — the candle the setup is read on |
| `entry.ema_period` | 9 | `strategy.md` §3 — trend filter |
| `entry.require_above_vwap` | `true` | `strategy.md` §3 |
| `entry.max_pullback_bars` | 2 | `strategy.md` §3 — the longest pause a micro pullback allows |
| `entry.surge_bars` / `min_surge_pct` | 3 / 3.0 | `strategy.md` §3 — the surge the pause must follow |
| `entry.max_retrace_pct` | 50 | `strategy.md` §3 — share of the surge the pause may give back |
| `entry.require_macd` / `require_volume_decline` | false / false | `strategy.md` §3 — optional filters |
| `entry.stop_buffer_pct` | 0.1 | `strategy.md` §3 — the stop sits under the pause low, not on it |
| `entry.min_stop_distance_pct` | 0.5 | `strategy.md` §3 — a nearer stop is widened to this |
| `entry.max_stop_distance_pct` | 4.0 | `strategy.md` §3 — a wider setup is refused; validated below `risk.stop_loss_pct` |
| `entry.max_entry_drift_pct` | 1.0 | `strategy.md` §3 — the live price may be at most this far above the trigger close when the order goes; must be > 0. PROPOSED |
| `risk.risk_per_trade_pct` | 1.0 | `risk.md` — the account fraction put at risk per trade; sizing follows from the stop |
| `risk.max_position_pct` | 33.0 | `risk.md` — notional cap; must exceed `risk_per_trade_pct ÷ entry.max_stop_distance_pct` or it binds on every trade |
| `risk.max_concurrent_positions` | 3 | `risk.md` — three at 33% keeps maximum exposure just under fully invested; validated so sizing × concurrency cannot exceed 100% |
| `risk.stop_loss_pct` | 10.0 | `risk.md` |
| `risk.allow_same_day_reentry` | `true` | **PROPOSED** — a symbol may be bought again once today's position in it has closed; one-position-per-symbol still holds |
| `exit.first_target_r` | 2.0 | `strategy.md` §4 — in multiples of the trade's own initial risk |
| `exit.first_target_fraction` | 0.5 | `strategy.md` §4 — validated in (0, 1): selling all of it is the rule that measured t = −9.76 |
| `exit.breakeven_after_target` | `true` | `strategy.md` §4 |
| `exit.eod_exit_offset_minutes` | 5 | `strategy.md` §4 |
| `exit.candle_trail` | `after_target` | `strategy.md` §4 — `off` / `after_target` / `always`; empty means `off` |
| `exit.candle_trail_interval` | unset (`entry.pattern_interval`) | the candle the trail is read on, built from `entry.pattern_interval` candles; must be a whole multiple of it. Wider settings measured no better — `decisions.md` 2026-10-03 |
| `exit.candle_trail_bars` | unset (1) | the stop goes under the lowest low of this many completed trail candles |
| `timing.sentiment_poll_interval` | 2m | `strategy.md` §1 — tightened with the shorter gate window |
| `timing.sentiment_window` | 5m | `strategy.md` §1 — shortened from 1h so the opening range is tradeable |
| `timing.entry_window` | 5h30m | `strategy.md` §3 — from a 09:30 open the last entry is 15:00; validated longer than `sentiment_window` |
| `timing.entry_cutoff_buffer` | 30m | `strategy.md` §3 — quiet time before the forced exit, enforced against the close so a half day tightens the window rather than collapsing the gap |
| `timing.screener_scan_interval` | 1m | `strategy.md` §2 |
| `timing.position_poll_interval` | 2s | **PROPOSED** — how often open positions are re-marked (also the engine tick interval) |
| `sentiment.symbols` | SPY, QQQ, IWM | IWM included because the strategy trades small caps |
| `sentiment.bearish_avg_pct` | −0.8 | **PROPOSED** — must be negative |
| `sentiment.require_all_negative` | `true` | **PROPOSED** |
| `execution.order_type` | `market` | **PROPOSED** — guarantees fills but can slip on thinly traded names |
| `execution.limit_slip_pct` | 0.5 | Used when `order_type: limit`, and as the pre-market fallback when `premarket.limit_slip_pct` is 0 |
| `web.listen_addr` | `:8080` | |
| `web.poll_interval` | 12s | `web-ui.md` (~10–15s) |
| `storage.database_path` | `data/agent.db` | Created on start; gitignored |
| `audit.directory` | `logs` | Append-only JSONL trail, one file per session date; gitignored |

Invalid configurations are rejected at startup rather than mid-session: unknown
keys, a non-negative bearish threshold, a non-positive screening floor, a chart stop
wider than the gap backstop behind it, a profit target that would sell the whole
position, an entry window that closes before the sentiment gate opens, and total
exposure over 100% all fail fast, because discovering them with real positions open is the
expensive way to find out.

## Persistence

- SQLite, single file under `data/` (gitignored — this holds real trade history, not something to check in).
- Schema changes go through `internal/store/migrations/` rather than ad-hoc `ALTER TABLE` in application code, so the schema's history stays reviewable. They are embedded in the binary and applied idempotently on open; they live inside the package rather than at the repo root because `go:embed` cannot reach outside its own directory and the deployment target is a single self-contained binary.
- The daemon restarts mid-day and resumes from whatever `store` has: open positions, today's sentiment readings (the poll cadence is derived from them, so a restart neither double-polls nor skips), the session's gate verdict, and which symbols have already been traded today.
- On startup `engine.Reconcile` treats the broker as the authority — adopting positions the store does not know about, closing ones the broker no longer holds as `RECONCILED`, and resolving orders that were submitted but never confirmed.

## Audit trail

Decisions and actions are appended to `logs/audit-<session-date>.jsonl`, one JSON
object per line. This is deliberately separate from the operational log on stderr:
that one is for watching the daemon work, this one is the record you read back weeks
later to answer "why did it buy that, and what did it know at the time?".

Recorded events: `AGENT_STARTED`, `SENTIMENT_READING`, `GATE_RESOLVED`,
`TRADING_HALTED`, `POSITION_OPENED`, `POSITION_SCALED_OUT`, `POSITION_CLOSED`,
`ENTRY_SKIPPED`, `ORDER_NOT_FILLED`, `RECONCILED`, `FAULT`. (`POSITION_HELD` was
removed on 2026-10-02: manual positions are force-closed at the bell like any other,
so nothing is left open through the forced exit.) Every event that traded
carries the broker's fill as `price` alongside `quoted_price` (what the daemon read
when it decided), `shares_ordered`, `order_status` and `fill_confirmed`, so slippage
and short fills can be read straight off the trail. Only state changes and actions appear — the screen runs every
minute, and recording each evaluation would bury the events that matter.

Properties that make it usable as evidence:

- **Append-only, one file per session date.** A restart continues the day's file
  rather than truncating it, which also gives rotation for nothing.
- **Flushed to disk on every event.** A trail that loses its last entries in a crash
  fails exactly when it is most needed; at a few dozen events a day the cost is nil.
- **`AGENT_STARTED` records the effective configuration** — every threshold in force
  — so a reader can tell what the rules were at the time rather than assuming they
  match today's config. **Credentials are deliberately excluded**: an audit trail has
  to be safe to hand to someone.
- **Buy records carry the criteria values, the sizing inputs and the account state**;
  sell records carry the trigger, the P&L and how long the position was held.
- **Repetition is suppressed, not silently dropped.** A persistent fault is recorded
  once until it changes or the agent recovers. A candidate passed over for the same
  reason is recorded once per session — being at the position cap would otherwise
  produce hundreds of identical rows.

A failed audit write is logged loudly but does **not** stop the agent. The daemon may
be holding open positions, and abandoning their exits to preserve a record would be
the wrong trade — so the gap is made visible rather than fatal.

Reading it back:

```bash
jq -r '"\(.at[11:19])  \(.kind)  \(.symbol // "-")  \(.summary)"' logs/audit-2026-09-28.jsonl
jq 'select(.kind == "POSITION_OPENED")' logs/audit-2026-09-28.jsonl
```

## Observability (v1 scope)

- The status web page (reading from `store`), the audit trail above, and structured log output (`log/slog`, to stderr) are the observability in v1. `/api/status` returns the same state as JSON, which is what makes a running daemon checkable without scraping HTML.
- Refusals that repeat every scan (a symbol already held, or already traded today) log at debug level; everything else that skips a candidate logs at info. At a one-minute cadence the routine ones would otherwise bury the log in hundreds of identical lines.
- No external alerting (Slack, email, etc.) yet. If/when that's wanted, the natural hook points are: position opened/closed, daily kill switch triggered, and unhandled errors — but don't build this speculatively ahead of it being asked for.

## Deployment

### Docker

```bash
cp .env.example .env            # then fill in the Alpaca paper keys
docker compose up -d --build    # paper trading
docker compose logs -f agent
docker compose --profile demo up agent-offline   # fake broker, no credentials, port 8082
```

The status page is published on `127.0.0.1:8081` only (the demo profile uses 8082) —
bound to loopback rather than every interface, because the page exposes positions and
the two manual buttons and has no authentication. The two services deliberately use
different host ports: the main agent runs detached under `restart: unless-stopped`, so
sharing one meant the demo command always failed with "port is already allocated".

Notes on the image, since several of these are the difference between working and
subtly wrong:

- **The binary embeds its own timezone database** (`internal/scheduler`), and the image
  deliberately does **not** install `tzdata`. Every session boundary is defined in
  exchange time, and without a timezone database the fallback engages and puts every
  boundary an hour out from March to November.

  What guards this is worth being precise about, because an earlier version of these
  docs got it wrong. Go's `LoadLocation` consults `$ZONEINFO`, the system zoneinfo
  directory, and `$GOROOT/lib/time/zoneinfo.zip` *before* the `time/tzdata` embed, and
  that GOROOT zip ships with every toolchain. So no unit test can prove the embed is
  present — `TestExchangeTimezoneHandlesDST` only proves `ET` is a real DST-aware zone,
  which catches the fallback engaging but not the import being removed. The real guard is
  that the runtime image has no system database, making the embed the only source, plus
  the container check below.
- **`CGO_ENABLED=0` is load-bearing, not tuning.** The SQLite driver is pure Go, so
  the binary is fully static; verified with `file` reporting "statically linked".
- **`data/` and `logs/` are named volumes.** The database is what makes a restart
  recoverable and the audit trail is append-only evidence; both must outlive the
  container. Note the volume covers the whole directory, which matters because WAL
  mode writes `agent.db-wal` and `agent.db-shm` alongside the database.
- **Config is bind-mounted read-only**, so thresholds change with a restart rather
  than a rebuild. A copy is baked into the image so it still runs unmounted.
- **The build stage runs `go vet` and the full test suite**, so a failing test cannot
  produce an image. This daemon places orders; "it compiled" is not the bar.
- **Runs as an unprivileged user** (uid 10001) with `no-new-privileges`.
- **`ENTRYPOINT` uses exec form** so SIGTERM reaches the process and triggers the
  graceful shutdown the daemon implements, with a 30s grace period — being killed
  mid-order-submission is the one moment worth being patient about.
- **The healthcheck probes `/healthz`**, which returns 503 when the agent state is
  `ERROR`. `/api/status` answers 200 in every state — it is an information endpoint — so
  pointing a healthcheck at it would have reported a wedged agent as healthy. A bearish
  halt stays *healthy*: that is the kill switch working, not a fault.

  The probe runs the binary with `-healthcheck` rather than `wget`, so it reads the port
  from the same config the server binds; a hardcoded port would leave the container
  permanently unhealthy after a `listen_addr` change while the daemon was fine.

  Note this *reports* health, it does not act on it: compose's `restart: unless-stopped`
  ignores health status, so an unhealthy container is surfaced and left running.
- Container logs are capped (10MB × 5); the durable record is the audit trail on the
  volume, not Docker's log driver.

Verifying the timezone guarantee against a running container — the only place it can be
checked, since no unit test can:

```bash
docker compose exec agent sh -c 'ls /usr/share/zoneinfo 2>&1 | head -1'   # expect "No such file"
curl -s http://127.0.0.1:8081/api/status | grep -o '"GeneratedAt":"[^"]*"'
#   must read EDT between March and November; EST there means the embed is gone and
#   every session boundary is an hour out
```

Going live inside a container still takes both deliberate steps — `ALPACA_BASE_URL`
pointing at the live endpoint *and* `-allow-live-trading` added to the command — and
the PDT constraint above is unresolved.

### Bare binary

- Single Go binary (CGO-free, so it is portable and needs no SQLite system library), run as a long-lived process rather than invoked repeatedly via cron, so it can hold the daily loop state and serve the web page continuously.
- `go run ./cmd/agent -offline` runs the whole loop against a seeded fake broker with compressed session timings — no credentials, no network, no orders. This is how to see the daemon work end to end before an Alpaca account exists.
- Starting against the live endpoint additionally requires `-allow-live-trading`; the base URL alone is not enough, given the unresolved PDT constraint above.
- Where/how it's hosted (local machine, a VPS, a cloud VM) hasn't been decided yet — revisit once the binary exists and paper trading is being run somewhere for real.
