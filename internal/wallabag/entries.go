package wallabag

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Detail levels for a listing. Metadata omits the article body, which makes
// syncing a large library dramatically cheaper; bodies are fetched per article
// on first read instead.
const (
	DetailMetadata = "metadata"
	DetailFull     = "full"
)

// defaultPerPage is the page size for listings. wallabag defaults to 30; 100
// cuts the number of round trips on a large library without producing
// responses big enough to matter.
const defaultPerPage = 100

// ListOptions selects which entries a listing returns.
type ListOptions struct {
	// Since restricts results to entries whose updated_at is at or after this
	// time. The zero value means "everything", which is what a first sync wants.
	Since time.Time

	// Detail is DetailMetadata or DetailFull; empty means DetailMetadata.
	Detail string

	// PerPage overrides the page size. Zero means defaultPerPage.
	PerPage int
}

// AllEntries walks every page of a listing and returns the entries.
//
// Results are sorted by update time ascending. That ordering is deliberate: a
// listing filtered by `since` is a moving target, and with descending order an
// entry updated mid-walk shifts every later entry down a slot, so paging past
// it silently skips records. Ascending order appends changes at the end
// instead, where at worst they are picked up by the next sync.
func (c *Client) AllEntries(ctx context.Context, opts ListOptions) ([]Entry, error) {
	if opts.PerPage <= 0 {
		opts.PerPage = defaultPerPage
	}
	if opts.Detail == "" {
		opts.Detail = DetailMetadata
	}

	var all []Entry
	for page := 1; ; page++ {
		query := url.Values{
			"page":    {strconv.Itoa(page)},
			"perPage": {strconv.Itoa(opts.PerPage)},
			"detail":  {opts.Detail},
			"sort":    {"updated"},
			"order":   {"asc"},
		}
		if !opts.Since.IsZero() {
			query.Set("since", strconv.FormatInt(opts.Since.Unix(), 10))
		}

		var result entryPage
		if err := c.get(ctx, "/api/entries.json", query, &result); err != nil {
			return nil, fmt.Errorf("wallabag: list entries (page %d): %w", page, err)
		}

		all = append(all, result.Embedded.Items...)

		// Stop on the reported last page, and independently on an empty page:
		// the second condition guarantees termination even if Pages is wrong
		// or absent, which an unbounded loop over a remote API needs.
		if len(result.Embedded.Items) == 0 || page >= result.Pages {
			break
		}
	}
	return all, nil
}

// EntryByID fetches one entry, including its article body and annotations.
func (c *Client) EntryByID(ctx context.Context, id int) (Entry, error) {
	var entry Entry
	path := fmt.Sprintf("/api/entries/%d.json", id)
	if err := c.get(ctx, path, nil, &entry); err != nil {
		return Entry{}, fmt.Errorf("wallabag: fetch entry %d: %w", id, err)
	}
	return entry, nil
}
