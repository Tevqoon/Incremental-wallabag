-- Page scans: photos of a paper book's marked pages, on their way to becoming
-- imported annotations of that book's document.
--
-- The photo itself is kept, not just whatever text a vision model recovers
-- from it. The reader writes margin notes by hand while looking at the
-- photo afterward — the model is deliberately never asked to read
-- handwriting, both because that is a much harder problem than reading
-- printed text and because a misread note is worse than no note at all. So
-- the photo has to survive the model's pass and stay around for the reader
-- to look at while they type.
--
-- image lives on this table rather than in document_images: it is not an
-- article image (nothing in the article body ever references it) and it is
-- never proxied out through internal/web/images.go — it is only ever read
-- back by the id of the scan it belongs to, for the reader's own review UI
-- and for the worker that sends it to the model. Every listing query below
-- (and every future one) must select everything but image; the column is
-- large and almost never wanted, the same reason document content_html is
-- kept off list queries elsewhere in this schema.
--
-- status is a small state machine, not unlike an element's own state:
--   pending    -- uploaded, waiting for a worker
--   processing -- claimed by a worker, in flight
--   done       -- the model answered; result holds its JSON
--   failed     -- attempts were exhausted; error holds why
-- attempts counts tries so a scan that can never succeed stops being retried
-- forever; error is kept on both a failure and (cleared) a success, so the
-- last attempt's outcome is always visible rather than only the final one.
--
-- triage records, per scan, whether the passages it eventually produces
-- should land in the triage pile or go straight into the queue — the same
-- choice ImportOptions.Triage offers an upload, but here it has to be
-- remembered per photo rather than decided once at upload time, because
-- ApplyScanPassages (internal/store/pagescans.go) runs once per finished
-- scan and needs to know which behaviour that scan's own passages want
-- without the caller re-supplying it.
--
-- On elements: note_pending marks a passage whose photo showed handwriting
-- next to it that the model (deliberately) did not transcribe — a nudge to
-- go look at the photo and type the note in, or dismiss it as nothing. It is
-- cleared the moment a note is typed in (UpdateAnnotation, or EditAnnotation
-- — the note-only write the margin-note form and the JSON API both use) or
-- the reader says there is nothing there (DismissNotePending). It lives on elements
-- rather than page_scans because a single photo can seed more than one
-- passage (a page turn splits a sentence across two photos) and the flag
-- belongs to the passage the reader will actually look at, not the source
-- image.
CREATE TABLE page_scans (
    id           INTEGER PRIMARY KEY,
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    filename     TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL,
    image        BLOB NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',   -- pending | processing | done | failed
    attempts     INTEGER NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    result       TEXT NOT NULL DEFAULT '',          -- the model's JSON once done
    triage       INTEGER NOT NULL DEFAULT 1,        -- 1: passages go to triage; 0: straight into the queue
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

-- The worker asks "what is next to do" (status, oldest first) and a
-- document's own scan list asks "everything for this book, in upload order".
-- One index each, the same division elements_triage/elements_ordinal already
-- draw in migration 016.
CREATE INDEX page_scans_status   ON page_scans (status, id);
CREATE INDEX page_scans_document ON page_scans (document_id, id);

ALTER TABLE elements ADD COLUMN note_pending INTEGER NOT NULL DEFAULT 0;
