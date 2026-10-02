-- A manual position's stop is left resting at the broker rather than evaluated on a
-- tick, so the position has to remember which order that is: every path that sells
-- the position cancels it first, and a stop order outliving its holding would sell
-- shares that are no longer there.
--
-- Empty means there is no protective order — which is the case for every automated
-- position, whose stop is still evaluated in managePositions, and for any manual
-- position opened before this.
ALTER TABLE positions ADD COLUMN stop_order_id TEXT NOT NULL DEFAULT '';
