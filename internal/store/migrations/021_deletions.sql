-- deletions remembers what the JSON API's consumers have to forget: every
-- annotation and every document deleted, and when.
--
-- The annotation feed (GET /api/documents?since=) is driven by the newest
-- updated_at among a document's surviving annotations, so a deletion moved
-- nothing a consumer could see: the document's timestamp stayed put, and a
-- document whose last annotation went dropped out of the feed altogether. A
-- mirror such as the org-roam export kept every deleted passage for good, and
-- every deleted document's file with it.
--
-- Filled by triggers rather than by each deleting call site, because rows
-- leave elements by several routes — DeleteExtract and everything its cascade
-- takes, the photo importer's absorbed fragments and retake cleanup, a whole
-- document's cascade — and a tombstone that depends on every one of them
-- remembering is one a future route forgets. Foreign-key cascades fire these
-- triggers too, so a deleted document leaves a tombstone for each of its
-- annotations as well as its own.
--
-- kind is 'annotation' (a non-root element; ref_id is its element id) or
-- 'document' (ref_id is the document id). external_ref is kept so a consumer
-- keyed on upstream ids can match without having stored increader's own.
-- deleted_at uses the same RFC3339 UTC form as every other timestamp here,
-- so it compares as text against them and against an API ?since.
CREATE TABLE deletions (
    id           INTEGER PRIMARY KEY,
    kind         TEXT NOT NULL,
    ref_id       INTEGER NOT NULL,
    document_id  INTEGER NOT NULL,
    external_ref TEXT NOT NULL DEFAULT '',
    deleted_at   TEXT NOT NULL
);

CREATE INDEX deletions_deleted_at ON deletions (deleted_at);
CREATE INDEX deletions_document   ON deletions (document_id, deleted_at);

CREATE TRIGGER elements_deleted AFTER DELETE ON elements
WHEN OLD.parent_id IS NOT NULL
BEGIN
    INSERT INTO deletions (kind, ref_id, document_id, external_ref, deleted_at)
    VALUES ('annotation', OLD.id, OLD.document_id, COALESCE(OLD.external_ref, ''),
            strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));
END;

CREATE TRIGGER documents_deleted AFTER DELETE ON documents
BEGIN
    INSERT INTO deletions (kind, ref_id, document_id, external_ref, deleted_at)
    VALUES ('document', OLD.id, OLD.id, COALESCE(OLD.external_id, ''),
            strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));
END;
