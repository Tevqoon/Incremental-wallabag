-- wallabag's per-article reading estimate. Genuinely useful while triaging a
-- queue: it answers "do I have time for this one now", which is a different
-- question from "is this important", and priority cannot express it.
ALTER TABLE documents ADD COLUMN reading_time INTEGER NOT NULL DEFAULT 0;

-- Tags, keyed by (source, label) rather than by the provider's own id.
--
-- Labels are what a reader thinks in and what the add-tag endpoint accepts;
-- the provider id is needed only to remove one, so it rides along as
-- external_id instead of being the primary key. That also keeps a second
-- provider's id space from colliding with wallabag's.
CREATE TABLE tags (
    id          INTEGER PRIMARY KEY,
    source      TEXT NOT NULL,
    external_id TEXT NOT NULL DEFAULT '',
    label       TEXT NOT NULL,
    slug        TEXT NOT NULL DEFAULT '',

    UNIQUE (source, label)
);

CREATE TABLE document_tags (
    document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    tag_id      INTEGER NOT NULL REFERENCES tags(id)      ON DELETE CASCADE,

    PRIMARY KEY (document_id, tag_id)
);
CREATE INDEX document_tags_by_tag ON document_tags (tag_id);

-- The outbox for changes increader makes to the provider.
--
-- A write is recorded here in the same transaction as the local change it
-- reflects, then drained. That is the whole point: marking an article Done and
-- recording "archive this upstream" either both commit or neither does, so the
-- two systems cannot silently disagree. Draining separately also means a
-- wallabag outage delays a write rather than losing it — and an outage is
-- exactly when a dropped write would go unnoticed, because nothing looks wrong
-- locally.
CREATE TABLE pending_writes (
    id          INTEGER PRIMARY KEY,
    source      TEXT NOT NULL,
    external_id TEXT NOT NULL,

    -- 'archive', 'star', 'tag_add' or 'tag_remove'.
    operation   TEXT NOT NULL,

    -- Operation argument: "1"/"0" for the flags, a tag label for the others.
    payload     TEXT NOT NULL DEFAULT '',

    -- Retried on every sync. The count and the last error are kept so a write
    -- that can never succeed — an entry deleted upstream — becomes visible
    -- instead of retrying silently forever.
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT '',

    created_at  TEXT NOT NULL
);
CREATE INDEX pending_writes_ready ON pending_writes (source, id);

-- Tags and reading time only ever arrive with a listing, and incremental sync
-- asks only for entries changed since the watermark — so an up-to-date library
-- would never fetch them. Same repair as migration 002.
UPDATE sync_state SET watermark = NULL;
