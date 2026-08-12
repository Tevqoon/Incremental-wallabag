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

	"golang.org/x/net/html"
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

// paywallClasses are the CSS class names hasPaywallMarker looks for on a
// post body's own elements, rather than substring-matching the raw HTML for
// words like "paywall" or "subscribe" — which would false-positive on an
// ordinary free post that happens to end with its own subscribe
// call-to-action prose, exactly the failure mode isPaywalled exists to
// avoid.
//
// Sourced from publicly documented Substack post markup, not confirmed
// against a live paywalled fetch: this package was built without network
// access to a real Substack account with an active subscription, so nothing
// here was actually run against a genuine `.paywall` node in a real
// response the way most of this codebase's HTML-shape claims are pinned
// down (compare, say, wallabag.CreateEntry's doc comment, which names the
// exact date it was checked against the live API). Treat this list as a
// starting point to verify against the operator's own publication on first
// real use, not as an established fact.
var paywallClasses = map[string]bool{
	"paywall":             true,
	"subscribe-widget":    true,
	"subscription-widget": true,
}

// hasPaywallMarker reports whether bodyHTML contains one of Substack's own
// paywall/subscribe-widget elements.
//
// Parses the markup and inspects class attributes rather than
// string-searching the raw HTML — see paywallClasses' own comment for why
// that distinction matters here.
//
// Uses html.Parse rather than html.ParseFragment (contrast cleanBody, which
// must use the fragment parser because it re-serializes its result): a full
// document parse wraps whatever it is given in an implied
// <html><head></head><body>...</body></html>, but that wrapper is just more
// ancestor structure sitting above the real content — it does not hide or
// alter any node already in bodyHTML, and nothing here ever renders the
// result back out. For a read-only walk over the tree, the wrapper is
// harmless.
func hasPaywallMarker(bodyHTML string) bool {
	if strings.TrimSpace(bodyHTML) == "" {
		return false
	}
	doc, err := html.Parse(strings.NewReader(bodyHTML))
	if err != nil {
		return false
	}

	var found bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found {
			return
		}
		if n.Type == html.ElementNode && nodeHasClass(n, paywallClasses) {
			found = true
			return
		}
		for c := n.FirstChild; c != nil && !found; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

// nodeHasClass reports whether n's class attribute contains any name in
// classes. Class attributes are space-separated lists (an element commonly
// carries several), so this splits on whitespace rather than comparing the
// whole attribute value.
func nodeHasClass(n *html.Node, classes map[string]bool) bool {
	for _, attr := range n.Attr {
		if attr.Key != "class" {
			continue
		}
		for _, class := range strings.Fields(attr.Val) {
			if classes[class] {
				return true
			}
		}
	}
	return false
}

// paywallBodyLengthThreshold is a belt-and-braces check alongside
// hasPaywallMarker: a paywalled response's body_html is Substack's free
// teaser paragraphs plus the paywall block, not the full article, so it is
// short by construction. This catches a body that is truncated but whose
// paywall block, for whatever reason — a markup change on Substack's side
// this package has not seen — does not carry one of paywallClasses.
//
// Picked generously low relative to real prose, since a short *free* post is
// entirely unremarkable on its own (a link post, a brief announcement). This
// only ever adds confidence alongside hasPaywallMarker inside isPaywalled;
// it does not decide anything by itself.
const paywallBodyLengthThreshold = 2000

// isPaywalled decides whether a fetched post is a truncated preview rather
// than the real thing.
//
// Deliberately gated on Audience == "only_paid" first. A free post could
// coincidentally end with its own subscribe call-to-action (tripping
// hasPaywallMarker) or happen to be short (tripping the length check), and
// neither is evidence of anything when the post itself was never behind a
// paywall to begin with. Only a post the publication itself marked
// paid-only can actually be "still paywalled" — which is exactly why this
// function, and not some cheaper proxy like "was the very first paid post
// paywalled", is what both post's cache-skip rule below and Ingest's
// consecutive-failure guard key off: a free post reaching this check always
// returns false, vacuously, so it can never by itself signal a lapsed
// cookie. Only a genuine paid-and-still-blocked result can, and only a run
// of several of those in a row is treated as proof — see Ingest.
func isPaywalled(p postBody) bool {
	if p.Audience != "only_paid" {
		return false
	}
	return hasPaywallMarker(p.BodyHTML) || len(p.BodyHTML) < paywallBodyLengthThreshold
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

// cachePath returns where slug's raw post JSON is cached: {CacheDir}/{Host}/{slug}.json.
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

// post fetches one post by slug, from CacheDir if a trustworthy cached copy
// exists, from the network otherwise.
//
// The cache is skipped, not trusted, when the cached body_html shows a
// paywall: that copy was fetched while the subscription had lapsed, and a
// paywall preview cached under a post's slug is worth nothing — keeping it
// around only so a later run can mistake it for "already have this one" is
// precisely the failure this package exists to avoid (see the package doc
// comment). A cache hit that is not paywalled is returned as-is, with no
// further validation: if the subscription was active when it was fetched,
// there is nothing more recent to prefer it over.
func (i *Importer) post(ctx context.Context, slug string) (postBody, bool, error) {
	if err := validateSlug(slug); err != nil {
		return postBody{}, false, err
	}
	path := i.cachePath(slug)

	if cached, ok, err := loadCachedPost(path); err != nil {
		return postBody{}, false, err
	} else if ok && !isPaywalled(cached) {
		return cached, true, nil
	}

	raw, err := i.fetchRaw(ctx, "/api/v1/posts/"+url.PathEscape(slug))
	if err != nil {
		return postBody{}, false, err
	}

	var fresh postBody
	if err := json.Unmarshal(raw, &fresh); err != nil {
		return postBody{}, false, fmt.Errorf("substack: decode post %q: %w", slug, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return postBody{}, false, fmt.Errorf("substack: create cache directory for %q: %w", slug, err)
	}
	// The raw bytes exactly as Substack sent them are what gets cached, not
	// a re-marshaled fresh — round-tripping through postBody would silently
	// drop every field this struct does not declare, the first time some
	// later change to postBody needs one that an already-cached file from
	// before that change never had a chance to carry.
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return postBody{}, false, fmt.Errorf("substack: write cache for %q: %w", slug, err)
	}

	return fresh, false, nil
}

// loadCachedPost reads and decodes a cached post, reporting (zero, false,
// nil) when there is no cache file yet — the ordinary, expected case on a
// slug's first run — rather than treating a missing file as an error.
func loadCachedPost(path string) (postBody, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return postBody{}, false, nil
	}
	if err != nil {
		return postBody{}, false, fmt.Errorf("substack: read cache %q: %w", path, err)
	}

	var cached postBody
	if err := json.Unmarshal(raw, &cached); err != nil {
		// A corrupt cache file is treated as though there were no cache at
		// all, rather than as a hard error that fails the whole run: a
		// refetch is always possible, and one damaged file on disk — from
		// an interrupted write, a manual edit, anything — is a smaller
		// problem than aborting over it.
		return postBody{}, false, nil
	}
	return cached, true, nil
}

// maxResponseBytes bounds how much of a single response fetchRaw will read,
// so a misbehaving proxy or an unexpectedly huge response cannot exhaust
// memory. A Substack post body is text plus image URLs, never embedded
// binary data, so a real response is nowhere near this.
const maxResponseBytes = 20 << 20 // 20 MiB

// backoffBase is exponential backoff's starting delay after a 429 or 5xx,
// doubled on each subsequent attempt.
const backoffBase = 500 * time.Millisecond

// getJSON performs a rate-limited, retrying GET against path on Host and
// decodes the JSON response into out.
func (i *Importer) getJSON(ctx context.Context, path string, out any) error {
	raw, err := i.fetchRaw(ctx, path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("substack: decode response from %s: %w", path, err)
	}
	return nil
}

// fetchRaw performs one rate-limited, retrying GET and returns the response
// body undecoded. Both getJSON (which decodes) and post's own cache-writing
// path (which needs the exact bytes Substack sent, not a re-encoded copy —
// see post) sit on top of this.
//
// It sleeps at least RequestGap before every request it sends, including
// the first of a batch and every retry — throttle enforces that across
// every caller sharing this Importer, not per-call. On a 429 or 5xx it
// retries with exponential backoff, up to MaxAttempts total attempts; any
// other non-200 status (a 404 for a slug that no longer exists, a 401/403
// for a bad cookie) is not the kind of transient failure a retry could fix,
// so it is returned immediately instead of burning through the retry budget
// on something that will never succeed.
func (i *Importer) fetchRaw(ctx context.Context, path string) ([]byte, error) {
	endpoint := "https://" + i.cfg.Host + path

	var lastErr error
	for attempt := 1; attempt <= i.cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		i.throttle(ctx)

		body, status, err := i.doRequest(ctx, endpoint)
		switch {
		case err != nil:
			lastErr = err
		case status == http.StatusTooManyRequests || status >= 500:
			lastErr = fmt.Errorf("substack: GET %s: %s", path, http.StatusText(status))
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

// doRequest sends one GET and returns the response body alongside its
// status code, so fetchRaw can decide whether the status is worth retrying
// without doRequest itself needing to know anything about retry policy.
func (i *Importer) doRequest(ctx context.Context, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("substack: build request for %s: %w", endpoint, err)
	}
	// The session cookie is the entire credential this package holds. It
	// must never be logged or wrapped into an error string anywhere in this
	// file — see Config.SessionID's own comment and the tests that pin this
	// down.
	req.Header.Set("Cookie", "substack.sid="+i.cfg.SessionID)
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
