-- The strategy now derives its stop from the chart and scales out of a winner, so a
-- position is no longer a fixed block of shares with an implied percentage stop.
--
--   shares_open     what is still held; shares stays as the original size
--   stop_price      the working stop in dollars, moved to breakeven after the target
--   initial_risk    entry - first stop, per share; profit targets are multiples of it
--   target_hit      latches so an oscillating price cannot scale out repeatedly
--   banked_dollars  P&L already realised through partial sales
--
-- Existing rows are backfilled so a database written by the previous version stays
-- readable: everything still held is the whole position, nothing has been banked, and
-- a zero stop_price means "no chart stop", which EvaluateExit reads as the percentage
-- backstop being the only floor — the behaviour those rows were opened under.
ALTER TABLE positions ADD COLUMN shares_open INTEGER NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN stop_price REAL NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN initial_risk REAL NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN target_hit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE positions ADD COLUMN banked_dollars REAL NOT NULL DEFAULT 0;

UPDATE positions SET shares_open = CASE WHEN is_open = 1 THEN shares ELSE 0 END;
UPDATE positions SET banked_dollars = (exit_price - entry_price) * shares WHERE is_open = 0;
