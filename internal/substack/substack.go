// Package substack pulls a Substack publication's full archive — including
// paywalled posts, using the operator's own paid subscription cookie — and
// hands back what it found as source.Document values.
//
// It is deliberately a leaf, the same discipline internal/wallabag and
// internal/source both hold to: it writes nowhere, and it imports nothing
// beyond the standard library, golang.org/x/net/html, and internal/source
// for the Document type it produces. Nothing here knows about wallabag or
// SQLite; a later package is responsible for taking what Ingest returns and
// actually storing it somewhere.
//
// The motivating case this was built for: an operator who subscribes to a
// publication for a month at a time, uses that window to backfill its whole
// archive, and then lets the subscription lapse until the next time they
// want more. A run has to be safe to repeat across that on-again-off-again
// pattern — the cache in post.go exists for that — and it must never
// mistake a paywall preview served while lapsed for the real article and
// import it over content that was already fetched correctly; see
// isPaywalled in post.go and the consecutive-paywall abort in Ingest below.
package substack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Tevqoon/increader/internal/source"
)

// defaultRequestGap is what RequestGap defaults to when a caller leaves it
// at zero: a deliberate pause between requests to a service with no
// documented rate limit, out of courtesy — this is not increader's own API
// to hammer.
const defaultRequestGap = 1500 * time.Millisecond

// defaultMaxAttempts is how many times a single request is tried, counting
// the first attempt, before getJSON gives up on it.
const defaultMaxAttempts = 3

// paywallAbortThreshold is how many consecutive paywalled fetches Ingest
// tolerates before concluding the session cookie is no longer valid and
// aborting the whole run, rather than continuing to import previews over
// what may already be good content. See Ingest's own comment for why this
// has to be "3 in a row" specifically, and not "the first paid post came
// back paywalled".
const paywallAbortThreshold = 3

// Config configures one Importer.
type Config struct {
	// Host is the publication's own domain: "example.substack.com", or a
	// custom domain the publication has mapped, e.g. "www.example.com".
	// Either way requests go straight to this host — increader does no
	// separate lookup from a publication name to a domain.
	Host string

	// SessionID is the substack.sid cookie value from a browser logged into
	// an account with an active paid subscription to Host. This is a
	// secret: it is the entire credential this package holds, equivalent to
	// a password, and it must never be written to a log line, an error
	// message, a Result, or a String() method — see Config.String() below
	// and the tests in substack_test.go that pin this down.
	SessionID string

	// CacheDir is where each fetched post's raw JSON is written, one file
	// per slug — see cachePath in post.go. Reused across runs so a later
	// run, made after the subscription has lapsed again, can skip
	// re-fetching anything already captured while it was active.
	CacheDir string

	// RequestGap is the minimum spacing between requests to Host. Zero
	// means defaultRequestGap.
	RequestGap time.Duration

	// MaxAttempts is how many times one request is tried before giving up
	// on it. Zero means defaultMaxAttempts.
	MaxAttempts int

	// HTTPClient is the client requests are sent on. Nil means a client
	// with a 60-second timeout — matching wallabag.New's own default, and
	// for the same reason: the zero-value http.Client has no timeout at
	// all, which turns a hung server into a hang of increader itself.
	HTTPClient *http.Client
}

// String implements fmt.Stringer with SessionID redacted.
//
// Go note: fmt's formatting logic checks whether a value satisfies Stringer
// — including a struct field nested inside something bigger being printed —
// before it falls back to reflecting over the fields directly. Defining
// this method means an accidental %v, %+v, or bare Print of a Config (or of
// anything holding one, like Importer) prints this redacted form instead of
// walking into SessionID, without every call site needing to remember to
// redact it itself.
func (c Config) String() string {
	return fmt.Sprintf(
		"substack.Config{Host:%q, SessionID:\"[redacted]\", CacheDir:%q, RequestGap:%v, MaxAttempts:%d}",
		c.Host, c.CacheDir, c.RequestGap, c.MaxAttempts,
	)
}

// Importer pulls one publication's archive.
//
// Safe for concurrent use: the only mutable state is the request-throttling
// clock (rateMu, notBefore), guarded by its own mutex — matching the
// convention wallabag.Client documents for its own token cache.
type Importer struct {
	cfg Config

	// rateMu and notBefore implement throttle in post.go: the earliest time
	// the next request to Host may go out, shared across every caller —
	// archive pages and post fetches alike — because both count against the
	// same rate limit on Substack's side.
	rateMu    sync.Mutex
	notBefore time.Time
}

// New validates cfg and returns a ready Importer. It performs no I/O of its
// own, matching wallabag.New: a misconfigured Importer fails fast at
// construction rather than partway through a long archive walk.
func New(cfg Config) (*Importer, error) {
	if cfg.Host == "" {
		return nil, errors.New("substack: Host is required")
	}
	if cfg.SessionID == "" {
		return nil, errors.New("substack: SessionID is required")
	}
	if cfg.CacheDir == "" {
		return nil, errors.New("substack: CacheDir is required")
	}
	if cfg.RequestGap <= 0 {
		cfg.RequestGap = defaultRequestGap
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Importer{cfg: cfg}, nil
}

// Name identifies this provider, matching the convention every
// source.Source implementation follows (see wallabag.Source.Name), even
// though Importer does not itself implement source.Source: Ingest's shape
// does not fit that interface's Fetch(ctx, since time.Time), because
// Substack's archive has no "changed since" listing to page through — only
// a fixed newest-first archive and a per-slug fetch. A later package is
// expected to adapt Ingest's output onto whatever storage-facing shape it
// needs, the same way wallabag.Source adapts wallabag.Client.
func (i *Importer) Name() string { return "substack" }

// Result summarises one Ingest run.
type Result struct {
	// Pages is how many archive listing requests walkArchive made.
	Pages int

	// Posts is how many distinct post ids the archive listing named, of
	// every type, before SkippedNonNewsletter's filtering removes any.
	Posts int

	// Cached is how many posts were served from CacheDir without a network
	// request, because a prior run already had a non-paywalled copy on
	// disk.
	Cached int

	// Fetched is how many posts required an actual network request: not yet
	// cached, or cached but paywalled and therefore not trusted — see
	// post's own cache-skip rule in post.go.
	Fetched int

	// SkippedNonNewsletter is how many archive entries were not imported
	// because their type was not "newsletter" — restacks, most commonly,
	// which have no body of their own and whose slugs 404 rather than
	// returning anything.
	SkippedNonNewsletter int

	// StillPaywalled is how many fetched posts came back as a paywall
	// preview rather than the full article — see isPaywalled in post.go.
	// These are counted but not imported.
	StillPaywalled int

	// Warnings collects non-fatal problems encountered along the way: a
	// single post's fetch failing partway through the run, cleanBody being
	// unable to parse a body, or the archive walk being cut off by
	// maxArchiveOffset before it found a natural end. Ingest continues past
	// every one of these; only the consecutive-paywall guard stops the run
	// outright, and that is reported as an error, not folded in here.
	Warnings []string
}

// Ingest walks the publication's archive, fetches every newsletter post it
// finds, cleans each body, and returns them as source.Document values. It
// writes nothing anywhere — see the package doc comment.
func (i *Importer) Ingest(ctx context.Context, logger *slog.Logger) ([]source.Document, Result, error) {
	if logger == nil {
		// walkArchive and the loop below narrate through logger
		// unconditionally; defaulting here once means neither has to guard
		// against a nil logger on every call.
		logger = slog.Default()
	}

	posts, pages, archiveWarnings, err := i.walkArchive(ctx, logger)
	result := Result{Pages: pages, Posts: len(posts), Warnings: archiveWarnings}
	if err != nil {
		return nil, result, fmt.Errorf("substack: walk archive: %w", err)
	}

	var documents []source.Document

	// consecutivePaywalled counts network fetches, in a row, that came back
	// as a paywall preview rather than the real article. It resets to zero
	// on any post that is not a paywalled preview — cached or freshly
	// fetched, paid or free — because what this is actually watching for is
	// "the cookie has stopped working", and a single good result in between
	// is direct evidence that is not (yet) true.
	//
	// Deliberately not keyed on "the first paid post was paywalled": a free
	// post cannot signal a lapsed cookie at all (isPaywalled returns false
	// for it vacuously, since it was never behind a paywall to begin with),
	// and a lone paid post being paywalled could just as well be that one
	// post's audience field being wrong, or a transient blip, rather than
	// the whole subscription having lapsed. Only a run of several in a row
	// is the pattern this guard exists to catch — importing dozens of
	// previews over good content is the exact failure it prevents, so it
	// aborts the entire run rather than skipping one post and continuing.
	consecutivePaywalled := 0

	for _, entry := range posts {
		// Restacks and any other non-newsletter entry have no body of their
		// own — their slugs 404 rather than returning something merely
		// empty — so this is a hard skip decided from the archive listing
		// itself, not something post could discover cheaply on its own by
		// trying and failing.
		if entry.Type != "newsletter" {
			result.SkippedNonNewsletter++
			continue
		}

		body, fromCache, err := i.post(ctx, entry.Slug)
		if err != nil {
			msg := fmt.Sprintf("post %q: %v", entry.Slug, err)
			result.Warnings = append(result.Warnings, msg)
			logger.Warn("skipping a post after its fetch failed", "slug", entry.Slug, "error", err)
			continue
		}

		if fromCache {
			result.Cached++
		} else {
			result.Fetched++
		}

		if isPaywalled(body) {
			result.StillPaywalled++
			// post's own cache-skip rule (see post.go) never hands back a
			// cached result that is paywalled — a cached-and-paywalled file
			// always triggers a fresh fetch instead. This still only counts
			// a genuine network round trip toward the abort guard, rather
			// than assuming that invariant holds: a stale or hand-edited
			// cache directory should not be able to trip the abort on its
			// own without a single real request having failed.
			if !fromCache {
				consecutivePaywalled++
				if consecutivePaywalled >= paywallAbortThreshold {
					return documents, result, fmt.Errorf(
						"substack: aborting after %d consecutive paywalled fetches — the session cookie is likely expired or the subscription has lapsed; refresh SessionID before running again",
						paywallAbortThreshold,
					)
				}
			}
			continue
		}
		consecutivePaywalled = 0

		cleaned, warnings := cleanBody(body.BodyHTML)
		result.Warnings = append(result.Warnings, warnings...)
		documents = append(documents, toDocument(body, cleaned))
	}

	return documents, result, nil
}
