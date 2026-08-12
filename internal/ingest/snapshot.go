// Package ingest is the sink for content that has to land in wallabag in
// place rather than as a new entry — a Substack post that already exists
// there as a paywall preview, most importantly, with highlights on it that
// have already been read and scheduled.
//
// It is deliberately the seam, not any one producer. Every fetcher that will
// ever feed this — the Substack archive crawl (internal/substack) today, a
// browser-pushed DOM later — produces the same value, []source.Document, and
// both need the identical destination machinery: create-or-update a wallabag
// entry in place, re-anchoring its annotations onto the new body without
// losing what has already been decided about them locally. So there is one
// sink and, so far, two callers, rather than a producer interface with one
// implementation apiece.
//
// Gather does all the reading against wallabag; BuildPlan is a pure function
// that decides what to do from that snapshot, with no context, no client and
// no clock beyond a passed-in now; Apply is the only piece that actually
// writes, and it is ordered so that a failure partway through never needs a
// rollback — see apply.go's own comment for why. Repair then folds the write
// back into the local store, so that a re-anchored highlight keeps the
// reading schedule already built up on it. There is no cross-system
// transaction covering any of this: wallabag and the local SQLite store are
// two different systems with no shared commit point, and every step here is
// written to degrade safely, not atomically, when interrupted.
package ingest

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/wallabag"
)

// Snapshot is wallabag's library state as of one read pass, gathered once by
// Gather and then reused by BuildPlan for every post in a batch — a
// once-a-month archive walk over a few hundred posts must not turn into a
// few hundred separate listing calls just to ask "does this already exist".
type Snapshot struct {
	// BySlug indexes every entry in the account whose URL or GivenURL
	// carries a Substack /p/{slug} segment, keyed on that slug — see slugOf
	// for what "carries" means and why the slug, not the URL itself, is the
	// unit of identity here. An entry with no such segment at all is simply
	// absent from every bucket; nothing in this package has any use for
	// wallabag entries that were never a Substack post to begin with.
	//
	// Built from one DetailMetadata listing over the *entire* account, not
	// filtered to this batch's posts — the cost of that pass does not grow
	// with how many of them are actually Substack, and metadata-detail
	// still carries each entry's own Annotations (see wallabag.Source.Fetch
	// for the same fact relied on elsewhere), which is what lets BuildPlan
	// decide the annotated-duplicate question without a second round trip.
	BySlug map[string][]wallabag.Entry

	// Details holds the full body and annotation set for every entry that
	// matched one of the batch's own posts by slug — a per-article
	// EntryByID fetch, not part of the metadata listing above. Restricted to
	// matched entries deliberately: fetching every Substack entry in a large
	// account regardless of whether this batch is touching it would be the
	// same mistake the lazy per-article body fetch elsewhere in increader
	// exists to avoid (see Source.Fetch's own comment on why listings omit
	// content).
	//
	// Keyed on Entry.ID, wallabag's own identifier — not the slug, since two
	// entries can legitimately share a slug (the conflict case BuildPlan
	// refuses to resolve) and BuildPlan needs to look each of them up by its
	// own identity, not by the ambiguous key that got them into that
	// situation in the first place.
	Details map[int]wallabag.Entry
}

// Gather reads wallabag once and returns the state BuildPlan needs to decide
// what to do with posts, without deciding anything itself.
//
// Exactly one AllEntries pass covers the whole account (roughly a dozen
// requests at wallabag's own default page size against a real personal
// library), and exactly one EntryByID follows per post whose slug actually
// matches something already up there — the "match" set, almost always far
// smaller than the account as a whole. A post with no match at all costs
// nothing extra here; BuildPlan turns that absence into ActionCreate on its
// own.
func Gather(ctx context.Context, client *wallabag.Client, posts []source.Document) (Snapshot, error) {
	entries, err := client.AllEntries(ctx, wallabag.ListOptions{Detail: wallabag.DetailMetadata})
	if err != nil {
		return Snapshot{}, fmt.Errorf("ingest: list wallabag entries: %w", err)
	}

	bySlug := make(map[string][]wallabag.Entry)
	for _, entry := range entries {
		// URL and GivenURL can each independently carry the slug (see
		// slugOf's own comment on the URL shapes this has to unify), and an
		// entry whose two fields happen to agree must only be indexed once
		// under that slug — registering it twice would make it look, to the
		// two-or-more-annotated-candidates conflict check in BuildPlan, like
		// two different entries sharing a slug when it is really one entry
		// counted twice.
		indexed := make(map[string]bool, 2)
		for _, candidate := range []string{entry.URL, entry.GivenURL} {
			slug := slugOf(candidate)
			if slug == "" || indexed[slug] {
				continue
			}
			indexed[slug] = true
			bySlug[slug] = append(bySlug[slug], entry)
		}
	}

	wanted := make(map[string]bool, len(posts))
	for _, post := range posts {
		if slug := slugOf(post.URL); slug != "" {
			wanted[slug] = true
		}
	}

	details := make(map[int]wallabag.Entry)
	for slug, candidates := range bySlug {
		if !wanted[slug] {
			continue
		}
		for _, candidate := range candidates {
			if _, already := details[candidate.ID]; already {
				// The same entry can appear under bySlug[slug] more than
				// once if this pass has not deduplicated candidates within
				// a single slug bucket elsewhere — defensive, since the
				// indexed guard above already prevents it for the common
				// case of one entry's own URL and GivenURL agreeing, but an
				// entry could in principle be wanted for two different
				// slugs it carries under those two different fields.
				continue
			}
			full, err := client.EntryByID(ctx, candidate.ID)
			if err != nil {
				return Snapshot{}, fmt.Errorf("ingest: fetch entry %d: %w", candidate.ID, err)
			}
			details[candidate.ID] = full
		}
	}

	return Snapshot{BySlug: bySlug, Details: details}, nil
}

// slugOf extracts the /p/{slug} path segment Substack gives every post,
// regardless of which of the several host and decoration shapes the URL
// arrived in:
//
//   - {publication}.substack.com/p/{slug}
//   - open.substack.com/pub/{publication}/p/{slug}
//   - a custom domain a publication has mapped onto Substack, still with
//     /p/{slug}
//
// each optionally carrying ?utm_source=..., &r=..., &showWelcome=... or
// &triedRedirect=true. Stripping query parameters alone does not unify the
// first two shapes — they are different hosts with different path prefixes
// entirely — which is why this keys on slug rather than on any normalised
// form of the URL itself: the /p/{slug} segment is the one thing every shape
// agrees on.
//
// Returns "" for a URL with no such segment — anything not a Substack post
// permalink at all, including a publication's own homepage or an /about
// page that might otherwise turn up in an entry's URL field — and such a
// URL is simply not indexed by Gather, the same as if it were never seen.
func slugOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i, segment := range segments {
		if segment != "p" {
			continue
		}
		if i+1 < len(segments) && segments[i+1] != "" {
			return segments[i+1]
		}
	}
	return ""
}
