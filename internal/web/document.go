package web

import (
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/store"
)

// chapterGroup is one run of a document's annotations under a heading.
type chapterGroup struct {
	// Chapter is the heading, empty for annotations that carry none. An
	// empty group is rendered as "Without a chapter" rather than hidden:
	// a document with no outline puts everything there, and that is the
	// case most in need of being visible.
	Chapter string

	Annotations []store.ExtractRow
}

// documentData is the contents page for one work.
type documentData struct {
	Title    string
	Document store.Document
	RootID   int64

	Groups []chapterGroup
	Counts store.TriageCounts

	// Readable reports whether there is an article behind this document to
	// open. Uploaded annotation files have none.
	Readable bool
}

// handleDocument shows everything harvested from one work, in the order it
// appears in the original and grouped by chapter.
//
// This is the page an uploaded book has instead of a reader: there is no text
// to read, and what the reader wants is all of a work's passages in one place,
// arranged the way the work itself is. It is offered for synced articles too —
// an article with forty highlights benefits from the same view — but for those
// it is a second way of looking at something the reader page already shows.
func (s *Server) handleDocument(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}

	document, err := s.store.DocumentByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}

	annotations, err := s.store.DocumentAnnotations(id)
	if err != nil {
		s.fail(w, err)
		return
	}

	counts, err := s.store.CountTriage(id)
	if err != nil {
		s.fail(w, err)
		return
	}

	root, err := s.store.RootElement(id)
	if err != nil {
		s.fail(w, err)
		return
	}

	s.render(w, "document.html", documentData{
		Title:    document.Heading(),
		Document: document,
		RootID:   root.ID,
		Groups:   groupByChapter(annotations),
		Counts:   counts,
		Readable: s.readable(document),
	})
}

// readable reports whether a document has an article behind it to open.
//
// Two ways to have one: the body is already stored, or the provider it came
// from is configured and can still be asked for it. An uploaded annotation
// file has neither, and offering a link to a reader that would only produce
// an error is worse than not offering it.
func (s *Server) readable(document store.Document) bool {
	if document.HasContent {
		return true
	}
	_, configured := s.sources[document.Source]
	return configured
}

// groupByChapter splits annotations into runs sharing a chapter.
//
// Runs, not buckets: the annotations arrive in the original's own order, and
// grouping by equal-and-adjacent preserves it. Collecting every annotation
// with the same chapter name into one bucket instead would merge two
// genuinely separate passes through a chapter — a second reading of chapter
// three, months later — into something that reads as one, and would reorder
// a document whose headings recur, which "Notes" and "Introduction" reliably
// do.
func groupByChapter(annotations []store.ExtractRow) []chapterGroup {
	var groups []chapterGroup
	for _, annotation := range annotations {
		chapter := strings.TrimSpace(annotation.Chapter)
		if len(groups) == 0 || groups[len(groups)-1].Chapter != chapter {
			groups = append(groups, chapterGroup{Chapter: chapter})
		}
		last := &groups[len(groups)-1]
		last.Annotations = append(last.Annotations, annotation)
	}
	return groups
}

// handleDocumentTitles saves the reader's own name for a work, and — for an
// uploaded one — its author.
func (s *Server) handleDocumentTitles(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	err = s.store.UpdateDocumentTitles(id,
		strings.TrimSpace(r.FormValue("display_title")),
		strings.TrimSpace(r.FormValue("subtitle")))
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}

	// author is only ever posted by the rename form for an uploaded work
	// (see document.html) — absent for anything else, so a document whose
	// author a regular sync still owns is never touched here.
	if _, present := r.PostForm["author"]; present {
		if err := s.store.UpdateDocumentAuthor(id, strings.TrimSpace(r.FormValue("author"))); err != nil {
			s.notFoundOrFail(w, err)
			return
		}
	}

	s.redirect(w, r, "/documents/"+strconv.FormatInt(id, 10))
}

// triageData is one step of a document's triage pass.
type triageData struct {
	Title    string
	Document store.Document
	Element  store.Element
	Counts   store.TriageCounts

	// Body is the annotation as it will appear in the queue. It is the
	// element's own stored HTML, which for an imported annotation this
	// application built itself from escaped text — but it goes through the
	// sanitiser anyway, because "we wrote it" is a property of today's code
	// and the render path must not depend on it.
	Body template.HTML

	// Intervals and Backlog are the same schedule-panel data the reader page
	// computes for its own grade buttons (see readerData) — triage's "keep"
	// offers exactly the same choices grading does, so it needs exactly the
	// same previews, not a second calculation that could drift from them.
	Intervals map[string]string
	Backlog   []ir.BacklogOption
}

// handleTriage offers the next annotation awaiting a decision.
//
// A separate pass from the main queue on purpose. The queue interleaves
// everything by priority, which is right for reading but wrong for deciding:
// going through a book's annotations means going through them in the book's
// own order, one after another, with the chapter you were just in still in
// mind. And a four-hundred-passage import needs a gate before the queue, not
// after it.
func (s *Server) handleTriage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}

	document, err := s.store.DocumentByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}

	counts, err := s.store.CountTriage(id)
	if err != nil {
		s.fail(w, err)
		return
	}

	element, err := s.store.NextUntriaged(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Nothing left to decide. Back to the contents page, which is
			// where the finished pass is visible as a whole.
			http.Redirect(w, r, "/documents/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
			return
		}
		s.fail(w, err)
		return
	}

	previews := ir.Previews(element.Schedule, s.today())
	intervals := map[string]string{
		"next":   previews[ir.GradeNext].Interval,
		"sooner": previews[ir.GradeSooner].Interval,
	}

	s.render(w, "triage.html", triageData{
		Title:     "Triage · " + document.Heading(),
		Document:  document,
		Element:   element,
		Counts:    counts,
		Body:      template.HTML(s.sanitize(element.ContentHTML)),
		Intervals: intervals,
		Backlog:   ir.BacklogOptions(element.ID),
	})
}

// triageSchedule resolves the schedule a "keep" decision applies, from the
// same form values the reader's own grade and backlog buttons send: grade=
// next or grade=sooner for the schedule row's two grade buttons, days=N for
// a backlog preset. Triage's "keep" is meant to offer exactly the choices
// grading does, not a cruder version of them.
//
// The interval comes from ir.Next, the same computation behind the button's
// own label (see handleTriage's Intervals) — the preview is the behaviour,
// same rule ir.Previews itself follows. But only the interval survives: it
// is carried over via ir.Backlog rather than by persisting ir.Next's own
// result, because triage is a sorting pass, not an engagement with the
// material. Reps and A-Factor stay exactly what they were, same as an
// ordinary "not now" reschedule already leaves them — grading this for real
// still lies ahead, whenever it actually comes up in the queue.
//
// State becomes 'new' regardless of choice, the one thing that is not left
// alone: an untriaged import starts suspended (see ImportOptions.Triage),
// and "keep" is precisely the decision that ends that — the same transition
// KeepTriaged has always made.
func triageSchedule(current ir.Schedule, r *http.Request, today time.Time) (ir.Schedule, error) {
	var next ir.Schedule

	if days := r.FormValue("days"); days != "" {
		n, err := strconv.Atoi(days)
		if err != nil {
			return ir.Schedule{}, fmt.Errorf("bad backlog days %q", days)
		}
		next = ir.Backlog(current, n, today)
	} else {
		var grade ir.Grade
		switch r.FormValue("grade") {
		case "next":
			grade = ir.GradeNext
		case "sooner":
			grade = ir.GradeSooner
		default:
			return ir.Schedule{}, fmt.Errorf("unknown keep grade %q", r.FormValue("grade"))
		}
		computed := ir.Next(current, grade, today)
		next = ir.Backlog(current, int(math.Round(computed.IntervalDays)), today)
	}

	next.State = ir.StateNew
	return next, nil
}

// triageDecisions are the choices a triage pass offers besides "keep",
// which needs the request's own form values to resolve a schedule and so is
// handled separately in handleTriageDecision.
//
// Triage is not grading: nothing has been read yet, and the only question is
// whether this passage is worth having in the queue at all. Every one of
// these records the decision, so the pass always advances — an action that
// left the annotation undecided would show it again immediately and the pass
// would not move.
var triageDecisions = map[string]func(*Server, store.Element) error{
	// Suspend parks it. It stays in the library and on this contents page,
	// and can be queued later from either — the point of a triage pass is
	// that most of a book's annotations end here.
	"suspend": func(s *Server, element store.Element) error {
		return s.store.SuspendTriaged(element.ID, time.Now())
	},

	// Skip records that it was looked at and decided about, leaving whatever
	// schedule it already had. For a re-run of a pass, where most answers are
	// "as before".
	"skip": func(s *Server, element store.Element) error {
		return s.store.MarkTriaged(element.ID, time.Now())
	},

	// Drop removes it outright — the malformed PDF extraction, the highlight
	// made by a slipped finger. It goes through the same delete path the
	// extract pages use, so an imported wallabag highlight is removed
	// upstream too rather than arriving again on the next sync.
	"drop": func(s *Server, element store.Element) error {
		return s.deleteExtract(element)
	},
}

// handleTriageDecision records one triage decision and moves on.
func (s *Server) handleTriageDecision(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad element id", http.StatusBadRequest)
		return
	}

	decision := r.FormValue("decision")
	if decision != "keep" {
		if _, ok := triageDecisions[decision]; !ok {
			http.Error(w, "unknown triage decision", http.StatusBadRequest)
			return
		}
	}

	element, err := s.store.ElementByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}
	if element.IsRoot() {
		http.Error(w, "a document itself is not triaged", http.StatusBadRequest)
		return
	}

	if decision == "keep" {
		schedule, err := triageSchedule(element.Schedule, r, s.today())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.store.KeepTriaged(element.ID, schedule, time.Now()); err != nil {
			s.fail(w, err)
			return
		}
	} else if err := triageDecisions[decision](s, element); err != nil {
		s.fail(w, err)
		return
	}

	// Straight back into the pass rather than to a confirmation. Triage is a
	// rhythm — decide, next, decide — and anything between the decision and
	// the next passage breaks it.
	s.redirect(w, r, redirectTarget(r,
		"/documents/"+strconv.FormatInt(element.DocumentID, 10)+"/triage"))
}

// handleTriageReset forgets every decision so the pass can be made again.
func (s *Server) handleTriageReset(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}

	if err := s.store.ResetTriage(id); err != nil {
		s.fail(w, err)
		return
	}
	s.redirect(w, r, "/documents/"+strconv.FormatInt(id, 10)+"/triage")
}
