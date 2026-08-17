# increader — architecture overview

A self-hosted incremental reader for [wallabag](https://wallabag.org). Single
static Go binary + SQLite, ~5 dependencies, ~54 MB container image (built on
poppler-utils rather than distroless — see the Dockerfile's own comment and
`internal/annotations/pdftext.go` for why `pdftotext` is a real process
dependency, not just a Go one). Server-rendered HTML with htmx partial
swaps; one hand-written ~500-line `app.js`; any browser is a client.

## What it does

Incremental reading in the SuperMemo sense, layered on top of wallabag as the
article source of record:

- Articles enter a **prioritised reading queue**; you read a slice, grade it,
  and it reschedules at a growing interval (SM-style scheduling with
  A-factor/priority clamping, backlog fuzzing for determinism + spread). The
  point is reading a thousand articles at once by refusing to finish any of
  them.
- Passages get pulled out as **extracts**, which are themselves queue items
  you re-read and refine. Articles and extracts have **separate queues**;
  queue truncation is reported rather than hidden behind a daily cap.
- Refined extracts can carry **cloze deletions** exported to **Anki** (the app
  authors cards but does not review them; org-roam is also a target). It
  schedules reading — the part Anki cannot do.
- **Writes back to wallabag**: finishing archives, re-queuing unarchives;
  stars, tags, and Done/Dismissed/Suspended states push upstream.
- Second ingestion route: **KOReader and PDF annotations** via file upload
  (both the plugin and KOReader's built-in exporter), with a per-work triage
  pass.
- Extra views: library with bulk actions, extracts browser, a dashboard led by
  articles-read (queue preview, streak, a 12-week bar chart) with extract
  activity folded into its own disclosure, a calendar of articles read (month
  grid plus a 12-month strip) with each day's own page splitting what was read
  from what was harvested/revisited, light/dark theme override.

## Package layout

Dependency direction is strictly downward — no leaf package reaches up into
`store` or `web`.

- `internal/source` — dependency-free leaf. Defines `Source` plus small
  *optional* capability interfaces (`Enricher`, `Writer`, `RangeResolver`)
  discovered by type assertion, so a read-only provider (a future Zotero,
  say) never has to stub write methods. Wallabag is one source. There is no
  corresponding `Target` interface: annotations leave through the JSON API
  instead (`internal/web/api.go`), so that a consumer which already knows its
  own format — org-roam, via a thin elisp layer — owns that knowledge rather
  than a Go exporter reimplementing it here.
- `internal/ir` — pure, I/O-free domain logic: block indexing, extraction,
  cloze rendering, scheduling (`schedule.go`, timezone-consistent "today" via
  `time.Local` pinned at startup). Fully unit-testable and fully unit-tested.
- `internal/store` — the *only* package touching SQLite (single-connection
  pool). Every multi-step mutation is wrapped in a transaction.
- `internal/wallabag` — plain API client library plus a thin adapter
  implementing the source interfaces. `ranges.go` does the XPath /
  UTF-16↔UTF-8 annotation-range math for wallabag's highlight API.
- `internal/syncer` — sync loop above store + source; knows the outbox
  pattern but no concrete provider. Outbox drains are serialized with a mutex
  (`drainMu`) so a scheduled tick and a manual "sync now" cannot
  double-publish.
- `internal/annotations` — KOReader/PDF annotation parsing; every call into
  the panic-prone `rsc.io/pdf` is `recover()`-guarded at file and page level.
  Recovering a highlight's actual quote text shells out to poppler's
  `pdftotext -bbox` (see `pdftext.go`) rather than reading glyphs from
  `rsc.io/pdf` directly — the only reliable way to reach text a scanned
  PDF's OCR pass buried in a Form XObject.
- `internal/web` — stdlib `http.ServeMux` (Go 1.22 method+path patterns, no
  router dependency), `html/template` parsed once from an `embed.FS`, htmx
  for partial swaps. Handlers split across `server.go`, `queue.go`
  (grading/tags/library/bulk actions), `reader.go`, `document.go`,
  `dashboard.go`, `calendar.go`, `import.go`, `images.go`, `embeds.go`,
  `footnotes.go`, `sanitize.go`, and `api.go` — a small read-mostly JSON
  surface over annotations, for mirroring them into org-roam. State-changing actions are plain HTML forms
  that work with JS disabled; `app.js` only layers convenience on top
  (selection→block/offset coordinates, scroll anchoring across swaps,
  bulk-select).
- `cmd/increader/main.go` — wires concrete types into the interfaces; the web
  server and syncer run as goroutines sharing one `*store.Store`.

## Key design patterns

- **Transactional outbox for upstream writes.** Every local change and its
  queued wallabag write commit in the same SQLite transaction, so a wallabag
  outage delays a write instead of losing it. Deleting an extract cascades
  away its not-yet-sent create-write. The drain side (read pending → call
  provider → complete) is serialized behind `drainMu`.
- **A passage's verbatim text is never written from outside.** `quote` is what
  a highlight is *located* by — re-derived against the article on every open —
  and what the outbox pushes upstream as a wallabag annotation's body. The
  JSON API therefore cannot write it: a correction goes to `edited_quote`,
  read only by the display (`Element.DisplayQuote`). That is what makes
  "fix the mangled maths from Emacs" safe by construction rather than by the
  caller knowing which rows happen to be anchored. increader's own annotation
  editor still rewrites `quote` directly, and clears the override when it
  does; see migration 018.
- **Stale-selection protection.** Extract/cloze creation re-derives the
  selected passage server-side from its own sanitized copy of the article and
  responds 409 on mismatch, so an annotation cannot silently attach to the
  wrong text after the article was re-fetched and reshaped.
- **Explicit threat model: hostile articles, not hostile peers.** There is no
  auth and no CSRF, by design — the deployment assumption is a private
  LAN/Tailscale network, and the port binding is the perimeter (stated in
  `config.yaml` and `compose.yaml`). Hardening instead targets the actual
  untrusted input, the article itself: a tightened bluemonday policy
  (`sanitize.go`: scheme allowlist, no relative URLs, class allowlist) is the
  only path to `template.HTML`, and the image proxy blocks SSRF at dial time
  (loopback, RFC1918, link-local, and the Tailscale CGNAT range
  `100.64.0.0/10`), which also defeats DNS rebinding.
- **Testing.** Meaningful coverage of the tricky parts: scheduling clamps and
  backlog fuzz, annotation-range round-trips, a concurrent-drain race test,
  SSRF boundary cases, an XSS regression test — plus an opt-in Playwright
  test (`internal/web/browser/`) for the browser-only bugs Go handler tests
  structurally cannot see.
