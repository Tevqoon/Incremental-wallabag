package web

import (
	"html/template"
	"net/http"
	"strconv"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/store"
)

// watchQueueLimit caps how many due items the watch queue offers. Smaller
// than the desktop daily_limit on purpose: this list is read by scrolling a
// screen a few centimetres across, not skimmed.
const watchQueueLimit = 30

// watchQueueData is what /w renders.
type watchQueueData struct {
	Title string
	Items []store.QueueItem
	Due   int
}

// handleWatchQueue is the watch-shaped counterpart to handleQueue: same
// data, a layout with no htmx and no nav bar, sized for a wrist rather than
// a browser window. See templates/watch_layout.html for why it exists.
func (s *Server) handleWatchQueue(w http.ResponseWriter, r *http.Request) {
	today := s.today()

	items, err := s.store.Queue(today, watchQueueLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	due, err := s.store.CountDue(today)
	if err != nil {
		s.fail(w, err)
		return
	}

	s.render(w, "watch_queue.html", watchQueueData{
		Title: "Queue",
		Items: items,
		Due:   due,
	})
}

// handleWatchNext is the watch-shaped counterpart to handleNext: jumps
// straight to the most important due element, so grading one on the watch
// page chains into the next without a stop at the list.
func (s *Server) handleWatchNext(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.Queue(s.today(), 1)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(items) == 0 {
		http.Redirect(w, r, "/w", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/w/read/"+strconv.FormatInt(items[0].ID, 10), http.StatusSeeOther)
}

// watchReadData is what /w/read/{id} renders.
type watchReadData struct {
	Title       string
	Element     store.Element
	Document    store.Document
	ArticleHTML template.HTML
	Intervals   map[string]string
}

// handleWatchRead is the watch-shaped counterpart to handleRead: the passage
// and nothing else that depends on JavaScript — no selection toolbar, no
// cloze editing, no MathJax. Grading posts through the same
// /elements/{id}/grade handler the full reader uses.
func (s *Server) handleWatchRead(w http.ResponseWriter, r *http.Request) {
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

	// Same guard handleRead applies: a document with no fetchable body has
	// nothing for this view either, so send the reader to its contents page.
	if element.IsRoot() && !s.readable(document) {
		http.Redirect(w, r, "/documents/"+strconv.FormatInt(document.ID, 10), http.StatusSeeOther)
		return
	}

	article, marks, imageURLs, err := s.parseArticle(r.Context(), element)
	if err != nil {
		s.fail(w, err)
		return
	}

	// Same source as the full reader's button labels: never a parallel
	// estimate that could drift from what grading would actually do.
	previews := ir.Previews(element.Schedule, s.today(), element.ID)
	intervals := map[string]string{
		"next":   previews[ir.GradeNext].Interval,
		"sooner": previews[ir.GradeSooner].Interval,
	}

	title := document.Heading()
	if !element.IsRoot() {
		title = element.Title
	}

	s.render(w, "watch_read.html", watchReadData{
		Title:    title,
		Element:  element,
		Document: document,
		ArticleHTML: template.HTML(article.Render(ir.RenderOptions{
			Marks:     marks,
			ReadPoint: element.ReadBlock,
			ImageURLs: imageURLs,
		})),
		Intervals: intervals,
	})
}
