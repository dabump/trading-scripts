-- The status page polls every ~12s and needs each open position's current price
-- for unrealized P&L. Persisting the last mark the trading loop already fetched
-- keeps the web handler off the market data API entirely.
ALTER TABLE positions ADD COLUMN last_price REAL NOT NULL DEFAULT 0;
