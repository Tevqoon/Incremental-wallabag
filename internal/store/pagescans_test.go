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

	if err := db.FailPageScan(id, "model timed out", true, time.Time{}, now); err != nil {
		t.Fatalf("FailPageScan (retry): %v", err)
	}
	scan, err := db.PageScan(id)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != ScanPending || scan.Error != "model timed out" {
		t.Errorf("after retry-fail: status = %q, error = %q", scan.Status, scan.Error)
	}

	if err := db.FailPageScan(id, "unreadable page", false, time.Time{}, now); err != nil {
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

func TestRetryPageScanOnlyFromFailed(t *testing.T) {
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
	if err := db.FailPageScan(failedID, "bad read", false, time.Time{}, now); err != nil {
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
	if err := db.CompletePageScan(doneID, `{"pages":[]}`, now); err != nil {
		t.Fatalf("CompletePageScan: %v", err)
	}
	// A read photo has been discarded, so there is nothing left to send.
	if err := db.RetryPageScan(doneID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RetryPageScan (done): err = %v, want ErrNotFound", err)
	}
}

func TestCompletePageScanDiscardsThePhoto(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Discard Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	id, err := db.AddPageScan(docID, "a.jpg", "image/jpeg", []byte("several megabytes of photo"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	if err := db.CompletePageScan(id, `{"pages":[]}`, now); err != nil {
		t.Fatalf("CompletePageScan: %v", err)
	}
	_, _, data, err := db.PageScanImage(id)
	if err != nil {
		t.Fatalf("PageScanImage: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("a read photo still holds %d bytes; it should be discarded", len(data))
	}
	scan, err := db.PageScan(id)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != ScanDone || scan.Result != `{"pages":[]}` {
		t.Errorf("the model's answer must survive the photo: %+v", scan)
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
	if err := db.FailPageScan(failedID, "nope", false, time.Time{}, now); err != nil {
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

// TestClaimPageScanHonoursRetryAfter: a scan put back to pending with a
// retryAt in the future is not claimable until then, so a backoff holds
// against every worker goroutine and not just the one that saw the failure.
func TestClaimPageScanHonoursRetryAfter(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Backoff Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	id, err := db.AddPageScan(docID, "a.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	if _, _, ok, err := db.ClaimPageScan(now); err != nil || !ok {
		t.Fatalf("first claim: ok = %v, err = %v", ok, err)
	}
	if err := db.FailPageScan(id, "rate limited", true, now.Add(time.Minute), now); err != nil {
		t.Fatalf("FailPageScan: %v", err)
	}

	if _, _, ok, err := db.ClaimPageScan(now.Add(30 * time.Second)); err != nil || ok {
		t.Errorf("claim before retryAt: ok = %v, err = %v, want false, nil", ok, err)
	}
	scan, _, ok, err := db.ClaimPageScan(now.Add(2 * time.Minute))
	if err != nil || !ok {
		t.Fatalf("claim after retryAt: ok = %v, err = %v, want true, nil", ok, err)
	}
	if scan.ID != id || scan.Attempts != 2 {
		t.Errorf("claimed %+v, want scan %d on its second attempt", scan, id)
	}
}

func TestRetryFailedPageScans(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Retry All Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	var ids []int64
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		id, err := db.AddPageScan(docID, name, "image/jpeg", []byte("img"), true, now)
		if err != nil {
			t.Fatalf("AddPageScan: %v", err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids[:2] {
		if err := db.FailPageScan(id, "outage", false, time.Time{}, now); err != nil {
			t.Fatalf("FailPageScan: %v", err)
		}
	}

	n, err := db.RetryFailedPageScans(docID, now)
	if err != nil {
		t.Fatalf("RetryFailedPageScans: %v", err)
	}
	if n != 2 {
		t.Errorf("retried %d, want 2", n)
	}
	counts, err := db.CountPageScans(docID)
	if err != nil {
		t.Fatalf("CountPageScans: %v", err)
	}
	if counts.Pending != 3 || counts.Failed != 0 {
		t.Errorf("counts = %+v, want all 3 pending", counts)
	}
}

// TestUnappliedPageScans: a scan the model has read but whose passages were
// never written is listed until ApplyScanPassages runs for it — the photo is
// gone by then, so this is the only record that the page still needs doing.
func TestUnappliedPageScans(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Unapplied Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	id, err := db.AddPageScan(docID, "a.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	unapplied, err := db.UnappliedPageScans()
	if err != nil {
		t.Fatalf("UnappliedPageScans: %v", err)
	}
	if len(unapplied) != 0 {
		t.Errorf("a pending scan is listed as unapplied: %+v", unapplied)
	}

	if err := db.CompletePageScan(id, `{"pages":[]}`, now); err != nil {
		t.Fatalf("CompletePageScan: %v", err)
	}
	unapplied, err = db.UnappliedPageScans()
	if err != nil {
		t.Fatalf("UnappliedPageScans: %v", err)
	}
	if len(unapplied) != 1 || unapplied[0].ID != id || unapplied[0].DocumentID != docID {
		t.Fatalf("unapplied = %+v, want just scan %d", unapplied, id)
	}

	if _, err := db.ApplyScanPassages(docID, id, nil, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages: %v", err)
	}
	unapplied, err = db.UnappliedPageScans()
	if err != nil {
		t.Fatalf("UnappliedPageScans: %v", err)
	}
	if len(unapplied) != 0 {
		t.Errorf("unapplied after ApplyScanPassages = %+v, want none", unapplied)
	}
}

// TestApplyScanPassagesRetakeRemovesMarksItNoLongerSees: a retake of a page
// replaces the earlier reading of it, so a passage only the earlier shot
// produced goes — unless the reader has made it their own since.
func TestApplyScanPassagesRetakeRemovesMarksItNoLongerSees(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Retake Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	first, err := db.AddPageScan(docID, "blurry.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	retake, err := db.AddPageScan(docID, "sharp.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	blurry := []ScanPassage{
		{Ref: "scan:p94:m0", Page: "94", Text: "First passage.", ScanIDs: []int64{first}},
		{Ref: "scan:p94:m1", Page: "94", Text: "A smudge read as a mark.", ScanIDs: []int64{first}},
		{Ref: "scan:p94:m2", Page: "94", Text: "Another smudge.", ScanIDs: []int64{first}},
		{Ref: "scan:p940:m5", Page: "940", Text: "A different page.", ScanIDs: []int64{first}},
	}
	if _, err := db.ApplyScanPassages(docID, first, blurry, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages (first shot): %v", err)
	}
	noted := findScanRef(t, db, docID, "scan:p94:m2")
	note := "keep me"
	if err := db.EditAnnotation(noted.ID, AnnotationEdit{Note: &note}, now); err != nil {
		t.Fatalf("EditAnnotation: %v", err)
	}

	sharp := []ScanPassage{
		{Ref: "scan:p94:m0", Page: "94", Text: "First passage.", ScanIDs: []int64{retake}},
		{Ref: "scan:p940:m5", Page: "940", Text: "A different page.", ScanIDs: []int64{first}},
	}
	result, err := db.ApplyScanPassages(docID, retake, sharp,
		ScanApplyOptions{PagePrefixes: []string{"scan:p94"}}, now)
	if err != nil {
		t.Fatalf("ApplyScanPassages (retake): %v", err)
	}
	if result.Removed != 1 {
		t.Errorf("result = %+v, want exactly the un-noted stale mark removed", result)
	}
	if !hasScanRef(t, db, docID, "scan:p94:m0") {
		t.Errorf("a passage the retake still reads was removed")
	}
	if hasScanRef(t, db, docID, "scan:p94:m1") {
		t.Errorf("a mark only the blurry shot saw survived the retake")
	}
	if !hasScanRef(t, db, docID, "scan:p94:m2") {
		t.Errorf("a passage the reader had written a note on was removed")
	}
	if !hasScanRef(t, db, docID, "scan:p940:m5") {
		t.Errorf("a passage on page 940 was removed as though it were on page 94")
	}
}

// TestApplyScanPassagesFlagsHandwritingOnTextThatGrew: the second half of a
// page-turn sentence, read after the first half was saved, brings its
// handwriting flag with it — but a re-run that changes nothing never
// re-raises a flag the reader dismissed.
func TestApplyScanPassagesFlagsHandwritingOnTextThatGrew(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Page Turn Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	p140, err := db.AddPageScan(docID, "140.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	p141, err := db.AddPageScan(docID, "141.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	alone := []ScanPassage{{Ref: "scan:p140:m0", Page: "140", Text: "The first half.", ScanIDs: []int64{p140}}}
	if _, err := db.ApplyScanPassages(docID, p140, alone, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages (140): %v", err)
	}
	if findScanRef(t, db, docID, "scan:p140:m0").NotePending {
		t.Fatalf("note_pending set with no handwriting yet")
	}

	joined := []ScanPassage{{
		Ref: "scan:p140:m0", Page: "140–141", Text: "The first half and the second half.",
		NotePending: true, ScanIDs: []int64{p140, p141},
	}}
	if _, err := db.ApplyScanPassages(docID, p141, joined, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages (141): %v", err)
	}
	element := findScanRef(t, db, docID, "scan:p140:m0")
	if !element.NotePending {
		t.Fatalf("note_pending not raised for handwriting beside the second half")
	}

	if err := db.DismissNotePending(element.ID); err != nil {
		t.Fatalf("DismissNotePending: %v", err)
	}
	if _, err := db.ApplyScanPassages(docID, p141, joined, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages (re-run): %v", err)
	}
	if findScanRef(t, db, docID, "scan:p140:m0").NotePending {
		t.Errorf("a re-run with unchanged text re-raised a dismissed flag")
	}
}

// TestRenameScanRefsSkipsATakenRef: typing in a page number another photo
// already supplied must not trip the unique index and fail the correction
// halfway — the clashing row keeps its old ref.
func TestRenameScanRefsSkipsATakenRef(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Clash Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	passages := []ScanPassage{
		{Ref: "scan:p94:m0", Text: "already page 94", ScanIDs: []int64{scanID}},
		{Ref: "scan:s12.0:m0", Text: "unnumbered first", ScanIDs: []int64{scanID}},
		{Ref: "scan:s12.0:m1", Text: "unnumbered second", ScanIDs: []int64{scanID}},
	}
	if _, err := db.ApplyScanPassages(docID, scanID, passages, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages: %v", err)
	}

	n, err := db.RenameScanRefs(docID, "scan:s12.0:", "scan:p94:", now)
	if err != nil {
		t.Fatalf("RenameScanRefs: %v", err)
	}
	if n != 1 {
		t.Errorf("renamed %d, want 1 (m1; m0's new ref is taken)", n)
	}
	if !hasScanRef(t, db, docID, "scan:p94:m1") || !hasScanRef(t, db, docID, "scan:s12.0:m0") {
		t.Errorf("want m1 renamed and m0 left under its old ref")
	}
}

// TestApplyScanPassagesRetakeKeepsPassagesTheReaderActedOn covers everything
// besides a note that makes a passage the reader's own. Deleting an element
// cascades into cloze_ranges and activity_log, so a retake guard that checked
// only notes, corrected wording and children would have silently taken a
// passage's cloze cards and review history with it, along with a passage the
// reader had already decided to keep in triage.
func TestApplyScanPassagesRetakeKeepsPassagesTheReaderActedOn(t *testing.T) {
	db := testStore(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Retake Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	first, err := db.AddPageScan(docID, "blurry.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	retake, err := db.AddPageScan(docID, "sharp.jpg", "image/jpeg", []byte("img"), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	blurry := []ScanPassage{
		{Ref: "scan:p94:m0", Page: "94", Text: "First passage.", ScanIDs: []int64{first}},
		{Ref: "scan:p94:m1", Page: "94", Text: "A passage with a cloze on it.", ScanIDs: []int64{first}},
		{Ref: "scan:p94:m2", Page: "94", Text: "A passage already reviewed.", ScanIDs: []int64{first}},
		{Ref: "scan:p94:m3", Page: "94", Text: "A passage kept in triage.", ScanIDs: []int64{first}},
		{Ref: "scan:p94:m4", Page: "94", Text: "A smudge nobody touched.", ScanIDs: []int64{first}},
	}
	if _, err := db.ApplyScanPassages(docID, first, blurry, ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages (first shot): %v", err)
	}

	clozed := findScanRef(t, db, docID, "scan:p94:m1")
	if _, err := db.AddCloze(clozed.ID, 2, 9, ""); err != nil {
		t.Fatalf("AddCloze: %v", err)
	}
	reviewed := findScanRef(t, db, docID, "scan:p94:m2")
	if err := db.SaveScheduleReviewed(reviewed.ID, reviewed.Schedule, now.Add(time.Minute)); err != nil {
		t.Fatalf("SaveScheduleReviewed: %v", err)
	}
	// Later than the import, as a real decision always is: an import queued
	// outright stamps triaged_at at the same instant as created_at, and that
	// is not the reader deciding anything.
	decided := findScanRef(t, db, docID, "scan:p94:m3")
	if err := db.MarkTriaged(decided.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkTriaged: %v", err)
	}

	sharp := []ScanPassage{
		{Ref: "scan:p94:m0", Page: "94", Text: "First passage.", ScanIDs: []int64{retake}},
	}
	result, err := db.ApplyScanPassages(docID, retake, sharp,
		ScanApplyOptions{PagePrefixes: []string{"scan:p94"}}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ApplyScanPassages (retake): %v", err)
	}
	if result.Removed != 1 {
		t.Errorf("removed %d, want only the untouched smudge", result.Removed)
	}
	for ref, why := range map[string]string{
		"scan:p94:m1": "it had a cloze on it",
		"scan:p94:m2": "it had already been reviewed",
		"scan:p94:m3": "the reader had kept it in triage",
	} {
		if !hasScanRef(t, db, docID, ref) {
			t.Errorf("%s was removed by the retake though %s", ref, why)
		}
	}
	if hasScanRef(t, db, docID, "scan:p94:m4") {
		t.Error("an untouched passage the retake no longer reads survived it")
	}
	if clozes, err := db.ClozesOf(clozed.ID); err != nil || len(clozes) != 1 {
		t.Errorf("the cloze did not survive the retake: %v, %v", clozes, err)
	}
}
