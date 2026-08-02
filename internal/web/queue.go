package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/store"
)

// handleSyncNow runs a full sync immediately rather than waiting for the
// scheduled interval, so a document just added at a provider shows up without
// a restart. It blocks for the duration of the sync, on purpose: the queue
// page it redirects back to should already reflect what was fetched.
func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	if s.syncNow != nil {
		if err := s.syncNow(r.Context()); err != nil {
			s.logger.Error("manual sync failed", "error", err)
		}
	}
	s.redirect(w, r, "/")
}

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

	// Burying changes where an element sits within today rather than which day
	// it is due, so it bypasses the scheduler entirely.
	if grade == ir.GradeBury {
		if err := s.store.Bury(id, s.today()); err != nil {
			s.fail(w, err)
			return
		}
		s.redirect(w, r, "/next")
		return
	}

	// The whole scheduling decision is one pure function call. Everything
	// stateful — reading the row, writing it back — stays here.
	updated := ir.Next(element.Schedule, grade, s.today())

	if err := s.store.SaveSchedule(id, updated, time.Now()); err != nil {
		s.fail(w, err)
		return
	}

	// Finishing with an article here means finishing with it in wallabag too.
	// Without this the two views drift: it disappears from increader's queue
	// but sits in wallabag's Unread list forever, and the next reader to look
	// there sees a backlog that is not real.
	//
	// Only whole articles: an extract has no identity upstream.
	if element.IsRoot() && (grade == ir.GradeDone || grade == ir.GradeDismiss) {
		if err := s.archiveUpstream(element, true); err != nil {
			s.fail(w, err)
			return
		}
	}

	s.redirect(w, r, "/next")
}

// handleDeleteExtract permanently removes an extract or item.
//
// The case this exists for is exactly "accidental entry": a stray selection
// turned into an extract, or a wallabag highlight that never should have been
// made. That is why it is a hard delete rather than a state change like the
// grades — Dismiss keeps the row around for the record, this does not.
//
// DELETE rather than POST is the correct verb here, and it is also the one
// htmx sends parameters for as a query string by default (methodsThatUseUrlParams
// defaults to get and delete) — which is why swap_only, appended directly to
// the URL in the templates, reads back with the ordinary r.FormValue.
func (s *Server) handleDeleteExtract(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "whole articles cannot be deleted this way", http.StatusBadRequest)
		return
	}

	if err := s.store.DeleteExtract(id); err != nil {
		s.fail(w, err)
		return
	}

	// If the extract came from an imported wallabag highlight, DeleteExtract
	// queued its removal upstream in the same transaction. That queued write
	// wants to reach wallabag promptly, same as any other write-back, rather
	// than sit until the next scheduled sync.
	s.publishSoon()

	// The extracts browse page deletes a row in place — swap_only tells the
	// handler to answer with an empty 200 so htmx's outerHTML swap on the
	// containing <li> removes it, with no navigation and no lost scroll
	// position. The reader has no "in place" to swap to, since the page it was
	// looking at no longer exists, so it moves up to the parent instead.
	if r.FormValue("swap_only") == "1" {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.redirect(w, r, "/read/"+strconv.FormatInt(element.ParentID, 10))
}

// archiveUpstream records an article's read state locally and queues it for the
// provider.
//
// Local and queued together, in one transaction inside the store, so the two
// cannot disagree — and so the next sync sees no change and the archive
// transition does not fire on increader's own write.
func (s *Server) archiveUpstream(element store.Element, archived bool) error {
	document, err := s.store.DocumentByID(element.DocumentID)
	if err != nil {
		return err
	}
	if document.IsArchived == archived {
		return nil
	}
	if err := s.store.SetArchived(document.ID, document.Source, document.ExternalID,
		archived, time.Now()); err != nil {
		return err
	}
	s.publishSoon()
	return nil
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

	// Putting an article back into the reading queue means it is unread again.
	// The symmetric counterpart to archiving on Done — without it the article
	// stays archived upstream, and the next full sync would see the archive
	// transition and suspend it right back out of the queue.
	element, err := s.store.ElementByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}
	if element.IsRoot() {
		if err := s.archiveUpstream(element, false); err != nil {
			s.fail(w, err)
			return
		}
	}

	s.redirect(w, r, "/read/"+strconv.FormatInt(id, 10))
}

// handleStar toggles wallabag's favourite flag on an article.
func (s *Server) handleStar(w http.ResponseWriter, r *http.Request) {
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
	document, err := s.store.DocumentByID(element.DocumentID)
	if err != nil {
		s.fail(w, err)
		return
	}

	starred := r.FormValue("starred") == "1"
	if err := s.store.SetStarred(document.ID, document.Source, document.ExternalID,
		starred, time.Now()); err != nil {
		s.fail(w, err)
		return
	}
	s.publishSoon()

	s.redirect(w, r, "/read/"+strconv.FormatInt(id, 10))
}

// handleAddTag attaches a label to an article and queues it upstream.
func (s *Server) handleAddTag(w http.ResponseWriter, r *http.Request) {
	id, document, ok := s.tagTarget(w, r)
	if !ok {
		return
	}

	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		http.Error(w, "a tag needs a label", http.StatusBadRequest)
		return
	}
	// wallabag treats a comma as a separator, so one containing a comma would
	// silently become two tags that the reader never asked for.
	if strings.Contains(label, ",") {
		http.Error(w, "a tag cannot contain a comma", http.StatusBadRequest)
		return
	}

	if err := s.store.AttachTag(document.ID, document.Source, document.ExternalID, label); err != nil {
		s.fail(w, err)
		return
	}
	s.publishSoon()
	s.redirect(w, r, "/read/"+strconv.FormatInt(id, 10))
}

// handleRemoveTag detaches a label from an article and queues the removal.
func (s *Server) handleRemoveTag(w http.ResponseWriter, r *http.Request) {
	id, document, ok := s.tagTarget(w, r)
	if !ok {
		return
	}

	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		http.Error(w, "a tag needs a label", http.StatusBadRequest)
		return
	}

	if err := s.store.DetachTag(document.ID, document.Source, document.ExternalID, label); err != nil {
		s.fail(w, err)
		return
	}
	s.publishSoon()
	s.redirect(w, r, "/read/"+strconv.FormatInt(id, 10))
}

// tagTarget resolves the element in the request to the document its tags
// belong to. Tags live on articles, so an extract tags its parent's article.
func (s *Server) tagTarget(w http.ResponseWriter, r *http.Request) (int64, store.Document, bool) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return 0, store.Document{}, false
	}

	element, err := s.store.ElementByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return 0, store.Document{}, false
	}

	document, err := s.store.DocumentByID(element.DocumentID)
	if err != nil {
		s.fail(w, err)
		return 0, store.Document{}, false
	}
	return id, document, true
}

// handleBacklog puts an element off by a fixed number of days, immediately —
// the explicit counterpart to grading it, for the schedule panel's preset
// buttons and for the reschedule control on the extracts and library pages.
// See ir.Backlog and ir.BacklogOptions.
//
// Behaves exactly like a grade: it is a complete decision about this element,
// so it redirects rather than staying put and swapping something in place —
// on the reader page, that means on to whatever is next, same as pressing
// Next or Sooner would. A list page instead sends its own current URL as
// redirect, so rescheduling a row from there returns to that row's list
// rather than dropping into the reading queue.
func (s *Server) handleBacklog(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	days, err := strconv.Atoi(r.FormValue("days"))
	if err != nil || days < 1 {
		http.Error(w, "days must be a positive integer", http.StatusBadRequest)
		return
	}

	element, err := s.store.ElementByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}

	// Same as handleGrade: the reader tracks scroll position alongside a
	// backlog button click too, since putting an element off usually happens
	// mid-read, not only at the top of the page.
	if block, err := strconv.Atoi(r.FormValue("block")); err == nil && block >= 0 {
		if err := s.store.SetReadBlock(id, block); err != nil {
			s.fail(w, err)
			return
		}
	}

	element.Schedule = ir.Backlog(element.Schedule, days, s.today())
	if err := s.store.SaveSchedule(id, element.Schedule, time.Now()); err != nil {
		s.fail(w, err)
		return
	}

	s.redirect(w, r, redirectTarget(r, "/next"))
}

// redirectTarget reads where a POST wants to land afterwards, falling back
// to def if none was given. Only ever a path on this site: an absolute URL
// or a protocol-relative one (//host/…) would send the browser somewhere
// this handler never intended, so both are rejected in favour of the
// default rather than trusted as given.
func redirectTarget(r *http.Request, def string) string {
	target := r.FormValue("redirect")
	if target == "" || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return def
	}
	return target
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
	case "next":
		return ir.GradeNext, true
	case "sooner":
		return ir.GradeSooner, true
	case "bury":
		return ir.GradeBury, true
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
	State   string
	Tag     string
	Entries []store.LibraryEntry
	Counts  map[string]int
	Tags    []store.Tag
}

// handleLibrary lists and searches every synced document.
//
// The queue answers "what should I read now"; this answers "where is that
// article I remember". Both are needed, and conflating them would make the
// queue's ordering meaningless.
func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	filter := store.LibraryFilter{
		Query: strings.TrimSpace(query.Get("q")),
		State: query.Get("state"),
		Tag:   query.Get("tag"),
	}
	switch filter.State {
	case "", "unread", "starred", "archived", "annotated", "missing", "scheduled":
	default:
		http.Error(w, "unknown state filter", http.StatusBadRequest)
		return
	}

	entries, err := s.store.SearchDocuments(filter, s.today())
	if err != nil {
		s.fail(w, err)
		return
	}
	counts, err := s.store.CountByState("wallabag", s.today())
	if err != nil {
		s.fail(w, err)
		return
	}
	tags, err := s.store.AllTags()
	if err != nil {
		s.fail(w, err)
		return
	}

	s.render(w, "library.html", libraryData{
		Title:   "Library",
		Query:   filter.Query,
		State:   filter.State,
		Tag:     filter.Tag,
		Entries: entries,
		Counts:  counts,
		Tags:    tags,
	})
}

// handleDeleteDocument permanently removes a document no longer found
// upstream, and everything under it.
//
// Scoped deliberately to documents ReconcileMissing has flagged: this is not
// a general "delete any article" button. increader otherwise treats a
// document's lifecycle as wallabag's to decide, and one still found upstream
// would just be re-created on the very next sync anyway — so the guard here
// is not just caution, it is what keeps the button from being pointless.
func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r) // generic {id} path value, not element-specific
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	document, err := s.store.DocumentByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}
	if !document.MissingUpstream {
		http.Error(w, "this document still exists upstream; only one missing from wallabag can be deleted here",
			http.StatusBadRequest)
		return
	}

	if err := s.store.DeleteDocument(id); err != nil {
		s.fail(w, err)
		return
	}

	// The library page deletes a row in place — swap_only tells the handler
	// to answer with an empty 200 so htmx's outerHTML swap on the containing
	// <li> removes it, with no navigation and no lost scroll position or
	// filter state.
	if r.FormValue("swap_only") == "1" {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.redirect(w, r, "/library")
}

// extractsData is what the extracts browse page renders.
type extractsData struct {
	Title       string
	Extracts    []store.ExtractRow
	Query       string
	Origin      string
	WithClozes  bool
	MissingOnly bool
	Sort        string
	Imported    int
	Manual      int
	Missing     int
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
		Origin:      query.Get("origin"),
		WithClozes:  query.Get("clozes") == "1",
		MissingOnly: query.Get("missing") == "1",
		Sort:        query.Get("sort"),
		Query:       strings.TrimSpace(query.Get("q")),
	}
	if filter.Origin != "" && filter.Origin != store.OriginImport && filter.Origin != store.OriginManual {
		http.Error(w, "unknown origin filter", http.StatusBadRequest)
		return
	}
	switch filter.Sort {
	case "", "due", "priority", "oldest":
	default:
		http.Error(w, "unknown sort", http.StatusBadRequest)
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
	missing, err := s.store.CountMissingHighlights()
	if err != nil {
		s.fail(w, err)
		return
	}

	s.render(w, "extracts.html", extractsData{
		Title:       "Extracts",
		Extracts:    extracts,
		Query:       filter.Query,
		Origin:      filter.Origin,
		WithClozes:  filter.WithClozes,
		MissingOnly: filter.MissingOnly,
		Sort:        filter.Sort,
		Imported:    imported,
		Manual:      manual,
		Missing:     missing,
	})
}
