package substack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/Tevqoon/increader/internal/source"
)

// PostFromURL splits a Substack post URL into the host to fetch it from and
// the post's own slug.
//
// A caller pasting in one article's URL — the single-post counterpart to
// Ingest's whole-archive walk — has neither of those on their own: FetchPost
// only knows about slugs (that is what Substack's own per-post endpoint
// takes), and the host an Importer is configured for is fixed at
// construction, not read back out of a URL. This is the seam between the
// two: given a URL, it says which publication it belongs to and which post
// it names, so a caller can build an Importer for that publication (see
// Config.Host) and pass the slug straight to FetchPost.
//
// Substack's own post URL shape is https://{host}/p/{slug}, optionally
// followed by more path segments (a "/comments" suffix Substack's own share
// links carry) or a query string (tracking parameters); both are simply
// ignored, since the slug is exactly the first path segment after "/p/".
func PostFromURL(raw string) (host, slug string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("substack: %q is not a valid URL: %w", raw, err)
	}
	if parsed.Host == "" {
		return "", "", fmt.Errorf("substack: %q has no host", raw)
	}

	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) < 2 || segments[0] != "p" || segments[1] == "" {
		return "", "", fmt.Errorf(`substack: %q does not look like a post URL (expected .../p/{slug})`, raw)
	}
	if err := validateSlug(segments[1]); err != nil {
		return "", "", err
	}
	return parsed.Host, segments[1], nil
}

// FetchPost fetches and cleans one post by its own slug, for a single
// explicit import rather than a whole-archive backfill — see Ingest for
// that.
//
// Three things set this apart from the loop inside Ingest that calls post
// for the same purpose there:
//
//   - No archive walk, and no advance judgement about whether the account
//     has any paid relationship with this publication at all. Ingest's
//     stage-1 check (verifySubscriptionState) exists to fail a whole
//     archive backfill fast and cheaply before spending its request
//     budget — but it hard-refuses a publication the account has no paid
//     access to at all, which is the wrong rule here: a single import is
//     exactly the shape a free article from a publication the caller does
//     not subscribe to takes, and that must still work.
//   - No cache. post's on-disk cache exists to make repeated archive runs
//     cheap; a one-off explicit fetch is naturally fresh every time, and
//     skipping the cache here also sidesteps a subtler problem: post only
//     ever writes a cache entry marked Authenticated (trustworthy for a
//     later run) because Ingest guarantees verifySession already ran
//     first (see post's own comment on that ordering) — a guarantee this
//     function does not want to have to uphold or depend on.
//   - The paywall check targets this post specifically rather than
//     searching an archive listing for a paid one to verify against:
//     fetched once with the session cookie, its own Audience field says
//     whether it is paid, and only if so is it fetched again without the
//     cookie for the same differential comparison verifySession makes —
//     see sessionCanaryMinRatio's own doc comment for why that comparison,
//     not a markup heuristic, is what actually catches a truncated
//     preview. A free post needs no such check: nothing about it could be
//     truncated by a missing or lapsed subscription.
//
// The returned warnings are cleanBody's own — the same ones a whole-archive
// Ingest run would fold into Result.Warnings, surfaced directly here since
// there is no Result for a single post to live in.
func (i *Importer) FetchPost(ctx context.Context, slug string) (source.Document, []string, error) {
	if err := validateSlug(slug); err != nil {
		return source.Document{}, nil, err
	}

	path := "/api/v1/posts/" + url.PathEscape(slug)
	authenticatedRaw, err := i.fetchRaw(ctx, path, true)
	if err != nil {
		return source.Document{}, nil, fmt.Errorf("substack: fetch %q: %w", slug, err)
	}

	var body postBody
	if err := json.Unmarshal(authenticatedRaw, &body); err != nil {
		return source.Document{}, nil, fmt.Errorf("substack: decode %q: %w", slug, err)
	}

	if body.Audience == "only_paid" {
		anonymousRaw, err := i.fetchRaw(ctx, path, false)
		if err != nil {
			return source.Document{}, nil, fmt.Errorf("substack: verify access to %q: %w", slug, err)
		}
		authLen, err := bodyHTMLLength(authenticatedRaw)
		if err != nil {
			return source.Document{}, nil, fmt.Errorf("substack: decode %q: %w", slug, err)
		}
		anonLen, err := bodyHTMLLength(anonymousRaw)
		if err != nil {
			return source.Document{}, nil, fmt.Errorf("substack: decode anonymous fetch of %q: %w", slug, err)
		}
		if float64(authLen) < float64(anonLen)*sessionCanaryMinRatio {
			return source.Document{}, nil, fmt.Errorf(
				"substack: %q is paywalled and this session does not appear to unlock it (authenticated body_html %d bytes vs %d unauthenticated) — a paid subscription to this publication is required",
				slug, authLen, anonLen,
			)
		}
	}

	cleaned, warnings := cleanBody(body.BodyHTML)
	return toDocument(body, cleaned), warnings, nil
}
