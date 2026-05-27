#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   ./market-heat.sh
#   ./market-heat.sh NVDA TSLA AMD META
#   WATCHLIST="NVDA TSLA AMD META COIN" ./market-heat.sh

python3 - "$@" <<'PY'
import os, sys, json, statistics, urllib.parse, urllib.request
from datetime import datetime, time
from zoneinfo import ZoneInfo

ET = ZoneInfo("America/New_York")
UTC = ZoneInfo("UTC")

DEFAULT_WATCHLIST = "NVDA TSLA AMD META AMZN AAPL MSFT GOOGL AVGO NFLX COIN MSTR PLTR SMCI"
watchlist = sys.argv[1:] or os.environ.get("WATCHLIST", DEFAULT_WATCHLIST).split()

FUTURES = {
    "ES=F": "S&P futures",
    "NQ=F": "Nasdaq futures",
    "YM=F": "Dow futures",
    "RTY=F": "Russell futures",
}

ETFS = {
    "SPY": "S&P ETF",
    "QQQ": "Nasdaq ETF",
    "IWM": "Russell ETF",
    "DIA": "Dow ETF",
}

VOL = {
    "^VIX": "VIX",
}

UA = "Mozilla/5.0 market-heat-check/1.0"


def fetch_chart(symbol, rng="5d", interval="5m", include_prepost=True):
    encoded = urllib.parse.quote(symbol, safe="")
    url = (
        f"https://query1.finance.yahoo.com/v8/finance/chart/{encoded}"
        f"?range={rng}&interval={interval}&includePrePost={'true' if include_prepost else 'false'}"
    )

    req = urllib.request.Request(url, headers={"User-Agent": UA})

    with urllib.request.urlopen(req, timeout=12) as r:
        data = json.loads(r.read().decode("utf-8"))

    result = data.get("chart", {}).get("result") or []

    if not result:
        err = data.get("chart", {}).get("error")
        raise RuntimeError(f"No chart data for {symbol}: {err}")

    return result[0]


def is_regular_session(t_et):
    tm = t_et.time()
    return t_et.weekday() < 5 and time(9, 30) <= tm <= time(16, 0)


def is_premarket(t_et):
    tm = t_et.time()
    return t_et.weekday() < 5 and time(4, 0) <= tm < time(9, 30)


def safe_pct(now, prev):
    if now is None or prev in (None, 0):
        return None
    return (now - prev) / prev * 100.0


def symbol_stats(symbol):
    c = fetch_chart(symbol)

    meta = c.get("meta", {})
    ts = c.get("timestamp") or []

    q = (c.get("indicators", {}).get("quote") or [{}])[0]
    closes = q.get("close") or []
    volumes = q.get("volume") or []

    points = []

    for i, epoch in enumerate(ts):
        close = closes[i] if i < len(closes) else None
        vol = volumes[i] if i < len(volumes) else 0

        if close is None:
            continue

        t_et = datetime.fromtimestamp(epoch, UTC).astimezone(ET)
        points.append((t_et, float(close), int(vol or 0)))

    if not points:
        raise RuntimeError(f"No price points for {symbol}")

    last_time, last_price, _ = points[-1]

    previous_close = (
        meta.get("regularMarketPreviousClose")
        or meta.get("previousClose")
        or meta.get("chartPreviousClose")
    )

    if not previous_close:
        today = datetime.now(ET).date()
        regular_before_today = [
            p for p in points
            if is_regular_session(p[0]) and p[0].date() < today
        ]

        if regular_before_today:
            previous_close = regular_before_today[-1][1]

    pct = safe_pct(last_price, float(previous_close) if previous_close else None)

    today = datetime.now(ET).date()

    today_pm_vol = sum(
        v for t, _, v in points
        if t.date() == today and is_premarket(t)
    )

    prev_pm_by_date = {}

    for t, _, v in points:
        if t.date() < today and is_premarket(t):
            prev_pm_by_date[t.date()] = prev_pm_by_date.get(t.date(), 0) + v

    prev_pm_values = list(prev_pm_by_date.values())[-3:]
    avg_prev_pm = statistics.mean(prev_pm_values) if prev_pm_values else None

    pm_vol_ratio = (
        today_pm_vol / avg_prev_pm
        if avg_prev_pm and avg_prev_pm > 0
        else None
    )

    return {
        "symbol": symbol,
        "last": last_price,
        "last_time": last_time,
        "prev_close": float(previous_close) if previous_close else None,
        "pct": pct,
        "today_pm_vol": today_pm_vol,
        "pm_vol_ratio": pm_vol_ratio,
    }


def sign(x):
    if x is None:
        return 0
    return 1 if x > 0 else -1 if x < 0 else 0


def fmt_pct(x):
    return "n/a" if x is None else f"{x:+.2f}%"


def fmt_int(x):
    if x is None:
        return "n/a"
    if x >= 1_000_000:
        return f"{x / 1_000_000:.1f}M"
    if x >= 1_000:
        return f"{x / 1_000:.0f}K"
    return str(x)


def fmt_ratio(x):
    return "n/a" if x is None else f"{x:.1f}x"


def load_many(symbols):
    out = {}
    failures = {}

    for s in symbols:
        try:
            out[s] = symbol_stats(s)
        except Exception as e:
            failures[s] = str(e)

    return out, failures


def add_score(reason, pts, score_box, reasons):
    if pts:
        score_box[0] += pts
        reasons.append(f"+{pts}: {reason}")


def main():
    now_et = datetime.now(ET)

    market_symbols = list(FUTURES) + list(ETFS) + list(VOL)

    market, market_failures = load_many(market_symbols)
    movers, mover_failures = load_many(watchlist)

    score = [0]
    reasons = []

    es = market.get("ES=F", {}).get("pct")
    nq = market.get("NQ=F", {}).get("pct")
    spy = market.get("SPY", {}).get("pct")
    qqq = market.get("QQQ", {}).get("pct")
    vix = market.get("^VIX", {}).get("pct")

    # 1. Futures movement
    fut_abs = [abs(x) for x in (es, nq) if x is not None]
    avg_fut_abs = statistics.mean(fut_abs) if fut_abs else 0

    if avg_fut_abs >= 0.40:
        add_score("index futures are moving strongly", 2, score, reasons)
    elif avg_fut_abs >= 0.20:
        add_score("index futures are active", 1, score, reasons)

    # 2. ETF gap
    etf_abs = [abs(x) for x in (spy, qqq) if x is not None]
    avg_etf_abs = statistics.mean(etf_abs) if etf_abs else 0

    if avg_etf_abs >= 0.40:
        add_score("SPY/QQQ have a strong pre-market gap", 2, score, reasons)
    elif avg_etf_abs >= 0.20:
        add_score("SPY/QQQ have a moderate pre-market gap", 1, score, reasons)

    # 3. Direction alignment
    broad_signs = [sign(x) for x in (es, nq, spy, qqq) if sign(x) != 0]

    if len(broad_signs) >= 3 and all(x == broad_signs[0] for x in broad_signs):
        add_score("futures and major ETFs agree on direction", 1, score, reasons)

    # 4. Premarket volume
    spy_ratio = market.get("SPY", {}).get("pm_vol_ratio")
    qqq_ratio = market.get("QQQ", {}).get("pm_vol_ratio")

    best_vol_ratio = max(
        [x for x in (spy_ratio, qqq_ratio) if x is not None],
        default=0
    )

    if best_vol_ratio >= 1.5:
        add_score("SPY/QQQ pre-market volume is above recent pre-market average", 1, score, reasons)

    # 5. VIX movement
    if vix is not None and abs(vix) >= 3.0:
        add_score("VIX is moving, so volatility is active", 1, score, reasons)

    # 6. Watchlist movers
    hot_movers = []
    active_movers = []

    for s, st in movers.items():
        pct = st.get("pct")
        vol = st.get("today_pm_vol") or 0

        if pct is None:
            continue

        if abs(pct) >= 3.0 and vol >= 50_000:
            hot_movers.append(s)
        elif abs(pct) >= 2.0 and vol >= 20_000:
            active_movers.append(s)

    if len(hot_movers) >= 3:
        add_score("several watchlist stocks have large, liquid pre-market gaps", 2, score, reasons)
    elif len(hot_movers) >= 1 or len(active_movers) >= 2:
        add_score("some watchlist stocks are moving with volume", 1, score, reasons)

    score[0] = min(score[0], 10)

    if score[0] >= 8:
        verdict = "HOT"
        action = "Good conditions for active day trading, but only trade clean setups."
    elif score[0] >= 5:
        verdict = "TRADEABLE"
        action = "There may be opportunities, but be selective and reduce size in chop."
    else:
        verdict = "COLD / CHOP RISK"
        action = "Conditions are not ideal. Consider waiting for cleaner direction or not trading."

    direction_inputs = [x for x in (es, nq, spy, qqq) if x is not None]

    direction = "mixed"

    if direction_inputs and all(x > 0 for x in direction_inputs):
        direction = "bullish"
    elif direction_inputs and all(x < 0 for x in direction_inputs):
        direction = "bearish"

    print()
    print("=== Market Heat Check ===")
    print(f"Time: {now_et:%Y-%m-%d %H:%M:%S %Z}")
    print(f"Verdict: {verdict}")
    print(f"Score: {score[0]}/10")
    print(f"Broad direction: {direction}")
    print(f"Guidance: {action}")

    print()
    print("--- Why ---")

    if reasons:
        for r in reasons:
            print(r)
    else:
        print("No strong heat signals detected.")

    print()
    print("--- Broad market ---")

    for s, label in {**FUTURES, **ETFS, **VOL}.items():
        st = market.get(s)

        if not st:
            print(f"{s:6} {label:16} unavailable")
            continue

        print(
            f"{s:6} {label:16} "
            f"last={st['last']:.2f} "
            f"move={fmt_pct(st['pct'])} "
            f"pmVol={fmt_int(st['today_pm_vol'])} "
            f"pmVolRatio={fmt_ratio(st['pm_vol_ratio'])}"
        )

    print()
    print("--- Watchlist movers ---")

    ranked = sorted(
        [st for st in movers.values() if st.get("pct") is not None],
        key=lambda x: abs(x["pct"]),
        reverse=True,
    )[:12]

    if not ranked:
        print("No watchlist data available.")
    else:
        for st in ranked:
            flag = "* " if abs(st["pct"]) >= 2.0 and (st["today_pm_vol"] or 0) >= 20_000 else "  "

            print(
                f"{flag}{st['symbol']:6} "
                f"last={st['last']:.2f} "
                f"move={fmt_pct(st['pct'])} "
                f"pmVol={fmt_int(st['today_pm_vol'])} "
                f"pmVolRatio={fmt_ratio(st['pm_vol_ratio'])}"
            )

    if market_failures or mover_failures:
        print()
        print("--- Data warnings ---")

        for s, e in {**market_failures, **mover_failures}.items():
            print(f"{s}: {e}")

    print()
    print("Note: This is a conditions scanner, not a buy/sell signal.")
    print("Premarket data can be delayed, thin, unavailable, or inaccurate from free sources.")
    print("Use limit orders and always define your stop before entering a trade.")


if __name__ == "__main__":
    main()
PY
