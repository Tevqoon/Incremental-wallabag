# SLOP WARNING
This is vibecoded slop solving a particular problem of mine. Should be fine since it's mostly an alternative frontend, but still.

# increader

A self-hosted incremental reader for [wallabag](https://wallabag.org), in the
SuperMemo sense: articles enter a prioritised queue, you read a slice, pull out
the passages that matter as **extracts**, and the article goes back in the queue
at a longer interval. Extracts have a prioritised queue of their own that you
re-read and refine, and a refined extract can carry **cloze deletions** that
become Anki cards.

The point is being able to read a thousand articles at once by refusing to finish
any of them.

One static Go binary, SQLite, no runtime, five dependencies, a ~13 MB container
image. Any device with a browser is a client.

## What it does and does not do

It **schedules reading** — which is the part Anki cannot do. It **authors** cloze
items but does not review them: those are exported to Anki, which already does
item spaced repetition well and is where a review habit usually already lives.

It **writes back to wallabag**: finishing an article archives it, re-queuing one
unarchives it, and stars and tags edited here are pushed upstream. Every such
change is recorded in an outbox in the same transaction as the local change, so
a wallabag outage delays a write rather than losing it.

Wallabag is one source behind a `Source` interface, and Anki/org-roam are targets
behind a `Target` interface, so adding Zotero later is an addition rather than a
rewrite. KOReader and PDF annotations arrive by a second route — an uploaded
file rather than a synced server — parsed in `internal/annotations`.

## Setup

### 1. Create a wallabag API client

In the wallabag web UI (for the hosted instance, <https://app.wallabag.it>):

> **Settings → API clients management → Create a new client**

Any redirect URI will do — increader uses the OAuth2 *password grant* and never
redirects. Keep the client id and secret.

### 2. Configure

```bash
cp .env.example .env      # then fill in the four wallabag values
```

`config.yaml` is committed and carries no secrets: `${VAR}` references are
replaced from the environment at load time. Adjust `timezone` — it decides when
"today" begins, and therefore when material becomes due.

### 3. Run

```bash
mkdir -p data && sudo chown 65532:65532 data && docker compose up -d --build
```

Two things about that line:

- The `chown` matters: the image runs as uid 65532 (distroless's `nonroot`), and
  without it the first write to the volume fails.
- **`--build` matters too.** Plain `docker compose up` only builds when the image
  is absent, so once `increader:latest` exists it will happily keep starting the
  old one. Every page footer and the startup log show the commit the binary was
  built from, so if something looks out of date, check there first:
  `docker compose exec increader /increader version`.

Compose publishes to `127.0.0.1:8080` on the host only. There is no login,
because the intended deployment reaches it over Tailscale and the tailnet is the
boundary. **Do not publish the port publicly without adding authentication
first** — handlers register through a middleware chain in `internal/web/server.go`,
so that is one insertion point rather than a refactor.

### Running it locally instead

```bash
go run ./cmd/increader -config config.local.yaml
```

with `bind: 127.0.0.1:8080` and `database: ./increader.db`. Sub-second iteration,
no image rebuild. `config.local.yaml` is gitignored.

## Using it

| | |
|---|---|
| **Queue** | Articles due today, most important first. |
| **Read next** | Jumps straight to the most important due article. |
| **Review** | The same page's other tab: extracts due today, most important first. Passages you took, wallabag highlights and book annotations alike. |
| **Extracts** | Everything harvested whether it is due or not, filterable by origin — your own extracts and the ones imported from wallabag highlights. |
| **Library** | Everything synced or uploaded, searchable — for finding a specific work, or putting an archived one back in the queue. |
| **Import** | Upload a book's annotations: a KOReader JSON export, the JSON an annotation extractor produced, or a PDF still carrying its own annotations. |

**Articles and extracts are two separate queues**, two tabs on the same page,
each with its own due count and its own "start" button. Reading articles and
refining extracts are different sittings, and every decision you make keeps you
in the queue you are already in: grade an extract and the next extract comes up,
grade an article and the next article does. **Later today** works the same way —
it sends the element to the back of its own queue, so skipping through extracts
never disturbs the order a reading session was working through.

(They used to be one list, blended so that whichever kind was rarer spread
evenly through the commoner one. That is closer to SuperMemo, and the machinery
for it is in the history if you want it back — but it only pays off if you
actually work a single mixed queue.)

**There is no daily cap.** Nothing limits how much you read in a day, and
nothing hides material that has piled up: a backlog — from a first import, or
from missing a day's review — is yours to work through, postpone with **Later**,
or drain by suspending in bulk from the Library. `queue_page_limit` exists only
to trim a very long *page*, defaults to 0 (list everything), and never affects
what **Read next** offers; when it does cut a list short, the page says
"showing 60 of 137" rather than leaving you to notice.

Articles you have **archived in wallabag do not enter the queue**: they stay in
the Library, keep their extracts, and can be put back with one click. Their
highlights are still imported, which matters because in a real library that is
where nearly all of them are.

In the reader, **select any text** to raise a toolbar:

- **Extract** turns the passage into its own queue element. It appears
  highlighted in the parent so you can see what has been harvested, and comes
  back on its own schedule (`extract_delay_days`, below) rather than
  immediately — the value of an extract is re-reading it once the article has
  faded, not twice in the same sitting. Links inside an extract survive.
- **Cloze** (on an extract) marks a deletion, promoting the extract to an item
  and previewing it as Anki will receive it: `an {{c1::extract}}, and let the …`

On an extract's own page, a **delete** link removes it permanently — for an
accidental selection, or a wallabag highlight that never should have been
made. If the extract came from an imported highlight, deleting it also queues
the highlight's removal in wallabag, so the next sync does not bring it right
back. The extracts browse page has the same action per row, for cleaning up a
backlog of imports without opening each one.

Grading a topic records an *intention*, not a recall judgement — while reading
there is nothing to recall, you are deciding what deserves attention next:

| | |
|---|---|
| **Pause** | Stop here. Records the read point, reschedules by interval × A-Factor, moves on. The everyday action. |
| **Sooner** | More interesting than expected. Halves the interval and slows future growth. |
| **Later** | Not now. Pushes it out, and *compounds* — repeatedly postponing something makes it recede faster and faster, so uninteresting material drains out of the queue without ever being explicitly abandoned. |
| **Suspend** | Park it indefinitely. Keeps the interval, A-Factor and read point, so unsuspending resumes rather than restarts. |
| **Done** / **Dismiss** | Finished with it / abandoning it unread. Both **archive the article in wallabag**, so it leaves your Unread list there too. |

Pausing marks the **read point** — the boundary between what you have read and
what you have not — and reopening the article scrolls there and shows it.

**Priority** (0 = most important) caps how far an interval can grow: a week at
the top of the scale, a year at the bottom. Important material can never drift
out of sight.

Highlights you already made in wallabag's own reader are imported as extracts
during sync, and located in the article text the first time you open it.

### Books and papers

Not everything worth reading incrementally comes from wallabag. **Import** takes
an annotation file and turns a work into a library entry whose passages all live
in one place:

- a **KOReader JSON export**, from its "Export highlights" plugin;
- the **JSON envelope** written by `org-roam-annotation-import`'s PyMuPDF
  extractor;
- a **PDF** still carrying its own annotations, read in-process.

There is no text behind these — the work is not stored, only what you marked in
it — so instead of a reader they get a **contents page**: every passage in the
order it appears in the original, grouped by chapter, with the chapter list on
top. A passage's page, colour and your own note on it are all kept.

A book yields far more passages than an article, so by default they arrive
**suspended** and a **triage pass** gates them: one at a time, in the book's own
order, each one kept (into the extract queue on the usual delay), parked, left
as it is, or deleted. That is a different thing from the extract queue, which is
ordered by priority — going through a work means going through it front to back,
with the chapter you were just in still in mind. A short piece can skip the pass
and go straight into the queue; the upload form asks.

Re-uploading is how these are corrected. A work is identified by its normalised
title and author, and each passage by a content hash, so exporting a book again
after adding highlights imports the new ones, updates chapters and notes that
changed, and leaves everything else alone. Where two exports of the same work
disagree about its title — a book read on an ereader and annotated in a PDF
reader — the form offers to merge into the work already stored.

Titles from these files are unreliable: KOReader reads one out of ebook
metadata, a PDF carries whatever produced it. Every document therefore has an
optional **title override** and a **subtitle** you set yourself, which a sync
never overwrites.

PDF annotations record *where* a highlight is, not what it covers, so the
passage is recovered from the glyphs underneath it. That works on a PDF with a
text layer and not at all on a scan; a highlight whose text cannot be recovered
is still imported with its page, colour and note, and the import says so.

Tags and the star toggle sit above the article and write straight through to
wallabag. The Library's filter tabs carry the same counts as wallabag's own
sidebar — Unread, Starred, Archive, Annotated — plus per-tag filters.

## Development

```bash
go test ./...
```

The substantive tests are in `internal/ir`, which is a leaf package of pure
functions — no database, no HTTP, no clock beyond what callers pass in.

```
internal/
  source/       the Source interface         ← seam in
  wallabag/     API client + Source adapter
  annotations/  KOReader / PDF file parsers  ← the other seam in
  ir/           addressing, extracts, clozes, scheduling (stdlib only)
  store/        SQLite, hand-written SQL
  export/       the Target interface         ← seam out
  syncer/       source → store
  web/          handlers, templates, assets
```

Two things are worth knowing before changing anything:

**Articles are addressed by block index and character offset.** The server
renders each block with `data-b="<index>"`; the browser reports a selection as
that index plus an offset into the element's `textContent`. `Block.Text`
therefore matches `textContent` *verbatim* — any whitespace normalisation would
shift every offset in the block. Offsets are always computed against the
**sanitised** HTML, since sanitising changes the document's shape; both sides
parse the same sanitised output, so the coordinates agree.

**Per-row queries follow the iteration, never run inside it.** The connection
pool is capped at one, because SQLite tolerates a single writer. A query issued
while iterating another query's rows waits for a connection that the loop itself
is holding — a deadlock rather than an error, so it hangs instead of failing.

**"Today" is decided in exactly one place.** Due dates are stored as bare dates,
so writing and comparing them must use the same zone. `main` pins `time.Local`
to the configured timezone at startup and everything reads that. The container
embeds the IANA database via `time/tzdata`, because distroless ships no zoneinfo
and every date would otherwise silently be UTC.

## Status

Working: two-way wallabag sync (archive state, tags, stars, reading time), the
reading queue, extracts and clozes, scheduling with read points and suspension,
annotation import from wallabag and from uploaded KOReader/PDF files, per-work
contents pages and triage passes, library and extract browsing.

Not yet built, but the schema is ready for it: editing a passage by hand.
PDF extraction is imperfect enough that correcting one should be possible, and
annotation colour is already recorded so that "this colour means a chapter
heading" can become a bulk chapter override later.

`increader sync -full` ignores the watermark and re-reads everything. Needed
after a release that starts storing a field it did not before, since incremental
sync would otherwise never see it on entries that have not changed.

Not yet built: the Anki and org-roam exporters. The `exports` ledger they need
already exists in the schema — idempotent re-export requires that history to
have been recorded all along, and it cannot be reconstructed after the fact.
