package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// fetchBodiesBulkAction is the library bulk bar's "Fetch bodies" button
// value. It is intercepted in handleLibraryBulk before the ordinary
// libraryBulkAction.apply dispatch, rather than expressed as one more entry
// that mechanism drives, because every other bulk action is a synchronous
// per-element effect and this one is not — see handleFetchBodiesBulk for why.
const fetchBodiesBulkAction = "fetch_bodies"

const (
	// bodyPrefetchWorkers caps how many article bodies are fetched at once.
	// Small on purpose: this exists to be polite to wallabag, not to race it,
	// matching imageResolveConcurrency's reasoning in images.go for the same
	// kind of fetch.
	bodyPrefetchWorkers = 3

	// bodyPrefetchTimeout bounds the whole background run, not any single
	// fetch — s.fetchBody, and the HTTP client underneath whichever source it
	// calls, already have their own per-request timeouts. A library-sized
	// backlog fetched three at a time can legitimately take several minutes,
	// so this is generous, but it is not unbounded: a source that hangs
	// instead of erroring must not be able to leave the guard mutex below
	// held forever.
	bodyPrefetchTimeout = 30 * time.Minute
)

// bodyPrefetchRunning guards against two overlapping bulk fetches — a second
// click on "Fetch bodies" while one is already draining must not start a
// competing set of workers double-fetching the same documents. Package-level
// rather than a field on Server: there is exactly one bulk fetch running for
// the whole process at a time, the same way the store's own connection pool
// is one for the whole process regardless of how many Servers might exist in
// a test binary.
var bodyPrefetchRunning sync.Mutex

// handleFetchBodiesBulk starts a background fetch of every selected
// document's body that is not already cached, and redirects immediately.
//
// This deliberately does not block the request the way every other bulk
// action in handleLibraryBulk does. Fetching a library-sized batch of
// article bodies is a matter of minutes, not the sub-second an HTTP request
// is expected to take, and there is nothing the reader needs to wait for:
// the library's own "not yet fetched" label (library.html) already tells
// them which documents are still pending, and it updates on its own the next
// time they refresh the page — that label is the progress indicator, with
// nothing extra to build.
func (s *Server) handleFetchBodiesBulk(w http.ResponseWriter, r *http.Request) {
	var elementIDs []int64
	for _, raw := range r.Form["ids"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		elementIDs = append(elementIDs, id)
	}

	toFetch, alreadyDone, err := s.prefetchTargets(elementIDs)
	if err != nil {
		s.fail(w, err)
		return
	}

	var notice string
	switch {
	case len(toFetch) == 0:
		notice = "Nothing to fetch — every selected article already has a body, or came from a source that is not configured."

	case !bodyPrefetchRunning.TryLock():
		// Someone else's bulk fetch already holds the lock. Saying so here,
		// rather than silently doing nothing, is the whole point of the
		// guard being visible: a reader who clicked twice should not be left
		// wondering whether the second click did anything.
		notice = "A bulk fetch is already running; try again once it finishes."

	default:
		requested := len(toFetch) + alreadyDone
		go s.runBodyPrefetch(toFetch, alreadyDone)
		notice = fmt.Sprintf("Fetching %d of %d selected article bodies in the background — refresh to watch \"not yet fetched\" drain.",
			len(toFetch), requested)
	}

	s.redirect(w, r, withNotice(redirectTarget(r, "/library"), notice))
}

// prefetchTargets resolves the library bulk bar's checked root element ids
// into the document ids actually worth fetching, deduplicated (two selected
// elements can share a document only if the caller passed duplicate ids, but
// nothing guarantees they will not) and filtered down to documents that both
// lack a body and came from a source this server has configured — fetching
// for an unconfigured source would just fail per-document later, so there is
// no reason to count it as an attempt at all.
//
// alreadyDone counts every selected document skipped for either of those
// reasons, so the caller can report "requested" as the full selection rather
// than just the subset actually dispatched to a worker.
//
// A row that no longer exists — deleted, or merged away, between the page
// loading and the button being pressed — is skipped rather than aborting,
// matching every other bulk action's tolerance for a stale selection; see
// handleLibraryBulk's own doc comment for why.
func (s *Server) prefetchTargets(elementIDs []int64) (toFetch []int64, alreadyDone int, err error) {
	seen := make(map[int64]bool, len(elementIDs))

	for _, id := range elementIDs {
		element, err := s.store.ElementByID(id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, 0, err
		}
		if seen[element.DocumentID] {
			continue
		}
		seen[element.DocumentID] = true

		document, err := s.store.DocumentByID(element.DocumentID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, 0, err
		}

		if document.HasContent {
			alreadyDone++
			continue
		}
		if _, configured := s.sources[document.Source]; !configured {
			alreadyDone++
			continue
		}
		toFetch = append(toFetch, document.ID)
	}
	return toFetch, alreadyDone, nil
}

// runBodyPrefetch fetches every document in toFetch, bodyPrefetchWorkers at a
// time, and stores each as it completes. preSkipped is folded into the
// finishing log's "skipped" count, so that count plus "fetched" plus "failed"
// always adds up to the full original selection, not just the subset this
// function itself was handed.
//
// Given its own background context with a generous timeout rather than the
// triggering request's: handleFetchBodiesBulk returns, and its request
// context is cancelled, the instant the redirect above is written — this
// runs well after that. Inheriting the request's context would make the
// fetch cancel itself after one or two documents with nothing in the
// response to explain why; ctx.Background() plus bodyPrefetchTimeout is what
// keeps this alive for the whole run while still bounding it against a
// source that hangs instead of erroring.
func (s *Server) runBodyPrefetch(toFetch []int64, preSkipped int) {
	defer bodyPrefetchRunning.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), bodyPrefetchTimeout)
	defer cancel()

	requested := len(toFetch) + preSkipped
	s.logger.Info("bulk body fetch starting", "requested", requested)

	var (
		// saveMu serializes every SetDocumentContent write below, and also
		// guards the three counters — both for the same underlying reason
		// resolveImages' own saveMu exists (see images.go): the store's
		// connection pool is capped at one connection, so letting every
		// worker write whenever its own fetch finishes would turn into
		// concurrent writes racing for that one connection. fetchBody itself
		// — the network call — deliberately runs outside this lock, so the
		// workers still fetch in parallel; only the write at the end of each
		// is serialized.
		saveMu                   sync.Mutex
		sem                      = make(chan struct{}, bodyPrefetchWorkers)
		wg                       sync.WaitGroup
		fetched, skipped, failed int
	)
	skipped = preSkipped

dispatch:
	for i, documentID := range toFetch {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// The budget is spent. Whatever was not even dispatched yet is
			// counted as skipped, same as a document already cached — a
			// later bulk fetch (or an ordinary first open) will pick it up,
			// there is nothing to retry here.
			saveMu.Lock()
			skipped += len(toFetch) - i
			saveMu.Unlock()
			break dispatch
		}

		wg.Add(1)
		go func(documentID int64) {
			defer wg.Done()
			defer func() { <-sem }()

			document, err := s.store.DocumentByID(documentID)
			if err != nil {
				s.logger.Warn("bulk body fetch: could not read document",
					"document", documentID, "error", err)
				saveMu.Lock()
				failed++
				saveMu.Unlock()
				return
			}
			if document.HasContent {
				// Raced with something else that fetched this document since
				// prefetchTargets ran — a reader opening it directly, most
				// plausibly. Nothing left to do.
				saveMu.Lock()
				skipped++
				saveMu.Unlock()
				return
			}

			// The same path a reader's first open already goes through —
			// see (*Server).fetchBody for the source.Enricher type assertion
			// and the plain Content fallback this reuses rather than
			// duplicating.
			body, _, err := s.fetchBody(ctx, document)
			if err != nil {
				// Logged and skipped, never fatal to the batch: a body that
				// will not fetch today is exactly the situation lazy
				// fetching on first open already tolerates, just discovered
				// here instead of at read time.
				s.logger.Warn("bulk body fetch: could not fetch article body",
					"document", documentID, "error", err)
				saveMu.Lock()
				failed++
				saveMu.Unlock()
				return
			}

			saveMu.Lock()
			if err := s.store.SetDocumentContent(documentID, body); err != nil {
				s.logger.Warn("bulk body fetch: could not save article body",
					"document", documentID, "error", err)
				failed++
			} else {
				fetched++
			}
			saveMu.Unlock()
		}(documentID)
	}
	wg.Wait()

	s.logger.Info("bulk body fetch finished",
		"requested", requested, "fetched", fetched, "skipped", skipped, "failed", failed)
}

// withNotice adds a "notice" query parameter to a redirect target, which
// library.html shows as a one-line banner — the only mechanism this app has
// for telling the reader something about a request that redirects away
// immediately, such as handleFetchBodiesBulk's own "already running" or
// "fetching N in the background". target may already carry its own query
// string (redirectTarget can return the library's current filtered URL), so
// this parses and re-encodes rather than concatenating.
func withNotice(target, notice string) string {
	if notice == "" {
		return target
	}
	parsed, err := url.Parse(target)
	if err != nil {
		// Not expected — target only ever comes from redirectTarget, which
		// already validates it — but falling back to the bare target beats
		// failing the whole redirect over a cosmetic banner.
		return target
	}
	query := parsed.Query()
	query.Set("notice", notice)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
