---
name: grill-me
description: Adversarial plan interview before implementation. Use when the user says "grill me", "interview my plan", or "stress test this idea"; before complex work (strategy/risk changes, schema or migration changes, new trading behaviour); or when a plan has unresolved decisions or seems vague. Not for reviewing existing code (use code-review) or simple one-off tasks.
version: 1.0.0
author: "Adapted from Rafael Zendron (rafaumeu) + Matt Pocock (mattpocock/skills, grilling) + Hermes Agent"
license: MIT
---

# Grill Me

Stress-tests a plan through structured adversarial questioning before any
code is written. Models the plan as a **design tree** — every decision
branches into the decisions that hang off it — and interviews the user in
rounds until every branch is resolved and nothing is silently assumed.

## When to Use

- User says "grill me", "interview my plan", "stress test this idea"
- Before complex work: changes to `internal/strategy`, `internal/risk`,
  `internal/engine` scheduling, store migrations, or anything else
  `docs/decisions.md` would need to record
- A plan has unresolved decisions or seems vague

Do NOT use for existing code (use the `code-review` skill) or simple
one-off tasks.

## Prerequisites

None. The skill works on any plan or raw idea the user describes.

## Core Mechanic: Frontier Rounds

Map the plan as a design tree. The **frontier** is every decision whose
prerequisites are already settled — the questions you can ask NOW without
guessing at answers you haven't heard yet.

Work in **rounds**: ask the whole current frontier in one message, numbered,
each question carrying your recommended answer. Then wait. A question whose
answer depends on another question still open in this round belongs to a
LATER round, not this one.

Format each round like so:

```
❓ Q1 — <question title>: <question body, options if relevant>
➡️ Recommendation: <your recommended answer + one-line why>

❓ Q2 — <question title>: <question body>
➡️ Recommendation: <...>
```

Each answer reshapes the tree: settled decisions push the frontier outward
and unblock dependent questions. Recompute the frontier and ask the next
round. Use `AskUserQuestion` when the round reduces to a small set of
mutually exclusive choices; use a plain numbered message when answers are
open-ended or there are more than a few questions.

**Facts are your job; decisions are the user's.** When a frontier question
needs a fact from the environment (this codebase, `docs/`, config, git
history), find it yourself with `Grep` / `Glob` / `Read` / `Bash` — or
dispatch a `fork` / `Explore` agent for a heavy exploration. Never ask the
user for anything you could look up, and never invent values for the
open items already tracked in `docs/decisions.md` — surface them as
questions instead. Don't block on an exploration: only the questions
downstream of it wait; ask the rest of the frontier now.

## Question Coverage (work these branches into the tree)

**Understanding** — the real goal and boundaries:
- What is the ACTUAL objective? What is explicitly IN and OUT of scope?
- What are the constraints (paper vs. live trading, PDT limits, existing
  config knobs)? Who/what consumes the result (the engine loop, the status
  page, `cmd/backtest`)?

**Technical decisions** — for each architectural choice:
- "Why this approach and not X?" / "What happens if Y fails?"
- "What's the worst case?" / "How would you roll back?"
- Cross-reference `docs/architecture.md`, `docs/strategy.md`, `docs/risk.md`,
  and the "Architecture notes" in `CLAUDE.md`; if the project already has a
  pattern for this, call it out instead of asking.

**Edge cases:**
- "What happens pre-market / post-market / on a half day?"
- "What if the broker rejects the order or returns late?"
- "What if this fires every tick instead of once?" (the scan loop runs
  every minute — repetition is a first-class concern here)
- "What are the risk/exposure implications?"

## Synthesis (when the frontier is empty)

1. Summarize ALL decisions in bullet points
2. List anything left open, and what is explicitly OUT of scope — note
   which open items belong in `docs/decisions.md`
3. Ask: "Aligned? Should I start implementing, or adjust anything?"

Do not act on the plan until the user confirms shared understanding.

## Pitfalls

1. **Asking questions out of dependency order.** A question that depends on
   an unanswered question is a guess wearing a question mark. Keep it for a
   later round.
2. **Skipping the codebase and docs.** Find facts in `docs/` and the code
   instead of asking the user.
3. **Accepting "I don't know" as final.** Suggest options, explain
   trade-offs, make a recommendation.
4. **Writing code during the interrogation.** Alignment only — code after
   the explicit green light.
5. **Being too agreeable.** Your job is to find problems. If everything
   looks fine, look harder.
6. **Inventing values for open items.** `docs/decisions.md` has unresolved
   items (unverified Alpaca endpoints, the PDT constraint) — surface them,
   don't guess past them.

## Verification

- [ ] Every question in a round had all its prerequisites already settled
- [ ] Provided a recommendation with each question
- [ ] Explored the codebase/docs for facts instead of asking the user
- [ ] Frontier empty (no branch silently assumed) before synthesizing
- [ ] Produced a clear summary of all decisions and open items
- [ ] Confirmed user alignment before stopping
