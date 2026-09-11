package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/pagescan"
	"github.com/Tevqoon/increader/internal/store"
)

// maxPhotoBatchBytes caps a whole batch of page photos in one upload — a
// phone photo runs 2-5 MB and a book photographed in one sitting can be
// dozens of them, so this is generous next to any real batch. See
// maxPhotoBytes for the cap that actually protects against a mistaken
// upload of something else entirely.
const maxPhotoBatchBytes = 1 << 30 // 1 GiB

// maxPhotoBytes caps one photo. SQLite stores these as ordinary blobs, so
// there is no format-level reason to allow an enormous file; this exists to
// catch a mistake (the wrong file picked, a video instead of a photo) rather
// than to pinch an ordinary phone photo.
const maxPhotoBytes = 50 << 20 // 50 MB

// uploadedPhoto is one file from the "photos" field, already read and
// confirmed to be an image increader can send to the vision model.
type uploadedPhoto struct {
	filename    string
	contentType string
	data        []byte
}

// handleImportPhotos adds a batch of page photos to a book — a new one, or
// an existing one being read in further stages — and wakes the background
// worker to start reading them.
//
// 404s like proofread's own gate when no vision model is configured: without
// wakeScanner there is nothing that will ever read a photo, so offering the
// upload would only collect files nobody processes.
func (s *Server) handleImportPhotos(w http.ResponseWriter, r *http.Request) {
	if s.wakeScanner == nil {
		http.Error(w, "photo import is not configured", http.StatusNotFound)
		return
	}

	// MaxBytesReader bounds the whole request; ParseMultipartForm's own
	// argument only decides how much is buffered in memory before spilling
	// to temp files, same reasoning as handleImport's own upload cap.
	r.Body = http.MaxBytesReader(w, r.Body, maxPhotoBatchBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.renderImport(w, importData{PhotoError: fmt.Sprintf(
				"That batch is larger than the %d GB upload limit.", maxPhotoBatchBytes>>30)})
			return
		}
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}

	headers := r.MultipartForm.File["photos"]
	if len(headers) == 0 {
		s.renderImport(w, importData{PhotoError: "Choose at least one photo."})
		return
	}

	var photos []uploadedPhoto
	var problems []string
	for _, header := range headers {
		if header.Size > maxPhotoBytes {
			problems = append(problems, header.Filename+" (larger than the 50 MB per-photo limit)")
			continue
		}
		data, contentType, ok, err := readUploadedPhoto(header)
		if err != nil {
			s.fail(w, err)
			return
		}
		if !ok {
			problems = append(problems, header.Filename)
			continue
		}
		photos = append(photos, uploadedPhoto{filename: header.Filename, contentType: contentType, data: data})
	}
	// The whole batch is rejected together, and nothing is stored: a photo
	// that turns out not to be a photo is worth naming and fixing, not
	// silently dropping from an otherwise-successful upload.
	if len(problems) > 0 {
		s.renderImport(w, importData{PhotoError: fmt.Sprintf(
			"These files could not be used, so nothing was uploaded: %s", strings.Join(problems, ", "))})
		return
	}

	into, err := parseOptionalID(r.FormValue("into"))
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if into == 0 && title == "" {
		s.renderImport(w, importData{PhotoError: "Give the book a title."})
		return
	}

	now := time.Now()
	documentID := into
	if documentID == 0 {
		documentID, err = s.store.CreateScannedBook(title,
			strings.TrimSpace(r.FormValue("author")), strings.TrimSpace(r.FormValue("subtitle")), now)
		if err != nil {
			s.fail(w, err)
			return
		}
	}

	// Triage unless the reader explicitly asked for the passages to go
	// straight into the queue — same default and same reasoning as the file
	// importer's own mode radio.
	triage := r.FormValue("mode") != "queue"
	for _, photo := range photos {
		if _, err := s.store.AddPageScan(documentID, photo.filename, photo.contentType, photo.data, triage, now); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "no such document", http.StatusBadRequest)
				return
			}
			s.fail(w, err)
			return
		}
	}

	s.wakeScanner()
	s.logger.Info("added page photos", "document", documentID, "count", len(photos))
	s.redirect(w, r, "/documents/"+strconv.FormatInt(documentID, 10)+"#scans")
}

// readUploadedPhoto reads one multipart file part and sniffs it as an image.
// ok is false, with no error, for a file that just isn't an image increader
// recognises — that is the caller's problem to report, not this function's
// to fail on.
func readUploadedPhoto(header *multipart.FileHeader) (data []byte, contentType string, ok bool, err error) {
	file, err := header.Open()
	if err != nil {
		return nil, "", false, fmt.Errorf("web: open uploaded photo %s: %w", header.Filename, err)
	}
	defer file.Close()

	data, err = io.ReadAll(file)
	if err != nil {
		return nil, "", false, fmt.Errorf("web: read uploaded photo %s: %w", header.Filename, err)
	}

	mime, recognised := pagescan.SniffImageType(data, header.Header.Get("Content-Type"))
	if !recognised {
		return nil, "", false, nil
	}
	return data, mime, true, nil
}

// handleScanImage serves one photo's own bytes, exactly as uploaded — they
// are also what the vision model read, and re-encoding them here would mean
// the photo on screen no longer matches what produced the transcription.
//
// Go note: Safari (the reader's own browser — see ARCHITECTURE.md's threat
// model) renders HEIC directly; Chrome does not. That is fine to leave as
// uploaded rather than transcoding, since this deployment has exactly one
// intended browser.
func (s *Server) handleScanImage(w http.ResponseWriter, r *http.Request) {
	documentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}
	scanID, err := strconv.ParseInt(r.PathValue("scanID"), 10, 64)
	if err != nil {
		http.Error(w, "bad scan id", http.StatusBadRequest)
		return
	}

	scanDocumentID, contentType, data, err := s.store.PageScanImage(scanID)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}
	if scanDocumentID != documentID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// A photo is discarded once the model has read it (see
	// Store.CompletePageScan); only a pending or failed one still has bytes.
	if len(data) == 0 {
		http.Error(w, "that photo has been read and is no longer kept", http.StatusNotFound)
		return
	}

	// The image never changes once uploaded, so a day's caching costs
	// nothing and saves re-sending a multi-megabyte photo on every reload of
	// the contents page's own thumbnails.
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(data)
}

// handleRetryScan asks the worker to read a failed photo again. A photo that
// was read has been discarded, so a misread page is re-photographed instead.
func (s *Server) handleRetryScan(w http.ResponseWriter, r *http.Request) {
	documentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}
	scanID, err := strconv.ParseInt(r.PathValue("scanID"), 10, 64)
	if err != nil {
		http.Error(w, "bad scan id", http.StatusBadRequest)
		return
	}
	if s.wakeScanner == nil {
		http.Error(w, "photo import is not configured", http.StatusNotFound)
		return
	}

	scan, err := s.store.PageScan(scanID)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}
	if scan.DocumentID != documentID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if err := s.store.RetryPageScan(scanID, time.Now()); err != nil {
		s.fail(w, err)
		return
	}
	s.wakeScanner()
	s.redirect(w, r, "/documents/"+strconv.FormatInt(documentID, 10)+"#scans")
}

// handleLabelScan saves a hand-typed page number for a page the model could
// not read, or corrects one it misread.
//
// The rename from the page's old external_ref prefix to its new one is what
// lets a passage already assembled from that page keep its identity —
// Store.ApplyScanPassages matches by ref, so without this rename a relabel
// would look like the old passage disappearing and a new one appearing in
// its place. See RenameScanRefs's own doc comment for why the trailing ":"
// on both prefixes is essential.
func (s *Server) handleLabelScan(w http.ResponseWriter, r *http.Request) {
	documentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}
	scanID, err := strconv.ParseInt(r.PathValue("scanID"), 10, 64)
	if err != nil {
		http.Error(w, "bad scan id", http.StatusBadRequest)
		return
	}
	if s.reassembleScans == nil {
		http.Error(w, "photo import is not configured", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	// An empty label is not a correction — nothing to do but go back.
	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		s.redirect(w, r, "/documents/"+strconv.FormatInt(documentID, 10)+"#scans")
		return
	}
	pageIndex, err := strconv.Atoi(r.FormValue("page"))
	if err != nil || pageIndex < 0 {
		http.Error(w, "bad page index", http.StatusBadRequest)
		return
	}

	scan, err := s.store.PageScan(scanID)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}
	if scan.DocumentID != documentID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if scan.Status != store.ScanDone {
		http.Error(w, "that photo has not been read yet", http.StatusBadRequest)
		return
	}

	scans, err := s.store.PageScans(documentID)
	if err != nil {
		s.fail(w, err)
		return
	}
	oldPrefix, ok := pageRefPrefix(s.doneScans(scans), scanID, pageIndex)
	if !ok {
		http.Error(w, "no such page in that photo", http.StatusBadRequest)
		return
	}

	newRaw, err := pagescan.SetPageLabel(scan.Result, pageIndex, label)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	now := time.Now()
	if err := s.store.SetPageScanResult(scanID, newRaw, now); err != nil {
		s.fail(w, err)
		return
	}

	newPrefix := pagescan.RefPrefix(scanID, pageIndex, label)
	if _, err := s.store.RenameScanRefs(documentID, oldPrefix+":", newPrefix+":", now); err != nil {
		s.fail(w, err)
		return
	}

	if err := s.reassembleScans(documentID, scanID); err != nil {
		s.fail(w, err)
		return
	}

	s.redirect(w, r, "/documents/"+strconv.FormatInt(documentID, 10)+"#scans")
}

// doneScans parses a document's finished scans into pagescan's own Scan
// type, ready for Pages — shared between the contents page's own summary
// (scanDataForDocument) and the label route's ref-prefix lookup, so the two
// never part ways about what "the document's done scans" means.
//
// A scan whose stored result fails to parse is skipped rather than failing
// the whole call: it should not happen for a result this application itself
// wrote, but one malformed row must not take the contents page or a label
// edit down with it.
func (s *Server) doneScans(scans []store.PageScan) []pagescan.Scan {
	var pgScans []pagescan.Scan
	for _, scan := range scans {
		if scan.Status != store.ScanDone {
			continue
		}
		result, err := pagescan.ParseResult(scan.Result)
		if err != nil {
			if s.logger != nil {
				s.logger.Error("stored scan result does not parse", "scan", scan.ID, "error", err)
			}
			continue
		}
		pgScans = append(pgScans, pagescan.Scan{ID: scan.ID, Result: result})
	}
	return pgScans
}

// pageRefPrefix finds one page's own current ref prefix among a book's
// assembled pages, by the scan and page position that identify it.
//
// Recomputed fresh from the stored scans each time rather than trusted from
// a stale form field: an earlier label edit, or another photo finishing,
// can have already changed what this page's own prefix is.
func pageRefPrefix(scans []pagescan.Scan, scanID int64, pageIndex int) (string, bool) {
	for _, page := range pagescan.Pages(scans) {
		if page.ScanID == scanID && page.PageIndex == pageIndex {
			return page.RefPrefix, true
		}
	}
	return "", false
}

// handleScansProgress is the small htmx fragment a busy document's contents
// page polls — see the "scanProgress" template in document.html, which this
// executes directly so the two can never render different markup for the
// same counts.
func (s *Server) handleScansProgress(w http.ResponseWriter, r *http.Request) {
	documentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}

	counts, err := s.store.CountPageScans(documentID)
	if err != nil {
		s.fail(w, err)
		return
	}

	page, ok := s.pages["document.html"]
	if !ok {
		s.fail(w, fmt.Errorf("web: no such page %q", "document.html"))
		return
	}

	var buf bytes.Buffer
	if err := page.ExecuteTemplate(&buf, "scanProgress", scanProgressData{
		DocumentID: documentID, ScanCounts: counts,
	}); err != nil {
		s.fail(w, fmt.Errorf("web: render scan progress: %w", err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// scanProgressData is what the "scanProgress" named template needs. Its
// field names deliberately match documentScanData's own DocumentID and
// ScanCounts fields (which documentData embeds), so the same template
// renders both the contents page's own copy and this standalone fragment.
type scanProgressData struct {
	DocumentID int64
	ScanCounts store.ScanCounts
}

// documentScanData is the page-scan-derived slice of a document's contents
// page — see scanDataForDocument, which is the only thing that builds one.
// Embedded into documentData so its fields promote straight onto the page's
// own template data; empty (HasScans false) for a document with no photos,
// which is what hides sections "scans" and "notes" entirely.
type documentScanData struct {
	HasScans bool

	DocumentID int64
	ScanCounts store.ScanCounts

	FailedScans     []store.PageScan
	UnnumberedPages []pagescan.PageInfo
	MarginNotes     []marginNoteRow

	// PhotosEnabled shows or hides the "Add more photos" form — separate
	// from HasScans, which only says a book already has some: a book read in
	// stages should keep offering more even if the feature were switched off
	// generally, this decides that half on its own.
	PhotosEnabled bool
}

// marginNoteRow is one passage awaiting a hand-typed margin note — see
// scanDataForDocument. There is no photo to show beside it: a photo is
// discarded once read, and the note is typed from the book itself, found by
// the passage's page number.
type marginNoteRow struct {
	store.ExtractRow

	// Summary is the passage, truncated for the list — see SummariseQuote.
	Summary string
}

// scanDataForDocument gathers everything the contents page shows about a
// book's photographed pages: progress, failed photos to retry, pages the
// model could not number, and passages waiting for a hand-typed margin
// note. annotations is the same slice handleDocument already read for its
// chapter groups — passed in rather than re-queried, since the margin-notes
// list is a filter over exactly that data.
func (s *Server) scanDataForDocument(documentID int64, annotations []store.ExtractRow) (documentScanData, error) {
	counts, err := s.store.CountPageScans(documentID)
	if err != nil {
		return documentScanData{}, err
	}
	if counts.Total() == 0 {
		return documentScanData{}, nil
	}

	scans, err := s.store.PageScans(documentID)
	if err != nil {
		return documentScanData{}, err
	}

	var failed []store.PageScan
	for _, scan := range scans {
		if scan.Status == store.ScanFailed {
			failed = append(failed, scan)
		}
	}

	pages := pagescan.Pages(s.doneScans(scans))
	var unnumbered []pagescan.PageInfo
	for _, page := range pages {
		if page.Label == "" {
			unnumbered = append(unnumbered, page)
		}
	}

	var notes []marginNoteRow
	for _, annotation := range annotations {
		if !annotation.NotePending {
			continue
		}
		notes = append(notes, marginNoteRow{
			ExtractRow: annotation,
			Summary:    store.SummariseQuote(annotation.DisplayQuote()),
		})
	}

	return documentScanData{
		HasScans:        true,
		DocumentID:      documentID,
		ScanCounts:      counts,
		FailedScans:     failed,
		UnnumberedPages: unnumbered,
		MarginNotes:     notes,
		PhotosEnabled:   s.wakeScanner != nil,
	}, nil
}

// handleMarginNote saves a margin note typed in by hand from the book.
//
// Goes through EditAnnotation rather than UpdateAnnotation deliberately:
// UpdateAnnotation rewrites quote wholesale (and clears edited_quote), which
// is right for a text correction but wrong here — a margin note has nothing
// to do with the passage's own wording, and this must change only note (and,
// via EditAnnotation's own guard, note_pending).
func (s *Server) handleMarginNote(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	element, err := s.store.ElementByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}
	if element.IsRoot() {
		http.Error(w, "a document itself has no margin note to save", http.StatusBadRequest)
		return
	}

	note := strings.TrimSpace(r.FormValue("note"))
	if err := s.store.EditAnnotation(id, store.AnnotationEdit{Note: &note}, time.Now()); err != nil {
		s.fail(w, err)
		return
	}

	s.redirect(w, r, "/documents/"+strconv.FormatInt(element.DocumentID, 10)+"#notes")
}

// handleDismissMarginNote clears the note_pending nudge without a note ever
// being typed in — the reader looked at the photo and decided there was
// nothing worth saving.
func (s *Server) handleDismissMarginNote(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	element, err := s.store.ElementByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}
	if element.IsRoot() {
		http.Error(w, "a document itself has no margin note to dismiss", http.StatusBadRequest)
		return
	}

	if err := s.store.DismissNotePending(id); err != nil {
		s.fail(w, err)
		return
	}
	s.redirect(w, r, "/documents/"+strconv.FormatInt(element.DocumentID, 10)+"#notes")
}
