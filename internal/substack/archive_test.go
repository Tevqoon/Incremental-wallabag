package substack

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// TestWalkArchiveWrapsAndRepeats covers the failure mode walkArchive's
// termination condition exists for: some publications' archives wrap past
// their real end and start serving earlier pages again rather than an empty
// page. Here, the third page (offset 24) repeats exactly what the first page
// (offset 0) already returned. walkArchive must recognise that as "nothing
// new" and stop, rather than looping on an empty-page check that a repeat
// would never satisfy.
func TestWalkArchiveWrapsAndRepeats(t *testing.T) {
	fake := newFakeSubstack(t)

	pageOne := []archiveFixture{}
	for id := 1; id <= 12; id++ {
		pageOne = append(pageOne, newArchiveFixture(id, "post-"+strconv.Itoa(id), "newsletter", "everyone"))
	}
	pageTwo := []archiveFixture{}
	for id := 13; id <= 24; id++ {
		pageTwo = append(pageTwo, newArchiveFixture(id, "post-"+strconv.Itoa(id), "newsletter", "everyone"))
	}

	fake.archivePages[0] = pageOne
	fake.archivePages[12] = pageTwo
	// offset 24 is the wrap: same ids as offset 0, not a new page.
	fake.archivePages[24] = pageOne

	importer := newTestImporter(t, fake.Server, nil)
	logger := testLogger(&strings.Builder{})

	posts, pages, warnings, err := importer.walkArchive(context.Background(), logger)
	if err != nil {
		t.Fatalf("walkArchive: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none (this is a clean, natural termination)", warnings)
	}

	if pages != 3 {
		t.Errorf("pages = %d, want 3 (offsets 0, 12, 24)", pages)
	}

	gotIDs := make(map[int]bool, len(posts))
	for _, p := range posts {
		gotIDs[p.ID] = true
	}
	if len(gotIDs) != len(posts) {
		t.Fatalf("walkArchive returned a duplicate id: %d unique among %d posts", len(gotIDs), len(posts))
	}
	for id := 1; id <= 24; id++ {
		if !gotIDs[id] {
			t.Errorf("missing id %d in result", id)
		}
	}
	if len(posts) != 24 {
		t.Errorf("len(posts) = %d, want 24", len(posts))
	}

	if got := fake.postRequestCountForArchive(); got != 3 {
		t.Errorf("archive requests = %d, want 3", got)
	}
}

// TestWalkArchiveNovelIDsForeverHitsCap covers the other half of the double
// termination condition: an archive that never repeats an id and never
// returns an empty page — the pathological case where dedup-by-id can never
// fire — must still be stopped by maxArchiveOffset, unconditionally.
//
// maxArchiveOffset is temporarily lowered so this test does not have to
// actually make ~417 fake requests to prove the real cap works; the
// mechanism being tested (the loop bound, not the specific number 5000) is
// identical at any cap.
func TestWalkArchiveNovelIDsForeverHitsCap(t *testing.T) {
	original := maxArchiveOffset
	maxArchiveOffset = 36 // three pages' worth
	t.Cleanup(func() { maxArchiveOffset = original })

	fake := newFakeSubstack(t)
	fake.archiveGenerator = func(offset int) []archiveFixture {
		var page []archiveFixture
		for i := 0; i < archivePageSize; i++ {
			id := offset + i + 1
			page = append(page, newArchiveFixture(id, "post-"+strconv.Itoa(id), "newsletter", "everyone"))
		}
		return page
	}

	importer := newTestImporter(t, fake.Server, nil)
	logger := testLogger(&strings.Builder{})

	posts, pages, warnings, err := importer.walkArchive(context.Background(), logger)
	if err != nil {
		t.Fatalf("walkArchive: %v", err)
	}

	wantPages := maxArchiveOffset / archivePageSize
	if pages != wantPages {
		t.Errorf("pages = %d, want %d (maxArchiveOffset / archivePageSize)", pages, wantPages)
	}
	if len(posts) != maxArchiveOffset {
		t.Errorf("len(posts) = %d, want %d", len(posts), maxArchiveOffset)
	}

	if len(warnings) == 0 {
		t.Error("expected a warning that the walk was cut off by the safety cap")
	}
}

// postRequestCountForArchive counts requests to /api/v1/archive, mirroring
// postRequestCount's role for post fetches.
func (f *fakeSubstack) postRequestCountForArchive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, path := range f.requestedPaths {
		if strings.HasPrefix(path, "/api/v1/archive") {
			count++
		}
	}
	return count
}
