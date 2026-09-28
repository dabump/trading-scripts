# Strategy

**Status:** implemented. Values below marked **PROPOSED** were not settled in design; they were filled in during implementation so the agent could run and are awaiting review (see [`decisions.md`](./decisions.md)). All of them live in `config/config.yaml` — change them there, and update this doc to match.

## 1. Sentiment gate (first hour)

- From market open, poll Alpaca market data every 10 minutes for one hour.
- No trades are placed during this hour regardless of what the data shows.
- At the end of the hour, classify the session as **overwhelmingly bearish** or **not overwhelmingly bearish**:
  - Overwhelmingly bearish → hand off to `risk`'s daily kill switch; no further trades today.
  - Anything else (clearly bullish *or* neutral/ambiguous) → proceed to screening for the rest of the day. Only a clear bearish reading halts trading — a neutral/unclear reading is treated the same as bullish, not the same as bearish.
- **Classification rule (PROPOSED):** the session is overwhelmingly bearish when the average intraday change across SPY, QQQ and IWM is at or below **−0.8%** *and* none of the three is positive. Anything else proceeds. IWM is in the basket deliberately: the strategy trades small caps, so a small-cap index belongs in the read of the tape, not just SPY and QQQ.
- If the daemon starts *after* the first hour, no readings exist and the gate cannot be judged. It halts for the day rather than trading without the safety check ever having run.

## 2. Screening (momentum candidates)

Screening only starts once the sentiment gate has passed (i.e. after the first hour) — there is no background screening during the no-trade hour.

Nothing in the screen filters on company size, so it admits large caps as readily as small ones.

**Candidate universe:** every tradable US equity (`broker.TradableAssets`, cached per session) is scanned each pass via batched snapshots. This replaced an earlier reliance on Alpaca's market-movers endpoint, which is hard-capped at 50 symbols and sorted by percentage change — the wrong ordering for a strategy that ranks on relative volume. Only gainers are acted on; the strategy buys strength and never shorts.

**Tradability floors — a gate, not a criterion.** Before any per-symbol enrichment, a symbol must clear `min_price` (default $1) and `min_dollar_volume` (default $1,000,000 traded so far today). `TradableAssets` excludes OTC but *not* warrants, units or penny stocks, and with the float criterion removed nothing else kept them out: a backtest over 2024-09 → 2026-09 found the candidate list dominated by warrants (`GIBOW` at $0.025), SPAC units (`QETAU`, a 20-session average volume of **15 shares**) and sub-$1 names (`ASBP`, 371 shares/day). None are fillable at a $1,000 position without moving the price, so they are not tradable regardless of how well they score on momentum. They are therefore rejected outright rather than shown as failing candidates — see [`decisions.md`](./decisions.md).

**Evaluation order:** the tradability floors are applied first, then the intraday-move criterion from the batched snapshot, and only symbols that clear both get the per-symbol news and average-volume lookups. At a one-minute cadence, enriching every mover would multiply API calls for names already disqualified on the cheapest criterion. Symbols that fail the move gate still appear on the status page, with the un-fetched criteria shown as unevaluated.

**Re-scan frequency:** every 1 minute once the sentiment gate has opened. This is deliberately much tighter than the first hour's 10-minute sentiment-poll cadence, chosen because a momentum setup can fully develop and finish within minutes — a slower interval risks discovering candidates only after the move that made them interesting is already over.

A candidate must pass **all three** of the following (AND, not scored/weighted — any one failing disqualifies the candidate):

1. **News catalyst** — at least one news headline for the ticker within the `news_lookback` window (presence check only; no sentiment/NLP scoring — the price/volume move itself is what confirms the catalyst is moving the stock, the headline just confirms one exists).

   The window reaches back **past the market open**, not from it. A gap-up's catalyst almost always breaks overnight or in the pre-market session, so searching from 09:30 would miss the very story that caused the move and report "no news" for precisely the candidates worth trading. The default 18h covers the prior afternoon through this morning's pre-market. Known limitation: over a weekend or holiday, 18h will not reach back to the last session's news — a Monday gap driven by Friday-evening news would be missed unless the lookback is widened.
2. **Price move** — already up ≥10% intraday.
3. **Volume** — ≥5x average volume, where "average" is the mean of the previous **20** sessions (PROPOSED), *excluding today*. Today's partial volume is the number being compared against the average, so folding it in would dilute the very spike the criterion looks for.

All three thresholds live in `config/config.yaml` as tunable values, not hardcoded, since they'll likely need adjusting after paper-trading results come in.

News headlines come from Alpaca's news endpoint and are counted per symbol for the current session.

**Tie-break (PROPOSED):** when more candidates qualify than there are free position slots, they are ranked by relative volume, highest first. It is the criterion that best separates a genuine, liquid move from a thin drift.

## 3. Entry

Entry uses **the same evaluation** the status page displays — one `screener.Evaluate`
pass per scan feeds both, so the two can never disagree about what qualifies.

- Day-trade only — no overnight holds.
- On a qualifying candidate: buy sized at 10% of portfolio (see [`risk.md`](./risk.md) for sizing and exposure cap interaction).

**Qualifying is not the same as being bought.** Four further gates apply only at
entry, and candidates are taken in relative-volume order until slots run out:

1. the concurrent-position cap,
2. not already holding the symbol,
3. the same-day re-entry rule,
4. sizing being able to afford at least one share from available cash.

Each qualifying candidate's outcome ("bought 2192 @ $4.56", "position cap reached",
"already traded today", …) is recorded and shown in the status page's Action column,
so the gap between qualifying and buying is visible rather than log-only.

A failure affecting one candidate — an unavailable price, a rejected order — skips
that candidate and the pass continues. Aborting would discard the remaining
qualifiers, which at a one-minute cadence means a transient blip on one symbol
silently costs the others their entry. Failures that are not candidate-specific (a
store error, say) still stop the pass and surface as `ERROR`.

## 4. Exit

A position exits on whichever of these triggers first:

1. **Profit target + trailing stop (PROPOSED: +15% target, 5% trail)** — the trailing stop *arms* once the peak reaches the profit target, then exits if price falls the trail percentage below the high-water mark. Before it arms, the hard stop-loss is the only floor. Once armed it stays armed, even if price falls back below the target.
2. **Hard stop-loss** at −10% (this one is settled — see [`risk.md`](./risk.md)).
3. **Forced end-of-day exit** at 30 minutes before market close, regardless of P&L — settled, no exceptions.

All three are checked continuously once a position is open; whichever fires first closes it. In code, `strategy.EvaluateExit` returns the *first* match and the order it checks them in **is** the priority: forced EOD, then stop-loss, then trailing stop. `risk.md` requires the stop-loss to outrank the momentum signals.

**Removed: the MACD bearish crossover.** A 15-minute MACD cross (fast=5, slow=10, signal=3) was the fourth trigger until 2026-09-28. Backtesting it against the agent's own implementation showed it firing on **24.9% of trades for a mean return of −0.08%** — indistinguishable from not having it, while costing an intraday-bar request per open position per tick and carrying a 13-candle (`slow + signal`) warm-up that made the trigger unavailable before roughly 12:45pm ET anyway. It was dropped rather than retuned: nothing in the evidence suggested a better set of periods existed, and the warm-up dead zone is structural to computing MACD inside a single session. `internal/strategy/macd.go` and `MarketData.IntradayBars` went with it.

## Same-day re-entry

A symbol with a position opened today is not bought again for the rest of the session, even after it closes (PROPOSED; `allow_same_day_reentry: false`). This avoids repeatedly buying back into a name that already stopped out. Enforced from the store rather than memory, so it survives a restart.

## Explicitly out of scope for v1

- **Backtesting.** Paper trading against live Alpaca data is the validation step for this project; no historical backtest engine is planned. Revisit only if paper-trading results suggest it's needed before going live.
- **Multiple strategies.** This is a single strategy (small-cap momentum) for v1 — the `strategy` package doesn't need a plugin/registry architecture yet.
