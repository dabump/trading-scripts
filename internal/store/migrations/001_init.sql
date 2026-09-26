CREATE TABLE sessions (
    session_date TEXT PRIMARY KEY,
    verdict      TEXT NOT NULL DEFAULT 'PENDING',
    halted       INTEGER NOT NULL DEFAULT 0,
    halt_reason  TEXT NOT NULL DEFAULT '',
    updated_at   TEXT NOT NULL
);

CREATE TABLE sentiment_readings (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    session_date   TEXT NOT NULL,
    taken_at       TEXT NOT NULL,
    classification TEXT NOT NULL,
    percentages    TEXT NOT NULL
);

CREATE INDEX sentiment_readings_session ON sentiment_readings (session_date, taken_at);

CREATE TABLE positions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_date TEXT NOT NULL,
    symbol       TEXT NOT NULL,
    shares       INTEGER NOT NULL,
    entry_price  REAL NOT NULL,
    entry_time   TEXT NOT NULL,
    peak_price   REAL NOT NULL,
    trail_armed  INTEGER NOT NULL DEFAULT 0,
    is_open      INTEGER NOT NULL DEFAULT 1,
    exit_price   REAL NOT NULL DEFAULT 0,
    exit_time    TEXT NOT NULL DEFAULT '',
    exit_reason  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX positions_session ON positions (session_date);

-- Holding the same symbol twice is never intended, and a duplicate would break
-- both exposure accounting and exit handling. Enforcing it here means a bug in
-- the entry path fails loudly instead of silently doubling a position.
CREATE UNIQUE INDEX positions_one_open_per_symbol ON positions (symbol) WHERE is_open = 1;

CREATE TABLE screen_snapshots (
    session_date TEXT NOT NULL,
    taken_at     TEXT NOT NULL,
    payload      TEXT NOT NULL
);

CREATE INDEX screen_snapshots_session ON screen_snapshots (session_date, taken_at);

-- Orders are recorded before they are sent so a crash between submission and
-- confirmation leaves a trace to reconcile against the broker on restart.
CREATE TABLE orders (
    client_order_id TEXT PRIMARY KEY,
    session_date    TEXT NOT NULL,
    symbol          TEXT NOT NULL,
    side            TEXT NOT NULL,
    shares          INTEGER NOT NULL,
    submitted_at    TEXT NOT NULL,
    status          TEXT NOT NULL,
    broker_order_id TEXT NOT NULL DEFAULT ''
);

CREATE INDEX orders_session ON orders (session_date);
