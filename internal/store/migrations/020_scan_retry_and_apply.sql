-- retry_after is the earliest a pending scan may be claimed again after a
-- failed attempt. The backoff used to live in the worker goroutine that saw
-- the failure — it slept, but the scan itself went straight back to pending,
-- so any other idle goroutine claimed it again at once. During an outage or
-- a rate limit a whole batch burned through its attempts in seconds and was
-- left failed. Kept on the row, the backoff holds whichever goroutine claims
-- next. NULL means "now".
ALTER TABLE page_scans ADD COLUMN retry_after TEXT;

-- applied is whether a done scan's passages have been written into elements.
-- CompletePageScan discards the photo and ApplyScanPassages writes the
-- passages in a separate transaction, so a restart between the two used to
-- lose that page's passages for good: the photo was gone, and nothing ever
-- assembled from that scan again. CompletePageScan clears this,
-- ApplyScanPassages sets it, and the worker re-applies any scan left at 0
-- when it starts. Every scan already done before this migration was applied
-- in its own time, hence the default.
ALTER TABLE page_scans ADD COLUMN applied INTEGER NOT NULL DEFAULT 1;
