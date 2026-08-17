package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/store"
)

// doneTagLabel, dismissedTagLabel and suspendedTagLabel mark, upstream at the
// provider, which of increader's own terminal-ish states an article is in.
// wallabag's own archive flag covers Done and Dismiss alike (see
// archiveUpstream), so on its own it cannot tell "read to the end and
// annotated" from "abandoned unread" from "parked for later, still unread
// and unarchived"; these tags are what let that distinction survive outside
// increader too, in wallabag's own tag list — the same reasoning doneTagLabel
// started with, extended to the other two states a document can sit in
// without being in circulation.
const (
	doneTagLabel      = "done"
	dismissedTagLabel = "dismissed"
	suspendedTagLabel = "suspended"
)

// stateTagLabels maps each state that gets a tag of its own onto that tag's
// label. The three are mutually exclusive — a document is Done, Dismissed or
// Suspended, never more than one at once — which is what lets setStateTags
// use this table to attach the current state's tag and remove the other two
// in a single pass.
var stateTagLabels = map[ir.State]string{
	ir.StateDone:      doneTagLabel,
	ir.StateDismissed: dismissedTagLabel,
	ir.StateSuspended: suspendedTagLabel,
}

// handleSyncNow runs a full sync immediately rather than waiting for the
// scheduled interval, so a document just added at a provider shows up without
// a restart. It blocks for the duration of the sync, on purpose: the queue
// page it redirects back to should already reflect what was fetched.
//
// Returns to whichever queue the button was pressed from. A sync brings in new
// articles and new highlights alike, so both tabs have a reason to want it, and
// neither should be bounced to the other for asking.
func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	hasKind := r.URL.Query().Has("kind")
	kind, ok := requestQueueKind(w, r)
	if !ok {
		return
	}
	if s.syncNow != nil {
		if err := s.syncNow(r.Context()); err != nil {
			s.logger.Error("manual sync failed", "error", err)
		}
	}
	if !hasKind {
		// The combined queue page's own sync button (see handleQueue) —
		// back to the page it was pressed from, same as the single-kind
		// case below.
		s.redirect(w, r, "/queue")
		return
	}
	s.redirect(w, r, queuePath(kind))
}

// queueOf reports which queue an element belongs to. The two queues partition
// the elements table on exactly the distinction IsRoot already draws, so an
// element never has to be told which queue it came from — every redirect back
// into a session derives it from the element just acted on. That is what keeps
// grading an extract inside the extract queue and grading an article inside the
// reading queue, with no hidden form field to lose.
func queueOf(element store.Element) store.QueueKind {
	if element.IsRoot() {
		return store.QueueArticles
	}
	return store.QueueExtracts
}

// queuePath and nextPath build the two URLs a queue is reached by. Spelled out
// here rather than formatted at each call site, so the parameter name only
// exists in one place.
func queuePath(kind store.QueueKind) string { return "/queue?kind=" + string(kind) }
func nextPath(kind store.QueueKind) string  { return "/next?kind=" + string(kind) }

// requestQueueKind reads which queue a request is about, rejecting anything
// that is not one of the two rather than quietly falling back — a typo'd kind
// should not silently show the reading queue and leave the reader thinking
// their extracts have vanished.
func requestQueueKind(w http.ResponseWriter, r *http.Request) (store.QueueKind, bool) {
	kind, ok := store.ParseQueueKind(r.FormValue("kind"))
	if !ok {
		http.Error(w, "unknown queue", http.StatusBadRequest)
		return "", false
	}
	return kind, true
}

// queueData is what the queue page renders.
type queueData struct {
	Title string
	Kind  store.QueueKind
	Items []store.QueueItem

	// Due is this queue's own due count; ArticlesDue and ExtractsDue carry
	// both, since the tabs show a count each and the reader needs to see the
	// other queue filling up without having to open it.
	Due         int
	ArticlesDue int
	ExtractsDue int

	Total int
	Today time.Time

	// Combined renders both queues on one page — Items holds the articles
	// list and ExtractItems the extracts one, the latter behind a drawer —
	// rather than the single list a specific kind asks for. Set only by the
	// toolbar's own "Queue" link, which names no kind; every other link into
	// this page does, so a session already working through one queue is
	// never shown the other's list mixed in.
	Combined     bool
	ExtractItems []store.QueueItem
}

// IsExtracts reports which tab is active, for a template that cannot compare
// typed constants itself.
func (d queueData) IsExtracts() bool { return d.Kind == store.QueueExtracts }

// Truncated reports whether queuePageLimit cut the list short of what is
// actually due. Derived by comparing the rows rendered against the unlimited
// count rather than by looking at the limit itself, so it stays right however
// the limit is configured — and so the page can never again show sixty rows
// under a heading that says a hundred and thirty-seven are due, without
// saying which it means.
func (d queueData) Truncated() bool { return len(d.Items) < d.Due }

// Showing is how many rows were actually rendered.
func (d queueData) Showing() int { return len(d.Items) }

// handleQueue shows what is due today in one of the two queues, most important
// first.
//
// Articles and extracts used to interleave here, blended by a stored rank in
// proportion to how many of each were due. They are now two lists reached by
// the same page, because the interleave was answering a question — "what next,
// across everything?" — that a reader who works in article sessions and extract
// sessions never asks. See store.QueueKind for what that bought.
//
// queuePageLimit applies per queue rather than across both, and is zero — no
// limit — by default: everything due is listed. It caps rendering only. Nothing
// here or anywhere else caps a day's reading, and a queue that has fallen
// behind is a backlog to work through or postpone, not something to hide half
// of. When a limit is set and does cut the list short, the page says so; see
// queueData.Truncated.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if !r.URL.Query().Has("kind") {
		s.handleQueueCombined(w, r)
		return
	}

	kind, ok := requestQueueKind(w, r)
	if !ok {
		return
	}
	today := s.today()

	items, err := s.store.Queue(today, kind, s.queuePageLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	articlesDue, extractsDue, err := s.dueCounts(today)
	if err != nil {
		s.fail(w, err)
		return
	}
	total, err := s.store.CountElements("")
	if err != nil {
		s.fail(w, err)
		return
	}

	due := articlesDue
	title := "Queue"
	if kind == store.QueueExtracts {
		due, title = extractsDue, "Extract queue"
	}

	s.render(w, "queue.html", queueData{
		Title:       title,
		Kind:        kind,
		Items:       items,
		Due:         due,
		ArticlesDue: articlesDue,
		ExtractsDue: extractsDue,
		Total:       total,
		Today:       today,
	})
}

// handleQueueCombined is the toolbar's own "Queue" link: both queues on one
// page rather than the tabs above, articles as the page's own list and
// extracts behind a drawer (see queue.html) — the mirror of handleNext,
// which tries the extract queue first. Choosing "Queue" is choosing to look
// at an article; choosing "Next" is choosing to catch up on highlights.
//
// A separate function rather than a branch threaded through the code above:
// every redirect elsewhere in this file names a kind on purpose, to keep a
// session in the queue it started in (see requestQueueKind's own doc), and
// keeping this apart makes that invariant easy to see still holds — nothing
// above ever has to consider the combined case.
func (s *Server) handleQueueCombined(w http.ResponseWriter, r *http.Request) {
	today := s.today()

	articles, err := s.store.Queue(today, store.QueueArticles, s.queuePageLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	extracts, err := s.store.Queue(today, store.QueueExtracts, s.queuePageLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	articlesDue, extractsDue, err := s.dueCounts(today)
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
		Title:        "Queue",
		Kind:         store.QueueArticles,
		Items:        articles,
		Due:          articlesDue,
		ArticlesDue:  articlesDue,
		ExtractsDue:  extractsDue,
		Total:        total,
		Today:        today,
		Combined:     true,
		ExtractItems: extracts,
	})
}

// dueCounts reads both queues' due counts, which every page showing one of
// them also wants the other of.
func (s *Server) dueCounts(today time.Time) (articles, extracts int, err error) {
	articles, err = s.store.CountDue(today, store.QueueArticles)
	if err != nil {
		return 0, 0, err
	}
	extracts, err = s.store.CountDue(today, store.QueueExtracts)
	if err != nil {
		return 0, 0, err
	}
	return articles, extracts, nil
}

// handleNext jumps to the most important due element in one queue, or back to
// that queue's list when nothing is left.
//
// This is where grading lands, so that finishing one element moves straight
// into the next without a stop at the list — which is what makes a reading
// session feel like a session rather than a series of visits. The queue comes
// from the request rather than being rediscovered, so a session stays in the
// queue it started in instead of drifting into the other one.
func (s *Server) handleNext(w http.ResponseWriter, r *http.Request) {
	if !r.URL.Query().Has("kind") {
		s.handleNextEither(w, r)
		return
	}

	kind, ok := requestQueueKind(w, r)
	if !ok {
		return
	}

	items, err := s.store.Queue(s.today(), kind, 1)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(items) == 0 {
		http.Redirect(w, r, queuePath(kind), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/read/"+strconv.FormatInt(items[0].ID, 10), http.StatusSeeOther)
}

// handleNextEither is the toolbar's own "Next" link: extracts before
// articles, the mirror of the combined queue page (see handleQueueCombined),
// which shows articles first. Choosing "Next" is choosing to catch up on
// highlights before starting something new; choosing "Queue" is choosing to
// look at an article.
//
// Every other entry into a reading session names a kind explicitly and stays
// in it (requestQueueKind's own doc explains why), so this is the one place
// a session's queue is chosen rather than asked for.
func (s *Server) handleNextEither(w http.ResponseWriter, r *http.Request) {
	for _, kind := range [...]store.QueueKind{store.QueueExtracts, store.QueueArticles} {
		items, err := s.store.Queue(s.today(), kind, 1)
		if err != nil {
			s.fail(w, err)
			return
		}
		if len(items) > 0 {
			http.Redirect(w, r, "/read/"+strconv.FormatInt(items[0].ID, 10), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/queue", http.StatusSeeOther)
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
	// it is due, so it bypasses the scheduler entirely. The real clock, not
	// s.today()'s truncated date: repeated burying is ordered by the moment
	// each one happened, not just which calendar day — see Store.Bury.
	//
	// "Later today" is per queue now: Store.Queue's bury terms sit inside its
	// kind filter, so this sends the element to the back of its own queue, and
	// the redirect below returns to that same queue to pick up the next one.
	if grade == ir.GradeBury {
		if err := s.store.Bury(id, time.Now()); err != nil {
			s.fail(w, err)
			return
		}
		s.redirect(w, r, redirectTarget(r, nextPath(queueOf(element))))
		return
	}

	if err := s.applyGrade(element, grade); err != nil {
		s.fail(w, err)
		return
	}

	s.redirect(w, r, redirectTarget(r, nextPath(queueOf(element))))
}

// applyGrade is the whole scheduling and write-back effect of grading an
// element — everything handleGrade does beyond the request-specific bits
// (parsing the grade, recording the read block, the early return for Bury).
// Pulled out so the library's bulk actions bar can apply the very same grade
// a reader would give one article at a time, to many at once, without a
// second implementation of what "Done" or "Suspend" actually does.
func (s *Server) applyGrade(element store.Element, grade ir.Grade) error {
	// The whole scheduling decision is one pure function call. Everything
	// stateful — reading the row, writing it back — stays here.
	updated := ir.Next(element.Schedule, grade, s.today(), element.ID)

	if err := s.store.SaveScheduleReviewed(element.ID, updated, time.Now()); err != nil {
		return err
	}

	// Finishing with an article here means finishing with it in wallabag too.
	// Without this the two views drift: it disappears from increader's queue
	// but sits in wallabag's Unread list forever, and the next reader to look
	// there sees a backlog that is not real. Suspend counts as finishing with
	// it for this purpose too — it is the same "out of the queue" outcome as
	// Done or Dismiss, and matches applyUnsuspend un-archiving on the way
	// back in, and archived-articles-arrive-suspended on import.
	//
	// Only whole articles: an extract has no identity upstream.
	if element.IsRoot() && (grade == ir.GradeDone || grade == ir.GradeDismiss || grade == ir.GradeSuspend) {
		if err := s.archiveUpstream(element, true); err != nil {
			return err
		}
	}

	// The tags track the state transition, not the grade in isolation: one
	// goes on when a grade lands the element on Done, Dismissed or Suspended,
	// and comes back off if something already in one of those states gets
	// graded again into anything else — same as unsuspending clears it.
	// Skipped entirely unless the state is actually moving into or out of one
	// of the three: grading is mostly Next and Sooner, the everyday case, and
	// those should not pay for a tag lookup that can never change anything.
	if element.IsRoot() {
		_, wasTagged := stateTagLabels[element.Schedule.State]
		_, isTagged := stateTagLabels[updated.State]
		if wasTagged || isTagged {
			if err := s.setStateTags(element.DocumentID, updated.State); err != nil {
				return err
			}
		}
	}
	return nil
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

	if err := s.deleteExtract(element); err != nil {
		s.fail(w, err)
		return
	}

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

// deleteExtract removes one extract and pushes the removal upstream.
//
// Split out from the handler so the triage pass can drop a passage through
// exactly the same path — the same convention applyGrade and applyUnsuspend
// follow, and for the same reason: a second caller must not be a second
// implementation. Getting this wrong for an imported highlight is not a
// cosmetic bug, it is the extract coming back on the next sync.
func (s *Server) deleteExtract(element store.Element) error {
	if err := s.store.DeleteExtract(element.ID); err != nil {
		return err
	}

	// If the extract came from an imported wallabag highlight, DeleteExtract
	// queued its removal upstream in the same transaction. That queued write
	// wants to reach wallabag promptly, same as any other write-back, rather
	// than sit until the next scheduled sync.
	s.publishSoon()
	return nil
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

// setStateTags brings a document's upstream tags in line with the state it
// just landed in: the tag for state, if it has one in stateTagLabels, is
// attached, and the tags for the other two states in that table are removed.
// The write-back counterpart to the library's Done/Dismissed/Suspended
// states, generalising what used to be a Done-only tag — see doneTagLabel.
//
// Guarded per tag the same way archiveUpstream guards the archive flag:
// a tag already in the state it should be in is left alone, so re-grading an
// already-Done article Done again, say, queues nothing.
func (s *Server) setStateTags(documentID int64, state ir.State) error {
	document, err := s.store.DocumentByID(documentID)
	if err != nil {
		return err
	}
	labels, err := s.store.TagsOf(documentID)
	if err != nil {
		return err
	}
	current := make(map[string]bool, len(labels))
	for _, label := range labels {
		current[label] = true
	}

	for candidate, tag := range stateTagLabels {
		want := candidate == state
		if current[tag] == want {
			continue
		}
		if want {
			err = s.store.AttachTag(documentID, document.Source, document.ExternalID, tag)
		} else {
			err = s.store.DetachTag(documentID, document.Source, document.ExternalID, tag)
		}
		if err != nil {
			return err
		}
		s.publishSoon()
	}
	return nil
}

// handleUnsuspend returns a suspended element to the queue.
//
// The counterpart to suspending, and also how an archived article is pulled
// back in for re-reading — archived material arrives suspended, so there is
// only one mechanism to understand.
//
// Defaults to landing back on the reader, same as pressing the button there
// does, but honours redirect the same way handleBacklog does — the library's
// "queue it" sends its own current URL, so unsuspending a row from a filtered
// list stays on that list instead of jumping into the article.
func (s *Server) handleUnsuspend(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.applyUnsuspend(id); err != nil {
		s.notFoundOrFail(w, err)
		return
	}

	s.redirect(w, r, redirectTarget(r, "/read/"+strconv.FormatInt(id, 10)))
}

// applyUnsuspend is the whole effect of unsuspending an element, pulled out
// of handleUnsuspend the same way applyGrade was pulled out of handleGrade —
// so the library's bulk "queue it" action can do exactly this to many rows
// at once rather than reimplementing it.
func (s *Server) applyUnsuspend(id int64) error {
	if err := s.store.Unsuspend(id, s.today(), time.Now()); err != nil {
		return err
	}

	// Putting an article back into the reading queue means it is unread again.
	// The symmetric counterpart to archiving on Done — without it the article
	// stays archived upstream, and the next full sync would see the archive
	// transition and suspend it right back out of the queue.
	element, err := s.store.ElementByID(id)
	if err != nil {
		return err
	}
	if element.IsRoot() {
		if err := s.archiveUpstream(element, false); err != nil {
			return err
		}
		// Bringing something back into circulation should not leave a stale
		// Done, Dismissed or Suspended tag behind — setStateTags is a no-op
		// on whichever of the three was not actually there.
		if err := s.setStateTags(element.DocumentID, ir.StateReading); err != nil {
			return err
		}
	}
	return nil
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
// on the reader page, that means on to whatever is next in the same queue this
// element belongs to, exactly as pressing Next or Sooner would. A list page
// instead sends its own current URL as redirect, so rescheduling a row from
// there returns to that row's list rather than dropping into a queue.
func (s *Server) handleBacklog(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 0 is valid — it is the "today" preset, for undoing a backlog.
	days, err := strconv.Atoi(r.FormValue("days"))
	if err != nil || days < 0 {
		http.Error(w, "days must not be negative", http.StatusBadRequest)
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

	s.redirect(w, r, redirectTarget(r, nextPath(queueOf(element))))
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
	Title       string
	Query       string
	State       string
	Tag         string
	Sort        string
	Entries     []store.LibraryEntry
	Counts      map[string]int
	Tags        []store.Tag
	CurrentURL  string
	BulkActions []libraryBulkAction

	// Notice is a one-line message carried through a redirect's "notice"
	// query parameter — see withNotice in prefetch.go, its only source
	// today. There is no session-based flash mechanism in this app; a query
	// parameter read back on the very next GET is the whole of it.
	Notice string
}

// libraryBulkAction is one action the library's selection bar can apply to
// every checked row in a single request — the same effect its single-row
// equivalent already has elsewhere in the app, reused rather than
// reimplemented for the many-at-once case. Value and Label render its
// button; Confirm, when set, is shown before the button's own form submits;
// apply is the effect itself.
type libraryBulkAction struct {
	Value   string
	Label   string
	Confirm string
	Danger  bool
	apply   func(*Server, store.Element) error
}

// libraryBulkActions lists every bulk action the library offers, in the
// order their buttons appear. Extending the selection bar with another one —
// a bulk "star", say — is exactly one more entry here, built on whatever
// single-element function already exists for it; neither the handler below
// nor the template need to change.
var libraryBulkActions = []libraryBulkAction{
	{
		Value: "queue", Label: "Queue it",
		apply: func(s *Server, element store.Element) error {
			return s.applyUnsuspend(element.ID)
		},
	},
	{
		Value: "suspend", Label: "Suspend",
		apply: func(s *Server, element store.Element) error {
			return s.applyGrade(element, ir.GradeSuspend)
		},
	},
	{
		Value: "done", Label: "Done",
		Confirm: "Mark the selected articles Done? This archives them in wallabag too.",
		apply: func(s *Server, element store.Element) error {
			return s.applyGrade(element, ir.GradeDone)
		},
	},
	{
		Value: "dismiss", Label: "Dismiss", Danger: true,
		Confirm: "Dismiss the selected articles, unread? This archives them in wallabag too.",
		apply: func(s *Server, element store.Element) error {
			return s.applyGrade(element, ir.GradeDismiss)
		},
	},
	{
		Value: fetchBodiesBulkAction, Label: "Fetch bodies",
		// apply is deliberately left nil: fetching bodies runs in the
		// background rather than once per element in handleLibraryBulk's own
		// loop, since a hundred article fetches would blow any sensible
		// request timeout. handleLibraryBulk intercepts this action's value
		// before ever reaching findLibraryBulkAction — see
		// handleFetchBodiesBulk in prefetch.go.
	},
}

// findLibraryBulkAction looks up a bulk action by its form value.
func findLibraryBulkAction(value string) (libraryBulkAction, bool) {
	for _, action := range libraryBulkActions {
		if action.Value == value {
			return action, true
		}
	}
	return libraryBulkAction{}, false
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
		Sort:  query.Get("sort"),
	}
	switch filter.State {
	case "", "books", "unread", "starred", "archived", "annotated", "missing", "scheduled", "suspended", "done":
	default:
		http.Error(w, "unknown state filter", http.StatusBadRequest)
		return
	}
	switch filter.Sort {
	case "", "due", "priority", "oldest":
	default:
		http.Error(w, "unknown sort", http.StatusBadRequest)
		return
	}

	entries, err := s.store.SearchDocuments(filter, s.today())
	if err != nil {
		s.fail(w, err)
		return
	}
	// Counted across every source, not just wallabag. SearchDocuments has
	// never filtered by source, so naming one here made the tabs disagree
	// with the list under them the moment uploads could create documents.
	counts, err := s.store.CountByState("", s.today())
	if err != nil {
		s.fail(w, err)
		return
	}
	tags, err := s.store.AllTags()
	if err != nil {
		s.fail(w, err)
		return
	}

	// notice is shown once and then dropped from CurrentURL before it feeds
	// into every filter link and the bulk form's own hidden redirect field
	// (see setParams in library.html and withNotice in prefetch.go) — left
	// in, it would keep announcing "fetching N bodies" on every navigation
	// from this page onward instead of just the one that just landed here.
	notice := query.Get("notice")
	currentURL := *r.URL
	if notice != "" {
		withoutNotice := query
		withoutNotice.Del("notice")
		currentURL.RawQuery = withoutNotice.Encode()
	}

	s.render(w, "library.html", libraryData{
		Title:       "Library",
		Query:       filter.Query,
		State:       filter.State,
		Tag:         filter.Tag,
		Sort:        filter.Sort,
		Entries:     entries,
		Counts:      counts,
		Tags:        tags,
		CurrentURL:  currentURL.RequestURI(),
		BulkActions: libraryBulkActions,
		Notice:      notice,
	})
}

// handleLibraryBulk applies one action to every row checked in the library's
// selection bar. It is a plain HTML form post rather than an htmx one — the
// button that submits it carries the action as its own value, so no
// JavaScript is needed to route a row's worth of checkboxes to an effect;
// see library.html's "select all" and confirmation prompt for what little
// JavaScript is layered on top for convenience.
//
// A row that no longer exists — deleted, or merged away, in the moment
// between the page loading and the button being pressed — is skipped rather
// than aborting the rest of the batch; nothing about a stale selection
// should stop everything else the reader actually meant to act on. Any other
// error does abort, since that means something is actually wrong rather than
// just out of date.
func (s *Server) handleLibraryBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	// "Fetch bodies" is asynchronous, unlike every other bulk action here, so
	// it is intercepted before the synchronous apply-per-element loop below
	// rather than made to fit that loop's shape — see handleFetchBodiesBulk.
	if r.FormValue("action") == fetchBodiesBulkAction {
		s.handleFetchBodiesBulk(w, r)
		return
	}

	action, ok := findLibraryBulkAction(r.FormValue("action"))
	if !ok {
		http.Error(w, "unknown bulk action", http.StatusBadRequest)
		return
	}

	for _, raw := range r.Form["ids"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}

		element, err := s.store.ElementByID(id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			s.fail(w, err)
			return
		}

		if err := action.apply(s, element); err != nil {
			s.fail(w, err)
			return
		}
	}

	s.redirect(w, r, redirectTarget(r, "/library"))
}

// handleDeleteDocument permanently removes a document and everything under
// it, both here and at the provider.
//
// Used to be scoped to documents ReconcileMissing had already flagged gone
// upstream, back when there was no way to reach wallabag from here at all —
// deleting one still present would just have been re-created on the very
// next sync. Store.DeleteDocument now queues the provider-side removal in
// the same transaction as the local one (see OpEntryDelete), which is what
// makes this safe to offer everywhere in the library instead: the entry
// actually leaves wallabag too, so there is nothing left to bring it back.
func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r) // generic {id} path value, not element-specific
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.store.DeleteDocument(id); err != nil {
		s.notFoundOrFail(w, err)
		return
	}
	s.publishSoon()

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
	CurrentURL  string
	BulkActions []extractBulkAction

	// Matching is how many extracts the current filter selects in total, as
	// against the len(Extracts) actually rendered. The page has no paging, so
	// a large harvest is always cut off somewhere; showing both numbers is
	// what keeps that a visible cap rather than a silently shortened list.
	Matching int
	Limit    int
}

// Truncated reports whether the filter matched more than the page rendered.
func (d extractsData) Truncated() bool { return d.Matching > len(d.Extracts) }

// Showing is how many rows were actually rendered.
func (d extractsData) Showing() int { return len(d.Extracts) }

// extractBulkAction is the extracts page's counterpart to libraryBulkAction:
// same shape, applied to a checked extract rather than a checked document,
// reusing whatever single-extract function its own per-row control already
// calls rather than a second implementation for the many-at-once case.
type extractBulkAction struct {
	Value   string
	Label   string
	Confirm string
	Danger  bool
	apply   func(*Server, store.Element) error
}

// extractBulkActions lists every bulk action the extracts page offers.
// Deliberately just the one to start: delete is the gap a reader actually
// hits browsing a large harvest (see deleteExtract), where rescheduling or
// grading a handful at once is served well enough by the per-row controls
// already there.
var extractBulkActions = []extractBulkAction{
	{
		Value: "delete", Label: "Delete", Danger: true,
		Confirm: "Delete the selected extracts? Imported ones are also removed from wallabag. This cannot be undone.",
		apply: func(s *Server, element store.Element) error {
			return s.deleteExtract(element)
		},
	},
}

// findExtractBulkAction looks up a bulk action by its form value.
func findExtractBulkAction(value string) (extractBulkAction, bool) {
	for _, action := range extractBulkActions {
		if action.Value == value {
			return action, true
		}
	}
	return extractBulkAction{}, false
}

// handleExtracts lists everything harvested, independently of what is due.
//
// Separate from the extract queue on purpose, and not made redundant by it.
// That queue answers "which passages are due now", ordered by priority and
// capped at a day's worth. This page answers a different question — what have
// I pulled out, and what still needs turning into cards — over everything
// harvested, due or not, with its own filters and orderings.
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
	matching, err := s.store.CountMatchingExtracts(filter)
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
		CurrentURL:  r.URL.RequestURI(),
		BulkActions: extractBulkActions,
		Matching:    matching,
		Limit:       store.ExtractsPageLimit,
	})
}

// handleExtractsBulk applies one action to every extract checked in the
// extracts page's selection bar — the same plain-form, no-JavaScript-required
// pattern as handleLibraryBulk, and for the same reasons: see its own doc
// comment for why a missing row is skipped rather than aborting the batch.
func (s *Server) handleExtractsBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	action, ok := findExtractBulkAction(r.FormValue("action"))
	if !ok {
		http.Error(w, "unknown bulk action", http.StatusBadRequest)
		return
	}

	for _, raw := range r.Form["ids"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}

		element, err := s.store.ElementByID(id)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			s.fail(w, err)
			return
		}
		// The selection bar only ever lists extracts, never a whole article;
		// guarded here too rather than trusted from the client, the same
		// caution handleDeleteExtract applies to a single id.
		if element.IsRoot() {
			continue
		}

		if err := action.apply(s, element); err != nil {
			s.fail(w, err)
			return
		}
	}

	s.redirect(w, r, redirectTarget(r, "/extracts"))
}
