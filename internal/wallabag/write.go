package wallabag

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

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
