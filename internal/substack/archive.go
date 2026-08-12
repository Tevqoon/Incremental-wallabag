package substack

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// archivePageSize is the archive listing's page size.
//
// Substack's own site asks for larger pages when browsing an archive in a
// real browser, but manually probing several publications' archive
// endpoints at a limit above 12 turned up inconsistent, overlapping results
// across consecutive requests — as though the API's own paging cursor does
// not agree with itself once the page is bigger. 12 is what stayed
// consistent, so this is not a tunable: raising it trades correctness for
// fewer requests, on a finding specific to this endpoint rather than a
// documented limit.
const archivePageSize = 12

// maxArchiveOffset bounds walkArchive's loop independently of anything the
// remote API reports.
//
// See AllEntries in internal/wallabag/entries.go (~lines 82-87) for the
// same pattern applied to wallabag's own pagination, and the reasoning
// behind it: an unbounded loop over a remote API needs a stop that does not
// depend on the remote behaving, because "when does this end" is not this
// package's call to make about a server it does not control. It matters
// more here than there. Some publications' archives are confirmed (see
// walkArchive's own comment below) to wrap past their real last page and
// start repeating earlier ones — the dedup-by-id termination this function
// implements is built specifically to catch that. But a hypothetical
// archive that kept producing genuinely novel ids forever — a bug on
// Substack's side, a pathological publication, anything not yet seen —
// would defeat that termination outright and loop forever without this
// second, independent cap.
//
// A var, not a const: substack_test.go lowers it for the "novel ids
// forever" test, so that test does not have to actually make ~417 fake
// requests to prove the cap works.
var maxArchiveOffset = 5000

// archivePost is one entry from the archive listing — everything walkArchive
// needs to decide whether a post is worth fetching, without paying for that
// fetch yet.
type archivePost struct {
	ID       int       `json:"id"`
	Slug     string    `json:"slug"`
	Type     string    `json:"type"`
	Audience string    `json:"audience"`
	Title    string    `json:"title"`
	PostDate time.Time `json:"post_date"`
}

// walkArchive pages through the publication's archive listing and returns
// every post it names. Filtering by type (newsletter vs. everything else,
// see Result.SkippedNonNewsletter) is left to the caller in Ingest, since
// deciding what to do about a skip is not this function's job — walkArchive
// only knows how to enumerate what the archive contains.
//
// Go note on the return shape: the brief this package was built from sketched
// this as returning just ([]archivePost, error). It grew two more values —
// the page count and a slice of warnings — because Result.Pages and
// Result.Warnings both need information only this loop has: how many
// requests it actually took, and whether it stopped early because of
// maxArchiveOffset rather than a natural end. Folding that into Ingest's own
// return values instead would have meant either duplicating the loop's
// bookkeeping in the caller or reaching back into Importer state from
// outside — Ingest already narrates through logger the same way, so this
// keeps the two return channels (structured data vs. narration) that this
// codebase generally prefers.
func (i *Importer) walkArchive(ctx context.Context, logger *slog.Logger) ([]archivePost, int, []string, error) {
	seen := make(map[int]bool)
	var all []archivePost
	var warnings []string
	pages := 0

	for offset := 0; offset < maxArchiveOffset; offset += archivePageSize {
		pages++
		path := fmt.Sprintf("/api/v1/archive?sort=new&search=&offset=%d&limit=%d", offset, archivePageSize)

		var page []archivePost
		if err := i.getJSON(ctx, path, &page); err != nil {
			return all, pages, warnings, fmt.Errorf("substack: fetch archive at offset %d: %w", offset, err)
		}

		newInPage := 0
		for _, post := range page {
			if seen[post.ID] {
				continue
			}
			seen[post.ID] = true
			newInPage++
			all = append(all, post)
		}

		logger.Debug("archive page fetched",
			"offset", offset, "returned", len(page), "new_ids", newInPage)

		// Terminate on a page yielding no new ids, not on an empty page.
		// Some publications' archives wrap past their real end and start
		// repeating earlier pages rather than ever returning nothing — an
		// empty-page check alone would never notice that, and the loop
		// would keep re-requesting the same handful of pages forever
		// instead of recognising it had already seen everything, relying
		// entirely on maxArchiveOffset to eventually cut it off. Checking
		// "any new ids at all" catches both a genuinely empty page and a
		// page that is merely a repeat, with the same test.
		if newInPage == 0 {
			return all, pages, warnings, nil
		}
	}

	warning := fmt.Sprintf(
		"archive walk stopped at the maxArchiveOffset safety cap (%d) while new ids were still arriving; the archive may be incompletely imported",
		maxArchiveOffset,
	)
	logger.Warn(warning)
	warnings = append(warnings, warning)
	return all, pages, warnings, nil
}
