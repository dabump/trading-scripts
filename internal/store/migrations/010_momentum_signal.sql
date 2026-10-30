-- MACD(9,20,9)/StochRSI(14,14,3,3) read on every watchlist symbol, purely a
-- display annotation -- never fed back into screening or entry. Keyed by
-- (session_date, symbol) and overwritten on every pass, unlike news_sentiment's
-- write-once semantics: this is a live reading recomputed every minute, not a
-- once-per-session analysis.
CREATE TABLE momentum_signal (
    session_date TEXT NOT NULL,
    symbol       TEXT NOT NULL,
    state        TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (session_date, symbol)
);
