-- Claude's own analysis of a losing closed trade: why it lost, and what would have
-- had to be different in the strategy for it to close in the green. The review
-- worker looks for closed, losing positions with no row here yet rather than being
-- told which ones to do, so a restart mid-sweep just finds the same ones again next
-- time — there is no in-progress status to leave stuck. A row is written exactly
-- once, after the Claude call returns either way.
CREATE TABLE trade_reviews (
    position_id INTEGER PRIMARY KEY REFERENCES positions (id),
    status      TEXT NOT NULL,
    report      TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);
