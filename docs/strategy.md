# Strategy

**Status:** design only. Values marked "not yet finalized" need explicit numbers agreed before `screener`/`strategy` are implemented — treat them as the next open decision, not as something to invent while coding.

## 1. Sentiment gate (first hour)

- From market open, poll Alpaca market data every 10 minutes for one hour.
- No trades are placed during this hour regardless of what the data shows.
- At the end of the hour, classify the session as **overwhelmingly bearish** or **not overwhelmingly bearish**:
  - Overwhelmingly bearish → hand off to `risk`'s daily kill switch; no further trades today.
  - Anything else (clearly bullish *or* neutral/ambiguous) → proceed to screening for the rest of the day. Only a clear bearish reading halts trading — a neutral/unclear reading is treated the same as bullish, not the same as bearish.
- **Not yet finalized:** the exact signal(s) and thresholds that produce this classification (e.g. broad-market ETF/futures direction and magnitude, breadth, VIX movement — `market-heat.sh`'s old heuristic in `archived/` is a reference point for the *kind* of signal, not a source of trusted numbers).

## 2. Screening (small-cap momentum candidates)

Screening only starts once the sentiment gate has passed (i.e. after the first hour) — there is no background screening during the no-trade hour.

**Candidate universe:** before any of the four criteria below can be checked, the screener needs a source list of tickers to check them against — scanning the entire US market ticker-by-ticker isn't practical. Check Alpaca's screener/most-actives data first (some Alpaca plans expose a market-movers/most-active endpoint); if it doesn't give enough of the market to be useful, this needs a dedicated screener API (e.g. one that supports server-side filtering by float, % change, and volume directly, rather than pulling a broad list and filtering client-side). **Not yet confirmed** — tracked as an open item in [`decisions.md`](./decisions.md); don't silently build against an assumed endpoint.

**Re-scan frequency:** every 1 minute once the sentiment gate has opened. This is deliberately much tighter than the first hour's 10-minute sentiment-poll cadence, chosen because a low-float momentum setup can fully develop and finish within minutes — a slower interval risks discovering candidates only after the move that made them interesting is already over.

A candidate must pass **all four** of the following (AND, not scored/weighted — any one failing disqualifies the candidate):

1. **Float** — fewer than 10,000,000 shares available to trade. This uses free float / shares available, not total market cap.
2. **News catalyst** — at least one news headline for the ticker within the current trading day (presence check only; no sentiment/NLP scoring — the price/volume move itself is what confirms the catalyst is moving the stock, the headline just confirms one exists).
3. **Price move** — already up ≥10% intraday.
4. **Volume** — ≥5x average volume.

All four thresholds live in `config/config.yaml` as tunable values, not hardcoded, since they'll likely need adjusting after paper-trading results come in.

**Open data-source question:** float and news-headline data are not standard Alpaca market-data-bar fields. Before implementing `screener`, confirm whether Alpaca's API surface (news endpoint, asset/reference data) covers both; if not, a single additional fundamentals/news provider needs to be added for whichever it doesn't cover. Don't silently pick a provider while coding — this is tracked as an open item in [`decisions.md`](./decisions.md).

## 3. Entry

- Day-trade only — no overnight holds.
- On a qualifying candidate: buy sized at 10% of portfolio (see [`risk.md`](./risk.md) for sizing and exposure cap interaction).

## 4. Exit

A position exits on whichever of these triggers first:

1. **Profit target + trailing stop** — take profit once up some %, then trail a stop below the peak. **Not yet finalized** (exact target % and trail %).
2. **Hard stop-loss** at −10% (this one is settled — see [`risk.md`](./risk.md)).
3. **MACD bearish crossover on the 15-minute chart** — exit when the MACD line crosses below its signal line. Uses periods fast=5, slow=10, signal=3, computed on the current day's candles from market open only (no multi-day lookback). Originally specified on the 30-minute chart, but that meant the slow EMA(10) needed 10×30min = 5 hours of same-day data before it could compute at all — since trading only runs from ~10:30am to the 3:30pm forced exit (a 5-hour window), MACD would have been usable only in roughly the last hour of the day. Moving to 15-minute candles halves that to 2.5 hours, so it becomes usable from around midday. **This is a mitigation, not a full fix** — a position opened and exited before ~12:30pm still can't trigger this exit; it only ever protects positions that are still open by midday. Accepted as a known limitation rather than switching to multi-day lookback or dropping the exit entirely.
4. **Forced end-of-day exit** at 30 minutes before market close, regardless of P&L — settled, no exceptions.

All four are checked continuously once a position is open; whichever fires first closes it.

## Explicitly out of scope for v1

- **Backtesting.** Paper trading against live Alpaca data is the validation step for this project; no historical backtest engine is planned. Revisit only if paper-trading results suggest it's needed before going live.
- **Multiple strategies.** This is a single strategy (small-cap momentum) for v1 — the `strategy` package doesn't need a plugin/registry architecture yet.
