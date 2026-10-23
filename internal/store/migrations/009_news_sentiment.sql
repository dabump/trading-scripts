-- Claude's assessment of whether a screened symbol's same-day news catalyst reads
-- as positive or negative — purely a watchlist annotation, never fed back into
-- screening or entry. Keyed by (session_date, symbol) rather than just symbol so a
-- stock screened again on a later day gets a fresh read, and so at most one
-- analysis is ever spent on one symbol in one session, which is the point: the
-- sentiment worker checks this table before calling Claude, not after.
CREATE TABLE news_sentiment (
    session_date TEXT NOT NULL,
    symbol       TEXT NOT NULL,
    sentiment    TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (session_date, symbol)
);
