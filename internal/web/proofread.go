package web

import (
	"html"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Tevqoon/increader/internal/proofread"
	"github.com/Tevqoon/increader/internal/store"
)

// proofreadSuggestion is one passage the model proposed changing.
type proofreadSuggestion struct {
	ID       int64
	Original string
	Proposed string

	// Diff is Original and Proposed merged into one marked-up passage —
	// unchanged words plain, removed ones in <del>, added ones in <ins> —
	// rather than the full text twice over. A model fixing one dropped
	// letter in an otherwise-long passage used to mean reading the whole
	// thing twice to find what actually changed; this is the point of
	// reviewing a proposal rather than just applying it.
	Diff template.HTML
}

// wordDiff renders a and b as one word-level diff: an LCS alignment (plain
// dynamic programming — passage-length word counts, not documents, so this
// is cheap) walked to emit runs of unchanged, removed and added words,
// rather than the two full passages side by side. A deleted run is always
// emitted immediately before the insertion that replaces it, which is what
// keeps "the wrong word" and "the right word" next to each other instead of
// on opposite ends of the passage the way a naive line-diff would.
//
// Splits on whitespace only, which is safe here specifically because every
// passage reaching this already went through cleanPassage on the way in
// (see annotations.cleanPassage) and so carries no internal line breaks to
// lose by doing that.
func wordDiff(a, b string) template.HTML {
	oldWords := strings.Fields(a)
	newWords := strings.Fields(b)
	n, m := len(oldWords), len(newWords)

	// lcs[i][j] is the length of the longest common subsequence of
	// oldWords[i:] and newWords[j:], filled bottom-up so the walk below can
	// greedily follow whichever neighbour holds the longer subsequence.
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case oldWords[i] == newWords[j]:
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var out strings.Builder
	space := func() {
		if out.Len() > 0 {
			out.WriteByte(' ')
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case oldWords[i] == newWords[j]:
			space()
			out.WriteString(html.EscapeString(oldWords[i]))
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			space()
			out.WriteString("<del>" + html.EscapeString(oldWords[i]) + "</del>")
			i++
		default:
			space()
			out.WriteString("<ins>" + html.EscapeString(newWords[j]) + "</ins>")
			j++
		}
	}
	for ; i < n; i++ {
		space()
		out.WriteString("<del>" + html.EscapeString(oldWords[i]) + "</del>")
	}
	for ; j < m; j++ {
		space()
		out.WriteString("<ins>" + html.EscapeString(newWords[j]) + "</ins>")
	}
	return template.HTML(out.String())
}

// proofreadReviewData is the page shown after a batch comes back — nothing
// is written to the store yet.
type proofreadReviewData struct {
	Title       string
	Document    store.Document
	Suggestions []proofreadSuggestion

	// Unchanged and Failed together account for every selected annotation
	// that is not in Suggestions: the model looked at it and proposed
	// nothing, or its whole batch errored out — see proofread.FixBatch's own
	// doc for why those are told apart rather than both just "not shown".
	Unchanged int
	Failed    int
}

// handleProofreadExtracts sends the checked annotations' own passages to the
// configured cheap model (see internal/proofread) and shows what it proposes
// changing, one at a time, for the reader to accept or reject — the OCR
// fixer this package exists for must never overwrite a passage's meaning
// unreviewed, so nothing is written here at all; see handleApplyProofread
// for the only path that actually calls Store.UpdateAnnotation.
//
// Reuses document.html's own bulk-selection checkboxes (see the "Fix typos"
// button there) rather than adding a second set: a submit button's
// formaction can point at a different endpoint than the form it belongs to
// without any JavaScript, so one set of checkboxes already serves both the
// chapter bulk-edit and this.
func (s *Server) handleProofreadExtracts(w http.ResponseWriter, r *http.Request) {
	documentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}
	if s.proofreader == nil {
		http.Error(w, "no LLM proofreading is configured", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	document, err := s.store.DocumentByID(documentID)
	if err != nil {
		s.notFoundOrFail(w, err)
		return
	}

	var elements []store.Element
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
		// Same document-scoping caution handleSetChapters applies: a
		// tampered id naming a row in a different work must not let one
		// document's bulk action reach across into another's. A blank
		// quote (a note-only annotation) has nothing for the model to
		// proofread, so it is silently skipped rather than sent as an
		// empty string.
		if element.IsRoot() || element.DocumentID != documentID || strings.TrimSpace(element.Quote) == "" {
			continue
		}
		elements = append(elements, element)
	}

	if len(elements) == 0 {
		s.redirect(w, r, "/documents/"+strconv.FormatInt(documentID, 10))
		return
	}

	items := make([]proofread.Item, len(elements))
	byID := make(map[string]store.Element, len(elements))
	for i, element := range elements {
		id := strconv.FormatInt(element.ID, 10)
		items[i] = proofread.Item{ID: id, Text: element.Quote}
		byID[id] = element
	}

	fixes, failed, err := s.proofreader.FixBatch(r.Context(), items)
	if err != nil {
		s.fail(w, err)
		return
	}

	suggestions := make([]proofreadSuggestion, 0, len(fixes))
	for id, proposed := range fixes {
		element := byID[id]
		if proposed == element.Quote {
			continue
		}
		suggestions = append(suggestions, proofreadSuggestion{
			ID: element.ID, Original: element.Quote, Proposed: proposed,
			Diff: wordDiff(element.Quote, proposed),
		})
	}
	sort.Slice(suggestions, func(i, j int) bool { return suggestions[i].ID < suggestions[j].ID })

	s.render(w, "proofread_review.html", proofreadReviewData{
		Title:       "Review proofreading · " + document.Heading(),
		Document:    document,
		Suggestions: suggestions,
		Unchanged:   len(elements) - len(fixes) - failed,
		Failed:      failed,
	})
}

// handleApplyProofread commits whichever suggestions the reader left checked
// on the review page — exactly the same write handleEditAnnotation makes for
// a manual correction, passage only, note and chapter carried over
// unchanged, because that is genuinely what this is: the model's proposal is
// just where the corrected passage text came from.
func (s *Server) handleApplyProofread(w http.ResponseWriter, r *http.Request) {
	documentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	for _, raw := range r.Form["ids"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		proposed := strings.TrimSpace(r.FormValue("quote_" + raw))
		if proposed == "" {
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
		if element.IsRoot() || element.DocumentID != documentID {
			continue
		}

		if err := s.store.UpdateAnnotation(id, proposed, element.Note, element.Chapter, time.Now()); err != nil {
			s.fail(w, err)
			return
		}
	}

	s.redirect(w, r, "/documents/"+strconv.FormatInt(documentID, 10))
}
