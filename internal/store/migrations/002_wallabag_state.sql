-- wallabag's own read state, which decides whether an article belongs in the
-- reading queue at all.
--
-- Without this every archived article — material already read and filed — sits
-- in the queue competing with what is actually unread. In a real library that is
-- the overwhelming majority of rows, and the queue stops meaning anything.
ALTER TABLE documents ADD COLUMN is_archived INTEGER NOT NULL DEFAULT 0;
ALTER TABLE documents ADD COLUMN is_starred  INTEGER NOT NULL DEFAULT 0;

-- Rows that predate this migration take the default 0, which is a guess rather
-- than a fact — for a real library it is wrong about the great majority.
--
-- The flag only ever arrives with a listing, and incremental sync asks for
-- entries changed since the watermark, so an up-to-date library would fetch
-- nothing and never correct itself. Clearing the watermark forces the next sync
-- to re-read everything, which is also how the annotations this release imports
-- get picked up for entries that have not changed since.
--
-- Safe because the import is idempotent: a full pass updates rows in place
-- rather than duplicating them, and root topics are only created for documents
-- that do not already have one.
UPDATE sync_state SET watermark = NULL;

-- No schema change is needed for suspension. 'suspended' is simply a new value
-- for elements.state, and read_block already exists — it was recorded from the
-- start but never shown.
CREATE INDEX documents_archived ON documents (is_archived);
