-- Annotation import: books and papers whose annotations arrive as an uploaded
-- file rather than from a synced provider.
--
-- Two shapes of new state, and they are new for different reasons.
--
-- On documents: a book's own title is unreliable in a way an article's is not.
-- KOReader reads it out of ebook metadata, a PDF's is frequently just whatever
-- the producing tool left behind, and neither is what you would call the thing.
-- display_title is an override rather than an edit to title because a synced
-- document's title is overwritten wholesale on every sync, so anything typed
-- into title itself would survive only until the next one. subtitle has no
-- upstream counterpart at all — it is purely the reader's own annotation of
-- what this is.
--
-- On elements: an annotation from a book carries structure an article
-- highlight has no equivalent of. chapter and page are where it came from,
-- and they are the whole reason a book's annotations are worth showing as a
-- table of contents rather than a flat list. note is the reader's own comment,
-- which the wallabag path has always dropped on the floor. color is recorded
-- now, unused, because PDF readers let colour mean something — a chapter
-- heading in a document with no outline, say — and that meaning cannot be
-- recovered from a file that has already been imported without it.
--
-- page is TEXT, not INTEGER: KOReader numbers PDF pages but addresses epub
-- positions with an xpointer string, and there is no useful arithmetic to do
-- on either.
--
-- ordinal is reading order within the document, kept explicitly rather than
-- leaning on rowid order, because re-importing an edited export can introduce
-- an annotation that belongs in the middle of ones already stored.
--
-- triaged_at is the per-book queue's only state: NULL means "not yet decided
-- about". Deliberately a timestamp rather than a flag, so that "when did I go
-- through this book" is answerable, and deliberately on every element rather
-- than only imported ones, so the same pass works on a wallabag article's
-- highlights too.

ALTER TABLE documents ADD COLUMN subtitle      TEXT NOT NULL DEFAULT '';
ALTER TABLE documents ADD COLUMN display_title TEXT NOT NULL DEFAULT '';

ALTER TABLE elements ADD COLUMN chapter TEXT NOT NULL DEFAULT '';
ALTER TABLE elements ADD COLUMN page    TEXT NOT NULL DEFAULT '';
ALTER TABLE elements ADD COLUMN note    TEXT NOT NULL DEFAULT '';
ALTER TABLE elements ADD COLUMN color   TEXT NOT NULL DEFAULT '';
ALTER TABLE elements ADD COLUMN ordinal INTEGER NOT NULL DEFAULT 0;
ALTER TABLE elements ADD COLUMN triaged_at TEXT NULL;

-- The triage queue asks "what is left in this book", and the contents page asks
-- "everything in this book, in reading order". One index each.
CREATE INDEX elements_triage  ON elements (document_id, triaged_at);
CREATE INDEX elements_ordinal ON elements (document_id, ordinal);
