# Strategy

**Status:** implemented. Values below marked **PROPOSED** were not settled in design; they were filled in during implementation so the agent could run and are awaiting review (see [`decisions.md`](./decisions.md)). All of them live in `config/config.yaml` — change them there, and update this doc to match.

## 1. Sentiment gate (first hour)

- From market open, poll Alpaca market data every 10 minutes for one hour.
- No trades are placed during this hour regardless of what the data shows.
- At the end of the hour, classify the session as **overwhelmingly bearish** or **not overwhelmingly bearish**:
  - Overwhelmingly bearish → hand off to `risk`'s daily kill switch; no further trades today.
  - Anything else (clearly bullish *or* neutral/ambiguous) → proceed to screening for the rest of the day. Only a clear bearish reading halts trading — a neutral/unclear reading is treated the same as bullish, not the same as bearish.
- **Classification rule (PROPOSED):** the session is overwhelmingly bearish when the average intraday change across SPY, QQQ and IWM is at or below **−0.8%** *and* none of the three is positive. Anything else proceeds. IWM is in the basket deliberately: the strategy trades small caps, so a small-cap index belongs in the read of the tape, not just SPY and QQQ.
- If the daemon starts *after* the first hour, no readings exist and the gate cannot be judged. It halts for the day rather than trading without the safety check ever having run. This is the one halt that can be dismissed from the status page — it is an absence of data rather than a risk decision, and a restart should not automatically cost the session. Dismissing it sets the verdict to `GATE_OVERRIDDEN` rather than `PROCEED`, so nothing later reads it as the gate having passed. A halt the gate genuinely *reached* on real readings is the kill switch and is not dismissible. See [`web-ui.md`](./web-ui.md).

**Pre-market has no gate, and cannot have one.** The gate's readings are taken after the open, so anything the agent does before the bell necessarily predates it. Pre-market screening is unaffected — it never buys anything by itself — but `premarket.allow_entry` would be trading with the kill switch not merely off but nonexistent. In its place, each pre-market pass that may buy takes a **live** reading of the same basket through the same classifier (`sentiment.Classify`) and withholds entry on an overwhelmingly bearish tape, or when the reading is unavailable at all. That reading is deliberately **not persisted**: the session's verdict belongs to the first hour, and a 07:30 sample must not be able to settle the day before the market has opened. It is a weaker guarantee than the gate — one sample rather than a window of them — which is one of the reasons `allow_entry` defaults to false.

## 2. Screening (momentum candidates)

During the regular session, screening only starts once the sentiment gate has passed (i.e. after the first hour) — there is no background screening during the no-trade hour. Pre-market screening is separate and is described in §2b.

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

## 2b. Pre-market screening

The US market trades from 04:00 ET, and the gap-and-go setups this strategy looks for are formed there: the catalyst breaks overnight, the name is already up 40% by 08:00, and 09:30 is where the move is *revealed*, not where it starts. `premarket.enabled` extends the agent's coverage back into that session.

**The criteria are unchanged.** A catalyst, a ≥10% move, unusual volume — the same three, evaluated by the same `screener.Evaluate`. What changes is two of the numbers they are measured against, because pre-market is the same symbols on a different market:

| | Regular session | Pre-market | Why |
|---|---|---|---|
| Liquidity floor | `screening.min_dollar_volume` ($1,000,000) | `premarket.min_dollar_volume` ($100,000) | The regular floor is a regular-session number. Applied before the bell it rejects essentially the whole tape, including the names genuinely trading. |
| Relative volume | `screening.min_volume_multiple` (5x) | `premarket.min_volume_multiple` (0.5x) | The comparison is identical — volume so far against the 20-session **daily** average — but a pre-market session is a fraction of a day. A name printing half a normal day's volume before the bell is the pre-market equivalent of the 5x the regular session looks for. |

The price band (`min_price` / `max_price`) and the move threshold are **not** split. A name is inside the strategy's band, or moving enough, regardless of which session is printing it; changing those would make it a different screen rather than the same screen on a different session.

**Coverage and cadence.** `premarket.start` defaults to **04:00**, the exchange's own pre-market open, because `PRE_MARKET` is a claim about what the market is doing — a start later than the real one makes the agent report `MARKET_CLOSED` while the tape is trading, which reads as the feature being broken. What absorbs the cost instead is the cadence: `premarket.scan_interval` (default 5m) rather than the regular 1m. One pass is roughly 130 mostly-serial API requests, so 04:00–09:30 is ~66 passes averaging ~26 requests a minute against Alpaca's 200/min — affordable, where the regular cadence (~330 passes) would not be. A later `start` remains a supported way to trim calls further, at the cost of being blind to anything that moved before it.

**Entry is a second switch.** `premarket.allow_entry` defaults to **false**, so by default the agent screens before the bell and buys nothing. The candidate table fills, and every qualifying row carries `pre-market entry is disabled` in its Action column — the same reasoning as the closed entry window in §3, where an empty table would read as a broken scanner rather than a deliberate stand-down.

With it on, three things change and all three are requirements rather than preferences:

1. **Orders are extended-hours day limit orders**, whatever `execution.order_type` says. Alpaca accepts nothing but a day limit order for the extended session, so there is no choice to express — `execution.order_type` keeps governing the regular session alone, and `engine.submit` sends a limit order whenever the phase is pre-market. It is priced off `premarket.limit_slip_pct`, falling back to `execution.limit_slip_pct`. Both entries and exits carry the extended-hours flag.

   That allowance is the setting to watch. Pre-market spreads on these names are several times wider than regular-session ones, so an allowance that is generous at 14:00 may never fill at 06:00 — and on the **exit** side, an unfilled stop leaves the position open until the bell. That is a real consequence of `allow_entry`, not a theoretical one, and it is unmeasured like the rest of §2b.
2. **Entry is gated on a live sentiment read** (see §1), because the first-hour gate has not run.
3. **The setup is read on pre-market candles.** The chart handed to `strategy.FindSetup` starts at `premarket.start` rather than the open, so the pullback, the EMA and the VWAP are all computed from the session actually trading.

Everything else is unchanged: the same position cap, the same sizing from the stop, the same exit rules. A position opened pre-market is managed from the moment it exists — its stop can fire at 08:00 rather than waiting for the bell — and is flattened at the regular forced end-of-day exit like any other.

**Post-market is deliberately not covered.** The forced end-of-day exit flattens the book before the close, so after 16:00 there is nothing to manage; and opening a position into the 16:00–20:00 session would mean holding it overnight, which this strategy never does.

**Where pre-market volume comes from.** Not the snapshot. Verified against a live account: before the bell, Alpaca's `dailyBar` is still the *previous* session's, and no daily bar for today exists yet, so a symbol's volume-so-far reads as **zero**. That zero fails both the liquidity floor and the relative-volume criterion, which made the first live pre-market scan reject all 66 of its movers. `broker.SessionVolumes` therefore sums each name's volume from **batched hourly bars** since `premarket.start`, and the tradability gate splits in two to accommodate it: the price band is answerable from the snapshot in either session and is applied first, then volume is fetched for the survivors, then turnover is judged. See [`decisions.md`](./decisions.md) for the raw payloads.

**These numbers are unmeasured.** `cmd/backtest` fetches 09:30–16:00 bars and has no pre-market data to sweep, so unlike the regular-session thresholds none of the `premarket.*` values has been measured — they are reasoned defaults. Run with `allow_entry: false` and read the candidate table for a while before turning it on. See [`decisions.md`](./decisions.md).

## 3. Entry — the setup gate

Screening says a name is *interesting*. It says nothing about whether this instant is
a sensible moment to buy it, or where the risk sits. Those are the entry's job, and
they are what changed when the strategy was aligned with the discretionary small-cap
momentum approach it was always modelled on.

Entry uses **the same evaluation** the status page displays — one `screener.Evaluate`
pass per scan feeds both, so the two can never disagree about what qualifies.

### The pattern

A qualifying candidate is bought only when its chart prints a **pullback and
resumption** (`strategy.FindSetup`), read on `entry.pattern_interval` candles:

```
         ← pole: the session's high so far
        /|
       / |  ‾\__   ← flag: one to a few bars that stay below the pole
      /  |      \__
  ___/                ▲ trigger: a bar closes back above the pole high
```

Plus two trend filters, because the strategy only buys strength: price must be above
its `entry.ema_period` EMA, and above the session VWAP — the line separating a name
being accumulated from one being distributed into.

### Why the pattern matters more than the pattern

The setup's real output is not the entry price, it is **the stop**. The flag's low is
the price that says the pullback was not a pullback, so it is where the stop belongs —
and that means *risk per share is known before the position is sized*. Buying on the
screen alone gives neither: there is no reference price, so the stop has to be an
arbitrary percentage of entry and the size an arbitrary fraction of the account.

A setup whose stop is further than `entry.max_stop_distance_pct` away is **refused
outright**, not sized around. That is part of the strategy rather than a safety rail:
with risk that wide the size has to shrink until a winner cannot pay for the losers.
A stop closer than `entry.min_stop_distance_pct` is widened to it instead, because
ordinary noise would otherwise trigger it immediately.

### Sizing follows the stop

```
shares = (portfolio value × risk.risk_per_trade_pct) ÷ (entry − stop)
```

Every trade risks the same fraction of the account (`risk.SizeForRisk`). Sizing by a
fixed fraction of portfolio value instead — which is what this project did before —
makes the dollar risk swing with the stock's volatility, so the account's worst days
are decided by which setups happened to be wide rather than by any decision anyone
made.

Two caps bound it: `risk.max_position_pct` on one position's notional, and available
cash. The notional cap **does** bind on tight stops, and when it does the trade risks
*less* than the nominal figure — conservative, and worth knowing when reading the
audit trail. It has to sit above `risk_per_trade_pct ÷ max_stop_distance_pct` or it
binds on every trade and sizing silently reverts to a fixed fraction.

### The entry window

New positions are only opened while **both** of these hold:

- it is within `timing.entry_window` of the open — 5h30m, so from a 09:30 open the last entry is **15:00**; and
- it is at least `timing.entry_cutoff_buffer` (30m) before the forced end-of-day exit.

The second is not arithmetic on the first, and it matters on a short session: the entry
window is measured from the open while the forced exit is measured from the close, so on
a half day (13:00 close, 12:30 forced exit) the buffer becomes the binding limit and the
last entry moves back to 12:00. Capping at the forced exit alone would let an early close
open a position a minute before it had to be liquidated.

**Screening continues after the window closes and the page keeps showing what is setting
up** — an empty afternoon table would read as a broken scanner rather than a deliberate
stand-down. Positions already held are managed to the close as normal.

The window used to be 2h. It was widened deliberately, against the measurement: on the
backtested year later entries were worse, and a 2h window ended at $8,871 where 4h ended
at $7,162 and all-day at $5,582 (t = −4.24). Trading more of the day buys opportunity at
a measured cost; see [`decisions.md`](./decisions.md).

The sentiment gate was shortened from an hour to `timing.sentiment_window` for the
same reason: an hour of kill-switch meant sitting out the single most important hour
of the day, since the moves this strategy looks for begin in the first fifteen
minutes. 09:30 to 09:35 is still not tradeable, which is a real cost and a deliberate
one.

**Qualifying is not the same as being bought.** These gates apply only at entry, and
candidates are taken in relative-volume order until slots run out:

1. the setup having triggered at all,
2. the entry window still being open,
3. the concurrent-position cap,
4. not already holding the symbol,
5. the same-day re-entry rule,
6. sizing being able to afford at least one share from available cash.

Each qualifying candidate's outcome ("bought 6600 @ $5.00, stop $4.85", "no setup:
close 9.93 has not cleared the 10.02 pullback high", "entry window closed", …) is
recorded and shown in the status page's Action column, so the gap between qualifying
and buying is visible rather than log-only.

A failure affecting one candidate — unavailable bars, a rejected order — skips that
candidate and the pass continues. Aborting would discard the remaining qualifiers,
which at a one-minute cadence means a transient blip on one symbol silently costs the
others their entry. Failures that are not candidate-specific (a store error, say)
still stop the pass and surface as `ERROR`.

**Not automated:** the discretionary reading this pattern is normally traded with —
Level 2, time and sales, the feel of a tape — has no mechanical equivalent here. If a
meaningful part of the edge lives there, this implementation cannot capture it, and
that is a limitation of automating the approach rather than something tuning will fix.

### Overriding the gate by hand

The status page offers an **Open** button on every qualifying candidate, which buys it without the setup. That is a deliberate escape hatch, not a second entry rule: the gate stays exactly as described above for everything the agent does on its own.

It exists because the gate's failure mode is known and one-sided. A name moving vertically never prints a 1–5 bar pullback to reclaim, so the strongest candidates the screen finds are the ones most likely to be refused — measured in the wild on KNRX (+357%, 4846x relative volume, 7 catalysts, never bought). A human can see that; `FindSetup` by construction cannot.

Everything downstream of the signal still applies to a hand-placed trade — the size, the stop, the cap, the exits. See [`web-ui.md`](./web-ui.md).

## 4. Exit

Three things can close or reduce a position, checked in this order:

1. **Forced end-of-day exit** at `exit.eod_exit_offset_minutes` before the close, regardless of P&L — settled, no exceptions.
2. **The stop.** The working stop is the chart stop the setup defined, moved up to the entry price once the first target is banked. Behind it sits `risk.stop_loss_pct` as a **gap backstop**, reachable only when price jumps straight through the chart stop. Config validation requires the backstop to be the wider of the two, or it would fire first and silently turn this back into a fixed-percentage stop.
3. **The first profit target**, at `exit.first_target_r` multiples of *this trade's own initial risk*. `exit.first_target_fraction` of the position is sold there and the rest keeps running, with its stop at breakeven.

Risk before reward: on a bar that traded through both the stop and the target, the
stop is what is recorded. `strategy.EvaluateExit` returns the first action due, and
the order it checks them in **is** the priority.

### Why the target is a fraction and a multiple of risk

Two exit rules were removed from here on evidence, and the shape of what replaced them
is not an accident:

- **A MACD bearish crossover** fired on 24.9% of trades for a mean of −0.08% — no work, at the cost of a bar request per position per tick and a 13-candle warm-up.
- **A fixed +15% profit target arming a fixed 5% trailing stop.** Armed at +15% and trailing 5% it could not mathematically exit above +9.25%, and in practice exited at **+9.31%** — while those same positions went on to average **+53%**. It capped the right tail at +9% and left the left tail at −10.7%, which at a 42% win rate cannot be profitable. Over one year and 1,697 trades it turned a per-trade mean of **+0.18% into −2.13%**, a t-statistic of −9.76. A 150-cell sweep of stop × target × trail found no combination that made money.

The lesson generalises, and it is why the current rule sells only *part* of the
position: **this strategy's return lives in a thin right tail, so any rule that
truncates a gain takes the part that pays for all the losses.** A position too small to
split is left to run rather than closed at the target, for exactly that reason.

Stating the target in multiples of risk rather than as a percentage follows from
sizing: a trade risking 2% and one risking 4% should not take profit at the same price
move.

## Same-day re-entry

A symbol with a position opened today is not bought again for the rest of the session, even after it closes (PROPOSED; `allow_same_day_reentry: false`). This avoids repeatedly buying back into a name that already stopped out. Enforced from the store rather than memory, so it survives a restart.

## Explicitly out of scope for v1

- **Backtesting.** Paper trading against live Alpaca data is the validation step for this project; no historical backtest engine is planned. Revisit only if paper-trading results suggest it's needed before going live.
- **Multiple strategies.** This is a single strategy (small-cap momentum) for v1 — the `strategy` package doesn't need a plugin/registry architecture yet.
