# Documentation

This directory is the working spec for the automated trading agent. It was written before the code existed, to front-load design decisions and open questions so implementation started from an agreed plan rather than assumptions made mid-coding; it is now kept current with what the daemon actually does, and each document states its status at the top.

- [`architecture.md`](./architecture.md) — components, data flow, how the daemon is structured
- [`strategy.md`](./strategy.md) — sentiment gate, screening criteria, entry/exit rules
- [`risk.md`](./risk.md) — position sizing, stop-loss, exposure limits, kill switch
- [`operations.md`](./operations.md) — deployment, config, secrets, observability
- [`web-ui.md`](./web-ui.md) — status page layout: agent-status legend, screening/positions tables, end-of-day summary
- [`decisions.md`](./decisions.md) — dated log of decisions made and why, plus open items still to be settled

Update these documents as decisions firm up or change — they should stay accurate to what's actually being built, not frozen at the moment they were written. `CLAUDE.md` at the repo root stays a short summary; this is where the detail lives.
