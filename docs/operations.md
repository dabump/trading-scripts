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
| `screening.max_float_shares` | 10,000,000 | `strategy.md` §2 |
| `screening.min_intraday_pct` | 10.0 | `strategy.md` §2 |
| `screening.min_volume_multiple` | 5.0 | `strategy.md` §2 |
| `screening.avg_volume_lookback_days` | 20 | **PROPOSED** — excludes today |
| `screening.universe_size` | 50 | **PROPOSED** — movers pulled per scan |
| `screening.float_provider` | `none` | Only `none` is accepted; no float source exists yet, so the criterion fails closed and nothing qualifies |
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
| `execution.order_type` | `market` | **PROPOSED** — guarantees fills but can slip on low-float names |
| `execution.limit_slip_pct` | 0.5 | Only used when `order_type: limit` |
| `web.listen_addr` | `:8080` | |
| `web.poll_interval` | 12s | `web-ui.md` (~10–15s) |
| `storage.database_path` | `data/agent.db` | Created on start; gitignored |

Invalid configurations are rejected at startup rather than mid-session: unknown
keys, a non-negative bearish threshold, MACD fast ≥ slow, and total exposure over
100% all fail fast, because discovering them with real positions open is the
expensive way to find out.

## Persistence

- SQLite, single file under `data/` (gitignored — this holds real trade history, not something to check in).
- Schema changes go through `internal/store/migrations/` rather than ad-hoc `ALTER TABLE` in application code, so the schema's history stays reviewable. They are embedded in the binary and applied idempotently on open; they live inside the package rather than at the repo root because `go:embed` cannot reach outside its own directory and the deployment target is a single self-contained binary.
- The daemon restarts mid-day and resumes from whatever `store` has: open positions, today's sentiment readings (the 10-minute cadence is derived from them, so a restart neither double-polls nor skips), the session's gate verdict, and which symbols have already been traded today.
- On startup `engine.Reconcile` treats the broker as the authority — adopting positions the store does not know about, closing ones the broker no longer holds as `RECONCILED`, and resolving orders that were submitted but never confirmed.

## Observability (v1 scope)

- The status web page (reading from `store`) and structured log output (`log/slog`, to stderr) are the only observability in v1. `/api/status` returns the same state as JSON, which is what makes a running daemon checkable without scraping HTML.
- Refusals that repeat every scan (a symbol already held, or already traded today) log at debug level; everything else that skips a candidate logs at info. At a one-minute cadence the routine ones would otherwise bury the log in hundreds of identical lines.
- No external alerting (Slack, email, etc.) yet. If/when that's wanted, the natural hook points are: position opened/closed, daily kill switch triggered, and unhandled errors — but don't build this speculatively ahead of it being asked for.

## Deployment

- Single Go binary (CGO-free, so it is portable and needs no SQLite system library), run as a long-lived process rather than invoked repeatedly via cron, so it can hold the daily loop state and serve the web page continuously.
- `go run ./cmd/agent -offline` runs the whole loop against a seeded fake broker with compressed session timings — no credentials, no network, no orders. This is how to see the daemon work end to end before an Alpaca account exists.
- Starting against the live endpoint additionally requires `-allow-live-trading`; the base URL alone is not enough, given the unresolved PDT constraint above.
- Where/how it's hosted (local machine, a VPS, a cloud VM) hasn't been decided yet — revisit once the binary exists and paper trading is being run somewhere for real.
