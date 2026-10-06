# Strategy

**Status:** implemented. Values below marked **PROPOSED** were not settled in design; they were filled in during implementation so the agent could run and are awaiting review (see [`decisions.md`](./decisions.md)). All of them live in `config/config.yaml` — change them there, and update this doc to match.

## 1. Sentiment gate (the opening window)

- From market open, poll Alpaca market data every `timing.sentiment_poll_interval` (2m) for `timing.sentiment_window` (5m). The window was an hour as originally specified; §3 records why it was shortened and what that costs.
- No trades are placed during that window regardless of what the data shows.
- At the end of the window, classify the session as **overwhelmingly bearish** or **not overwhelmingly bearish**:
  - Overwhelmingly bearish → hand off to `risk`'s daily kill switch; no further trades today.
  - Anything else (clearly bullish *or* neutral/ambiguous) → proceed to screening for the rest of the day. Only a clear bearish reading halts trading — a neutral/unclear reading is treated the same as bullish, not the same as bearish.
- **Classification rule (PROPOSED):** the session is overwhelmingly bearish when the average intraday change across SPY, QQQ and IWM is at or below **−0.8%** *and* none of the three is positive. Anything else proceeds. IWM is in the basket deliberately: the strategy trades small caps, so a small-cap index belongs in the read of the tape, not just SPY and QQQ.
- If the daemon starts *after* the gate window, no readings exist and the gate cannot be judged. It halts for the day rather than trading without the safety check ever having run. This is the one halt that can be dismissed from the status page — it is an absence of data rather than a risk decision, and a restart should not automatically cost the session. Dismissing it sets the verdict to `GATE_OVERRIDDEN` rather than `PROCEED`, so nothing later reads it as the gate having passed. A halt the gate genuinely *reached* on real readings is the kill switch and is not dismissible. See [`web-ui.md`](./web-ui.md).

**Pre-market has no gate, and cannot have one.** The gate's readings are taken after the open, so anything the agent does before the bell necessarily predates it. Pre-market screening is unaffected — it never buys anything by itself — but `premarket.allow_entry` would be trading with the kill switch not merely off but nonexistent. In its place, each pre-market pass that may buy takes a **live** reading of the same basket through the same classifier (`sentiment.Classify`) and withholds entry on an overwhelmingly bearish tape, or when the reading is unavailable at all. That reading is deliberately **not persisted**: the session's verdict belongs to the gate window, and a 07:30 sample must not be able to settle the day before the market has opened. It is a weaker guarantee than the gate — one sample rather than a window of them — which is one of the reasons `allow_entry` is the switch to leave off until the pre-market numbers have been watched (§2b).

## 2. Screening (momentum candidates)

During the regular session, screening only starts once the sentiment gate has passed (i.e. once `timing.sentiment_window` has elapsed and the gate has resolved) — there is no background screening during the no-trade window. Pre-market screening is separate and is described in §2b.

Nothing in the screen filters on company size, so it admits large caps as readily as small ones.

**Candidate universe:** every tradable US equity (`broker.TradableAssets`, cached per session) is scanned each pass via batched snapshots. This replaced an earlier reliance on Alpaca's market-movers endpoint, which is hard-capped at 50 symbols and sorted by percentage change — the wrong ordering for a strategy that ranks on relative volume. Only gainers are acted on; the strategy buys strength and never shorts.

**Tradability floors — a gate, not a criterion.** Before any per-symbol enrichment, a symbol must clear `min_price` (default $1) and `min_dollar_volume` (default $1,000,000 traded so far today). `TradableAssets` excludes OTC but *not* warrants, units or penny stocks, and with the float criterion removed nothing else kept them out: a backtest over 2024-09 → 2026-09 found the candidate list dominated by warrants (`GIBOW` at $0.025), SPAC units (`QETAU`, a 20-session average volume of **15 shares**) and sub-$1 names (`ASBP`, 371 shares/day). None are fillable at a $1,000 position without moving the price, so they are not tradable regardless of how well they score on momentum. They are therefore rejected outright rather than shown as failing candidates — see [`decisions.md`](./decisions.md).

**Evaluation order:** the tradability floors are applied first, then the intraday-move criterion from the batched snapshot, and only symbols that clear both get the per-symbol news and average-volume lookups. At a one-minute cadence, enriching every mover would multiply API calls for names already disqualified on the cheapest criterion. Symbols that fail the move gate still appear on the status page, with the un-fetched criteria shown as unevaluated.

**Re-scan frequency:** every 1 minute once the sentiment gate has opened — tighter than the gate's own 2-minute poll, chosen because a momentum setup can fully develop and finish within minutes — a slower interval risks discovering candidates only after the move that made them interesting is already over.

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

**Coverage and cadence.** `premarket.start` defaults to **04:00**, the exchange's own pre-market open, because `PRE_MARKET` is a claim about what the market is doing — a start later than the real one makes the agent report `MARKET_CLOSED` while the tape is trading, which reads as the feature being broken. `premarket.scan_interval` exists so the cost can be absorbed by scanning less often than the regular session, but it is currently set to the same **1m**. One pass is roughly 130 mostly-serial API requests, so 04:00–09:30 is ~330 passes and the morning runs at the regular session's ~130 requests a minute against Alpaca's 200/min — where a 5m cadence would be ~66 passes at ~26 a minute. Nothing measures pass duration, so slowing this key is the cheap way to buy headroom if the morning shows signs of overrunning. A later `start` remains a supported way to trim calls further, at the cost of being blind to anything that moved before it.

**Entry is a second switch.** `premarket.allow_entry` is **on** in the shipped config, so the agent does buy before the bell — against the recommendation in [`decisions.md`](./decisions.md), which asks for it to stay off until the PDT count is resolved, since a pre-market entry that exits the same session is still a day trade. With it off the agent screens before the bell and buys nothing: the candidate table still fills, and every qualifying row carries `pre-market entry is disabled` in its Action column — the same reasoning as the closed entry window in §3, where an empty table would read as a broken scanner rather than a deliberate stand-down.

With it on, three things change and all three are requirements rather than preferences:

1. **Orders are extended-hours day limit orders**, whatever `execution.order_type` says. Alpaca accepts nothing but a day limit order for the extended session, so there is no choice to express — `execution.order_type` keeps governing the regular session alone, and `engine.submit` sends a limit order whenever the phase is pre-market. It is priced off `premarket.limit_slip_pct`, falling back to `execution.limit_slip_pct`. Both entries and exits carry the extended-hours flag.

   That allowance is the setting to watch. Pre-market spreads on these names are several times wider than regular-session ones, so an allowance that is generous at 14:00 may never fill at 06:00 — and on the **exit** side, an unfilled stop leaves the position open until the bell. That is a real consequence of `allow_entry`, not a theoretical one, and it is unmeasured like the rest of §2b.
2. **Entry is gated on a live sentiment read** (see §1), because the post-open gate has not run.
3. **The setup is read on pre-market candles.** The chart handed to `strategy.FindSetup` starts at `premarket.start` rather than the open, so the pullback, the EMA and the VWAP are all computed from the session actually trading.

Everything else is unchanged: the same position cap, the same sizing from the stop, the same exit rules. A position opened pre-market is managed from the moment it exists — its stop can fire at 08:00 rather than waiting for the bell — and is flattened at the regular forced end-of-day exit like any other, a manual position included (§4). A manual pre-market open is the one case where the stop is enforced on the tick rather than by the resting order: Alpaca accepts a stop order before the bell but will not trigger it, so the order is placed and the engine holds the floor until 09:30.

**Post-market is deliberately not covered.** The forced end-of-day exit flattens the strategy's book before the close, so after 16:00 there is nothing it manages (a manual position may still be held, but no rule acts on it); and opening a position into the 16:00–20:00 session would mean holding it overnight, which this strategy never does.

**Where pre-market volume comes from.** Not the snapshot. Verified against a live account: before the bell, Alpaca's `dailyBar` is still the *previous* session's, and no daily bar for today exists yet, so a symbol's volume-so-far reads as **zero**. That zero fails both the liquidity floor and the relative-volume criterion, which made the first live pre-market scan reject all 66 of its movers. `broker.SessionVolumes` therefore sums each name's volume from **batched hourly bars** since `premarket.start`, and the tradability gate splits in two to accommodate it: the price band is answerable from the snapshot in either session and is applied first, then volume is fetched for the survivors, then turnover is judged. See [`decisions.md`](./decisions.md) for the raw payloads.

**These numbers are unmeasured.** `cmd/backtest` fetches 09:30–16:00 bars and has no pre-market data to sweep, so unlike the regular-session thresholds none of the `premarket.*` values has been measured — they are reasoned defaults. The recommendation is to run with `allow_entry: false` and read the candidate table for a while before turning it on; the shipped config has it on, which is a choice worth revisiting rather than a measured one. See [`decisions.md`](./decisions.md).

## 3. Entry — the setup gate

Screening says a name is *interesting*. It says nothing about whether this instant is
a sensible moment to buy it, or where the risk sits. Those are the entry's job, and
they are what changed when the strategy was aligned with the discretionary small-cap
momentum approach it was always modelled on.

Entry uses **the same evaluation** the status page displays — one `screener.Evaluate`
pass per scan feeds both, so the two can never disagree about what qualifies.

**The screen and the setup run at different speeds.** The screen (news, move, volume)
costs ~130 requests and takes most of a minute; its answer changes over minutes. The
setup changes every candle and costs one bar request per qualifying name. So the screen
publishes a *watchlist* in the background, and the setup check reads each watchlist
name's chart **once per completed candle, a second or two after it closes**. Until
2026-10-03 the setup waited behind the screen, and every entry was decided on a candle
that had closed almost a minute earlier (docs/decisions.md).

### The pattern: a micro pullback

A qualifying candidate is bought only when its chart prints a **micro pullback**
(`strategy.FindSetup`), read on `entry.pattern_interval` candles:

```
            ┃ ← top of the surge: the high of day
          ┃ ┃ ╻   ← pause: 1–2 candles that close red or undercut the previous low
        ┃   ╹ ┃ ← trigger: closes above the last pause candle's high
      ┃
  surge_bars candles, up at least min_surge_pct
```

- **The surge.** The `entry.surge_bars` candles before the pause must have risen at
  least `entry.min_surge_pct`, from their lowest low to the top, and the top must be
  the high of day.
- **The pause.** 1 to `entry.max_pullback_bars` candles that each **give something
  back**: they close red, or they trade below the previous candle's low. A candle
  that ticks a marginal new high and then closes red still counts. The pause may
  give back at most `entry.max_retrace_pct` of the surge's range.

  Merely *failing to extend* is not a pause. Until 2026-10-06 it was — any candle
  whose high did not exceed the previous candle's qualified, with no magnitude
  attached — and on a vertical mover that admits pure continuation. AIFA was bought
  at 10:09 ET that day on a "pause" that closed green, on a higher low and a higher
  close, whose sole qualification was a high $0.0006 below the previous candle's.
  The entry was 7.9% into an unbroken run of green candles and the stop went under a
  level nothing had defended; it was stopped out eleven seconds after the fill. See
  docs/decisions.md.
- **The trigger.** A candle closes above the last pause candle's high.

Plus two trend filters, because the strategy only buys strength: price must be above
its `entry.ema_period` EMA, and above the session VWAP — the line separating a name
being accumulated from one being distributed into. Two more filters are optional and
off: MACD(12, 26, 9) above its signal line (`entry.require_macd`), and lighter volume
on the pause than on the surge (`entry.require_volume_decline`).

**It replaced a flag detector** that waited for a close above the *session* high after
a 1–5 candle pullback. That detector read every marginal-new-high candle as a new pole,
so on a vertical mover it reported a 0-bar pullback for as long as the move was clean.
That is why KNRX was refused at 09:59 (docs/decisions.md). The two were compared on a
year of history before the switch; the numbers are in docs/decisions.md.

**The daemon enters after the close, not at the break.** It reads closed candles, so
it waits for the trigger candle to close above the level — only closed ones: the feed
includes the candle still forming, and that is never read as a trigger. The
discretionary approach buys the moment price trades through, using a buy-stop order.
That order type is not built. `cmd/backtest` measures both entries
(`strategy.ArmMicroPullback` gives the buy-stop price) so the gap between them is a
number, not a guess.

**The price is re-read before buying** (`strategy.CheckEntryPrice`). The trigger close
is history by the time an order can go, and on these names a few seconds is several
percent. Just before ordering, the live price is read and the trade is refused if it is

- at or below the stop,
- back under the pause high — the breakout has failed,
- more than `entry.max_entry_drift_pct` (1%) above the trigger close — a chase, or
- puts the stop outside the `min_stop_distance_pct`–`max_stop_distance_pct` band
  measured from the live price. A stop that is too close here is refused, not widened:
  it is close because price fell back toward it, not because the chart drew it tight.

A trade that passes is **sized from the live price**, not the trigger close. The order
is still a market order, so the fill can differ again; what this removes is deciding
and sizing on a price that is a minute old. All three automated entries on 2026-10-02
would have been refused. `cmd/backtest` applies the same check to the next bar's open.

### Why the pattern matters more than the pattern

The setup's real output is not the entry price, it is **the stop**. The pause's low is
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
6. the live price still agreeing with the setup (above),
7. sizing being able to afford at least one share from available cash.

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

The status page offers an **Open** button on every screened symbol not already held — qualifying or not — which buys it without the setup (and, on a failing row, without the screen). That is a deliberate escape hatch, not a second entry rule: the gate stays exactly as described above for everything the agent does on its own.

It exists because the gate's failure mode is known and one-sided. It was added when the flag detector refused KNRX (+357%, 4846x relative volume, 7 catalysts, never bought), because a vertical mover never printed the 1–5 bar pullback that detector wanted. The micro pullback that replaced it can see a one-candle pause, but any mechanical rule still misses charts a human reads correctly.

Everything downstream of the signal still applies to a hand-placed trade — the size, the stop, the cap, the exits. See [`web-ui.md`](./web-ui.md).

## 4. Exit

Three things can close or reduce a position, checked in this order. The candle trail
described below works through the second, raising the stop:

1. **Forced end-of-day exit** at `exit.eod_exit_offset_minutes` before the close, regardless of P&L — settled, and it applies to a position opened by hand as well (see below).
2. **The stop.** The working stop is the chart stop the setup defined, moved up to the entry price once the first target is banked. Behind it sits `risk.stop_loss_pct` as a **gap backstop**, reachable only when price jumps straight through the chart stop. Config validation requires the backstop to be the wider of the two, or it would fire first and silently turn this back into a fixed-percentage stop.
3. **The first profit target**, at `exit.first_target_r` multiples of *this trade's own initial risk*. `exit.first_target_fraction` of the position is sold there and the rest keeps running, with its stop at breakeven.

**Positions opened by hand have a shorter rule: the forced exit, then their own
stop.** The signal rules — the `risk.stop_loss_pct` backstop, the candle trail and the
scale-out at the target — do not apply, because they are tuned to the pattern the
setup gate looks for and a manual open overrode that gate. The stop does apply: it is
what sized the position, so letting price through it means losing more than the trade
was opened under. `strategy.EvaluateManualExit` is that rule.

In the ordinary case the engine does not evaluate it at all. The stop is left with the
broker as a resting good-till-cancelled order, attached to the buy itself, so it is enforced between ticks and
while the daemon is not running; the engine covers only what the order cannot — the
close, and the windows where the order is missing or cannot yet trigger. Every path
that sells cancels the resting order first. See [`risk.md`](./risk.md).

Risk before reward: on a bar that traded through both the stop and the target, the
stop is what is recorded. `strategy.EvaluateExit` returns the first action due, and
the order it checks them in **is** the priority.

### The candle trail: sell on the first candle to make a new low

This is the micro pullback's own exit (`exit.candle_trail`): sell when a candle trades
below the previous candle's low. It raises the stop to the low of each
`entry.pattern_interval` candle that completes while the position is held
(`strategy.CandleTrailStop`), so the stop rule above does the selling. It records
`STOP_LOSS`; the close event's `initial_stop` shows how far the trail had raised it.
It never lowers a stop.

- `always` (the discretionary version, and the setting until 2026-10-03) trails from
  the first candle held whole. A trade that makes a new low before it pays is abandoned rather
  than held to the pause-low stop. After the target, it trails what is left.

**The candle the buy landed in does not count.** A live order fills part-way through
a candle, so that candle's low is usually a price printed *before* the fill — a
fraction under the entry, and on a thin pre-market book sometimes the fill price
itself. Trailing to it moved the stop to within about 1% of entry inside the first
minute: on 2026-09-29 it stopped out 10 of 11 trailed positions, four of them at a
"breakeven" that lost money once the spread was paid. The chart stop covers the entry
candle. The backtest already worked this way for its default fill at a bar's open,
which holds the whole bar; its buy-stop variant now skips the entry bar too.
- `after_target` (the current setting since 2026-10-03, when `always` measured a real
  loss over a year) trails only what is left after the first target.
- `off` holds what is left to the breakeven stop or the bell.

`exit.candle_trail_interval` and `exit.candle_trail_bars` widen the trail: candles
longer than the setup's, built from them, and the lowest low of several. Unset, they
give the one-candle trail above. Every wider version measured no better over a year
(docs/decisions.md, 2026-10-03), so none is set.

The engine reads one bar request per held position per completed candle, and never
trails on the candle still forming. **On a year of history it did not improve the
strategy** (docs/decisions.md): it is on because it is the approach's exit, not
because it measured better.

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

A symbol already traded today **may** be bought again once that position has closed (PROPOSED; `allow_same_day_reentry: true`). Setting it to `false` blocks the re-entry for the rest of the session, which avoids repeatedly buying back into a name that already stopped out; that is what the docs originally specified and what the key exists to restore. Either way this is a rule about a *closed* position — one-position-per-symbol refuses the second buy while the first is still open, and it is not configurable. Enforced from the store rather than memory, so it survives a restart.

## Explicitly out of scope for v1

- **Backtesting — superseded.** This said paper trading was the only planned validation and no backtest engine would be built. `cmd/backtest` was added on 2026-09-28 and is now how any change to these rules is measured; it imports the production rule packages so the measurement cannot drift from the daemon. See [`decisions.md`](./decisions.md).
- **Multiple strategies.** This is a single strategy for v1 — the `strategy` package doesn't need a plugin/registry architecture yet. It is modelled on discretionary small-cap momentum, but the screen has no size criterion, so "small-cap" describes the intent rather than the filter — an open item in [`decisions.md`](./decisions.md).
