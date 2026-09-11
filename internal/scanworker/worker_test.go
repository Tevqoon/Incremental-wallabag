package scanworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/pagescan"
	"github.com/Tevqoon/increader/internal/store"
)

// --- test scaffolding ---

func testStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// jpegMagic is a real JPEG's first bytes (the JFIF APP0 marker) — enough for
// http.DetectContentType, which SniffImageType relies on, to sniff whatever
// follows as image/jpeg regardless of what it actually is.
const jpegMagic = "\xff\xd8\xff\xe0"

// scanImage builds a fake "photo": real JPEG magic bytes followed by a key
// string, so a test can hand the store an image that passes SniffImageType
// and a fake Reader can recover which canned response it should answer with
// by reading the key back out of the bytes it was actually given — the same
// thing a real ReadPage does with real image bytes, just standing in for
// "which page is this" with a label instead of pixels.
func scanImage(key string) []byte {
	return []byte(jpegMagic + key)
}

// testdataScanFiles lists the five real model responses this package borrows
// from internal/pagescan's own golden test, in filename order — the same
// order testdata/expected.json was produced against, so scans added in this
// order become scan ids 1..5 matching that file.
func testdataScanFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../pagescan/testdata/scans/*.json")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if len(files) != 5 {
		t.Fatalf("got %d testdata scans, want 5", len(files))
	}
	return files
}

// scanKey is a testdata scan file's basename with no extension — both the
// image key scanImage embeds and the map key loadCannedResponses indexes by.
func scanKey(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".json")
}

// loadCannedResponses reads every testdata scan's raw model response, keyed
// by scanKey.
func loadCannedResponses(t *testing.T) map[string]string {
	t.Helper()
	responses := make(map[string]string)
	for _, f := range testdataScanFiles(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		responses[scanKey(f)] = string(raw)
	}
	return responses
}

// expectedPassage mirrors the fields testdata/expected.json carries that
// this package's tests check.
type expectedPassage struct {
	Page        string `json:"page"`
	Text        string `json:"text"`
	NotePending bool   `json:"note_pending"`
	Chapter     string `json:"chapter"`
}

func loadExpected(t *testing.T) []expectedPassage {
	t.Helper()
	raw, err := os.ReadFile("../pagescan/testdata/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected []expectedPassage
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	return expected
}

// addAllTestdataScans uploads all five testdata photos to documentID, in
// filename order (so they become scan ids 1..5), all marked for triage.
func addAllTestdataScans(t *testing.T, db *store.Store, documentID int64) {
	t.Helper()
	for _, f := range testdataScanFiles(t) {
		key := scanKey(f)
		if _, err := db.AddPageScan(documentID, key+".jpg", "image/jpeg", scanImage(key), true, time.Now()); err != nil {
			t.Fatalf("AddPageScan(%s): %v", key, err)
		}
	}
}

// funcReader adapts a plain function to Reader, for a test that needs a
// one-off canned behaviour (always fails, blocks until ctx is done) rather
// than fakeReader's key-driven lookup.
type funcReader func(ctx context.Context, image []byte, contentType string) (string, error)

func (f funcReader) ReadPage(ctx context.Context, image []byte, contentType string) (string, error) {
	return f(ctx, image, contentType)
}

// fakeReader answers ReadPage from a fixed map of canned responses, keyed by
// the string scanImage embedded after the JPEG magic bytes — standing in
// for the real vision model. failing marks keys that should error instead,
// toggled with setFailing so a test can simulate a transient failure that
// clears on a later attempt.
type fakeReader struct {
	mu        sync.Mutex
	responses map[string]string
	failing   map[string]bool
}

func newFakeReader(responses map[string]string) *fakeReader {
	return &fakeReader{responses: responses}
}

func (f *fakeReader) ReadPage(ctx context.Context, image []byte, contentType string) (string, error) {
	if !strings.HasPrefix(string(image), jpegMagic) {
		return "", fmt.Errorf("fake reader: image missing the expected magic prefix")
	}
	key := strings.TrimPrefix(string(image), jpegMagic)

	f.mu.Lock()
	fail := f.failing[key]
	response, ok := f.responses[key]
	f.mu.Unlock()

	if fail {
		return "", fmt.Errorf("fake reader: simulated failure for %s", key)
	}
	if !ok {
		return "", fmt.Errorf("fake reader: no canned response for %s", key)
	}
	return response, nil
}

func (f *fakeReader) setFailing(key string, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing == nil {
		f.failing = map[string]bool{}
	}
	f.failing[key] = fail
}

// assertPassage checks the fields DisplayQuote/Page/Chapter/NotePending a
// passage assembled from a page scan should carry, against one entry of
// testdata/expected.json.
func assertPassage(t *testing.T, got store.ExtractRow, want expectedPassage) {
	t.Helper()
	if got.Quote != want.Text {
		t.Errorf("Quote: got %q, want %q", got.Quote, want.Text)
	}
	if got.Page != want.Page {
		t.Errorf("Page: got %q, want %q", got.Page, want.Page)
	}
	if got.Chapter != want.Chapter {
		t.Errorf("Chapter: got %q, want %q", got.Chapter, want.Chapter)
	}
	if got.NotePending != want.NotePending {
		t.Errorf("NotePending: got %v, want %v", got.NotePending, want.NotePending)
	}
}

// --- tests ---

// TestDrainEndToEnd uploads all five testdata photos, drains once, and
// checks the resulting passages against testdata/expected.json — the same
// ground truth internal/pagescan's own golden test uses, carried all the way
// through claim, model call, store, and re-assembly this time rather than
// just Assemble in isolation.
func TestDrainEndToEnd(t *testing.T) {
	db := testStore(t)
	reader := newFakeReader(loadCannedResponses(t))
	w := New(db, reader, testLogger(), Options{RetryDelay: time.Millisecond})

	documentID, err := db.CreateScannedBook("Test Book", "Test Author", "", time.Now())
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	addAllTestdataScans(t, db, documentID)

	if err := w.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	expected := loadExpected(t)
	if len(annotations) != len(expected) {
		t.Fatalf("got %d annotations, want %d", len(annotations), len(expected))
	}
	for i, want := range expected {
		assertPassage(t, annotations[i], want)
		if annotations[i].Schedule.State != ir.StateSuspended {
			t.Errorf("passage %d: state = %q, want %q (triage was true)", i, annotations[i].Schedule.State, ir.StateSuspended)
		}
	}
}

// TestDrainJoinsAPageSpanningPassageThatArrivesLate covers the situation
// Reassemble's mutex exists for: a passage assembled from more than one
// photo (here, IMG_6521's own two-page spread) must still join correctly
// even when its scan does not finish on the very first attempt. Its scan is
// made to fail once — permanently, with MaxAttempts: 1, so the first Drain
// call cannot silently retry its way past the failure inside the same
// call — while the other four photos' scans succeed normally. After the
// first Drain only four passages exist and the fifth scan sits failed; a
// manual retry (RetryPageScan, the same path a reader's "retry" button
// uses) followed by a second Drain call completes it, and Reassemble joins
// its two pages into the fifth, page-spanning passage with nothing
// duplicated.
func TestDrainJoinsAPageSpanningPassageThatArrivesLate(t *testing.T) {
	db := testStore(t)
	reader := newFakeReader(loadCannedResponses(t))
	reader.setFailing("IMG_6521", true)
	w := New(db, reader, testLogger(), Options{RetryDelay: time.Millisecond, MaxAttempts: 1})

	documentID, err := db.CreateScannedBook("Test Book", "Test Author", "", time.Now())
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}

	var spreadScanID int64
	for _, f := range testdataScanFiles(t) {
		key := scanKey(f)
		id, err := db.AddPageScan(documentID, key+".jpg", "image/jpeg", scanImage(key), true, time.Now())
		if err != nil {
			t.Fatalf("AddPageScan(%s): %v", key, err)
		}
		if key == "IMG_6521" {
			spreadScanID = id
		}
	}

	if err := w.Drain(context.Background()); err != nil {
		t.Fatalf("first Drain: %v", err)
	}

	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if len(annotations) != 4 {
		t.Fatalf("after first Drain: got %d annotations, want 4 (the spread photo's scan failed)", len(annotations))
	}

	spreadScan, err := db.PageScan(spreadScanID)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if spreadScan.Status != store.ScanFailed {
		t.Fatalf("spread scan status = %q, want %q", spreadScan.Status, store.ScanFailed)
	}

	// The retry: the reader now answers this key, and the scan goes back to
	// pending the same way a reader's own "retry" action would.
	reader.setFailing("IMG_6521", false)
	if err := db.RetryPageScan(spreadScanID, time.Now()); err != nil {
		t.Fatalf("RetryPageScan: %v", err)
	}
	if err := w.Drain(context.Background()); err != nil {
		t.Fatalf("second Drain: %v", err)
	}

	annotations, err = db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	expected := loadExpected(t)
	if len(annotations) != len(expected) {
		t.Fatalf("after second Drain: got %d annotations, want %d", len(annotations), len(expected))
	}
	// Text/Page/NotePending only, not Chapter: the running-head grouping the
	// first (incomplete) Reassemble computed over just four photos already
	// assigned chapters to the other passages, and Assemble's own
	// inheritance rule (see its doc comment) deliberately keeps whatever
	// chapter a passage already has rather than recomputing it once more
	// context arrives — the same stickiness that protects a reader's manual
	// rename. That is correct, unrelated behaviour this test is not about;
	// what matters here is that no passage was duplicated or left unjoined.
	for i, want := range expected {
		got := annotations[i]
		if got.Quote != want.Text {
			t.Errorf("passage %d: Quote: got %q, want %q", i, got.Quote, want.Text)
		}
		if got.Page != want.Page {
			t.Errorf("passage %d: Page: got %q, want %q", i, got.Page, want.Page)
		}
		if got.NotePending != want.NotePending {
			t.Errorf("passage %d: NotePending: got %v, want %v", i, got.NotePending, want.NotePending)
		}
	}
}

// TestRunConcurrencyProcessesAllScansWithoutDuplication runs the full Run
// loop with several goroutines against all five testdata photos and checks
// that the result is identical to a single-goroutine Drain: same five
// passages, nothing duplicated. Meant to be run with -race, so that any
// unsynchronized access Reassemble's mutex is supposed to prevent shows up
// as a race failure rather than (rarely, and only under the right timing) a
// wrong answer.
func TestRunConcurrencyProcessesAllScansWithoutDuplication(t *testing.T) {
	db := testStore(t)
	reader := newFakeReader(loadCannedResponses(t))
	w := New(db, reader, testLogger(), Options{
		Concurrency:  3,
		RetryDelay:   time.Millisecond,
		PollInterval: 20 * time.Millisecond,
	})

	documentID, err := db.CreateScannedBook("Test Book", "Test Author", "", time.Now())
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	addAllTestdataScans(t, db, documentID)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(runDone)
	}()

	w.Wake()

	deadline := time.Now().Add(10 * time.Second)
	for {
		counts, err := db.CountPageScans(documentID)
		if err != nil {
			t.Fatalf("CountPageScans: %v", err)
		}
		if counts.Done == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for all scans to finish, last counts: %+v", counts)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after ctx was cancelled")
	}

	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	expected := loadExpected(t)
	if len(annotations) != len(expected) {
		t.Fatalf("got %d annotations, want %d", len(annotations), len(expected))
	}
	for i, want := range expected {
		assertPassage(t, annotations[i], want)
	}
}

// TestDrainFailsPermanentlyAfterMaxAttempts covers a scan whose photo the
// model can genuinely never read: after MaxAttempts claims it is left
// failed with the last error's text, and no passage is ever created for it.
func TestDrainFailsPermanentlyAfterMaxAttempts(t *testing.T) {
	db := testStore(t)
	reader := funcReader(func(ctx context.Context, image []byte, contentType string) (string, error) {
		return "", errors.New("model exploded")
	})
	w := New(db, reader, testLogger(), Options{RetryDelay: time.Millisecond, MaxAttempts: 3})

	documentID, err := db.CreateScannedBook("Test Book", "Test Author", "", time.Now())
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(documentID, "bad.jpg", "image/jpeg", scanImage("bad"), true, time.Now())
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	if err := w.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	scan, err := db.PageScan(scanID)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != store.ScanFailed {
		t.Errorf("status = %q, want %q", scan.Status, store.ScanFailed)
	}
	if scan.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", scan.Attempts)
	}
	if !strings.Contains(scan.Error, "model exploded") {
		t.Errorf("error = %q, want it to contain the reader's own error text", scan.Error)
	}

	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if len(annotations) != 0 {
		t.Errorf("got %d annotations, want 0", len(annotations))
	}
}

// TestDrainRejectsNonImageWithoutCallingReader: bytes that are not a
// recognisable image fail immediately, for good, and never reach the model.
func TestDrainRejectsNonImageWithoutCallingReader(t *testing.T) {
	db := testStore(t)
	called := false
	reader := funcReader(func(ctx context.Context, image []byte, contentType string) (string, error) {
		called = true
		return "", errors.New("should never be called")
	})
	w := New(db, reader, testLogger(), Options{RetryDelay: time.Millisecond})

	documentID, err := db.CreateScannedBook("Test Book", "Test Author", "", time.Now())
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(documentID, "hello.txt", "text/plain", []byte("hello"), true, time.Now())
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	if err := w.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if called {
		t.Error("the reader was called for bytes that are not an image")
	}
	scan, err := db.PageScan(scanID)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != store.ScanFailed {
		t.Errorf("status = %q, want %q", scan.Status, store.ScanFailed)
	}
	if scan.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (failed for good on the first attempt)", scan.Attempts)
	}
}

// TestDrainShutdownLeavesScanProcessing: cancelling the run context while a
// read is in flight must not record a failed attempt. The scan is left
// "processing" for ResetProcessingScans to put back to pending on the next
// start, so the interrupted read is retried in full rather than counted
// against MaxAttempts.
func TestDrainShutdownLeavesScanProcessing(t *testing.T) {
	db := testStore(t)
	started := make(chan struct{})
	reader := funcReader(func(ctx context.Context, image []byte, contentType string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	w := New(db, reader, testLogger(), Options{RetryDelay: time.Millisecond, ReadTimeout: time.Minute})

	documentID, err := db.CreateScannedBook("Test Book", "Test Author", "", time.Now())
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(documentID, "slow.jpg", "image/jpeg", scanImage("slow"), true, time.Now())
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	drainErr := make(chan error, 1)
	go func() { drainErr <- w.Drain(ctx) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the reader was never called")
	}
	cancel()

	select {
	case err := <-drainErr:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after the context was cancelled")
	}

	scan, err := db.PageScan(scanID)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != store.ScanProcessing {
		t.Errorf("status = %q, want %q (left in place for ResetProcessingScans)", scan.Status, store.ScanProcessing)
	}

	n, err := db.ResetProcessingScans(time.Now())
	if err != nil {
		t.Fatalf("ResetProcessingScans: %v", err)
	}
	if n != 1 {
		t.Errorf("ResetProcessingScans reset %d scans, want 1", n)
	}
	scan, err = db.PageScan(scanID)
	if err != nil {
		t.Fatalf("PageScan: %v", err)
	}
	if scan.Status != store.ScanPending {
		t.Errorf("status after reset = %q, want %q", scan.Status, store.ScanPending)
	}
}

// TestReassembleAfterHandCorrectedLabel: a page the model could not read
// starts out addressed by its scan id; once the reader types in the real
// page number by hand (SetPageLabel, SetPageScanResult, RenameScanRefs — the
// same three calls the web layer would make), a second Reassemble must move
// the existing passage onto the new ref rather than leaving a duplicate
// behind under the old one.
func TestReassembleAfterHandCorrectedLabel(t *testing.T) {
	db := testStore(t)
	w := New(db, funcReader(func(context.Context, []byte, string) (string, error) {
		return "", errors.New("not used by this test")
	}), testLogger(), Options{})

	documentID, err := db.CreateScannedBook("Test Book", "Test Author", "", time.Now())
	if err != nil {
		t.Fatalf("CreateScannedBook: %v", err)
	}
	scanID, err := db.AddPageScan(documentID, "unlabelled.jpg", "image/jpeg", scanImage("unlabelled"), true, time.Now())
	if err != nil {
		t.Fatalf("AddPageScan: %v", err)
	}

	const raw = `{"pages":[{"page_label":"","running_head":"","headings":[],` +
		`"lines":["A sentence on the page."],"marks":[{"first_line":1,"last_line":1,` +
		`"text":"A sentence on the page.","kind":"bracket","has_handwriting":false,` +
		`"continues_from_previous_page":false,"continues_to_next_page":false}]}]}`
	if err := db.CompletePageScan(scanID, raw, time.Now()); err != nil {
		t.Fatalf("CompletePageScan: %v", err)
	}

	if _, err := w.Reassemble(documentID, scanID); err != nil {
		t.Fatalf("first Reassemble: %v", err)
	}

	annotations, err := db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if len(annotations) != 1 {
		t.Fatalf("got %d annotations, want 1", len(annotations))
	}
	wantOldRef := "scan:s" + strconv.FormatInt(scanID, 10) + ".0:m0"
	if annotations[0].ExternalRef != wantOldRef {
		t.Fatalf("ExternalRef = %q, want %q", annotations[0].ExternalRef, wantOldRef)
	}

	corrected, err := pagescan.SetPageLabel(raw, 0, "42")
	if err != nil {
		t.Fatalf("SetPageLabel: %v", err)
	}
	if err := db.SetPageScanResult(scanID, corrected, time.Now()); err != nil {
		t.Fatalf("SetPageScanResult: %v", err)
	}
	oldPrefix := "scan:s" + strconv.FormatInt(scanID, 10) + ".0:"
	if _, err := db.RenameScanRefs(documentID, oldPrefix, "scan:p42:", time.Now()); err != nil {
		t.Fatalf("RenameScanRefs: %v", err)
	}

	if _, err := w.Reassemble(documentID, scanID); err != nil {
		t.Fatalf("second Reassemble: %v", err)
	}

	annotations, err = db.DocumentAnnotations(documentID)
	if err != nil {
		t.Fatalf("DocumentAnnotations: %v", err)
	}
	if len(annotations) != 1 {
		t.Fatalf("got %d annotations after the label correction, want 1 (still exactly one)", len(annotations))
	}
	if annotations[0].Page != "42" {
		t.Errorf("Page = %q, want %q", annotations[0].Page, "42")
	}
	if annotations[0].ExternalRef != "scan:p42:m0" {
		t.Errorf("ExternalRef = %q, want %q", annotations[0].ExternalRef, "scan:p42:m0")
	}
}
