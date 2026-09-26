# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

An automated US-equities trading agent is planned but **not yet implemented**; its full design is captured below so implementation can start from an agreed spec rather than rediscovered assumptions. Do not assume any of the planned directories/files under "Planned: automated trading agent" exist until they've actually been created.

## Planned: automated trading agent

Go daemon, single continuously-running process, that trades US small-cap equities on a momentum strategy via Alpaca (paper trading first) and serves a read-only status web page. Full design lives in `docs/` — read it before implementing anything in this area:

- [`docs/architecture.md`](./docs/architecture.md) — components, data flow, planned package layout
- [`docs/strategy.md`](./docs/strategy.md) — sentiment gate, screening, entry/exit rules
- [`docs/risk.md`](./docs/risk.md) — position sizing, stop-loss, exposure cap, kill switch
- [`docs/operations.md`](./docs/operations.md) — deployment, secrets, persistence, observability
- [`docs/decisions.md`](./docs/decisions.md) — why these choices were made, and what's still open

These decisions came from a `grill-me` skill (`.claude/skills/grill-me/`) interview and represent committed choices, not assumptions to silently override. Several specific numbers (screening thresholds, profit-target %) are explicitly not yet finalized — see `docs/decisions.md`'s open items before inventing values for them.
