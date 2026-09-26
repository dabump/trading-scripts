# Operations

**Status:** design only.

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

| Key | Current value | Source |
|---|---|---|
| Max float (shares) | 10,000,000 | `strategy.md` §2 |
| Min intraday price move | ≥10% | `strategy.md` §2 |
| Min volume multiple | ≥5x average volume | `strategy.md` §2 |
| Average-volume lookback period | **not yet finalized** | `strategy.md` §2 |
| Position size | 10% of portfolio per trade | `risk.md` |
| Max concurrent positions | 5 | `risk.md` |
| Per-trade hard stop-loss | −10% | `risk.md` |
| Profit target % | **not yet finalized** | `strategy.md` §4 |
| Trailing stop % | **not yet finalized** | `strategy.md` §4 |
| MACD periods (fast/slow/signal) | 5 / 10 / 3 | `strategy.md` §4 |
| MACD candle interval | 15 minutes | `strategy.md` §4 |
| Forced EOD exit offset | 30 min before close | `strategy.md` §4 |
| First-hour sentiment poll interval | 10 minutes | `strategy.md` §1 |
| Screener re-scan interval | 1 minute | `strategy.md` §2 |
| Sentiment classification thresholds (what counts as "overwhelmingly bearish") | **not yet finalized** | `strategy.md` §1 |

## Persistence

- SQLite, single file under `data/` (gitignored — this holds real trade history, not something to check in).
- Schema changes go through `migrations/` rather than ad-hoc `ALTER TABLE` in application code, so the schema's history stays reviewable.
- The daemon must be able to restart mid-day and resume from whatever `store` has — open positions, today's sentiment readings, and where in the daily loop it left off — without re-entering positions it already holds or re-running the first-hour sentiment poll from scratch.

## Observability (v1 scope)

- The status web page (reading from `store`) and local log output are the only observability in v1.
- No external alerting (Slack, email, etc.) yet. If/when that's wanted, the natural hook points are: position opened/closed, daily kill switch triggered, and unhandled errors — but don't build this speculatively ahead of it being asked for.

## Deployment

- Single Go binary, run as a long-lived process (not invoked repeatedly via cron) so it can hold the daily loop state and serve the web page continuously.
- Where/how it's hosted (local machine, a VPS, a cloud VM) hasn't been decided yet — revisit once the binary exists and paper trading is being run somewhere for real.
