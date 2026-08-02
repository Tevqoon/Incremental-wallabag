-- Images referenced by an article's content_html, fetched once and cached
-- here rather than hotlinked. A hotlinked <img> would ping its origin host
-- on every single read of the article — and incremental reading means
-- re-reading the same article many times over, so that is not a one-off
-- leak, it is a recurring one. Fetching once, server-side, and serving the
-- bytes back from increader's own origin turns that into a single fetch
-- from increader's IP, ever, no matter how many times or from where the
-- article is opened afterwards.
CREATE TABLE document_images (
    id           INTEGER PRIMARY KEY,
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,

    -- The absolute URL as it appeared in the article's sanitised HTML.
    url          TEXT NOT NULL,

    content_type TEXT NOT NULL DEFAULT '',
    data         BLOB NOT NULL DEFAULT x'',

    -- Whether the fetch succeeded. A failed fetch is cached too — with no
    -- data — so a dead image link is not retried on every single render of
    -- an article; ok=0 rows are simply skipped when rendering.
    ok           INTEGER NOT NULL DEFAULT 0,

    fetched_at   TEXT NOT NULL,

    UNIQUE (document_id, url)
);
