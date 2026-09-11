package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
)

// findScanRef locates one of a document's annotations by its external_ref,
// failing the test if it is not there.
func findScanRef(t *testing.T, db *Store, documentID int64, ref string) Element {
	t.Helper()
	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	for _, annotation := range annotations {
		if annotation.ExternalRef == ref {
			return annotation.Element
		}
	}
	t.Fatalf("no element with external_ref %q in document %d", ref, documentID)
	return Element{}
}

// hasScanRef reports whether a document currently has an annotation with the
// given external_ref, without failing the test either way.
func hasScanRef(t *testing.T, db *Store, documentID int64, ref string) bool {
	t.Helper()
	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	for _, annotation := range annotations {
		if annotation.ExternalRef == ref {
			return true
		}
	}
	return false
}

func TestPageScansMigrationCreatesTableAndColumn(t *testing.T) {
	db := testStore(t)

	var name string
	err := db.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'page_scans'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("page_scans table missing: %v", err)
	}

	rows, err := db.db.Query(`PRAGMA table_info(elements)`)
	if err != nil {
		t.Fatalf("table_info(elements): %v", err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var (
			cid              int
			colName, colType string
			notNull, pk      int
			dflt             any
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan column info: %v", err)
		}
		if colName == "note_pending" {
			found = true
		}
	}
	if !found {
		t.Errorf("elements.note_pending column missing")
	}
}

func TestCreateScannedBookCreatesSuspendedUpload(t *testing.T) {
	db := testStore(t)
	now := time.Now()

	docID, err := db.CreateScannedBook("A Paper Book", "Some Author", "A Subtitle", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}

	document, err := db.DocumentByID(docID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Source != SourceUpload {
		t.Errorf("source = %q, want %q", document.Source, SourceUpload)
	}
	if document.Title != "A Paper Book" || document.Author != "Some Author" || document.Subtitle != "A Subtitle" {
		t.Errorf("document = %+v", document)
	}

	// No body to read yet — the passages this book will eventually hold
	// don't exist until the worker has been through its photos — so the root
	// is suspended exactly like ImportAnnotations' own.
	root, err := db.RootElement(docID)
	if err != nil {
		t.Fatalf("RootElement: %v", err)
	}
	if root.Schedule.State != ir.StateSuspended {
		t.Errorf("root state = %q, want suspended", root.Schedule.State)
	}

	if _, err := db.CreateScannedBook("", "", "", now); err == nil {
		t.Errorf("CreateScannedBook with empty title = nil error, want an error")
	}
}

func TestAddPageScanMissingDocument(t *testing.T) {
	db := testStore(t)
	_, err := db.AddPageScan(999, "a.jpg", "image/jpeg", []byte("x"), true, time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("AddPageScan on missing document = %v, want ErrNotFound", err)
	}
}

func TestClaimPageScanOrdering(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Ordering Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}

	images := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	names := []string{"a.jpg", "b.jpg", "c.jpg"}
	var ids []int64
	for i := range images {
		id, err := db.AddPageScan(docID, names[i], "image/jpeg", images[i], true, now)
		if err != nil {
			t.Fatalf("AddPageScan: %v", err)
		}
		ids = append(ids, id)
	}

	for i, wantID := range ids {
		scan, image, ok, err := db.ClaimPageScan(now)
		if err != nil {
			t.Fatalf("ClaimPageScan %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("claim %d: ok = false, want true", i)
		}
		if scan.ID != wantID {
			t.Errorf("claim %d: id = %d, want %d (oldest first)", i, scan.ID, wantID)
		}
		if scan.Status != ScanProcessing {
			t.Errorf("claim %d: status = %q, want processing", i, scan.Status)
		}
		if scan.Attempts != 1 {
			t.Errorf("claim %d: attempts = %d, want 1", i, scan.Attempts)
		}
		if string(image) != string(images[i]) {
			t.Errorf("claim %d: image = %q, want %q", i, image, images[i])
		}
	}

	if _, _, ok, err := db.ClaimPageScan(now); err != nil || ok {
		t.Errorf("claim after exhausted: ok = %v, err = %v, want false, nil", ok, err)
	}
}

// TestClaimPageScanConcurrency covers the reason ClaimPageScan retries its
// select-then-update rather than trusting a single read: several worker
// goroutines racing to claim the same backlog must never end up processing
// the same photo twice.
func TestClaimPageScanConcurrency(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Concurrent Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}

	const total = 25
	for i := 0; i < total; i++ {
		if _, err := db.AddPageScan(docID, "p.jpg", "image/jpeg", []byte{byte(i)}, true, now); err != nil {
			t.Fatalf("AddPageScan: %v", err)
		}
	}

	var (
		mu      sync.Mutex
		claimed = map[int64]int{}
		wg      sync.WaitGroup
	)
	const workers = 8
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				scan, _, ok, err := db.ClaimPageScan(time.Now())
				if err != nil {
					t.Errorf("ClaimPageScan: %v", err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claimed[scan.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != total {
		t.Fatalf("claimed %d distinct scans, want %d", len(claimed), total)
	}
	for id, count := range claimed {
		if count != 1 {
			t.Errorf("scan %d claimed %d times, want exactly 1", id, count)
		}
	}
}

func TestFailPageScanRetryVsFailed(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Fail Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	id, err := db.AddPageScan(docID, "a.jpg", "image/jpeg", []byte("x"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	if err := db.FailPageScan(id, "model timed out", true, now); err != nil {
		t.Fatalf("FailPageScan (retry): %v", err)
	}
	scan, err := db.PageScan(id)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != ScanPending || scan.Error != "model timed out" {
		t.Errorf("after retry-fail: status = %q, error = %q", scan.Status, scan.Error)
	}

	if err := db.FailPageScan(id, "unreadable page", false, now); err != nil {
		t.Fatalf("FailPageScan (final): %v", err)
	}
	scan, err = db.PageScan(id)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != ScanFailed || scan.Error != "unreadable page" {
		t.Errorf("after final fail: status = %q, error = %q", scan.Status, scan.Error)
	}
}

func TestResetProcessingScans(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Reset Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	if _, err := db.AddPageScan(docID, "a.jpg", "image/jpeg", []byte("x"), true, now); err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	if _, err := db.AddPageScan(docID, "b.jpg", "image/jpeg", []byte("y"), true, now); err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	if _, _, ok, err := db.ClaimPageScan(now); err != nil || !ok {
		t.Fatalf("claim: ok = %v, err = %v", ok, err)
	}

	n, err := db.ResetProcessingScans(now)
	if err != nil {
		t.Fatalf("ResetProcessingScans: %v", err)
	}
	if n != 1 {
		t.Errorf("reset %d scans, want 1", n)
	}

	counts, err := db.CountPageScans(docID)
	if err != nil {
		t.Fatalf("CountPageScans: %v", err)
	}
	if counts.Pending != 2 || counts.Processing != 0 {
		t.Errorf("counts = %+v, want 2 pending, 0 processing", counts)
	}
}

func TestRetryPageScanFromFailedAndDone(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Retry Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}

	failedID, err := db.AddPageScan(docID, "a.jpg", "image/jpeg", []byte("x"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	if err := db.FailPageScan(failedID, "bad read", false, now); err != nil {
		t.Fatalf("FailPageScan: %v", err)
	}
	if err := db.RetryPageScan(failedID, now); err != nil {
		t.Fatalf("RetryPageScan (failed): %v", err)
	}
	scan, err := db.PageScan(failedID)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != ScanPending || scan.Attempts != 0 || scan.Error != "" {
		t.Errorf("after retry from failed: %+v", scan)
	}

	doneID, err := db.AddPageScan(docID, "b.jpg", "image/jpeg", []byte("y"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	if err := db.CompletePageScan(doneID, `{"page":"12"}`, now); err != nil {
		t.Fatalf("CompletePageScan: %v", err)
	}
	// done is a legitimate source state too: a re-read is also how the
	// reader asks the model to try again on a page it technically finished
	// but misread.
	if err := db.RetryPageScan(doneID, now); err != nil {
		t.Fatalf("RetryPageScan (done): %v", err)
	}
	scan, err = db.PageScan(doneID)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != ScanPending {
		t.Errorf("after retry from done: status = %q, want pending", scan.Status)
	}
}

func TestCountPageScans(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Count Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}

	if _, err := db.AddPageScan(docID, "a.jpg", "image/jpeg", []byte("x"), true, now); err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	if _, err := db.AddPageScan(docID, "b.jpg", "image/jpeg", []byte("y"), true, now); err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	if _, _, ok, err := db.ClaimPageScan(now); err != nil || !ok {
		t.Fatalf("claim: ok = %v, err = %v", ok, err)
	}

	doneID, err := db.AddPageScan(docID, "c.jpg", "image/jpeg", []byte("z"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	if err := db.CompletePageScan(doneID, "{}", now); err != nil {
		t.Fatalf("CompletePageScan: %v", err)
	}

	failedID, err := db.AddPageScan(docID, "d.jpg", "image/jpeg", []byte("w"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	if err := db.FailPageScan(failedID, "nope", false, now); err != nil {
		t.Fatalf("FailPageScan: %v", err)
	}

	counts, err := db.CountPageScans(docID)
	if err != nil {
		t.Fatalf("CountPageScans: %v", err)
	}
	if counts.Pending != 1 || counts.Processing != 1 || counts.Done != 1 || counts.Failed != 1 {
		t.Errorf("counts = %+v, want 1 of each", counts)
	}
	if counts.Total() != 4 {
		t.Errorf("Total() = %d, want 4", counts.Total())
	}
	if !counts.Busy() {
		t.Errorf("Busy() = false, want true (pending + processing > 0)")
	}
}

func TestPageScansOrderingAndFields(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("List Book", "Author", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}

	idA, err := db.AddPageScan(docID, "a.jpg", "image/jpeg", []byte("aaa"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	idB, err := db.AddPageScan(docID, "b.jpg", "image/png", []byte("bbb"), false, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	// PageScans lists metadata only — its return type, PageScan, carries no
	// image field at all, which is what keeps this query from ever being
	// able to select the blob by accident. There is nothing further to
	// assert about that beyond the fields below coming back right.
	scans, err := db.PageScans(docID)
	if err != nil {
		t.Fatalf("PageScans: %v", err)
	}
	if len(scans) != 2 {
		t.Fatalf("got %d scans, want 2", len(scans))
	}
	if scans[0].ID != idA || scans[1].ID != idB {
		t.Errorf("order = [%d, %d], want [%d, %d]", scans[0].ID, scans[1].ID, idA, idB)
	}
	if scans[0].Filename != "a.jpg" || scans[0].ContentType != "image/jpeg" || !scans[0].Triage {
		t.Errorf("scans[0] = %+v", scans[0])
	}
	if scans[1].Filename != "b.jpg" || scans[1].Triage {
		t.Errorf("scans[1] = %+v", scans[1])
	}
}

func TestApplyScanPassagesInsertTriageOn(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Triage Book", "Author", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	passages := []ScanPassage{{
		Ref: "scan:p1:0", Page: "1", Ordinal: 1, Text: "hello there",
		Chapter: "Chapter One", NotePending: true, ScanIDs: []int64{scanID},
	}}
	result, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now)
	if err != nil {
		t.Fatalf("ApplyScanPassages: %v", err)
	}
	if result.Inserted != 1 || result.Updated != 0 {
		t.Fatalf("result = %+v, want 1 inserted", result)
	}

	elem := findScanRef(t, db, docID, "scan:p1:0")
	if elem.Schedule.State != ir.StateSuspended {
		t.Errorf("state = %q, want suspended", elem.Schedule.State)
	}
	if elem.Triaged() {
		t.Errorf("triaged_at set, want NULL — triage mode parks for a triage pass")
	}
	if elem.Chapter != "Chapter One" {
		t.Errorf("chapter = %q, want Chapter One", elem.Chapter)
	}
	if !elem.NotePending {
		t.Errorf("note_pending = false, want true")
	}
	if elem.Quote != "hello there" || elem.Page != "1" || elem.Ordinal != 1 {
		t.Errorf("elem = %+v", elem)
	}
}

func TestApplyScanPassagesInsertTriageOff(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Queued Book", "Author", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), false, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	passages := []ScanPassage{{Ref: "scan:p1:0", Page: "1", Ordinal: 1, Text: "hello there", ScanIDs: []int64{scanID}}}
	result, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now)
	if err != nil {
		t.Fatalf("ApplyScanPassages: %v", err)
	}
	if result.Inserted != 1 {
		t.Fatalf("result = %+v, want 1 inserted", result)
	}

	elem := findScanRef(t, db, docID, "scan:p1:0")
	if elem.Schedule.State != ir.StateNew {
		t.Errorf("state = %q, want new", elem.Schedule.State)
	}
	if !elem.Triaged() {
		t.Errorf("triaged_at not set, want set — queuing outright is itself the decision")
	}
}

func TestApplyScanPassagesRepeatCallIsNoOp(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Repeat Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	passages := []ScanPassage{{Ref: "scan:p1:0", Page: "1", Ordinal: 1, Text: "same text", ScanIDs: []int64{scanID}}}
	if _, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("first ApplyScanPassages: %v", err)
	}

	result, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now)
	if err != nil {
		t.Fatalf("second ApplyScanPassages: %v", err)
	}
	if result.Inserted != 0 || result.Updated != 0 {
		t.Errorf("result = %+v, want a true no-op", result)
	}
}

func TestApplyScanPassagesUpdatePreservesReaderEdits(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Edited Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	passages := []ScanPassage{{Ref: "scan:p1:0", Page: "1", Ordinal: 1, Text: "original text", ScanIDs: []int64{scanID}}}
	if _, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("initial ApplyScanPassages: %v", err)
	}

	elem := findScanRef(t, db, docID, "scan:p1:0")
	if err := db.UpdateAnnotation(elem.ID, elem.Quote, "reader's own note", "Reader's Chapter", now); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}

	passages[0].Text = "corrected text"
	passages[0].Page = "2"
	passages[0].Ordinal = 5
	result, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now)
	if err != nil {
		t.Fatalf("second ApplyScanPassages: %v", err)
	}
	if result.Updated != 1 {
		t.Fatalf("result = %+v, want 1 updated", result)
	}

	elem = findScanRef(t, db, docID, "scan:p1:0")
	if elem.Quote != "corrected text" || elem.Page != "2" || elem.Ordinal != 5 {
		t.Errorf("elem after update = %+v", elem)
	}
	if elem.Chapter != "Reader's Chapter" || elem.Note != "reader's own note" {
		t.Errorf("reassembly clobbered a reader edit: chapter = %q, note = %q", elem.Chapter, elem.Note)
	}
}

func TestApplyScanPassagesSkipsUntouchedPassages(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Untouched Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	// This passage's own ScanIDs never mention scanID, so a call triggered
	// by scanID must leave it alone entirely.
	passages := []ScanPassage{{Ref: "scan:p9:0", Text: "not from this scan", ScanIDs: []int64{99999}}}
	result, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now)
	if err != nil {
		t.Fatalf("ApplyScanPassages: %v", err)
	}
	if result.Inserted != 0 || result.Updated != 0 {
		t.Errorf("result = %+v, want no-op", result)
	}
	if hasScanRef(t, db, docID, "scan:p9:0") {
		t.Errorf("passage was inserted despite fromScan not being in its ScanIDs")
	}
}

func TestApplyScanPassagesDeletionIsSticky(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Sticky Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanA, err := db.AddPageScan(docID, "a.jpg", "image/jpeg", []byte("a"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	scanC, err := db.AddPageScan(docID, "c.jpg", "image/jpeg", []byte("c"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	passage := ScanPassage{Ref: "scan:p5:0", Text: "will be deleted", ScanIDs: []int64{scanA}}
	if _, err := db.ApplyScanPassages(docID, scanA, []ScanPassage{passage}, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("initial ApplyScanPassages: %v", err)
	}
	elem := findScanRef(t, db, docID, "scan:p5:0")
	if err := db.DeleteExtract(elem.ID); err != nil {
		t.Fatalf("DeleteExtract: %v", err)
	}

	// A different photo of the same book finishes and triggers a re-run over
	// the very same reassembled passage list. scanC is not among this
	// passage's own ScanIDs, so rule 1 must leave it alone — the deletion
	// stays sticky.
	result, err := db.ApplyScanPassages(docID, scanC, []ScanPassage{passage}, ScanApplyOptions{}, now)
	if err != nil {
		t.Fatalf("second ApplyScanPassages: %v", err)
	}
	if result.Inserted != 0 {
		t.Errorf("result = %+v, want the deleted passage to stay gone", result)
	}
	if hasScanRef(t, db, docID, "scan:p5:0") {
		t.Errorf("deleted passage was resurrected by a call from an unrelated scan")
	}
}

func TestApplyScanPassagesAbsorbedRefs(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Absorb Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	// A fragment inserted on an earlier pass, before the assembler had the
	// photo after it and could see this belonged with another passage.
	frag := ScanPassage{Ref: "scan:p10:frag", Text: "fragment of a sentence", ScanIDs: []int64{scanID}}
	if _, err := db.ApplyScanPassages(docID, scanID, []ScanPassage{frag}, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("insert fragment: %v", err)
	}
	fragElem := findScanRef(t, db, docID, "scan:p10:frag")
	if err := db.UpdateAnnotation(fragElem.ID, fragElem.Quote, "a note on the fragment", "", now); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}

	// A second fragment that has grown a child of its own (a manual extract
	// pulled from it) and must survive being nominally "absorbed".
	frag2 := ScanPassage{Ref: "scan:p11:frag", Text: "another fragment", ScanIDs: []int64{scanID}}
	if _, err := db.ApplyScanPassages(docID, scanID, []ScanPassage{frag2}, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("insert fragment2: %v", err)
	}
	frag2Elem := findScanRef(t, db, docID, "scan:p11:frag")
	if _, err := db.CreateExtract(NewExtract{
		ParentID: frag2Elem.ID, DocumentID: docID, Quote: "a child extract",
	}, now); err != nil {
		t.Fatalf("CreateExtract: %v", err)
	}

	merged := ScanPassage{
		Ref:          "scan:p10:0",
		Text:         "fragment of a sentence, continued",
		AbsorbedRefs: []string{"scan:p10:frag", "scan:p11:frag"},
		ScanIDs:      []int64{scanID},
	}
	result, err := db.ApplyScanPassages(docID, scanID, []ScanPassage{merged}, ScanApplyOptions{}, now)
	if err != nil {
		t.Fatalf("ApplyScanPassages merge: %v", err)
	}
	if result.Inserted != 1 {
		t.Errorf("result = %+v, want the merged passage inserted", result)
	}
	if result.Removed != 1 {
		t.Errorf("result = %+v, want exactly one absorbed ref removed (the childless one)", result)
	}

	if hasScanRef(t, db, docID, "scan:p10:frag") {
		t.Errorf("childless absorbed ref should have been removed")
	}
	if !hasScanRef(t, db, docID, "scan:p11:frag") {
		t.Errorf("absorbed ref with a child was removed, should have been left standing")
	}

	mergedElem := findScanRef(t, db, docID, "scan:p10:0")
	if mergedElem.Note != "a note on the fragment" {
		t.Errorf("note = %q, want the note carried across from the absorbed fragment", mergedElem.Note)
	}
}

func TestRenameScanRefs(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Rename Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	passages := []ScanPassage{
		{Ref: "scan:s12.0:m0", Text: "first", ScanIDs: []int64{scanID}},
		{Ref: "scan:s12.1:m0", Text: "second", ScanIDs: []int64{scanID}},
	}
	if _, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages: %v", err)
	}

	// Same-looking ref in a different document, to check the rename does not
	// leak across documents.
	otherDocID, err := db.CreateScannedBook("Other Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	otherScanID, err := db.AddPageScan(otherDocID, "q1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	otherPassage := []ScanPassage{{Ref: "scan:s12.0:m0", Text: "unrelated", ScanIDs: []int64{otherScanID}}}
	if _, err := db.ApplyScanPassages(otherDocID, otherScanID, otherPassage, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages (other doc): %v", err)
	}

	n, err := db.RenameScanRefs(docID, "scan:s12.0:", "scan:p94:", now)
	if err != nil {
		t.Fatalf("RenameScanRefs: %v", err)
	}
	if n != 1 {
		t.Fatalf("renamed %d refs, want 1", n)
	}

	if !hasScanRef(t, db, docID, "scan:p94:m0") {
		t.Errorf("matching ref was not renamed")
	}
	if hasScanRef(t, db, docID, "scan:s12.0:m0") {
		t.Errorf("old ref still present after rename")
	}
	if !hasScanRef(t, db, docID, "scan:s12.1:m0") {
		t.Errorf("non-matching ref in the same document was touched")
	}
	if !hasScanRef(t, db, otherDocID, "scan:s12.0:m0") {
		t.Errorf("rename leaked into a different document")
	}
}

func TestScanElementChapters(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Chapters Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	passages := []ScanPassage{
		{Ref: "scan:p1:0", Text: "intro text", Chapter: "Introduction", ScanIDs: []int64{scanID}},
		{Ref: "scan:p2:0", Text: "chapter one text", Chapter: "Chapter One", ScanIDs: []int64{scanID}},
	}
	if _, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages: %v", err)
	}

	// A non-scan highlight merged into the same document, to check it is
	// excluded from the map.
	if _, err := db.ImportAnnotations(book(), ImportOptions{IntoDocumentID: docID}, now); err != nil {
		t.Fatalf("ImportAnnotations: %v", err)
	}

	chapters, err := db.ScanElementChapters(docID)
	if err != nil {
		t.Fatalf("ScanElementChapters: %v", err)
	}
	want := map[string]string{"scan:p1:0": "Introduction", "scan:p2:0": "Chapter One"}
	if len(chapters) != len(want) {
		t.Fatalf("chapters = %+v, want %+v", chapters, want)
	}
	for ref, chapter := range want {
		if chapters[ref] != chapter {
			t.Errorf("chapters[%q] = %q, want %q", ref, chapters[ref], chapter)
		}
	}
}

func TestDismissNotePending(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Dismiss Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	passages := []ScanPassage{{Ref: "scan:p1:0", Text: "handwritten note nearby", NotePending: true, ScanIDs: []int64{scanID}}}
	if _, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages: %v", err)
	}
	elem := findScanRef(t, db, docID, "scan:p1:0")
	if !elem.NotePending {
		t.Fatalf("setup: note_pending not set")
	}

	if err := db.DismissNotePending(elem.ID); err != nil {
		t.Fatalf("DismissNotePending: %v", err)
	}
	elem = findScanRef(t, db, docID, "scan:p1:0")
	if elem.NotePending {
		t.Errorf("note_pending still set after dismiss")
	}
}

func TestUpdateAnnotationClearsNotePending(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Note Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	passages := []ScanPassage{{Ref: "scan:p1:0", Text: "handwriting nearby", NotePending: true, ScanIDs: []int64{scanID}}}
	if _, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages: %v", err)
	}
	elem := findScanRef(t, db, docID, "scan:p1:0")

	// An edit that leaves the note blank is not the same act as dismissing
	// the nudge outright — see UpdateAnnotation's own guard.
	if err := db.UpdateAnnotation(elem.ID, elem.Quote, "", elem.Chapter, now); err != nil {
		t.Fatalf("UpdateAnnotation (blank note): %v", err)
	}
	elem = findScanRef(t, db, docID, "scan:p1:0")
	if !elem.NotePending {
		t.Errorf("note_pending cleared by an edit that left the note blank")
	}

	if err := db.UpdateAnnotation(elem.ID, elem.Quote, "here it is", elem.Chapter, now); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}
	elem = findScanRef(t, db, docID, "scan:p1:0")
	if elem.NotePending {
		t.Errorf("note_pending still set after typing in a note")
	}
	if elem.Note != "here it is" {
		t.Errorf("note = %q", elem.Note)
	}
}
