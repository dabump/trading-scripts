# Operations

**Status:** implemented.

## Running modes

- **Paper trading** is the v1 target — Alpaca's paper account, same API shape as live. This is explicitly the validation step before any real capital is involved.
- **Live trading** is a later, deliberate switch (different Alpaca base URL/keys), not a default or something to enable casually. Treat any change from paper to live as requiring explicit user sign-off, not an automatic config flip during normal development.

### Pattern Day Trader (PDT) constraint — hard blocker before going live

FINRA's Pattern Day Trader rule restricts margin accounts under $25,000 equity to 3 day-trades per rolling 5 business days. This strategy is designed to open and close up to 5 day-trades *per day* — as of 2026-09-26 the account is under $25k (or funding level undecided), so running this live as designed would very quickly get the account flagged/restricted by Alpaca.

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
| `screening.min_intraday_pct` | 10.0 | `strategy.md` §2 |
| `screening.min_volume_multiple` | 5.0 | `strategy.md` §2 |
| `screening.avg_volume_lookback_days` | 20 | **PROPOSED** — excludes today |
| `screening.max_enriched` | 100 | **PROPOSED** — caps the per-symbol lookups after the move filter, not the scan itself |
| `screening.news_lookback` | 18h | **PROPOSED** — must reach back past the open so pre-market catalysts are visible; validated to be ≥7h |
| `risk.position_size_pct` | 10.0 | `risk.md` |
| `risk.max_concurrent_positions` | 5 | `risk.md`; validated so sizing × concurrency cannot exceed 100% |
| `risk.stop_loss_pct` | 10.0 | `risk.md` |
| `risk.allow_same_day_reentry` | `false` | **PROPOSED** |
| `exit.profit_target_pct` | 15.0 | **PROPOSED** — arms the trailing stop |
| `exit.trailing_stop_pct` | 5.0 | **PROPOSED** |
| `exit.macd_fast` / `macd_slow` / `macd_signal` | 5 / 10 / 3 | `strategy.md` §4 |
| `exit.macd_interval_minutes` | 15 | `strategy.md` §4; warm-up is slow+signal = 13 bars ≈ 3h15m |
| `exit.eod_exit_offset_minutes` | 30 | `strategy.md` §4 |
| `timing.sentiment_poll_interval` | 10m | `strategy.md` §1 |
| `timing.sentiment_window` | 1h | `strategy.md` §1 |
| `timing.screener_scan_interval` | 1m | `strategy.md` §2 |
| `timing.position_poll_interval` | 15s | **PROPOSED** — how often open positions are re-marked |
| `sentiment.symbols` | SPY, QQQ, IWM | IWM included because the strategy trades small caps |
| `sentiment.bearish_avg_pct` | −0.8 | **PROPOSED** — must be negative |
| `sentiment.require_all_negative` | `true` | **PROPOSED** |
| `execution.order_type` | `market` | **PROPOSED** — guarantees fills but can slip on thinly traded names |
| `execution.limit_slip_pct` | 0.5 | Only used when `order_type: limit` |
| `web.listen_addr` | `:8080` | |
| `web.poll_interval` | 12s | `web-ui.md` (~10–15s) |
| `storage.database_path` | `data/agent.db` | Created on start; gitignored |
| `audit.directory` | `logs` | Append-only JSONL trail, one file per session date; gitignored |

Invalid configurations are rejected at startup rather than mid-session: unknown
keys, a non-negative bearish threshold, MACD fast ≥ slow, and total exposure over
100% all fail fast, because discovering them with real positions open is the
expensive way to find out.

## Persistence

- SQLite, single file under `data/` (gitignored — this holds real trade history, not something to check in).
- Schema changes go through `internal/store/migrations/` rather than ad-hoc `ALTER TABLE` in application code, so the schema's history stays reviewable. They are embedded in the binary and applied idempotently on open; they live inside the package rather than at the repo root because `go:embed` cannot reach outside its own directory and the deployment target is a single self-contained binary.
- The daemon restarts mid-day and resumes from whatever `store` has: open positions, today's sentiment readings (the 10-minute cadence is derived from them, so a restart neither double-polls nor skips), the session's gate verdict, and which symbols have already been traded today.
- On startup `engine.Reconcile` treats the broker as the authority — adopting positions the store does not know about, closing ones the broker no longer holds as `RECONCILED`, and resolving orders that were submitted but never confirmed.

## Audit trail

Decisions and actions are appended to `logs/audit-<session-date>.jsonl`, one JSON
object per line. This is deliberately separate from the operational log on stderr:
that one is for watching the daemon work, this one is the record you read back weeks
later to answer "why did it buy that, and what did it know at the time?".

Recorded events: `AGENT_STARTED`, `SENTIMENT_READING`, `GATE_RESOLVED`,
`TRADING_HALTED`, `POSITION_OPENED`, `POSITION_CLOSED`, `ENTRY_SKIPPED`,
`RECONCILED`, `FAULT`. Only state changes and actions appear — the screen runs every
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
