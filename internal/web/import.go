package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/annotations"
	"github.com/Tevqoon/increader/internal/store"
)

// maxUploadBytes caps an uploaded annotation file.
//
// Generous next to what any of these formats actually weigh — a KOReader
// export of a heavily annotated book is a few hundred kilobytes — because the
// PDF route uploads the whole book, and a scanned monograph is genuinely
// large. The cap exists so that a mistaken upload of something else entirely
// fails fast instead of being read into memory in full.
const maxUploadBytes = 64 << 20

// importData is the upload page.
type importData struct {
	Title string

	// Existing lists documents an upload can be merged into, for a work
	// whose two exports disagree about what it is called.
	Existing []store.Document

	// Result and Warnings describe the upload that just happened, when this
	// page is being shown after one that only partly worked. A completely
	// clean import redirects to the document instead of coming back here.
	Result   *store.ImportResult
	Warnings []string
	Error    string
	Filename string
}

func (s *Server) handleImportForm(w http.ResponseWriter, r *http.Request) {
	s.renderImport(w, importData{Title: "Import annotations"})
}

// renderImport fills in the parts of the page that are the same however it
// was reached.
func (s *Server) renderImport(w http.ResponseWriter, data importData) {
	existing, err := s.store.UploadedDocuments()
	if err != nil {
		s.fail(w, err)
		return
	}
	data.Existing = existing
	if data.Title == "" {
		data.Title = "Import annotations"
	}
	s.render(w, "import.html", data)
}

// handleImport reads an uploaded annotation file into the library.
//
// A clean import redirects to the new document, because the next thing anyone
// wants after uploading a book is to look at what came out of it. An import
// that produced warnings stays here and shows them: a PDF whose text layer
// only partly recovered is worth knowing about before rather than after
// going through three hundred passages.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	// MaxBytesReader rather than ParseMultipartForm's own limit: that one
	// only decides how much is buffered in memory before spilling to a
	// temporary file, and does not bound the request at all.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.renderImport(w, importData{Error: fmt.Sprintf(
				"That file is larger than the %d MB upload limit.", maxUploadBytes>>20)})
			return
		}
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		s.renderImport(w, importData{Error: "Choose a file to import."})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		s.fail(w, fmt.Errorf("web: read upload: %w", err))
		return
	}

	filename := header.Filename
	parsed, err := annotations.Parse(filename, data, time.Now())
	if err != nil {
		// A file that cannot be read is the reader's problem to fix, not a
		// server fault: the message says which of the three shapes it failed
		// to be, and they choose another file.
		s.renderImport(w, importData{
			Filename: filename,
			Error:    strings.TrimPrefix(err.Error(), "annotations: "),
		})
		return
	}

	into, err := parseOptionalID(r.FormValue("into"))
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}

	result, err := s.store.ImportAnnotations(parsed.Document, store.ImportOptions{
		DisplayTitle:   strings.TrimSpace(r.FormValue("title")),
		Subtitle:       strings.TrimSpace(r.FormValue("subtitle")),
		IntoDocumentID: into,
		// Triage unless the reader explicitly asked for the whole file to go
		// into the queue. The default is the safe one: a book that turns out
		// to hold four hundred passages should not be able to swamp the
		// queue because a radio button was left alone.
		Triage:     r.FormValue("mode") != "queue",
		FloorDays:  s.annotationDelayDays,
		SpreadDays: s.annotationDelaySpreadDays,
	}, time.Now())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such document", http.StatusBadRequest)
			return
		}
		s.fail(w, err)
		return
	}

	s.logger.Info("imported annotations",
		"file", filename, "format", parsed.Format,
		"document", result.DocumentID, "offered", result.Offered,
		"imported", result.Imported, "warnings", len(parsed.Warnings))

	if len(parsed.Warnings) == 0 {
		s.redirect(w, r, "/documents/"+strconv.FormatInt(result.DocumentID, 10))
		return
	}
	s.renderImport(w, importData{
		Filename: filename,
		Result:   &result,
		Warnings: parsed.Warnings,
	})
}

// parseOptionalID reads a form field holding an id that may be absent.
func parseOptionalID(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}
