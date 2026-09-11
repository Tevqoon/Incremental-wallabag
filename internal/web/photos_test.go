package web

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/pagescan"
	"github.com/Tevqoon/increader/internal/store"
)

// scanCalls records what a test server's WakeScanner and ReassembleScans
// closures were called with. Tests in this file are single-threaded (one
// request at a time), so this needs no locking of its own.
type scanCalls struct {
	wakes       int
	reassembled [][2]int64
}

// newTestServerWithScans builds a server the same way newTestServer does,
// but with WakeScanner and ReassembleScans wired to counting/recording
// fakes — the photo importer, the retry button and the label route are all
// hidden (see Server.wakeScanner) without them.
func newTestServerWithScans(t *testing.T) (*Server, *store.Store, *scanCalls) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	calls := &scanCalls{}
	server, err := New(Options{
		Store:  db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		WakeScanner: func() {
			calls.wakes++
		},
		ReassembleScans: func(documentID, scanID int64) error {
			calls.reassembled = append(calls.reassembled, [2]int64{documentID, scanID})
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server, db, calls
}

// tinyPNG and tinyJPEG build minimal, genuinely valid images — small enough
// to keep the test fast, real enough that pagescan.SniffImageType's own
// sniffing (which reads the actual bytes, not a declared content type)
// recognises them the way a real phone photo would be recognised.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// photoFile is one file to post under the "photos" field — a slice rather
// than a map, so upload order (which AddPageScan preserves as the passages'
// own reading order) is deterministic.
type photoFile struct {
	filename string
	data     []byte
}

// postPhotos posts a batch of photos as multipart/form-data under the
// "photos" field, the shape /import/photos and the contents page's own "add
// more photos" form both send. Modelled on postFile in import_test.go, the
// single-file counterpart.
func postPhotos(t *testing.T, server *Server, path string, photos []photoFile, fields url.Values) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, values := range fields {
		for _, value := range values {
			if err := writer.WriteField(name, value); err != nil {
				t.Fatalf("write field %s: %v", name, err)
			}
		}
	}
	for _, photo := range photos {
		part, err := writer.CreateFormFile("photos", photo.filename)
		if err != nil {
			t.Fatalf("create form file %s: %v", photo.filename, err)
		}
		if _, err := part.Write(photo.data); err != nil {
			t.Fatalf("write form file %s: %v", photo.filename, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, path, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// documentIDFromScansRedirect reads the document id out of a "/documents/{id}#scans"
// redirect, the shape every photo-batch handler in photos.go redirects to.
func documentIDFromScansRedirect(t *testing.T, response *httptest.ResponseRecorder) int64 {
	t.Helper()
	location := response.Header().Get("Location")
	idStr := strings.TrimSuffix(strings.TrimPrefix(location, "/documents/"), "#scans")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		t.Fatalf("redirected to %q, want a document page with a #scans fragment", location)
	}
	return id
}

// unnumberedPageResult, labeledPageOneResult and labeledPageFourteenResult
// are hand-written model responses — a page with one line and one mark, so
// pagescan.Assemble produces exactly one passage from each. page_label is
// spelled out here (null or a printed number) the way the model's own JSON
// actually carries it, rather than built through Go structs, so these fixtures
// read the same shape a real response has.
const unnumberedPageResult = `{"pages":[{"page_label":null,"lines":["A printed line."],"marks":[{"first_line":1,"last_line":1,"text":"A printed line."}]}]}`
const labeledPageOneResult = `{"pages":[{"page_label":"1","lines":["A printed line."],"marks":[{"first_line":1,"last_line":1,"text":"A printed line."}]}]}`
const labeledPageFourteenResult = `{"pages":[{"page_label":"14","lines":["Another line."],"marks":[{"first_line":1,"last_line":1,"text":"Another line."}]}]}`

// handwritingPageResult is a done page carrying a mark flagged as having
// handwriting nearby — the shape that produces a NotePending passage, for
// the margin-note tests.
const handwritingPageResult = `{"pages":[{"page_label":"12","lines":["Original line."],"marks":[{"first_line":1,"last_line":1,"text":"Original line.","has_handwriting":true}]}]}`

// completeAndAssembleScan marks a scan done with rawResult and folds its
// assembled passages into the elements table — the two store calls
// production code makes from internal/scanworker, which this package (web)
// never calls directly. Tests build the element state a handler needs to
// observe this way instead.
func completeAndAssembleScan(t *testing.T, db *store.Store, documentID, scanID int64, rawResult string, now time.Time) {
	t.Helper()
	if err := db.CompletePageScan(scanID, rawResult, now); err != nil {
		t.Fatalf("CompletePageScan: %v", err)
	}
	result, err := pagescan.ParseResult(rawResult)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	passages := pagescan.Assemble([]pagescan.Scan{{ID: scanID, Result: result}}, nil)
	storePassages := make([]store.ScanPassage, len(passages))
	for i, p := range passages {
		storePassages[i] = store.ScanPassage{
			Ref: p.Ref, AbsorbedRefs: p.AbsorbedRefs, Page: p.Page, Ordinal: p.Ordinal,
			Text: p.Text, NotePending: p.NotePending, Chapter: p.Chapter, ScanIDs: p.ScanIDs,
		}
	}
	if _, err := db.ApplyScanPassages(documentID, scanID, storePassages, store.ScanApplyOptions{}, now); err != nil {
		t.Fatalf("ApplyScanPassages: %v", err)
	}
}

// findElementByRef and hasElementRef locate an annotation by its
// external_ref, the same way pagescans_test.go's own findScanRef/hasScanRef
// do for the store package's tests.
func findElementByRef(t *testing.T, db *store.Store, documentID int64, ref string) store.Element {
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
	return store.Element{}
}

func hasElementRef(t *testing.T, db *store.Store, documentID int64, ref string) bool {
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

// ---- import page --------------------------------------------------------

func TestPhotosSectionHiddenWithoutScanner(t *testing.T) {
	server, _, _ := newTestServer(t, false)

	body := get(t, server, "/import").Body.String()
	if strings.Contains(body, "Photos of a paper book") {
		t.Error("the photos section shows even though no scanner is configured")
	}
}

func TestPhotosSectionShownWithScanner(t *testing.T) {
	server, _, _ := newTestServerWithScans(t)

	body := get(t, server, "/import").Body.String()
	if !strings.Contains(body, "Photos of a paper book") {
		t.Error("the photos section does not show even though a scanner is configured")
	}
	if !strings.Contains(body, `name="photos"`) || !strings.Contains(body, `multiple`) {
		t.Error("the photos form has no multi-file input")
	}
	if !strings.Contains(body, `enctype="multipart/form-data"`) {
		t.Error("the photos form is not multipart, so no file would ever arrive")
	}
}

// ---- upload --------------------------------------------------------------

func TestImportPhotosCreatesBookWithPendingScansInOrder(t *testing.T) {
	server, db, calls := newTestServerWithScans(t)

	photos := []photoFile{
		{filename: "a.png", data: tinyPNG(t)},
		{filename: "b.jpg", data: tinyJPEG(t)},
	}
	response := postPhotos(t, server, "/import/photos", photos, url.Values{
		"title": {"A Paper Book"}, "author": {"Someone"},
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", response.Code, response.Body.String())
	}
	id := documentIDFromScansRedirect(t, response)

	document, err := db.DocumentByID(id)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if document.Title != "A Paper Book" || document.Author != "Someone" {
		t.Errorf("document = %+v", document)
	}
	if document.Source != store.SourceUpload {
		t.Errorf("source = %q, want %q", document.Source, store.SourceUpload)
	}

	scans, err := db.PageScans(id)
	if err != nil {
		t.Fatalf("PageScans: %v", err)
	}
	if len(scans) != 2 {
		t.Fatalf("got %d scans, want 2", len(scans))
	}
	// The declared Content-Type of a CreateFormFile part is always
	// application/octet-stream, so a correct content type here can only
	// have come from sniffing the bytes, not from what was declared.
	if scans[0].Filename != "a.png" || scans[0].ContentType != "image/png" {
		t.Errorf("scans[0] = %+v, want a.png sniffed as image/png", scans[0])
	}
	if scans[1].Filename != "b.jpg" || scans[1].ContentType != "image/jpeg" {
		t.Errorf("scans[1] = %+v, want b.jpg sniffed as image/jpeg", scans[1])
	}
	for _, scan := range scans {
		if scan.Status != store.ScanPending {
			t.Errorf("scan %d status = %q, want pending", scan.ID, scan.Status)
		}
		if !scan.Triage {
			t.Errorf("scan %d triage = false, want true (the default)", scan.ID)
		}
	}

	if calls.wakes != 1 {
		t.Errorf("WakeScanner called %d times, want 1", calls.wakes)
	}
}

func TestImportPhotosQueueModeSetsTriageFalse(t *testing.T) {
	server, db, _ := newTestServerWithScans(t)

	response := postPhotos(t, server, "/import/photos",
		[]photoFile{{filename: "a.png", data: tinyPNG(t)}},
		url.Values{"title": {"Queued Book"}, "mode": {"queue"}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", response.Code, response.Body.String())
	}
	id := documentIDFromScansRedirect(t, response)

	scans, err := db.PageScans(id)
	if err != nil {
		t.Fatalf("PageScans: %v", err)
	}
	if len(scans) != 1 || scans[0].Triage {
		t.Errorf("scans = %+v, want one scan with triage false", scans)
	}
}

func TestImportPhotosIntoExistingDocument(t *testing.T) {
	server, db, _ := newTestServerWithScans(t)

	first := postPhotos(t, server, "/import/photos",
		[]photoFile{{filename: "a.png", data: tinyPNG(t)}}, url.Values{"title": {"Staged Book"}})
	id := documentIDFromScansRedirect(t, first)

	second := postPhotos(t, server, "/import/photos",
		[]photoFile{{filename: "b.png", data: tinyPNG(t)}},
		url.Values{"into": {strconv.FormatInt(id, 10)}})
	if second.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", second.Code, second.Body.String())
	}
	if secondID := documentIDFromScansRedirect(t, second); secondID != id {
		t.Fatalf("second upload created document %d, want it added to %d", secondID, id)
	}

	scans, err := db.PageScans(id)
	if err != nil {
		t.Fatalf("PageScans: %v", err)
	}
	if len(scans) != 2 {
		t.Errorf("got %d scans, want 2 after adding to the existing book", len(scans))
	}
}

func TestImportPhotosIntoMissingDocumentIs400(t *testing.T) {
	server, _, _ := newTestServerWithScans(t)

	response := postPhotos(t, server, "/import/photos",
		[]photoFile{{filename: "a.png", data: tinyPNG(t)}}, url.Values{"into": {"999"}})
	if response.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a missing document", response.Code)
	}
}

func TestImportPhotosRejectsWholeBatchOnANonImage(t *testing.T) {
	server, db, calls := newTestServerWithScans(t)

	response := postPhotos(t, server, "/import/photos", []photoFile{
		{filename: "a.png", data: tinyPNG(t)},
		{filename: "notes.txt", data: []byte("just some notes")},
	}, url.Values{"title": {"Rejected Book"}})
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, want the form again with an error", response.Code)
	}
	if !strings.Contains(response.Body.String(), "could not be used") {
		t.Error("no explanation was shown for the rejected batch")
	}

	documents, err := db.UploadedDocuments()
	if err != nil {
		t.Fatalf("UploadedDocuments: %v", err)
	}
	if len(documents) != 0 {
		t.Errorf("a document was created despite the batch being rejected: %+v", documents)
	}
	if calls.wakes != 0 {
		t.Errorf("WakeScanner called %d times, want 0", calls.wakes)
	}
}

func TestImportPhotosRequiresATitleForANewBook(t *testing.T) {
	server, _, calls := newTestServerWithScans(t)

	response := postPhotos(t, server, "/import/photos",
		[]photoFile{{filename: "a.png", data: tinyPNG(t)}}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, want the form again", response.Code)
	}
	if !strings.Contains(response.Body.String(), "Give the book a title") {
		t.Error("no explanation was shown for the missing title")
	}
	if calls.wakes != 0 {
		t.Errorf("WakeScanner called %d times, want 0", calls.wakes)
	}
}

func TestImportPhotosRequiresAtLeastOnePhoto(t *testing.T) {
	server, _, _ := newTestServerWithScans(t)

	response := postPhotos(t, server, "/import/photos", nil, url.Values{"title": {"Empty Book"}})
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, want the form again", response.Code)
	}
	if !strings.Contains(response.Body.String(), "Choose at least one photo") {
		t.Error("no explanation was shown for an empty batch")
	}
}

// ---- image route -----------------------------------------------------------

func TestScanImageServesBytesAndScopesToItsDocument(t *testing.T) {
	server, db, _ := newTestServerWithScans(t)

	png := tinyPNG(t)
	response := postPhotos(t, server, "/import/photos",
		[]photoFile{{filename: "a.png", data: png}}, url.Values{"title": {"Image Book"}})
	id := documentIDFromScansRedirect(t, response)
	scans, err := db.PageScans(id)
	if err != nil {
		t.Fatalf("PageScans: %v", err)
	}
	scanID := scans[0].ID

	img := get(t, server, "/documents/"+strconv.FormatInt(id, 10)+"/scans/"+strconv.FormatInt(scanID, 10)+"/image")
	if img.Code != http.StatusOK {
		t.Fatalf("status %d", img.Code)
	}
	if img.Header().Get("Content-Type") != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", img.Header().Get("Content-Type"))
	}
	if !bytes.Equal(img.Body.Bytes(), png) {
		t.Error("served bytes do not match the uploaded photo")
	}
	if !strings.Contains(img.Header().Get("Cache-Control"), "max-age=86400") {
		t.Errorf("Cache-Control = %q", img.Header().Get("Cache-Control"))
	}

	other := postPhotos(t, server, "/import/photos",
		[]photoFile{{filename: "b.png", data: tinyPNG(t)}}, url.Values{"title": {"Other Book"}})
	otherID := documentIDFromScansRedirect(t, other)

	wrongDoc := get(t, server, "/documents/"+strconv.FormatInt(otherID, 10)+"/scans/"+strconv.FormatInt(scanID, 10)+"/image")
	if wrongDoc.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404 for a scan belonging to a different document", wrongDoc.Code)
	}
}

// ---- retry -----------------------------------------------------------------

func TestRetryScanResetsAFailedScanAndWakes(t *testing.T) {
	server, db, calls := newTestServerWithScans(t)

	response := postPhotos(t, server, "/import/photos",
		[]photoFile{{filename: "a.png", data: tinyPNG(t)}}, url.Values{"title": {"Retry Book"}})
	id := documentIDFromScansRedirect(t, response)
	scans, err := db.PageScans(id)
	if err != nil {
		t.Fatalf("PageScans: %v", err)
	}
	scanID := scans[0].ID

	if err := db.FailPageScan(scanID, "model timed out", false, time.Now()); err != nil {
		t.Fatalf("FailPageScan: %v", err)
	}
	calls.wakes = 0 // ignore the upload's own wake

	retry := post(t, server, "/documents/"+strconv.FormatInt(id, 10)+"/scans/"+strconv.FormatInt(scanID, 10)+"/retry", nil)
	if retry.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", retry.Code, retry.Body.String())
	}
	if got := retry.Header().Get("Location"); got != "/documents/"+strconv.FormatInt(id, 10)+"#scans" {
		t.Errorf("Location = %q", got)
	}

	scan, err := db.PageScan(scanID)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != store.ScanPending {
		t.Errorf("status = %q, want pending", scan.Status)
	}
	if calls.wakes != 1 {
		t.Errorf("WakeScanner called %d times, want 1", calls.wakes)
	}
}

// ---- label -------------------------------------------------------------

func TestLabelScanRenamesRefAndReassembles(t *testing.T) {
	server, db, calls := newTestServerWithScans(t)
	now := time.Now()

	docID, err := db.CreateScannedBook("Label Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", tinyJPEG(t), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	completeAndAssembleScan(t, db, docID, scanID, unnumberedPageResult, now)

	unlabeledRef := "scan:s" + strconv.FormatInt(scanID, 10) + ".0:m0"
	if !hasElementRef(t, db, docID, unlabeledRef) {
		t.Fatalf("setup: expected element with the unlabeled ref %q", unlabeledRef)
	}

	response := post(t, server, "/documents/"+strconv.FormatInt(docID, 10)+"/scans/"+strconv.FormatInt(scanID, 10)+"/label",
		url.Values{"page": {"0"}, "label": {"94"}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Location"); got != "/documents/"+strconv.FormatInt(docID, 10)+"#scans" {
		t.Errorf("Location = %q", got)
	}

	scan, err := db.PageScan(scanID)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	result, err := pagescan.ParseResult(scan.Result)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if len(result.Pages) != 1 || result.Pages[0].Label != "94" {
		t.Errorf("stored result pages = %+v, want page_label 94", result.Pages)
	}

	// The element's own page/quote columns are only rebuilt by the real
	// reassembly this test's ReassembleScans fake stands in for (see
	// scanworker.Worker.Reassemble) — RenameScanRefs itself only moves the
	// ref, which is what this checks.
	if !hasElementRef(t, db, docID, "scan:p94:m0") {
		t.Fatalf("no element with the new ref scan:p94:m0")
	}
	if hasElementRef(t, db, docID, unlabeledRef) {
		t.Error("the old unlabeled ref is still present after relabeling")
	}

	if len(calls.reassembled) != 1 || calls.reassembled[0] != [2]int64{docID, scanID} {
		t.Errorf("ReassembleScans calls = %+v, want exactly [(%d,%d)]", calls.reassembled, docID, scanID)
	}
}

// TestLabelScanDoesNotCorruptAPrefixCollision guards RenameScanRefs's own
// trailing-colon requirement (see its doc comment): relabeling a page whose
// current ref prefix is "scan:p1" must not touch an unrelated element whose
// ref happens to start with those same characters, "scan:p14:m0".
func TestLabelScanDoesNotCorruptAPrefixCollision(t *testing.T) {
	server, db, _ := newTestServerWithScans(t)
	now := time.Now()

	docID, err := db.CreateScannedBook("Collision Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}

	scanFourteen, err := db.AddPageScan(docID, "p14.jpg", "image/jpeg", tinyJPEG(t), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	completeAndAssembleScan(t, db, docID, scanFourteen, labeledPageFourteenResult, now)
	if !hasElementRef(t, db, docID, "scan:p14:m0") {
		t.Fatalf("setup: expected scan:p14:m0 to exist")
	}

	scanOne, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", tinyJPEG(t), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	completeAndAssembleScan(t, db, docID, scanOne, labeledPageOneResult, now)
	if !hasElementRef(t, db, docID, "scan:p1:m0") {
		t.Fatalf("setup: expected scan:p1:m0 to exist")
	}

	// Relabel the already-"1"-labeled page to "1" again — a hand correction
	// that leaves the number the same. If the handler's rename ever dropped
	// its trailing ":" on the old prefix, this would incorrectly sweep up
	// "scan:p14:m0" too, since it also starts with "scan:p1".
	response := post(t, server, "/documents/"+strconv.FormatInt(docID, 10)+"/scans/"+strconv.FormatInt(scanOne, 10)+"/label",
		url.Values{"page": {"0"}, "label": {"1"}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", response.Code, response.Body.String())
	}

	if !hasElementRef(t, db, docID, "scan:p14:m0") {
		t.Error("the unrelated scan:p14:m0 element was renamed away — the trailing ':' guard failed")
	}
	if !hasElementRef(t, db, docID, "scan:p1:m0") {
		t.Error("the relabeled element lost its own ref")
	}
}

func TestLabelScanWithEmptyLabelJustRedirects(t *testing.T) {
	server, db, calls := newTestServerWithScans(t)
	now := time.Now()

	docID, err := db.CreateScannedBook("Empty Label Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", tinyJPEG(t), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	completeAndAssembleScan(t, db, docID, scanID, unnumberedPageResult, now)

	response := post(t, server, "/documents/"+strconv.FormatInt(docID, 10)+"/scans/"+strconv.FormatInt(scanID, 10)+"/label",
		url.Values{"page": {"0"}, "label": {""}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want a plain redirect", response.Code)
	}
	if len(calls.reassembled) != 0 {
		t.Errorf("ReassembleScans called %d times, want 0 for an empty label", len(calls.reassembled))
	}
}

// ---- margin notes -----------------------------------------------------------

func TestMarginNoteSaveSetsNoteAndClearsNotePending(t *testing.T) {
	server, db, _ := newTestServerWithScans(t)
	now := time.Now()

	docID, err := db.CreateScannedBook("Margin Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", tinyJPEG(t), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	completeAndAssembleScan(t, db, docID, scanID, handwritingPageResult, now)

	elem := findElementByRef(t, db, docID, "scan:p12:m0")
	if !elem.NotePending {
		t.Fatalf("setup: expected NotePending true from a handwriting-flagged mark")
	}
	originalQuote := elem.Quote

	response := post(t, server, "/elements/"+strconv.FormatInt(elem.ID, 10)+"/margin-note",
		url.Values{"note": {"a note from the margin"}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Location"); got != "/documents/"+strconv.FormatInt(docID, 10)+"#notes" {
		t.Errorf("Location = %q", got)
	}

	updated, err := db.ElementByID(elem.ID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if updated.Note != "a note from the margin" {
		t.Errorf("note = %q", updated.Note)
	}
	if updated.NotePending {
		t.Error("NotePending still set after saving a note")
	}
	if updated.Quote != originalQuote {
		t.Errorf("quote = %q, want unchanged %q — a margin note must never touch the passage", updated.Quote, originalQuote)
	}
	if updated.EditedQuote != "" {
		t.Errorf("edited_quote = %q, want unchanged", updated.EditedQuote)
	}
}

func TestMarginNoteDismissClearsNotePending(t *testing.T) {
	server, db, _ := newTestServerWithScans(t)
	now := time.Now()

	docID, err := db.CreateScannedBook("Dismiss Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", tinyJPEG(t), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	completeAndAssembleScan(t, db, docID, scanID, handwritingPageResult, now)
	elem := findElementByRef(t, db, docID, "scan:p12:m0")

	response := post(t, server, "/elements/"+strconv.FormatInt(elem.ID, 10)+"/margin-note/dismiss", nil)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Location"); got != "/documents/"+strconv.FormatInt(docID, 10)+"#notes" {
		t.Errorf("Location = %q", got)
	}

	updated, err := db.ElementByID(elem.ID)
	if err != nil {
		t.Fatalf("ElementByID: %v", err)
	}
	if updated.NotePending {
		t.Error("NotePending still set after dismissing")
	}
	if updated.Note != "" {
		t.Errorf("note = %q, want still empty — dismissing types nothing in", updated.Note)
	}
}

func TestMarginNoteRejectsARootElement(t *testing.T) {
	server, db, _ := newTestServerWithScans(t)
	now := time.Now()
	docID, err := db.CreateScannedBook("Root Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	root, err := db.RootElement(docID)
	if err != nil {
		t.Fatalf("RootElement: %v", err)
	}

	response := post(t, server, "/elements/"+strconv.FormatInt(root.ID, 10)+"/margin-note", url.Values{"note": {"x"}})
	if response.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.Code)
	}
}

// ---- document page rendering ------------------------------------------------

func TestDocumentPageRendersScanSections(t *testing.T) {
	server, db, _ := newTestServerWithScans(t)
	now := time.Now()

	docID, err := db.CreateScannedBook("Full Scan Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}

	failedID, err := db.AddPageScan(docID, "failed.jpg", "image/jpeg", tinyJPEG(t), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	if err := db.FailPageScan(failedID, "model timed out", false, now); err != nil {
		t.Fatalf("FailPageScan: %v", err)
	}

	doneID, err := db.AddPageScan(docID, "done.jpg", "image/jpeg", tinyJPEG(t), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	completeAndAssembleScan(t, db, docID, doneID, handwritingPageResult, now)

	body := get(t, server, "/documents/"+strconv.FormatInt(docID, 10)).Body.String()

	if !strings.Contains(body, `id="scans"`) {
		t.Error("the contents page does not show the scans section")
	}
	if !strings.Contains(body, "of 2 photos read") {
		t.Errorf("the contents page does not show the progress line:\n%s", body)
	}
	if !strings.Contains(body, "failed.jpg") || !strings.Contains(body, "model timed out") {
		t.Error("the contents page does not list the failed photo")
	}
	if !strings.Contains(body, "/documents/"+strconv.FormatInt(docID, 10)+"/scans/"+strconv.FormatInt(failedID, 10)+"/retry") {
		t.Error("the contents page does not offer a retry form for the failed photo")
	}
	if !strings.Contains(body, `id="notes"`) {
		t.Error("the contents page does not show the margin notes section")
	}
	if !strings.Contains(body, "/elements/") || !strings.Contains(body, "/margin-note") {
		t.Error("the contents page does not offer a margin-note form")
	}
	if !strings.Contains(body, "Add more photos") {
		t.Error("the contents page does not offer to add more photos")
	}

	// A document with no scans at all shows none of this.
	plain, _, _ := newTestServer(t, false)
	plainBody := get(t, plain, "/documents/1").Body.String()
	for _, absent := range []string{`id="scans"`, `id="notes"`, "Add more photos"} {
		if strings.Contains(plainBody, absent) {
			t.Errorf("a document with no scans shows %q", absent)
		}
	}
}

// TestDocumentPageShowsUnnumberedPageForm covers the specific case where a
// done page's own label could not be read — a separate scenario from the
// broader rendering smoke test above, since it needs its own scan with a
// null page_label rather than the numbered one that test's NotePending
// fixture already carries.
func TestDocumentPageShowsUnnumberedPageForm(t *testing.T) {
	server, db, _ := newTestServerWithScans(t)
	now := time.Now()

	docID, err := db.CreateScannedBook("Unnumbered Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(docID, "p1.jpg", "image/jpeg", tinyJPEG(t), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}
	completeAndAssembleScan(t, db, docID, scanID, unnumberedPageResult, now)

	body := get(t, server, "/documents/"+strconv.FormatInt(docID, 10)).Body.String()
	labelAction := "/documents/" + strconv.FormatInt(docID, 10) + "/scans/" + strconv.FormatInt(scanID, 10) + "/label"
	if !strings.Contains(body, labelAction) {
		t.Errorf("the contents page does not offer a label form for the unnumbered page:\n%s", body)
	}
	if !strings.Contains(body, `name="page" value="0"`) {
		t.Error("the label form does not carry the page's own index")
	}
}

// ---- progress fragment ---------------------------------------------------

func TestScansProgressFragment(t *testing.T) {
	server, db, _ := newTestServerWithScans(t)
	now := time.Now()

	docID, err := db.CreateScannedBook("Progress Book", "", "", now)
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	pendingID, err := db.AddPageScan(docID, "a.jpg", "image/jpeg", tinyJPEG(t), true, now)
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	busy := get(t, server, "/documents/"+strconv.FormatInt(docID, 10)+"/scans/progress")
	if busy.Code != http.StatusOK {
		t.Fatalf("status %d", busy.Code)
	}
	if !strings.Contains(busy.Body.String(), `hx-trigger="every 5s"`) {
		t.Errorf("the busy fragment does not keep polling:\n%s", busy.Body.String())
	}

	if err := db.CompletePageScan(pendingID, `{"pages":[]}`, now); err != nil {
		t.Fatalf("CompletePageScan: %v", err)
	}
	done := get(t, server, "/documents/"+strconv.FormatInt(docID, 10)+"/scans/progress")
	if done.Code != http.StatusOK {
		t.Fatalf("status %d", done.Code)
	}
	if strings.Contains(done.Body.String(), "hx-trigger") {
		t.Error("the finished fragment still polls")
	}
	if !strings.Contains(done.Body.String(), "reload to see the new passages") {
		t.Errorf("the finished fragment does not offer to reload:\n%s", done.Body.String())
	}
}
