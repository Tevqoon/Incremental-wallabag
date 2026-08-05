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
	ClozeRows    []clozeRow
	ClozePreview string
	Remaining    int
	Tags         []string
	AllTags      []store.Tag

	// Intervals labels each grade button with what it would actually do,
	// keyed by the form value the button posts.
	Intervals map[string]string

	// Backlog is the resolved (fuzz applied) preset durations for the
	// schedule panel's "put this off" buttons — see ir.BacklogOptions.
	Backlog []ir.BacklogOption
}

// clozeRow is one deletion as the reader needs to manage it individually —
// ir.Cloze plus the text it actually covers, sliced out here because a
// template has no clean way to index a string by a pair of byte offsets
// itself.
type clozeRow struct {
	Ordinal int
	Text    string
	Hint    string
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

	// A document that arrived as an uploaded annotation file has no body and
	// no provider to fetch one from, so there is nothing here to read. Its
	// contents page is the equivalent view, and sending the reader there is
	// better than the 500 that fetching a body from an unconfigured source
	// would otherwise produce. Only for the root: an extract from such a
	// document carries its own text and reads perfectly well.
	if element.IsRoot() && !s.readable(document) {
		http.Redirect(w, r, "/documents/"+strconv.FormatInt(document.ID, 10), http.StatusSeeOther)
		return
	}

	article, marks, imageURLs, err := s.parseArticle(r.Context(), element)
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

	// Sliced out per cloze so each one can be managed — and deleted —
	// individually, rather than only seen as part of the combined preview
	// below. Bounds are re-checked here rather than trusted, the same
	// defensiveness RenderCloze already applies just below: the extract's
	// own quote cannot change once stored, but nothing stops guarding
	// against it anyway rather than a page that fails to render at all.
	clozeRows := make([]clozeRow, 0, len(clozes))
	for _, cloze := range clozes {
		if cloze.Start < 0 || cloze.End > len(element.Quote) || cloze.End <= cloze.Start {
			continue
		}
		clozeRows = append(clozeRows, clozeRow{
			Ordinal: cloze.Ordinal,
			Text:    element.Quote[cloze.Start:cloze.End],
			Hint:    cloze.Hint,
		})
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

	tags, err := s.store.TagsOf(document.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	allTags, err := s.store.AllTags()
	if err != nil {
		s.fail(w, err)
		return
	}

	// Each button is labelled with the interval its grade would produce. The
	// previews come from the scheduler itself rather than a parallel
	// calculation, so a button cannot advertise something that would not happen.
	previews := ir.Previews(element.Schedule, s.today(), element.ID)
	intervals := map[string]string{
		"next":   previews[ir.GradeNext].Interval,
		"sooner": previews[ir.GradeSooner].Interval,
	}
	backlog := ir.BacklogOptions(element.ID)

	// The page heading is always the article's own title, not the extract's
	// stored title: that field is a truncated echo of the passage, which is
	// already right there in the body below — showing it again as the
	// heading just repeats it.
	title := document.Heading()

	s.render(w, "reader.html", readerData{
		Title:    title,
		Element:  element,
		Document: document,
		// Marked as safe because it was produced by ir.Render from sanitised
		// input, not because it came from the article. Every path into this
		// field runs through the sanitiser first.
		ArticleHTML: template.HTML(article.Render(ir.RenderOptions{
			Marks:     marks,
			ReadPoint: element.ReadBlock,
			ImageURLs: imageURLs,
		})),
		Extracts:     children,
		Clozes:       clozes,
		ClozeRows:    clozeRows,
		ClozePreview: preview,
		Remaining:    due,
		Tags:         tags,
		AllTags:      allTags,
		Intervals:    intervals,
		Backlog:      backlog,
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

	article, marks, imageURLs, err := s.parseArticle(r.Context(), parent)
	if err != nil {
		s.fail(w, err)
		return
	}

	// The browser measures its selection in runes; everything from here on
	// works in bytes, like the rest of this package. See Article.ByteOffset.
	byteRange, ok := article.ByteRange(chosen.Range)
	if !ok {
		// Same status as a stale-quote mismatch below: an out-of-range block
		// means the article no longer has the shape the browser saw.
		http.Error(w, "that selection no longer fits the article; reload and try again",
			http.StatusConflict)
		return
	}
	chosen.Range = byteRange

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
		Title:       store.SummariseQuote(serverText),
		ContentHTML: extractHTML,
		Quote:       serverText,
		Range:       chosen.Range,
		HasRange:    true,
		// An extract inherits its parent's priority: the reason a passage was
		// worth pulling out is that its article was worth reading.
		Priority:  parent.Schedule.Priority,
		DelayDays: s.extractDelay,
	}, time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}

	// CreateExtract may have just queued pushing this upstream as a new
	// wallabag annotation. Nudge it out promptly, the same as every other
	// write-back in this package, rather than leaving it to wait for the
	// next scheduled sync.
	s.publishSoon()

	// Re-render the article with the new highlight in place. Returning the
	// fragment rather than redirecting is what keeps the reader's scroll
	// position, which matters when extracting from the middle of a long piece.
	marks = append(marks, ir.Mark{Range: chosen.Range, ElementID: newID})
	s.writeArticleFragment(w, parent.ID, article.Render(ir.RenderOptions{
		Marks:     marks,
		ReadPoint: parent.ReadBlock,
		ImageURLs: imageURLs,
	}))
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

	start, end, err := s.clozeOffsets(r, element)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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

// handleDeleteCloze removes one deletion from an item, addressed by the same
// ordinal Anki would turn into a card number — not a database id, which
// nothing outside this package ever sees.
func (s *Server) handleDeleteCloze(w http.ResponseWriter, r *http.Request) {
	id, err := elementID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ordinal, err := strconv.Atoi(r.PathValue("ordinal"))
	if err != nil {
		http.Error(w, "invalid cloze ordinal", http.StatusBadRequest)
		return
	}

	if err := s.store.DeleteCloze(id, ordinal); err != nil {
		s.notFoundOrFail(w, err)
		return
	}

	// An item is defined by having at least one deletion — that is the whole
	// distinction from a plain extract — so removing the last one demotes it
	// back, the exact reverse of the promotion handleCloze makes on the
	// first cloze added.
	remaining, err := s.store.ClozesOf(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(remaining) == 0 {
		if err := s.store.SetKind(id, store.KindTopic, time.Now()); err != nil {
			s.fail(w, err)
			return
		}
	}

	s.redirect(w, r, "/read/"+strconv.FormatInt(id, 10))
}

// clozeOffsets converts a selection inside an extract into offsets against the
// extract's stored text, which is what cloze deletions are recorded in.
//
// The browser reports positions in block coordinates, like everywhere else. For
// a single-block extract those already equal the flat offsets and the
// conversion changes nothing; for an extract spanning paragraphs they differ by
// the separators, and using the block offsets directly would delete the wrong
// words with nothing to show that it had happened.
func (s *Server) clozeOffsets(r *http.Request, element store.Element) (int, int, error) {
	chosen, err := parseSelection(r)
	if err != nil {
		return 0, 0, err
	}

	// An extract's own content is the article here, not the document it came
	// from: the offsets are relative to the passage being clozed.
	article, err := ir.ParseArticle(s.sanitize(element.ContentHTML))
	if err != nil {
		return 0, 0, err
	}

	// The browser measures in runes; FlatOffset, like the rest of this
	// package, works in bytes. See Article.ByteOffset.
	byteRange, ok := article.ByteRange(chosen.Range)
	if !ok {
		return 0, 0, errors.New("that selection is outside the extract")
	}

	start, startOK := article.FlatOffset(byteRange.StartBlock, byteRange.StartOffset)
	end, endOK := article.FlatOffset(byteRange.EndBlock, byteRange.EndOffset)
	if !startOK || !endOK || end <= start {
		return 0, 0, errors.New("that deletion does not fit the extract")
	}
	if end > len(element.Quote) {
		return 0, 0, errors.New("that deletion runs past the end of the extract")
	}

	// The stored quote and the re-parsed content must agree, or the offsets
	// address different text than the export will render.
	if got := element.Quote[start:end]; ir.NormalizeSpace(got) != ir.NormalizeSpace(chosen.Quote) {
		s.logger.Warn("cloze selection did not match the stored extract",
			"element", element.ID, "browser", chosen.Quote, "stored", got)
		return 0, 0, errors.New("that selection no longer matches the extract; reload and try again")
	}

	return start, end, nil
}

// writeArticleFragment returns the article container for an htmx swap.
func (s *Server) writeArticleFragment(w http.ResponseWriter, elementID int64, rendered string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div id="article" data-element="%d">%s</div>`, elementID, rendered)
}

// isNotFound reports whether an error came from a missing row.
func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
