package wallabag

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Tevqoon/increader/internal/source"
)

// Compile-time proof that *Source can write as well as read.
var _ source.Writer = (*Source)(nil)

// tagCache maps tag labels to wallabag's numeric ids.
//
// Needed because removing a tag from an entry addresses it by id
// (DELETE /api/entries/{entry}/tags/{tag}) while everything else in increader
// speaks in labels. The mapping changes only when tags are created or deleted,
// so it is fetched once and refreshed on a miss.
type tagCache struct {
	mu     sync.Mutex
	byName map[string]int
}

// SetArchived marks an entry read or unread in wallabag.
func (s *Source) SetArchived(ctx context.Context, externalID string, archived bool) error {
	return s.patchEntry(ctx, externalID, url.Values{"archive": {boolParam(archived)}})
}

// SetStarred marks an entry as a favourite in wallabag.
func (s *Source) SetStarred(ctx context.Context, externalID string, starred bool) error {
	return s.patchEntry(ctx, externalID, url.Values{"starred": {boolParam(starred)}})
}

// patchEntry applies a partial update to an entry.
func (s *Source) patchEntry(ctx context.Context, externalID string, form url.Values) error {
	id, err := entryID(externalID)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/entries/%d.json", id)
	return s.client.send(ctx, "PATCH", path, form, nil)
}

// AddTags attaches labels to an entry, creating any that do not exist yet.
func (s *Source) AddTags(ctx context.Context, externalID string, labels []string) error {
	cleaned := make([]string, 0, len(labels))
	for _, label := range labels {
		if trimmed := strings.TrimSpace(label); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}

	id, err := entryID(externalID)
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/api/entries/%d/tags.json", id)
	// The endpoint takes one comma-separated list, so a label containing a
	// comma would silently become two tags. Rejecting is better than quietly
	// producing something the reader did not ask for.
	for _, label := range cleaned {
		if strings.Contains(label, ",") {
			return fmt.Errorf("wallabag: tag %q contains a comma, which wallabag treats as a separator", label)
		}
	}

	if err := s.client.send(ctx, "POST", path, url.Values{"tags": {strings.Join(cleaned, ",")}}, nil); err != nil {
		return err
	}

	// New tags were possibly created, so the id cache is stale.
	s.tags.invalidate()
	return nil
}

// RemoveTag detaches one label from an entry.
func (s *Source) RemoveTag(ctx context.Context, externalID string, label string) error {
	id, err := entryID(externalID)
	if err != nil {
		return err
	}

	tagID, err := s.tagID(ctx, label)
	if err != nil {
		return err
	}
	if tagID == 0 {
		// wallabag does not know this tag, so the entry cannot be carrying it.
		// Treated as done rather than as an error: the desired end state — the
		// entry not having this tag — already holds, and failing here would
		// leave the write retrying forever against a tag that will never exist.
		return nil
	}

	path := fmt.Sprintf("/api/entries/%d/tags/%d.json", id, tagID)
	return s.client.send(ctx, "DELETE", path, nil, nil)
}

// DeleteEntry removes a whole entry from wallabag, not just an annotation on
// it — the library's "delete" button uses this so a document actually
// leaves wallabag instead of only disappearing locally until the next sync
// brings it right back.
func (s *Source) DeleteEntry(ctx context.Context, externalID string) error {
	id, err := entryID(externalID)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/entries/%d.json", id)
	return s.client.send(ctx, "DELETE", path, nil, nil)
}

// DeleteHighlight removes one annotation, identified by its own id — not the
// entry it sits on. Verified against the live API: DELETE returns 200 with the
// deleted annotation's body, and it does not require the entry id at all.
func (s *Source) DeleteHighlight(ctx context.Context, highlightExternalID string) error {
	id, err := strconv.Atoi(highlightExternalID)
	if err != nil {
		return fmt.Errorf("wallabag: %q is not a valid annotation id: %w", highlightExternalID, err)
	}
	path := fmt.Sprintf("/api/annotations/%d.json", id)
	return s.client.send(ctx, "DELETE", path, nil, nil)
}

// UpdateHighlightLocation gives an annotation a location by replacing it,
// not by updating it in place — confirmed against wallabag's own
// EditAnnotationType, which accepts a comment field and nothing else, so
// there is no PUT that could attach ranges after the fact.
//
// Ordered create-then-delete deliberately, not the other way round: if the
// create half fails, the old (locationless, but real) annotation is still
// there. Deleting first and having the create fail would instead leave
// nothing behind at all — the annotation quietly gone rather than merely
// still missing a location, which is a worse failure for a best-effort
// operation to risk.
//
// A failure to delete the old annotation is deliberately not returned as an
// error once the new one exists, even though that leaves a duplicate
// behind. The caller's outbox retries a failed write by calling this again
// with the same old id — which by then may already be gone, or about to be
// replaced a second time regardless — so surfacing the error would not fix
// the duplicate, only manufacture another one on every retry until the
// delete eventually succeeds or the attempt cap gives up. A stray duplicate
// annotation is a smaller, quieter problem than that.
func (s *Source) UpdateHighlightLocation(ctx context.Context, highlightExternalID, documentExternalID, quote string) (string, error) {
	newID, err := s.CreateHighlight(ctx, documentExternalID, quote)
	if err != nil {
		return "", err
	}
	_ = s.DeleteHighlight(ctx, highlightExternalID)
	return newID, nil
}

// maxHighlightQuoteLength guards against a limit that exists only on
// app.wallabag.it's actual database, not in its published source: wallabag's
// own Annotation entity declares an Assert\Length(max: 10000) on quote, but a
// POST past roughly 1000 bytes 500s in practice — confirmed by bisecting
// against the live API, plain-ASCII text included to rule out a UTF-8
// artifact, not inferred from the validator. Whatever the real column is
// sized for, it is far short of what the application claims to accept.
//
// Left well under the observed 1000-byte failure line rather than pushed
// right up against it, since the true limit could differ slightly on another
// wallabag deployment or version.
const maxHighlightQuoteLength = 900

// truncateQuote shortens quote to fit maxHighlightQuoteLength, if needed.
//
// The cut lands on a UTF-8 rune boundary rather than an arbitrary byte
// offset — the quotes this handles are real prose, not ASCII, and slicing
// mid-character would corrupt the last rune sent rather than merely
// shortening the text. DecodeLastRuneInString is what makes that reliable:
// checking whether the trailing byte itself starts a rune is not enough,
// since the last byte of a complete multi-byte character is never a rune
// start either — that check alone would trim one rune too many every time.
// RuneError specifically means the cut landed inside an incomplete sequence.
//
// Trimming stops only once the tail decodes cleanly, not after a single
// byte — confirmed insufficient against real stored data on 2026-08-12: a
// live wallabag account had an annotation whose quote broke at character
// 880 of 881 on a U+FFFD replacement character, meaning a single-byte trim
// had left 1-2 dangling bytes of a 3- or 4-byte rune behind. Those dangling
// bytes are not valid UTF-8 on their own, so encoding/json (sendJSON's own
// json.Marshal, called downstream in CreateHighlight) silently rewrote them
// into an actual U+FFFD character on the way out — which is how a
// replacement character ended up stored in wallabag's own database. A
// single fixed-size trim only ever rescues the case where the cut landed
// one byte into a 2-byte rune; anything wider needs to keep trimming.
//
// The loop tells genuine debris apart from a legitimate U+FFFD already
// present in the source text by the size DecodeLastRuneInString returns,
// not by the rune value alone: it returns (RuneError, 1) when the trailing
// bytes are actually invalid — the case this loop exists to fix — but
// returns (RuneError, 3) when cut legitimately ends in an already-encoded
// U+FFFD that was real prose before truncation ever touched it (U+FFFD
// encodes to 3 bytes in UTF-8). Only a returned size of 1 means "these are
// debris bytes, strip them and check again"; a size greater than 1 means
// the decode found a genuine rune whose value happens to be RuneError, and
// the loop must leave it alone.
func truncateQuote(quote string) string {
	if len(quote) <= maxHighlightQuoteLength {
		return quote
	}
	cut := quote[:maxHighlightQuoteLength]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + "…"
}

// TrimTruncationMarker undoes what truncateQuote did to a quote on its way
// upstream, so a quote read back from wallabag can be located in article
// text again by an exact search — QuoteAnchored and QuoteOccurrences both
// expect a substring that actually occurs in the document, and neither the
// "…" truncateQuote appends nor any debris its pre-fix version left behind
// occurs in the article itself.
//
// Confirmed against a live dry run over real data on 2026-08-12: 13 of 16
// annotations a Substack backfill classified as VerdictMissing — quote not
// found in the new content at all — turned out to be nothing but this. The
// passage each one highlights is still there, unedited, right up to the
// byte truncateQuote happened to cut at; only the trailing marker this
// function strips was ever missing from the article.
//
// Strips, in this order: a trailing U+2026 ellipsis (truncateQuote's own
// marker), then any trailing U+FFFD replacement characters (debris the
// pre-fix version of truncateQuote — see its own comment above — could
// leave behind by cutting a multi-byte rune mid-sequence; that corruption
// is already live in the operator's wallabag account and cannot be repaired
// retroactively by anything running today, only worked around on read),
// then surrounding whitespace. A quote with neither marker is returned
// unchanged.
//
// There is no way to tell a quote truncateQuote actually shortened apart
// from one that happened to end in "…" on its own before truncation ever
// touched it — both look identical by the time they come back from
// wallabag's API, and this trims either way. The false positive is
// harmless: for a quote that genuinely ended in an ellipsis, trimming that
// one character off the end still leaves a string that is an exact
// substring of the original passage, so locating it in article text still
// succeeds; only a search for the literal ellipsis itself would fail, and
// nothing here ever wants that.
func TrimTruncationMarker(quote string) string {
	trimmed := strings.TrimSuffix(quote, "…")
	for strings.HasSuffix(trimmed, "�") {
		trimmed = strings.TrimSuffix(trimmed, "�")
	}
	return strings.TrimSpace(trimmed)
}

// CreateHighlight adds a new annotation to an entry, for a passage extracted
// locally in increader rather than in wallabag's own reader. It returns the
// new annotation's id, which the caller must keep: without it, deleting this
// extract later would have no way to remove the matching highlight upstream.
//
// Only what is sent upstream is shortened by truncateQuote when the passage
// is long; the local extract keeps the full text regardless, since this
// limit is a wallabag peculiarity that has no bearing on increader's own copy.
//
// Unlike every other write in this file, wallabag's annotation endpoint reads
// a JSON body (json_decode($request->getContent())) rather than the form body
// the rest of the API uses — confirmed against wallabag's own
// AnnotationController source, not assumed.
//
// ranges — the XPath-based location wallabag's own web and Android clients
// need to actually draw the highlight in place, rather than just store the
// text — is computed by fetching the entry's own content and locating quote
// within it (see computeRanges). Best-effort: a failure to fetch or locate
// it is not fatal, and simply falls back to an empty ranges array, exactly
// what this sent unconditionally before ranges existed. The annotation
// itself, and Quote, are what a human reads regardless of whether wallabag's
// reader can also draw it in place.
func (s *Source) CreateHighlight(ctx context.Context, documentExternalID, quote string) (string, error) {
	id, err := entryID(documentExternalID)
	if err != nil {
		return "", err
	}

	ranges := []serializedRange{}
	if content, err := s.Content(ctx, documentExternalID); err == nil {
		if found := computeRanges(content, quote); found != nil {
			ranges = found
		}
	}

	path := fmt.Sprintf("/api/annotations/%d.json", id)
	body := struct {
		Text   string            `json:"text"`
		Quote  string            `json:"quote"`
		Ranges []serializedRange `json:"ranges"`
	}{
		Quote:  truncateQuote(quote),
		Ranges: ranges,
	}

	var created Annotation
	if err := s.client.sendJSON(ctx, "POST", path, body, &created); err != nil {
		return "", err
	}
	return strconv.Itoa(created.ID), nil
}

// tagID resolves a label to wallabag's id for it, or 0 when there is none.
func (s *Source) tagID(ctx context.Context, label string) (int, error) {
	s.tags.mu.Lock()
	cached := s.tags.byName
	s.tags.mu.Unlock()

	if cached != nil {
		if id, found := cached[label]; found {
			return id, nil
		}
	}

	// A miss might mean a stale cache rather than a missing tag, so refresh
	// once before concluding the tag does not exist.
	fresh, err := s.fetchTags(ctx)
	if err != nil {
		return 0, err
	}
	return fresh[label], nil
}

func (s *Source) fetchTags(ctx context.Context) (map[string]int, error) {
	var tags []Tag
	if err := s.client.get(ctx, "/api/tags.json", nil, &tags); err != nil {
		return nil, fmt.Errorf("wallabag: list tags: %w", err)
	}

	byName := make(map[string]int, len(tags))
	for _, tag := range tags {
		byName[tag.Label] = tag.ID
	}

	s.tags.mu.Lock()
	s.tags.byName = byName
	s.tags.mu.Unlock()
	return byName, nil
}

func (c *tagCache) invalidate() {
	c.mu.Lock()
	c.byName = nil
	c.mu.Unlock()
}

// Tags lists every tag the account knows about.
func (s *Source) Tags(ctx context.Context) ([]Tag, error) {
	var tags []Tag
	if err := s.client.get(ctx, "/api/tags.json", nil, &tags); err != nil {
		return nil, fmt.Errorf("wallabag: list tags: %w", err)
	}
	return tags, nil
}

// entryID converts increader's provider-neutral identifier back to wallabag's.
func entryID(externalID string) (int, error) {
	id, err := strconv.Atoi(externalID)
	if err != nil {
		return 0, fmt.Errorf("wallabag: %q is not a valid entry id: %w", externalID, err)
	}
	return id, nil
}

// boolParam renders a flag the way wallabag's API expects it: 0 or 1, not
// true or false.
func boolParam(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
