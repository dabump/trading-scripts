-- What the broker actually executed, read back after each order. Until this existed
-- the status column kept Alpaca's acknowledgement (pending_new) forever and the
-- positions table carried the price the daemon *quoted*, not the fill: on 2026-09-29
-- that put the day at -$541.80 when the fills said -$632.15.
--
--   filled_price    the broker's average fill price; 0 when nothing filled
--   filled_shares   how many of the ordered shares filled
ALTER TABLE orders ADD COLUMN filled_price REAL NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN filled_shares INTEGER NOT NULL DEFAULT 0;
