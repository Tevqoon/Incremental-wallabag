-- Burying sorted every element skipped today into one bucket, ordered by
-- queue_rank inside it — a fixed, unchanging order no matter how many times
-- something was skipped. Pressing "Later" over and over just walked the same
-- rotation forever: bury everything once, and the very next skip started the
-- identical sequence again from the top.
--
-- buried_at is a real timestamp, not a date, so repeated burying of the same
-- element keeps moving it — see Store.Bury and the ORDER BY in Store.Queue.
ALTER TABLE elements ADD COLUMN buried_at TEXT;
