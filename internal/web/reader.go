package web

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/store"
)

// readerData is what the reader page renders.
type readerData struct {
	Title        string
	Element      store.Element
	Document     store.Document
	ArticleHTML  template.HTML
	Extracts     []store.Element
	Clozes       []ir.Cloze
	ClozePreview string
	Remaining    int
}

// handleRead shows one element: an article to read, or an extract to refine.
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
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

	article, marks, err := s.parseArticle(r.Context(), element)
	if err != nil {
		s.fail(w, err)
		return
	}

	children, err := s.store.ChildrenOf(element.ID)
	if err != nil {
		s.fail(w, err)
		return
	}

	clozes, err := s.store.ClozesOf(element.ID)
	if err != nil {
		s.fail(w, err)
		return
	}

	preview := ""
	if len(clozes) > 0 {
		// A malformed set of deletions should not stop the page rendering; the
		// reader still needs to see the text in order to fix them.
		if rendered, err := ir.RenderCloze(element.Quote, clozes); err == nil {
			preview = rendered
		}
	}

	due, err := s.store.CountDue(s.today())
	if err != nil {
		s.fail(w, err)
		return
	}

	title := element.Title
	if title == "" {
		title = document.Title
	}

	s.render(w, "reader.html", readerData{
		Title:    title,
		Element:  element,
		Document: document,
		// Marked as safe because it was produced by ir.Render from sanitised
		// input, not because it came from the article. Every path into this
		// field runs through the sanitiser first.
		ArticleHTML:  template.HTML(article.Render(marks)),
		Extracts:     children,
		Clozes:       clozes,
		ClozePreview: preview,
		Remaining:    due,
	})
}

// selection is the payload the browser sends for a highlighted passage.
type selection struct {
	Range ir.Range
	Quote string
}

// parseSelection reads and validates a selection from a form post.
func parseSelection(r *http.Request) (selection, error) {
	field := func(name string) (int, error) {
		value, err := strconv.Atoi(r.FormValue(name))
		if err != nil {
			return 0, fmt.Errorf("web: %s must be an integer: %w", name, err)
		}
		return value, nil
	}

	var (
		result selection
		err    error
	)
	if result.Range.StartBlock, err = field("start_block"); err != nil {
		return selection{}, err
	}
	if result.Range.StartOffset, err = field("start_offset"); err != nil {
		return selection{}, err
	}
	if result.Range.EndBlock, err = field("end_block"); err != nil {
		return selection{}, err
	}
	if result.Range.EndOffset, err = field("end_offset"); err != nil {
		return selection{}, err
	}

	result.Quote = r.FormValue("quote")
	if strings.TrimSpace(result.Quote) == "" {
		return selection{}, errors.New("web: the selection is empty")
	}
	return result, nil
}

// handleExtract turns a highlighted passage into a child element.
func (s *Server) handleExtract(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	chosen, err := parseSelection(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	parent, err := s.store.ElementByID(id)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}

	article, marks, err := s.parseArticle(r.Context(), parent)
	if err != nil {
		s.fail(w, err)
		return
	}

	// Re-derive the passage from the server's own copy and check it against
	// what the browser reported. If the two disagree the offsets are stale —
	// usually because the article was re-fetched and changed shape — and
	// saving the extract anyway would silently attach it to the wrong text.
	// Comparison is whitespace-insensitive because browsers disagree about the
	// separator between blocks.
	serverText, err := article.Text(chosen.Range)
	if err != nil {
		http.Error(w, "that selection no longer fits the article; reload and try again",
			http.StatusConflict)
		return
	}
	if ir.NormalizeSpace(serverText) != ir.NormalizeSpace(chosen.Quote) {
		s.logger.Warn("selection did not match the stored article",
			"element", id, "browser", chosen.Quote, "server", serverText)
		http.Error(w, "that selection no longer matches the article; reload and try again",
			http.StatusConflict)
		return
	}

	extractHTML, err := article.HTML(chosen.Range)
	if err != nil {
		s.fail(w, err)
		return
	}

	newID, err := s.store.CreateExtract(store.NewExtract{
		ParentID:    parent.ID,
		DocumentID:  parent.DocumentID,
		Kind:        store.KindTopic,
		Title:       summarise(serverText),
		ContentHTML: extractHTML,
		Quote:       serverText,
		Range:       chosen.Range,
		HasRange:    true,
		// An extract inherits its parent's priority: the reason a passage was
		// worth pulling out is that its article was worth reading.
		Priority: parent.Schedule.Priority,
	}, time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}

	// Re-render the article with the new highlight in place. Returning the
	// fragment rather than redirecting is what keeps the reader's scroll
	// position, which matters when extracting from the middle of a long piece.
	marks = append(marks, ir.Mark{Range: chosen.Range, ElementID: newID})
	s.writeArticleFragment(w, parent.ID, article.Render(marks))
}

// handleCloze marks a deletion on an extract, producing a card for export.
func (s *Server) handleCloze(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "clozes are made on extracts, not on whole articles",
			http.StatusBadRequest)
		return
	}

	start, startErr := strconv.Atoi(r.FormValue("start"))
	end, endErr := strconv.Atoi(r.FormValue("end"))
	if startErr != nil || endErr != nil {
		http.Error(w, "start and end must be integers", http.StatusBadRequest)
		return
	}
	if start < 0 || end > len(element.Quote) || end <= start {
		http.Error(w, "that deletion does not fit the extract", http.StatusBadRequest)
		return
	}

	if _, err := s.store.AddCloze(id, start, end, r.FormValue("hint")); err != nil {
		s.fail(w, err)
		return
	}

	// Promote the extract to an item the first time a deletion is added: it
	// now produces a card, which is what distinguishes the two kinds.
	if element.Kind != store.KindItem {
		if err := s.store.SetKind(id, store.KindItem, time.Now()); err != nil {
			s.fail(w, err)
			return
		}
	}

	s.redirect(w, r, "/read/"+strconv.FormatInt(id, 10))
}

// writeArticleFragment returns the article container for an htmx swap.
func (s *Server) writeArticleFragment(w http.ResponseWriter, elementID int64, rendered string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div id="article" data-element="%d">%s</div>`, elementID, rendered)
}

// summarise builds a short title for an extract from its opening words.
func summarise(text string) string {
	const limit = 80

	normalised := ir.NormalizeSpace(text)
	if len(normalised) <= limit {
		return normalised
	}

	// Cut at a word boundary so the title does not end mid-word.
	truncated := normalised[:limit]
	if space := strings.LastIndex(truncated, " "); space > limit/2 {
		truncated = truncated[:space]
	}
	return truncated + "…"
}

// isNotFound reports whether an error came from a missing row.
func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
