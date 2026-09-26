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

`docs/decisions.md` has an open-items list. Several are load-bearing (the unverified Alpaca endpoints, the PDT constraint). Do not invent values or providers for open items while coding — surface them.

## Audit trail

Decisions and actions append to `logs/audit-<date>.jsonl`. Separate from the stderr
operational log: this is the durable "why did it do that" record. When adding a new
decision point, record it — and think about repetition first, because the scan loop
runs every minute (`recordSkip` and the fault de-duplication exist for exactly that).
Never put credentials in an event.

## Commands

```bash
go build ./...                       # build everything
go test ./...                        # full suite
go test ./internal/strategy/ -run TestEvaluateExitPriority -v   # one test
go vet ./...
gofmt -l .                           # must print nothing

go run ./cmd/agent -offline          # run with a fake broker: no credentials, no network, no real orders
jq -r '"\(.at[11:19])  \(.kind)  \(.summary)"' logs/audit-*.jsonl   # read the audit trail
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
- **The scan covers the whole market, cheap filter first.** `gatherCandidates` batches snapshots for every tradable US equity (universe cached per session), filters on the price move, and only then makes per-symbol news/average-volume calls. Keep that order — it is what makes a full-market scan affordable at a one-minute cadence. When the movers exceed `max_enriched`, selection is by **dollar volume**, never percentage change: the strategy ranks entries by relative volume, so a %-change sort picks the wrong names.
- **News queries must batch and paginate.** Alpaca caps a news page at 50 stories. A single unpaginated request across a large symbol list makes most symbols look newsless, which silently disqualifies valid candidates — this was a real bug. `NewsCounts` batches 25 symbols per request and follows `next_page_token`.
- **The news window starts before the market open**, via `screening.news_lookback`. Gap catalysts break overnight or pre-market, so a window starting at 09:30 cannot see the story that caused the move. Do not "simplify" this back to `sess.Open`.
- **`market_data.feed` is load-bearing.** The relative-volume criterion only means anything on the `sip` consolidated tape; on the free `iex` feed it compares one exchange against a market-wide average.
- **One evaluation feeds both the page and the buy decision.** `maybeScreen` calls `screen()` once and passes the same `evals` slice to `SaveScreenSnapshot` and `enterPositions`. Do not add a second evaluation path — they would drift. Entry then applies four further gates (position cap, already held, same-day re-entry, affordable size), so qualifying ≠ bought; the outcome per candidate is recorded on the evaluation and shown in the page's Action column.
- **Entry failures are per-candidate.** An unavailable price or rejected order skips that one candidate; the pass continues. Only non-candidate-specific failures (e.g. store errors) abort and fault the agent. Don't "simplify" this back to returning on first error.
- **Screening is three criteria**: a same-day news catalyst, ≥10% intraday move, and ≥5x average volume. Nothing filters on company size, so the screen admits large caps — worth knowing, since parts of the docs still describe the strategy as small-cap.
- **The two manual buttons cannot trade.** `engine.CheckSentiment` and `engine.ScreenNow` back them; both share code with the automated path but stop short of `enterPositions`, and neither persists anything. If you refactor the screening path, keep that separation — the automated scan buys, and the buttons are expected to work with the market closed.
- **The web layer never calls the broker** (except through those two read-only actions). The trading loop persists each position's last mark, so the ~12s page poll costs no market-data API calls. `/fragment` returns the same template's content block, which is what the page swaps in — markup is never duplicated in JavaScript.
- **Migrations live in `internal/store/migrations/`**, not at the repo root, because `go:embed` cannot reach outside its package and the deployment target is a single self-contained binary.
- **Live trading is gated behind `-allow-live-trading`** on top of the base URL, because the Pattern Day Trader constraint in `docs/operations.md` is unresolved.
