package store

import (
	"testing"
	"time"
)

// TestDeletionsAreRecordedByEveryRoute: a tombstone is written however a row
// leaves — an extract deleted directly, one taken by its parent's cascade,
// and a whole document with everything in it.
func TestDeletionsAreRecordedByEveryRoute(t *testing.T) {
	db := testStore(t)
	earlier := time.Now().Add(-time.Hour)

	result, err := db.ImportAnnotations(book(), ImportOptions{Triage: true}, earlier)
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}
	annotations, err := db.DocumentAnnotations(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	first := annotations[0]
	child, err := db.CreateExtract(NewExtract{
		ParentID: first.ID, DocumentID: result.DocumentID, Quote: "the painter", ContentHTML: "<p>the painter</p>",
	}, earlier)
	if err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	if err := db.DeleteExtract(first.ID); err != nil {
		t.Fatalf("DeleteExtract: %v", err)
	}
	deletions, err := db.Deletions(time.Time{}, 0)
	if err != nil {
		t.Fatalf("Deletions: %v", err)
	}
	gone := map[int64]Deletion{}
	for _, deletion := range deletions {
		gone[deletion.ID] = deletion
	}
	if len(deletions) != 2 || gone[first.ID].Kind != "annotation" || gone[child].Kind != "annotation" {
		t.Fatalf("deletions = %+v, want the extract and the child its cascade took", deletions)
	}
	if gone[first.ID].ExternalRef != first.ExternalRef || gone[first.ID].DocumentID != result.DocumentID {
		t.Errorf("tombstone = %+v, want external ref %q in document %d", gone[first.ID], first.ExternalRef, result.DocumentID)
	}
	if gone[first.ID].DeletedAt.Before(earlier) {
		t.Errorf("deleted_at = %v, want it stamped at deletion time", gone[first.ID].DeletedAt)
	}

	if err := db.DeleteDocument(result.DocumentID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	deletions, err = db.Deletions(time.Time{}, 0)
	if err != nil {
		t.Fatalf("Deletions: %v", err)
	}
	var documents, rest int
	for _, deletion := range deletions {
		switch {
		case deletion.Kind == "document" && deletion.ID == result.DocumentID:
			documents++
		case deletion.Kind == "annotation":
			rest++
		}
	}
	if documents != 1 || rest != len(annotations)+1 {
		t.Errorf("after deleting the document: %d document and %d annotation tombstones, want 1 and %d",
			documents, rest, len(annotations)+1)
	}
}

// TestAnnotatedDocumentsReportsDeletions: deleting an annotation moves its
// document's feed timestamp on, and a document that loses its last
// annotation still appears in a since-query, with a count of zero, so a
// mirror learns to empty it.
func TestAnnotatedDocumentsReportsDeletions(t *testing.T) {
	db := testStore(t)
	earlier := time.Now().Add(-time.Hour)

	result, err := db.ImportAnnotations(book(), ImportOptions{Triage: true}, earlier)
	if err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}
	feed, err := db.AnnotatedDocuments(AnnotatedFilter{})
	if err != nil || len(feed) != 1 {
		t.Fatalf("AnnotatedDocuments: %v, %d rows", err, len(feed))
	}
	watermark := feed[0].AnnotationsUpdatedAt

	annotations, err := db.DocumentAnnotations(result.DocumentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if err := db.DeleteExtract(annotations[0].ID); err != nil {
		t.Fatalf("DeleteExtract: %v", err)
	}
	feed, err = db.AnnotatedDocuments(AnnotatedFilter{Since: watermark})
	if err != nil {
		t.Fatalf("AnnotatedDocuments: %v", err)
	}
	if len(feed) != 1 || feed[0].Annotations != len(annotations)-1 {
		t.Fatalf("feed since the watermark = %+v, want the document with one fewer annotation", feed)
	}
	watermark = feed[0].AnnotationsUpdatedAt

	for _, annotation := range annotations[1:] {
		if err := db.DeleteExtract(annotation.ID); err != nil {
			t.Fatalf("DeleteExtract: %v", err)
		}
	}
	// Deletions within the same second as the watermark are not after it;
	// step the watermark back to see them, as a consumer polling later would.
	feed, err = db.AnnotatedDocuments(AnnotatedFilter{Since: watermark.Add(-time.Second)})
	if err != nil {
		t.Fatalf("AnnotatedDocuments: %v", err)
	}
	if len(feed) != 1 || feed[0].ID != result.DocumentID || feed[0].Annotations != 0 {
		t.Fatalf("feed = %+v, want the emptied document listed with no annotations", feed)
	}

	all, err := db.AnnotatedDocuments(AnnotatedFilter{})
	if err != nil {
		t.Fatalf("AnnotatedDocuments: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("full listing = %+v, want an emptied document left out of it", all)
	}
}
