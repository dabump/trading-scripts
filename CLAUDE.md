# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go daemon that trades US small-cap equities on a momentum strategy via Alpaca (paper trading) and serves a read-only dark-theme status page. The design was agreed before implementation and lives in `docs/` — **read the relevant doc before changing behaviour in that area**, and update it in the same change if the behaviour moves:

- [`docs/architecture.md`](./docs/architecture.md) — components, data flow, package layout
- [`docs/strategy.md`](./docs/strategy.md) — sentiment gate, screening, entry/exit rules
- [`docs/risk.md`](./docs/risk.md) — position sizing, stop-loss, exposure cap, kill switch
- [`docs/operations.md`](./docs/operations.md) — deployment, secrets, persistence, config reference
- [`docs/web-ui.md`](./docs/web-ui.md) — status page layout and agent-state legend
- [`docs/decisions.md`](./docs/decisions.md) — why each choice was made, and what is still open

`docs/decisions.md` has an open-items list. Several are load-bearing (no float data source, the PDT constraint). Do not invent values or providers for open items while coding — surface them.

## Commands

```bash
go build ./...                       # build everything
go test ./...                        # full suite
go test ./internal/strategy/ -run TestEvaluateExitPriority -v   # one test
go vet ./...
gofmt -l .                           # must print nothing

go run ./cmd/agent -offline          # run with a fake broker: no credentials, no network, no real orders
go run ./cmd/agent                   # run for real; needs ALPACA_* env vars (see .env.example)
```

`-offline` seeds a fake broker and compresses the session timings so a whole
simulated trading day (sentiment gate → screening → entry → forced EOD exit)
plays out in a few minutes. It is the way to exercise the daemon end to end
without an Alpaca account. The status page is then at `http://localhost:8080`,
with `/api/status` returning the same state as JSON.

## Architecture notes that aren't obvious from a single file

- **One `Tick`, no per-phase goroutines.** `internal/engine` decides what is due on every tick based on the clock's position in the session (`internal/scheduler.PhaseAt`). Adding behaviour means extending `Tick`, not spawning a timer.
- **The clock is injected** (`engine.Deps.Now`). This is what makes a whole trading day testable deterministically — see `internal/engine/engine_test.go`. Never call `time.Now()` directly inside engine logic.
- **Exit-rule order is load-bearing.** `strategy.EvaluateExit` returns the *first* matching trigger, and the order encodes priority: forced EOD, then stop-loss, then trailing stop, then MACD. `docs/risk.md` requires the stop-loss to outrank the momentum signals.
- **Price thresholds compare with an epsilon** (`strategy.priceEpsilon`). Thresholds are products like `peak*0.95`, which binary floating point cannot represent exactly, so an exact-boundary price would otherwise miss its exit.
- **Nothing can qualify for entry yet.** No float data source is implemented, so the float criterion fails closed for every candidate (`broker.NoFloatProvider`). This is deliberate, not a bug: see the open item in `docs/decisions.md`. The status page says so out loud.
- **The web layer never calls the broker.** The trading loop persists each position's last mark, so the ~12s page poll costs no market-data API calls. `/fragment` returns the same template's content block, which is what the page swaps in — markup is never duplicated in JavaScript.
- **Migrations live in `internal/store/migrations/`**, not at the repo root, because `go:embed` cannot reach outside its package and the deployment target is a single self-contained binary.
- **Live trading is gated behind `-allow-live-trading`** on top of the base URL, because the Pattern Day Trader constraint in `docs/operations.md` is unresolved.
