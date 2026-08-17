package web

import (
	"context"
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

	// SubstackEnabled shows or hides the "import from a URL" section —
	// see Server.importSubstackURL. There is nothing useful that section's
	// own form can do without it configured, so it does not render at all
	// rather than rendering disabled.
	SubstackEnabled bool

	// SubstackURL, SubstackReport and SubstackError describe a URL import
	// that just happened, the same way Filename/Result/Warnings/Error
	// describe a file upload above — kept as separate fields rather than
	// reusing Error/Filename because either form can fail with the other
	// left completely blank (an empty file field never reaches Filename;
	// an empty URL field never reaches SubstackURL), which would otherwise
	// make the two indistinguishable in the template. SubstackURL re-fills
	// the form (a failed import is usually retried after fixing the same
	// URL, not retyped from scratch); SubstackReport is ingest.WriteReport's
	// own plain-text summary, rendered as-is rather than picked apart into
	// template fields.
	SubstackURL    string
	SubstackReport string
	SubstackError  string

	// FeedRefreshEnabled shows or hides the "check for new articles"
	// button — see Server.refreshSubstackFeed. A separate field from
	// SubstackEnabled, not just a reuse of it, because the two describe
	// different actions (one post by URL; the whole configured archive)
	// and the template needs to tell them apart to show/hide each
	// independently.
	FeedRefreshEnabled bool

	// FeedRefreshReport and FeedRefreshError describe a feed refresh that
	// just happened, the same way SubstackReport/SubstackError describe a
	// URL import — kept separate so the two actions' results are never
	// shown attributed to the wrong one.
	FeedRefreshReport string
	FeedRefreshError  string
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
	data.SubstackEnabled = s.importSubstackURL != nil
	data.FeedRefreshEnabled = s.refreshSubstackFeed != nil
	if data.Title == "" {
		data.Title = "Import annotations"
	}
	s.render(w, "import.html", data)
}

// importSubstackTimeout bounds one URL import: a handful of Substack
// requests (throttled roughly a second and a half apart — see
// substack.defaultRequestGap) plus the wallabag reconcile pass afterward,
// comfortably inside a minute even on a paid post that needs the extra
// differential fetch.
const importSubstackTimeout = 60 * time.Second

// handleImportSubstackURL fetches one Substack post directly — bypassing
// wallabag's own readability extraction, which drops structure (headings,
// most visibly) that internal/substack's cleaning keeps — and reconciles it
// into wallabag in place: creating the entry if wallabag has never seen
// this post, replacing its content if what is there is not yet the full
// article, and re-anchoring any annotations already made on it either way.
//
// The web-triggered, single-URL counterpart to `increader import-substack`,
// which does the same reconciliation across a publication's whole archive.
// See internal/substack.FetchPost and internal/ingest for what actually
// does the work; importSubstackURL (built in cmd/increader/main.go) is only
// the wiring, and this handler only reads the form and reports the result.
func (s *Server) handleImportSubstackURL(w http.ResponseWriter, r *http.Request) {
	if s.importSubstackURL == nil {
		http.Error(w, "substack import is not configured", http.StatusNotImplemented)
		return
	}

	rawURL := strings.TrimSpace(r.FormValue("substack_url"))
	if rawURL == "" {
		s.renderImport(w, importData{SubstackError: "Paste a Substack post URL to import."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), importSubstackTimeout)
	defer cancel()

	report, err := s.importSubstackURL(ctx, rawURL)
	if err != nil {
		s.renderImport(w, importData{SubstackURL: rawURL, SubstackError: err.Error()})
		return
	}
	s.renderImport(w, importData{SubstackURL: rawURL, SubstackReport: report})
}

// refreshFeedTimeout bounds a whole-archive refresh: mostly cache hits on
// every run after the first (post's own on-disk cache — see
// internal/substack/post.go — makes an already-fetched post free to skip),
// but the archive *listing* itself still has to be walked page by page,
// throttled the same as any other request to Substack, and a large,
// long-running publication can genuinely have hundreds of pages. Generous
// next to importSubstackTimeout above for exactly that reason — this is not
// the same shape of request.
const refreshFeedTimeout = 20 * time.Minute

// handleRefreshSubstackFeed re-walks the whole archive ingest.substack is
// configured for and reconciles anything new (or anything still showing as
// a paywall preview) into wallabag — the web-triggered counterpart to
// `increader import-substack`, without a dry-run step: see
// refreshSubstackFeedHandler's own comment in cmd/increader/main.go for why
// committing unconditionally is safe here even though the CLI defaults to
// reporting only.
func (s *Server) handleRefreshSubstackFeed(w http.ResponseWriter, r *http.Request) {
	if s.refreshSubstackFeed == nil {
		http.Error(w, "substack import is not configured", http.StatusNotImplemented)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), refreshFeedTimeout)
	defer cancel()

	report, err := s.refreshSubstackFeed(ctx)
	if err != nil {
		s.renderImport(w, importData{FeedRefreshError: err.Error()})
		return
	}
	s.renderImport(w, importData{FeedRefreshReport: report})
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
		DisplayTitle:       strings.TrimSpace(r.FormValue("title")),
		Subtitle:           strings.TrimSpace(r.FormValue("subtitle")),
		Author:             strings.TrimSpace(r.FormValue("author")),
		ChapterMarkerColor: strings.TrimSpace(r.FormValue("chapter_marker_color")),
		IntoDocumentID:     into,
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
