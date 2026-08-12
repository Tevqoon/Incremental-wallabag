package wallabag

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
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

	// Annotated restricts results to entries that carry annotations.
	//
	// wallabag added this filter in 2.6; on older servers the parameter is
	// ignored and the listing comes back unfiltered, which is why callers must
	// check SupportsAnnotationFilter rather than relying on it silently.
	Annotated bool
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
		if opts.Annotated {
			query.Set("annotations", "1")
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

// NewEntry is the payload for creating an entry via CreateEntry.
//
// Every field but URL is optional. A field left at its zero value is simply
// omitted from the request (see entryForm) rather than sent as an explicit
// blank, so that leaving, say, Content empty lets wallabag's own extractor
// run instead of telling it "the content is the empty string" — those are
// not the same instruction, and only one of them was ever tested.
type NewEntry struct {
	URL, Title, Content, Language, Authors string
	PublishedAt                            time.Time
	Tags                                   []string
	Archived, Starred                      bool
}

// CreateEntry saves a new entry in wallabag, from a URL and, optionally,
// content and metadata increader already has in hand — a document fetched
// and extracted elsewhere (increader's own local extraction path, or a
// sibling project like ft2wallabag) that never needs wallabag's own graby to
// touch it at all.
//
// Every fact below was confirmed against the live app.wallabag.it API on
// 2026-08-12 with a throwaway entry, not assumed from wallabag's published
// source — each is exactly the kind of thing that looks obvious until it
// turns out not to be true:
//
//   - The endpoint answers 200, not 201, despite creating something new.
//     doOnce in client.go treats any status other than 200 as a failure,
//     which means a real 201 response would have made a successful create
//     look exactly like an error — but wallabag does not send one, so doOnce
//     needs no change to handle this correctly as it stands today. Recorded
//     here specifically so nobody "fixes" doOnce later to accept 201,
//     believing that is what a create response ought to look like.
//   - POST is an upsert keyed on url, not a pure create: posting the same
//     url a second time returns the same entry id, with whatever new fields
//     were sent applied to it, rather than creating a duplicate entry. That
//     cuts both ways. It is a safety net when increader's own idea of a
//     document's URL is not a perfect match for wallabag's — a near-miss
//     updates the existing entry instead of duplicating it. It is also a
//     trap in the opposite direction: a url that happens to normalize the
//     same way as some unrelated existing entry's silently overwrites that
//     entry's content instead of creating a new, separate one, with no
//     warning that this happened.
//   - Supplying Content suppresses wallabag's own graby extractor. Content
//     round-tripped byte-identical for simple markup — a single bare <p>
//     paragraph, nothing nested or styled — which is as far as this was
//     actually tested; no claim is made here about anything more elaborate
//     surviving the round trip intact.
//
// See EntryUpdate.form for a further, separate finding from the same
// testing session about what a later PATCH against an entry created this way
// can and cannot be trusted to preserve.
func (c *Client) CreateEntry(ctx context.Context, e NewEntry) (Entry, error) {
	form, err := entryForm(e)
	if err != nil {
		return Entry{}, err
	}

	var entry Entry
	if err := c.send(ctx, "POST", "/api/entries.json", form, &entry); err != nil {
		return Entry{}, fmt.Errorf("wallabag: create entry %q: %w", e.URL, err)
	}
	return entry, nil
}

// entryForm builds the form-encoded body CreateEntry sends.
//
// Confirmed against the live API alongside the findings on CreateEntry
// itself: published_at is accepted as Unix seconds, sent as a decimal
// string, and reads back as e.g. "2019-03-14T09:26:53+0000" — the format
// wallabag.Time already parses. authors is a single comma-separated string
// on the wire (and reads back split into Entry.PublishedBy); NewEntry.Authors
// is already typed as that same single string, so it is passed straight
// through with no joining of its own to do. tags is likewise one
// comma-separated string, which is why a tag label containing a comma is
// rejected outright below — exactly the same rule, and the same reasoning,
// as AddTags already applies in write.go for the dedicated tag-add endpoint:
// wallabag has no escaping convention for a comma inside one label, so a
// label containing one would silently become two different tags instead of
// the one the caller asked for. archive and starred are 0/1, via the
// existing boolParam helper.
//
// Title, Content, and Language are omitted when empty rather than sent as
// blanks, matching NewEntry's own doc comment: an empty Content in
// particular was never tested, and "not provided, let wallabag decide" is
// the safer reading of that gap than "explicitly blank" would be.
func entryForm(e NewEntry) (url.Values, error) {
	form := url.Values{"url": {e.URL}}

	if e.Title != "" {
		form.Set("title", e.Title)
	}
	if e.Content != "" {
		form.Set("content", e.Content)
	}
	if e.Language != "" {
		form.Set("language", e.Language)
	}
	if e.Authors != "" {
		form.Set("authors", e.Authors)
	}
	if !e.PublishedAt.IsZero() {
		form.Set("published_at", strconv.FormatInt(e.PublishedAt.Unix(), 10))
	}

	if len(e.Tags) > 0 {
		cleaned := make([]string, 0, len(e.Tags))
		for _, tag := range e.Tags {
			trimmed := strings.TrimSpace(tag)
			if trimmed == "" {
				continue
			}
			if strings.Contains(trimmed, ",") {
				return nil, fmt.Errorf("wallabag: tag %q contains a comma, which wallabag treats as a separator", trimmed)
			}
			cleaned = append(cleaned, trimmed)
		}
		if len(cleaned) > 0 {
			form.Set("tags", strings.Join(cleaned, ","))
		}
	}

	form.Set("archive", boolParam(e.Archived))
	form.Set("starred", boolParam(e.Starred))
	return form, nil
}

// EntryUpdate is a partial update to an existing entry's content and
// metadata, applied by UpdateEntry via a PATCH.
//
// See form for the asymmetry this type's zero-value handling creates, which
// is the single most important thing to understand before calling
// UpdateEntry: leaving a field unset is not the same as leaving it alone for
// every field equally.
type EntryUpdate struct {
	Title, Content, Language, Authors string
	PublishedAt                       time.Time
}

// UpdateEntry replaces some of an existing entry's content and metadata with
// a PATCH.
func (c *Client) UpdateEntry(ctx context.Context, id int, u EntryUpdate) (Entry, error) {
	var entry Entry
	path := fmt.Sprintf("/api/entries/%d.json", id)
	if err := c.send(ctx, "PATCH", path, u.form(), &entry); err != nil {
		return Entry{}, fmt.Errorf("wallabag: update entry %d: %w", id, err)
	}
	return entry, nil
}

// form encodes u for UpdateEntry's PATCH body, omitting every field left at
// its zero value rather than sending it as an explicit blank.
//
// That omission is not a stylistic default picked up front — it was forced
// by a finding from the same live-API testing session CreateEntry's own doc
// comment describes: a PATCH that set only Content, against an entry that
// already had a title, came back with the title preserved but Entry.PublishedBy
// blanked out. Sending every field on every PATCH, title included, would
// have been the more obviously "correct" design — until it turned out that a
// content-only update would otherwise have silently erased the entry's
// title along with it. Omitting empty fields here is what makes a
// content-only update actually safe to make.
//
// The asymmetry that leaves behind is the surprising part, and worth stating
// plainly rather than leaving implicit: omitting Authors here does not mean
// "leave the existing authors alone" the way it might reasonably be assumed
// to. It means "authors was not present in this particular request", and
// what happens to a field that is absent from one PATCH but was set by an
// earlier write is apparently not settled by presence alone — Authors did
// not survive the sequence in the one case this was actually run through.
// The probe behind this finding could not cleanly separate whether it was
// the upsert-POST that created the entry, or the follow-up PATCH itself,
// that actually did the blanking — only that Authors was gone afterward.
// Practically: a caller that wants Authors to survive a later content-only
// edit must re-send it on every single write that touches the entry, not
// just the one that first set it.
func (u EntryUpdate) form() url.Values {
	form := url.Values{}
	if u.Title != "" {
		form.Set("title", u.Title)
	}
	if u.Content != "" {
		form.Set("content", u.Content)
	}
	if u.Language != "" {
		form.Set("language", u.Language)
	}
	if u.Authors != "" {
		form.Set("authors", u.Authors)
	}
	if !u.PublishedAt.IsZero() {
		form.Set("published_at", strconv.FormatInt(u.PublishedAt.Unix(), 10))
	}
	return form
}
