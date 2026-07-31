package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/store"
)

// queueData is what the queue page renders.
type queueData struct {
	Title string
	Items []store.QueueItem
	Due   int
	Total int
	Today time.Time
}

// handleQueue shows what is due today, most important first.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	today := s.today()

	items, err := s.store.Queue(today, s.dailyLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	due, err := s.store.CountDue(today)
	if err != nil {
		s.fail(w, err)
		return
	}
	total, err := s.store.CountElements("")
	if err != nil {
		s.fail(w, err)
		return
	}

	s.render(w, "queue.html", queueData{
		Title: "Queue",
		Items: items,
		Due:   due,
		Total: total,
		Today: today,
	})
}

// handleNext jumps to the most important due element, or back to the queue when
// nothing is left.
//
// This is where grading lands, so that finishing one element moves straight
// into the next without a stop at the list — which is what makes a reading
// session feel like a session rather than a series of visits.
func (s *Server) handleNext(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.Queue(s.today(), 1)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(items) == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/read/"+strconv.FormatInt(items[0].ID, 10), http.StatusSeeOther)
}

// handleGrade applies a grade and moves on.
//
// Every grade first records the read point, because every one of them is a
// decision to stop reading here. Taking it from the request rather than from
// the background scroll tracker matters: the tracker is throttled to a second,
// and the moment the reader presses a button is exactly when it is most likely
// to be stale.
func (s *Server) handleGrade(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	grade, ok := parseGrade(r.FormValue("grade"))
	if !ok {
		http.Error(w, "unknown grade", http.StatusBadRequest)
		return
	}

	element, err := s.store.ElementByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}

	if block, err := strconv.Atoi(r.FormValue("block")); err == nil && block >= 0 {
		if err := s.store.SetReadBlock(id, block); err != nil {
			s.fail(w, err)
			return
		}
	}

	// The whole scheduling decision is one pure function call. Everything
	// stateful — reading the row, writing it back — stays here.
	updated := ir.Next(element.Schedule, grade, s.today())

	if err := s.store.SaveSchedule(id, updated, time.Now()); err != nil {
		s.fail(w, err)
		return
	}

	s.redirect(w, r, "/next")
}

// handleUnsuspend returns a suspended element to the queue.
//
// The counterpart to suspending, and also how an archived article is pulled
// back in for re-reading — archived material arrives suspended, so there is
// only one mechanism to understand.
func (s *Server) handleUnsuspend(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.store.Unsuspend(id, s.today(), time.Now()); err != nil {
		s.fail(w, err)
		return
	}

	s.redirect(w, r, "/read/"+strconv.FormatInt(id, 10))
}

// handlePriority changes how urgently an element competes for attention.
func (s *Server) handlePriority(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	priority, err := strconv.ParseFloat(r.FormValue("priority"), 64)
	if err != nil {
		http.Error(w, "priority must be a number", http.StatusBadRequest)
		return
	}
	if priority < 0 || priority > 1 {
		http.Error(w, "priority must be between 0 and 1", http.StatusBadRequest)
		return
	}

	if err := s.store.SetPriority(id, priority, time.Now()); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProgress records how far through a topic the reader has scrolled.
//
// Called repeatedly while reading, so it answers with 204 and no body: there is
// nothing to swap, and a response body would only be discarded.
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	block, err := strconv.Atoi(r.FormValue("block"))
	if err != nil || block < 0 {
		http.Error(w, "block must be a non-negative integer", http.StatusBadRequest)
		return
	}

	if err := s.store.SetReadBlock(id, block); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseGrade maps a form value onto a grade.
func parseGrade(value string) (ir.Grade, bool) {
	switch value {
	case "pause":
		return ir.GradePause, true
	case "later":
		return ir.GradeLater, true
	case "sooner":
		return ir.GradeSooner, true
	case "done":
		return ir.GradeDone, true
	case "dismiss":
		return ir.GradeDismiss, true
	case "suspend":
		return ir.GradeSuspend, true
	default:
		return 0, false
	}
}

// redirect sends the browser to a new page, using htmx's own header when the
// request came from htmx.
//
// A plain 303 to an htmx request would swap the whole next page into whatever
// element the button targeted, nesting a document inside a fragment. HX-Redirect
// tells htmx to navigate instead, and the header is ignored by an ordinary form
// post, so the same handler works with JavaScript disabled.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// notFoundOrFail turns a missing row into a 404 and anything else into a 500.
func (s *Server) notFoundOrFail(w http.ResponseWriter, err error) {
	if isNotFound(err) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.fail(w, err)
}

// libraryData is what the library page renders.
type libraryData struct {
	Title   string
	Query   string
	Entries []store.LibraryEntry
}

// handleLibrary lists and searches every synced document.
//
// The queue answers "what should I read now"; this answers "where is that
// article I remember". Both are needed, and conflating them would make the
// queue's ordering meaningless.
func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	entries, err := s.store.SearchDocuments(query, 200)
	if err != nil {
		s.fail(w, err)
		return
	}

	s.render(w, "library.html", libraryData{
		Title:   "Library",
		Query:   query,
		Entries: entries,
	})
}

// extractsData is what the extracts browse page renders.
type extractsData struct {
	Title      string
	Extracts   []store.ExtractRow
	Query      string
	Origin     string
	WithClozes bool
	Imported   int
	Manual     int
}

// handleExtracts lists everything harvested, independently of what is due.
//
// Separate from the queue on purpose. The queue interleaves articles and
// extracts by priority — that mixing is much of what makes incremental reading
// work — so filtering it by kind would undermine the ordering it exists to
// provide. This page answers the other question: what have I pulled out, and
// what still needs turning into cards.
func (s *Server) handleExtracts(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	filter := store.ExtractFilter{
		Origin:     query.Get("origin"),
		WithClozes: query.Get("clozes") == "1",
		Query:      strings.TrimSpace(query.Get("q")),
	}
	if filter.Origin != "" && filter.Origin != store.OriginImport && filter.Origin != store.OriginManual {
		http.Error(w, "unknown origin filter", http.StatusBadRequest)
		return
	}

	extracts, err := s.store.Extracts(filter)
	if err != nil {
		s.fail(w, err)
		return
	}
	imported, err := s.store.CountExtracts(store.OriginImport)
	if err != nil {
		s.fail(w, err)
		return
	}
	manual, err := s.store.CountExtracts(store.OriginManual)
	if err != nil {
		s.fail(w, err)
		return
	}

	s.render(w, "extracts.html", extractsData{
		Title:      "Extracts",
		Extracts:   extracts,
		Query:      filter.Query,
		Origin:     filter.Origin,
		WithClozes: filter.WithClozes,
		Imported:   imported,
		Manual:     manual,
	})
}
