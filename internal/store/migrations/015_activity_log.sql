-- activity_log records a discrete "the reader did something today" event.
-- Not a general audit log: exactly what the dashboard's streak and volume
-- charts need. One row per grading decision, and per manually-harvested
-- extract — see Store.SaveScheduleReviewed and Store.CreateExtract.
CREATE TABLE activity_log (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,        -- 'review' | 'extract'
    element_id  INTEGER NOT NULL REFERENCES elements(id) ON DELETE CASCADE,
    grade       TEXT,                 -- resulting schedule state; NULL for 'extract'
    occurred_on TEXT NOT NULL,        -- YYYY-MM-DD, same convention as due_on
    created_at  TEXT NOT NULL         -- RFC3339
);

CREATE INDEX idx_activity_log_occurred_on ON activity_log(occurred_on);
