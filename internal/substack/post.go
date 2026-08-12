package substack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// postBody is the response shape of GET .../api/v1/posts/{slug} — the
// fields this package actually reads out of it. Substack's real response
// carries a great deal more (reactions, comment counts, cover image
// metadata, and more); everything not declared here is simply dropped by
// json.Unmarshal, which is fine for the in-memory value but is exactly why
// post caches the raw response bytes rather than a re-marshaled postBody —
// see post below.
type postBody struct {
	ID               int       `json:"id"`
	Slug             string    `json:"slug"`
	Type             string    `json:"type"`
	Audience         string    `json:"audience"`
	Title            string    `json:"title"`
	CanonicalURL     string    `json:"canonical_url"`
	PostDate         time.Time `json:"post_date"`
	BodyHTML         string    `json:"body_html"`
	Language         string    `json:"language"`
	PublishedBylines []byline  `json:"publishedBylines"`
}

// byline is one entry of postBody's publishedBylines array. Substack lists
// every credited author here; toDocument only ever reads the first.
type byline struct {
	Name string `json:"name"`
}

// validateSlug rejects a slug that could escape CacheDir when joined into a
// path: one containing a path separator, or a ".." segment.
//
// Substack's own slugs are URL path components — every real one seen is
// matched by [a-z0-9-] — so a slug failing this check did not come from a
// well-behaved archive listing. Treating it as untrusted input here, rather
// than trusting whatever the API happened to send, is what stops a hostile
// or corrupted response from writing a cache file outside CacheDir
// entirely.
func validateSlug(slug string) error {
	if slug == "" {
		return errors.New("substack: archive entry has an empty slug")
	}
	if strings.ContainsAny(slug, "/\\") || strings.Contains(slug, "..") {
		return fmt.Errorf("substack: slug %q is not safe as a path element", slug)
	}
	return nil
}

// cachePath returns where slug's cache entry is written: {CacheDir}/{Host}/{slug}.json.
//
// post always calls validateSlug before this, so in normal operation this
// never sees anything unsafe. filepath.Base is still applied here as a
// second, independent line of defense: even a slug that somehow reached
// this function without going through that check can produce at worst an
// oddly-named file inside CacheDir/Host, never a path that climbs out of it
// via a leading "../" or an embedded "/".
func (i *Importer) cachePath(slug string) string {
	return filepath.Join(i.cfg.CacheDir, i.cfg.Host, filepath.Base(slug)+".json")
}

// cacheEntry is what actually gets written to disk under CacheDir — a small
// wrapper around Substack's own raw post JSON (Post), not that JSON stored
// bare.
//
// The wrapper exists for one reason: Substack's own response carries no
// record of whether the cookie it was fetched with actually worked. A
// paywalled preview and the real article are, per verifySession's own doc
// comment in session.go, structurally indistinguishable from the JSON alone — so nothing
// in Post itself can tell a later run whether a cached only_paid post is the
// genuine article or a preview served while the subscription had lapsed.
// Authenticated records that fact at write time, when it is actually known:
// post only ever writes a cache entry after verifySession has confirmed the
// session cookie works for this run, so every entry this package itself
// writes has Authenticated true. False — or the field simply being absent,
// which decodes to the same zero value — only happens for a file left
// behind by a version of this package written before this wrapper existed
// (see loadCachedPost), or one that was hand-edited.
//
// FetchedAt is diagnostic only: nothing in this package's own logic reads it
// back. It exists so an operator inspecting CacheDir by hand can tell how
// stale a given file is without needing to correlate it against a run's own
// logs.
type cacheEntry struct {
	FetchedAt     time.Time       `json:"fetched_at"`
	Authenticated bool            `json:"authenticated"`
	Post          json.RawMessage `json:"post"`
}

// post fetches one post by slug, from CacheDir if a trustworthy cached copy
// exists, from the network otherwise.
//
// A cached free post (Audience != "only_paid") is always trusted: Substack
// serves a free post's full content to anyone, cookie or not, so there is
// nothing a lapsed subscription could have truncated. A cached paid post is
// trusted only if it was written under a session verifySession had already
// confirmed worked (see cacheEntry) — otherwise it is exactly the kind of
// file this package must not mistake for "already have this one": a preview
// cached while lapsed, indistinguishable from the real thing by its own
// content (see verifySession), with only the wrapper's own Authenticated
// flag able to say so.
func (i *Importer) post(ctx context.Context, slug string) (postBody, bool, error) {
	if err := validateSlug(slug); err != nil {
		return postBody{}, false, err
	}
	path := i.cachePath(slug)

	cached, authenticated, found, err := loadCachedPost(path)
	if err != nil {
		return postBody{}, false, err
	}
	if found && (cached.Audience != "only_paid" || authenticated) {
		return cached, true, nil
	}

	raw, err := i.fetchRaw(ctx, "/api/v1/posts/"+url.PathEscape(slug), true)
	if err != nil {
		return postBody{}, false, err
	}

	var fresh postBody
	if err := json.Unmarshal(raw, &fresh); err != nil {
		return postBody{}, false, fmt.Errorf("substack: decode post %q: %w", slug, err)
	}

	// Every network fetch post makes goes out with the cookie (fetchRaw's
	// withCookie=true above), and Ingest never reaches this loop at all
	// unless verifySession already confirmed that cookie works — see
	// Ingest's own ordering. So Authenticated is unconditionally true for
	// anything this line writes; it is only ever false for a file this
	// package did not just write itself.
	entry := cacheEntry{
		FetchedAt:     time.Now(),
		Authenticated: true,
		// The raw bytes exactly as Substack sent them are what gets
		// wrapped and cached, not a re-marshaled fresh — round-tripping
		// through postBody would silently drop every field this struct
		// does not declare, the first time some later change to postBody
		// needs one that an already-cached file from before that change
		// never had a chance to carry.
		Post: json.RawMessage(raw),
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return postBody{}, false, fmt.Errorf("substack: encode cache entry for %q: %w", slug, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return postBody{}, false, fmt.Errorf("substack: create cache directory for %q: %w", slug, err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return postBody{}, false, fmt.Errorf("substack: write cache for %q: %w", slug, err)
	}

	return fresh, false, nil
}

// loadCachedPost reads and decodes a cached post, reporting (zero, false,
// false, nil) when there is no cache file yet — the ordinary, expected case
// on a slug's first run — rather than treating a missing file as an error.
//
// A file that fails to decode as a cacheEntry — including, deliberately, one
// written by the version of this package that predates the wrapper and
// stored Substack's raw post JSON bare, with no "post" key to unwrap at all
// — is treated the same as no cache: json.Unmarshal into cacheEntry leaves
// Post empty, the subsequent unmarshal of Post into postBody fails, and this
// falls through to the same "no usable cache" return. That is a deliberate
// fallback, not a bug: a refetch is always possible, and erring toward one
// extra request is a smaller problem than trusting a file whose provenance
// this cannot establish.
func loadCachedPost(path string) (postBody, bool, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return postBody{}, false, false, nil
	}
	if err != nil {
		return postBody{}, false, false, fmt.Errorf("substack: read cache %q: %w", path, err)
	}

	var entry cacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return postBody{}, false, false, nil
	}

	var cached postBody
	if err := json.Unmarshal(entry.Post, &cached); err != nil {
		return postBody{}, false, false, nil
	}
	return cached, entry.Authenticated, true, nil
}

// maxResponseBytes bounds how much of a single response fetchRaw will read,
// so a misbehaving proxy or an unexpectedly huge response cannot exhaust
// memory. A Substack post body is text plus image URLs, never embedded
// binary data, so a real response is nowhere near this.
const maxResponseBytes = 20 << 20 // 20 MiB

// backoffBase is exponential backoff's starting delay after a 429 or 5xx,
// doubled on each subsequent attempt.
const backoffBase = 500 * time.Millisecond

// getJSON performs a rate-limited, retrying, authenticated GET against path
// on Host and decodes the JSON response into out.
func (i *Importer) getJSON(ctx context.Context, path string, out any) error {
	raw, err := i.fetchRaw(ctx, path, true)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("substack: decode response from %s: %w", path, err)
	}
	return nil
}

// errUnauthorized reports a 401 from Substack's own API — the session
// cookie itself was rejected outright, as distinct from every other way a
// request can fail. Mirrors the sentinel-error pattern wallabag's own
// client uses for the same status (see errUnauthorized in
// internal/wallabag/client.go): a caller checks for this specific failure
// with errors.Is rather than parsing http.StatusText out of an error
// string, which is what lets verifySubscriptionState in session.go give the
// operator "your cookie itself is dead" as a genuinely different diagnosis
// from "your cookie works but has no paid access" — two problems with two
// different fixes, easy to conflate if the only signal is "something went
// wrong".
var errUnauthorized = errors.New("substack: unauthorized")

// errNotFound reports a 404 from Substack's own API, wrapped the same way
// errUnauthorized is, for the same reason: a caller that needs to know
// specifically "this was a 404" — verifySubscriptionState in session.go,
// which cannot otherwise tell a dead session apart from a nonexistent
// publication on the subscription endpoint (see
// diagnoseSubscriptionFailure's own doc comment for the live finding
// behind that) — checks for it with errors.Is instead of parsing
// http.StatusText out of an error string.
var errNotFound = errors.New("substack: not found")

// fetchRaw performs one rate-limited, retrying GET and returns the response
// body undecoded. getJSON (which decodes), post's own cache-writing path
// (which needs the exact bytes Substack sent, not a re-encoded copy — see
// post), and both halves of session.go's two-stage session check all sit on
// top of this.
//
// withCookie controls whether the Cookie header is sent at all — true for
// every ordinary call in this package, false only from verifySession's own
// deliberately-unauthenticated half of its differential comparison.
//
// It sleeps at least RequestGap before every request it sends, including
// the first of a batch and every retry — throttle enforces that across
// every caller sharing this Importer, not per-call. On a 429 or 5xx it
// retries with exponential backoff, up to MaxAttempts total attempts. A 401
// or 404 is returned immediately as errUnauthorized or errNotFound, not
// retried — neither a rejected cookie nor a missing resource starts
// working on a second attempt. Any other non-200 status is likewise not
// the kind of transient failure a retry could fix, so it too is returned
// immediately instead of burning through the retry budget on something
// that will never succeed.
func (i *Importer) fetchRaw(ctx context.Context, path string, withCookie bool) ([]byte, error) {
	endpoint := "https://" + i.cfg.Host + path

	var lastErr error
	for attempt := 1; attempt <= i.cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		i.throttle(ctx)

		body, status, err := i.doRequest(ctx, endpoint, withCookie)
		switch {
		case err != nil:
			lastErr = err
		case status == http.StatusTooManyRequests || status >= 500:
			lastErr = fmt.Errorf("substack: GET %s: %s", path, http.StatusText(status))
		case status == http.StatusUnauthorized:
			return nil, fmt.Errorf("substack: GET %s: %w", path, errUnauthorized)
		case status == http.StatusNotFound:
			return nil, fmt.Errorf("substack: GET %s: %w", path, errNotFound)
		case status != http.StatusOK:
			return nil, fmt.Errorf("substack: GET %s: %s", path, http.StatusText(status))
		default:
			return body, nil
		}

		if attempt < i.cfg.MaxAttempts {
			i.backoff(ctx, attempt)
		}
	}
	return nil, fmt.Errorf("substack: GET %s failed after %d attempts: %w", path, i.cfg.MaxAttempts, lastErr)
}

// fetchOnce performs a single, non-retried GET against an arbitrary full
// endpoint URL (unlike fetchRaw, which always targets Config.Host) and
// classifies the result the same way fetchRaw does for a 401 — wrapping
// errUnauthorized — but with no retry loop at all, even for a 429 or 5xx.
//
// This exists only for diagnoseSubscriptionFailure's disambiguating probe
// in session.go, which is deliberately capped at exactly one extra request:
// retrying it would contradict the reason it exists, "one extra request on
// the error path, never more" — and it targets a fixed, publication-
// independent host rather than Config.Host, which fetchRaw has no way to
// do at all.
func (i *Importer) fetchOnce(ctx context.Context, endpoint string, withCookie bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	i.throttle(ctx)

	body, status, err := i.doRequest(ctx, endpoint, withCookie)
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusUnauthorized:
		return nil, fmt.Errorf("substack: GET %s: %w", endpoint, errUnauthorized)
	case status != http.StatusOK:
		return nil, fmt.Errorf("substack: GET %s: %s", endpoint, http.StatusText(status))
	default:
		return body, nil
	}
}

// doRequest sends one GET and returns the response body alongside its
// status code, so fetchRaw can decide whether the status is worth retrying
// without doRequest itself needing to know anything about retry policy.
func (i *Importer) doRequest(ctx context.Context, endpoint string, withCookie bool) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("substack: build request for %s: %w", endpoint, err)
	}
	// The session cookie is the entire credential this package holds. It
	// must never be logged or wrapped into an error string anywhere in this
	// file — see Config.SessionID's own comment and the tests that pin this
	// down. withCookie is false exactly once in this package, from
	// verifySession's deliberately-anonymous half of its comparison; every
	// other caller leaves it true.
	if withCookie {
		req.Header.Set("Cookie", "substack.sid="+i.cfg.SessionID)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := i.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("substack: GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("substack: read response from %s: %w", endpoint, err)
	}
	return body, resp.StatusCode, nil
}

// throttle blocks until at least RequestGap has passed since the start of
// the previous request this Importer sent, across every caller.
//
// notBefore is advanced past the wait it just handed out, not just past
// "now + RequestGap": if two calls race in (only possible if a future
// caller ever uses one Importer from multiple goroutines; Ingest itself is
// single-threaded), the second must still wait a full RequestGap after the
// first's computed start time, not after whatever moment it happened to
// check the clock.
func (i *Importer) throttle(ctx context.Context) {
	i.rateMu.Lock()
	now := time.Now()
	wait := i.notBefore.Sub(now)
	if wait < 0 {
		wait = 0
	}
	i.notBefore = now.Add(wait).Add(i.cfg.RequestGap)
	i.rateMu.Unlock()

	if wait <= 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// backoff pauses for an exponentially increasing delay after a retryable
// failure, cancellable by ctx the same way throttle is.
func (i *Importer) backoff(ctx context.Context, attempt int) {
	delay := backoffBase * time.Duration(uint(1)<<uint(attempt-1))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
