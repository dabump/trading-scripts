# Trading Agent

A Go daemon that day-trades US equities on an intraday momentum strategy through the
[Alpaca](https://alpaca.markets) API, and serves a read-only, dark-theme status page
showing what it is doing and why.

> **Paper trading by default.** Live trading needs two deliberate steps (the live
> base URL *and* the `-allow-live-trading` flag), and the Pattern Day Trader
> constraint in [`docs/operations.md`](./docs/operations.md) is unresolved. Read that
> before pointing this at real money.

## What it does

Each trading day the agent runs through a fixed sequence, driven by one scheduler tick:

1. **Pre-market screening** (from 04:00 ET, optional) — scans the market before the
   bell with looser volume thresholds. Pre-market entry is configurable and uses
   extended-hours limit orders.
2. **Sentiment gate** — for the first few minutes after the 09:30 open it samples
   SPY, QQQ and IWM. An overwhelmingly bearish tape trips a kill switch and the agent
   does not trade that day.
3. **Screening** — every minute it scans every tradable US equity and keeps symbols
   that pass all three criteria: a same-day **news catalyst**, a **≥10% intraday
   move**, and **≥5× average volume**. Price and dollar-volume floors filter out
   untradable names first.
4. **Entry** — a candidate is only bought on a **micro pullback** setup (a surge, a
   one- or two-candle pause, then a break above the pause) while above the EMA and
   VWAP. The setup's stop sits just under the pause low.
5. **Sizing** — position size comes from that stop, so each trade risks a fixed
   fraction of equity (1% by default), capped per position and by a maximum number
   of concurrent positions.
6. **Exits** — the stop (with a candle trail), a partial scale-out at the first
   target with the stop moved to breakeven, and a forced exit shortly before the
   close.

Every decision is written to an append-only audit trail (`logs/audit-<date>.jsonl`),
and state is kept in SQLite (`data/agent.db`), so a restart picks up where it left
off. The status page shows the agent's state, the screening table, open and closed
positions, and the account balance, and lets you open or close a position by hand.

`cmd/backtest` replays the same production rules against historical data to measure
the strategy before its settings are changed.

The design lives in [`docs/`](./docs/README.md): architecture, strategy, risk,
operations (including the full config reference), the web UI and the decision log.

## Requirements

- **An Alpaca trading account and API keys.** A
  [paper trading account](https://app.alpaca.markets) is enough to run the agent
  and needs no funding or identity verification. Generate an API key and secret from
  the paper dashboard.
- **Market data subscription.** The shipped config uses the `sip` consolidated feed
  (`market_data.feed`), which requires Alpaca's paid *Algo Trader Plus* data plan.
  The free `iex` feed works, but the relative-volume criterion is not meaningful on a
  single exchange's volume.
- **Go 1.27+** to run stand-alone, or **Docker with Compose** to run in a container.

You can try the agent without any of this: offline mode (below) uses a fake broker.

## Configuration

**Credentials** come from environment variables. Copy the template and fill in the
keys from your Alpaca dashboard:

```bash
cp .env.example .env
```

```dotenv
ALPACA_API_KEY=your-key
ALPACA_API_SECRET=your-secret
ALPACA_BASE_URL=https://paper-api.alpaca.markets
```

`.env` is gitignored — never commit it.

**Strategy settings** live in [`config/config.yaml`](./config/config.yaml):
screening thresholds, entry pattern, risk per trade, exits, timings, sentiment
basket, order type, the web listen address and storage paths. Each key is
documented in the file and in the config table in
[`docs/operations.md`](./docs/operations.md). Values marked `PROPOSED` are
placeholders awaiting review.

## Running stand-alone

```bash
./run.sh                    # loads .env and runs the agent against Alpaca paper trading
./run.sh -verbose           # extra arguments are passed through to the agent
```

`run.sh` is equivalent to exporting the `ALPACA_*` variables and running
`go run ./cmd/agent`. To build a binary instead:

```bash
CGO_ENABLED=0 go build -o agent ./cmd/agent
set -a; . ./.env; set +a
./agent -config config/config.yaml
```

The status page is at <http://localhost:8080> (JSON at `/api/status`, healthcheck at
`/healthz`).

| Flag | Meaning |
|---|---|
| `-config <path>` | Config file (default `config/config.yaml`) |
| `-offline` | Fake broker and compressed session timings — no credentials, no network, no real orders |
| `-verbose` | Debug logging |
| `-allow-live-trading` | Required, together with the live base URL, to trade real money |

**Offline demo** — plays a whole simulated trading day (pre-market → sentiment gate →
screening → entry → forced exit) in a few minutes:

```bash
go run ./cmd/agent -offline
```

**Backtest:**

```bash
go run ./cmd/backtest -from 2025-09-28 -to 2026-09-26
```

## Running with Docker

```bash
cp .env.example .env              # then fill in your Alpaca keys
docker compose up -d --build      # build and start the agent, paper trading
docker compose logs -f agent      # follow the operational log
docker compose down               # stop (graceful, up to 30s)
```

The status page is at <http://localhost:8081>. The image runs the test suite during
the build, runs as a non-root user, and has a healthcheck on `/healthz`.

- `config/config.yaml` is bind-mounted read-only, so settings can be changed with a
  `docker compose restart agent` — no rebuild needed.
- The database and audit trail live in the named volumes `agent-data` and
  `agent-logs`, so they survive container rebuilds.

**Offline demo in Docker** (no credentials needed, separate volumes, port 8082):

```bash
docker compose --profile demo up agent-offline
```

Then open <http://localhost:8082>.

## Reading the audit trail

```bash
jq -r '"\(.at[11:19])  \(.kind)  \(.summary)"' logs/audit-*.jsonl
```

In Docker, the logs are in the `agent-logs` volume:

```bash
docker compose exec agent sh -c 'cat /app/logs/audit-*.jsonl'
```

## Development

```bash
go build ./...
go test ./...
go vet ./...
gofmt -l .     # must print nothing
```

See [`CLAUDE.md`](./CLAUDE.md) for architecture notes that aren't obvious from a
single file.
