---
name: grill-me
description: Interrogates the user with pointed questions to pin down the design decisions behind an automated trading agent (markets/instruments, strategy type, data source, execution/broker, backtest vs paper vs live, risk controls, runtime, deployment, monitoring, secrets), then proposes a concrete project directory layout from the answers. Use this whenever the user wants to start, scaffold, bootstrap, or lay out a new automated/algo trading bot or agent project, or says things like "help me structure this trading project," "grill me," or "let's plan out the trading agent" — even if they haven't written any code yet. Do NOT use it for one-off analysis scripts (like a market scanner) that aren't meant to place or manage trades.
---

# Grill Me: Trading Agent Project Setup

Founders and engineers building an automated trading agent tend to jump straight to code before deciding what the thing is actually supposed to do. A wrong or unstated assumption here (e.g. "paper trading" vs "live trading", or "no broker chosen yet") compounds into wasted scaffolding later. This skill's job is to force those decisions into the open *before* any files get created, then turn the answers into a project layout that actually fits the system being built — not a generic template.

## How to run this

1. **Ask, don't assume.** Work through the question groups below using `AskUserQuestion` (batch related questions together, one call per group is fine) rather than a single wall-of-text prompt. Skip a question only if the user already answered it earlier in the conversation.
2. **Push back on vague answers.** If the user says something like "just do momentum trading on stocks" without a data source or risk plan, don't let it slide — ask the follow-up. The point of "grilling" is surfacing gaps, not just collecting a checklist.
3. **Synthesize before building.** Once you have enough to reason about the shape of the system, write a short summary of the decisions back to the user and propose a concrete directory layout (see below). Get explicit confirmation before creating any files or directories.
4. **Keep the layout proportional.** Don't scaffold `backtest/`, `execution/`, `risk/` etc. as empty ceremony if the user's answers don't call for them yet (e.g. a backtest-only project doesn't need a live `execution/` module). Match the folders to what was actually decided.

## Question groups

Ask these in order — each group builds on the last, and later answers often make earlier ones more specific.

**1. Scope and markets**
- What instruments: stocks, crypto, futures, options, forex?
- What's the strategy category (momentum, mean-reversion, market-making, stat-arb, trend-following, something else)? It's fine if this is still fuzzy, but get at least a direction.

**2. Data and execution**
- Where does market data come from (broker feed, paid data API, existing scripts in this repo like `market-heat.sh`)? Real-time or delayed is fine, but name the source.
- Where do trades actually get placed — which broker/exchange, and do they have an API (e.g. Alpaca, Interactive Brokers, a crypto exchange API)?

**3. Trading mode**
- Backtesting only, paper trading, live trading, or a progression through some/all of those? This one matters a lot for layout — a backtest-only project doesn't need the same scaffolding as a live one.

**4. Risk management**
- Position sizing approach?
- Stop-loss / max-drawdown rules?
- Is there a kill switch or manual override to halt the agent?

If the user hasn't thought about this, don't let it go unanswered — flag that skipping risk controls on anything that touches live capital is the highest-leverage mistake to avoid at this stage, and ask them to at least commit to a placeholder rule.

**5. Runtime and deployment**
- Language/runtime (Python, given the existing `market-heat.sh` calls into Python, is a reasonable default to confirm rather than assume).
- How does it run — cron job, long-running daemon/process, cloud scheduler, manually triggered?

**6. Observability and secrets**
- What logging/monitoring/alerting do they want (files, a dashboard, Slack/email alerts on errors or trades)?
- How will API keys and broker credentials be stored (env vars, a secrets manager, a local `.env` that's gitignored)? Flag explicitly that credentials must never be committed to the repo.

## Synthesizing the layout

After the interview, write back a short recap ("Here's what I heard: ...") and propose a directory structure adapted to the answers. Use this as a starting reference, not a template to apply unchanged:

```
project-root/
├── strategies/     # strategy logic, one module per strategy
├── data/           # data source adapters / cached data (only if data fetching is nontrivial)
├── backtest/       # backtest engine + historical runs (only if backtesting is in scope)
├── execution/       # broker/exchange order placement (only if paper/live trading is in scope)
├── risk/           # position sizing, stop-loss, kill-switch logic
├── config/         # strategy params, environment-specific settings (not secrets)
├── logs/           # runtime logs (gitignored)
├── tests/
└── .env.example    # documents required secrets without real values (real .env gitignored)
```

Adjust names/folders to match the user's actual answers — e.g. a crypto market-maker needs an `orderbook/` concept that a daily momentum stock strategy doesn't; a backtest-only project can drop `execution/` and `risk/`'s live-trading pieces entirely.

Only create the directories/files after the user confirms the proposed layout.
