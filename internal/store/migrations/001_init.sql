-- Documents are imported content, one row per article from a provider.
CREATE TABLE documents (
    id           INTEGER PRIMARY KEY,

    -- (source, external_id) is the provider's identity for this content.
    -- The UNIQUE constraint is what makes re-syncing an update rather than a
    -- duplicate.
    source       TEXT NOT NULL,
    external_id  TEXT NOT NULL,

    url          TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    author       TEXT NOT NULL DEFAULT '',
    language     TEXT NOT NULL DEFAULT '',

    -- Raw article HTML, exactly as the provider supplied it. Untrusted: it is
    -- sanitised at render time, not at write time, so the original survives a
    -- change to the sanitiser policy.
    content_html TEXT NOT NULL DEFAULT '',

    -- Whether content_html has been fetched yet. Listings are synced without
    -- bodies; the body arrives when the article is first opened.
    has_content  INTEGER NOT NULL DEFAULT 0,

    published_at      TEXT,
    source_updated_at TEXT NOT NULL,
    imported_at       TEXT NOT NULL,

    UNIQUE (source, external_id)
);

-- Elements are the incremental-reading tree, in SuperMemo's sense: a single
-- table holds both topics (things you read) and items (things you answer), so
-- the daily queue is one ordered query rather than two interleaved ones.
CREATE TABLE elements (
    id          INTEGER PRIMARY KEY,
    document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,

    -- NULL parent means this is the document's root topic. Any other value
    -- means it is an extract taken from that parent.
    parent_id   INTEGER REFERENCES elements(id) ON DELETE CASCADE,

    -- 'topic' (read and extract from it) or 'item' (a cloze, destined for Anki).
    kind        TEXT NOT NULL,

    title        TEXT NOT NULL DEFAULT '',

    -- For extracts and items: the passage itself. Empty for a root topic,
    -- whose content lives on the document.
    content_html TEXT NOT NULL DEFAULT '',

    -- The extract's plain text, stored verbatim so the passage survives even
    -- if the parent's HTML is later re-fetched and the offsets stop matching.
    quote        TEXT NOT NULL DEFAULT '',

    -- Where this extract sits in its parent, as block index plus character
    -- offset within that block. Used to render already-harvested passages as
    -- highlighted. NULL on root topics.
    start_block  INTEGER,
    start_offset INTEGER,
    end_block    INTEGER,
    end_offset   INTEGER,

    -- SuperMemo's priority convention: 0.0 is most important, 1.0 least.
    priority     REAL NOT NULL DEFAULT 0.5,

    -- 'new', 'reading', 'done' or 'dismissed'.
    state        TEXT NOT NULL DEFAULT 'new',

    -- Scheduling. due_on is a date (YYYY-MM-DD), not a timestamp: incremental
    -- reading works in whole days, and comparing dates avoids a whole class of
    -- timezone bugs at the day boundary.
    due_on        TEXT,
    interval_days REAL NOT NULL DEFAULT 0,

    -- A-Factor: the multiplier applied to the interval at each repetition.
    afactor       REAL NOT NULL DEFAULT 2.0,
    reps          INTEGER NOT NULL DEFAULT 0,

    -- Index of the block where reading stopped, so the next pass resumes there.
    -- A block index rather than a scroll position, so it survives a resize.
    read_block    INTEGER NOT NULL DEFAULT 0,

    -- 'manual' for extracts made here, 'import' for provider annotations.
    origin        TEXT NOT NULL DEFAULT 'manual',

    -- The provider's annotation id when origin is 'import', so re-importing
    -- the same highlight updates rather than duplicates.
    external_ref  TEXT,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- The queue query: due, active elements ordered by priority.
CREATE INDEX elements_queue ON elements (state, due_on, priority);
CREATE INDEX elements_parent ON elements (parent_id);
CREATE INDEX elements_document ON elements (document_id);

-- Partial unique index: only rows that actually came from an import are
-- constrained, so the many NULL external_refs on manual extracts do not
-- collide with each other.
CREATE UNIQUE INDEX elements_external_ref
    ON elements (document_id, external_ref)
    WHERE external_ref IS NOT NULL;

-- Cloze deletions marked on an item's text, numbered c1, c2, ... in Anki's
-- convention. Offsets are character indices into the parent element's quote.
CREATE TABLE cloze_ranges (
    id           INTEGER PRIMARY KEY,
    element_id   INTEGER NOT NULL REFERENCES elements(id) ON DELETE CASCADE,
    ordinal      INTEGER NOT NULL,
    start_offset INTEGER NOT NULL,
    end_offset   INTEGER NOT NULL,
    hint         TEXT NOT NULL DEFAULT '',

    UNIQUE (element_id, ordinal)
);

-- The export ledger records what has been sent where. It exists from the first
-- migration even though exporters ship later: idempotent re-export needs this
-- history to have been recorded all along, and it cannot be reconstructed
-- after the fact.
CREATE TABLE exports (
    id           INTEGER PRIMARY KEY,
    element_id   INTEGER NOT NULL REFERENCES elements(id) ON DELETE CASCADE,

    -- 'anki', 'orgroam', ...
    target       TEXT NOT NULL,

    -- The target's own identifier for what was written: an Anki GUID, an org
    -- node ID. Lets a later export update in place.
    external_ref TEXT NOT NULL DEFAULT '',

    -- Hash of the exported content, so an unchanged element can be skipped and
    -- a changed one re-sent.
    content_hash TEXT NOT NULL,

    exported_at  TEXT NOT NULL,

    UNIQUE (element_id, target)
);

-- One row per source, holding the incremental-sync watermark.
CREATE TABLE sync_state (
    source     TEXT PRIMARY KEY,

    -- The provider's own updated_at for the newest document seen. Provider
    -- time, never local time: the two clocks disagree, and using local time
    -- here silently drops records.
    --
    -- Nullable on purpose. A source that has synced but imported nothing — an
    -- empty library, or a first sync that failed before fetching — has no
    -- watermark yet, and NULL reads back as "fetch everything". A NOT NULL
    -- column here would turn that ordinary case into a constraint error.
    watermark  TEXT,

    last_run   TEXT,
    last_error TEXT NOT NULL DEFAULT ''
);
