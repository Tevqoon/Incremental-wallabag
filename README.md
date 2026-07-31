# increader

A self-hosted incremental reader for [wallabag](https://wallabag.org), in the
SuperMemo sense: articles enter a prioritised queue, you read a slice, pull out
the passages that matter as **extracts**, and the article goes back in the queue
at a longer interval. Extracts are themselves queue items you re-read and refine,
and a refined extract can carry **cloze deletions** that become Anki cards.

The point is being able to read a thousand articles at once by refusing to finish
any of them.

One static Go binary, SQLite, no runtime, four dependencies, a ~13 MB container
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
behind a `Target` interface, so adding KOReader or Zotero later is an addition
rather than a rewrite.

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

The `chown` matters: the image runs as uid 65532 (distroless's `nonroot`), and
without it the first write to the volume fails.

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
| **Queue** | What is due today, most important first. Articles and extracts interleave by priority. |
| **Read next** | Jumps straight to the most important due element. |
| **Extracts** | Everything harvested, filterable by origin — your own extracts and the ones imported from wallabag highlights. |
| **Library** | Everything synced, searchable — for finding a specific article, or putting an archived one back in the queue. |

Articles you have **archived in wallabag do not enter the queue**: they stay in
the Library, keep their extracts, and can be put back with one click. Their
highlights are still imported, which matters because in a real library that is
where nearly all of them are.

In the reader, **select any text** to raise a toolbar:

- **Extract** turns the passage into its own queue element. It appears
  highlighted in the parent so you can see what has been harvested, and is due
  immediately so you can refine it in the same session. Links inside an extract
  survive.
- **Cloze** (on an extract) marks a deletion, promoting the extract to an item
  and previewing it as Anki will receive it: `an {{c1::extract}}, and let the …`

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
  source/     the Source interface           ← seam in
  wallabag/   API client + Source adapter
  ir/         addressing, extracts, clozes, scheduling   (stdlib only)
  store/      SQLite, hand-written SQL
  export/     the Target interface           ← seam out
  syncer/     source → store
  web/        handlers, templates, assets
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
annotation import, library and extract browsing.

`increader sync -full` ignores the watermark and re-reads everything. Needed
after a release that starts storing a field it did not before, since incremental
sync would otherwise never see it on entries that have not changed.

Not yet built: the Anki and org-roam exporters. The `exports` ledger they need
already exists in the schema — idempotent re-export requires that history to
have been recorded all along, and it cannot be reconstructed after the fact.
