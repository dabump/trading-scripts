-- A position opened by hand from the status page is closed only by hand: no stop,
-- no gap backstop, no candle trail, no scale-out, and no forced end-of-day exit, so
-- it can be held across sessions. stop_price and initial_risk are still stored —
-- the stop is what sized it and what the R column is measured against — but nothing
-- acts on them.
--
-- Existing rows stay 0: the positions opened by hand before this were managed by the
-- rules, and that is what their exits record.
ALTER TABLE positions ADD COLUMN manual INTEGER NOT NULL DEFAULT 0;
