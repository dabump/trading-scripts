-- The trailing stop was removed from the strategy after a one-year backtest measured
-- it capping the right tail at +9% while leaving the left tail at -10.7%; see
-- docs/decisions.md. trail_armed recorded whether that stop had latched on, so with
-- the rule gone the column is not merely unused, it is misleading: anyone reading the
-- table would take it for live state.
--
-- peak_price stays. It is still written on every mark and still shown on the status
-- page, where it says how much of a gain an open position has given back.
ALTER TABLE positions DROP COLUMN trail_armed;
