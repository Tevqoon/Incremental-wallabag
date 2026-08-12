package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/Tevqoon/increader/internal/store"
)

// The JSON API exists for one job: letting something outside increader mirror
// its annotations without scraping the HTML.
//
// The concrete consumer it was designed against is an org-roam exporter — a
// thin elisp layer that turns each document's passages into one org file, so
// that annotations are linkable from notes without increader having to know
// anything about org's file layout, ID conventions or capture templates.
// Putting that knowledge on the Emacs side rather than behind a Target
// interface here is the entire point: Emacs already has it, and a Go exporter
// would reimplement it worse and drift from the user's own configuration.
//
// Three deliberate limits, each of which is what keeps this cheap:
//
// It is annotation-shaped, not a mirror of the site. There is no endpoint for
// grading, extracting or anything else the reader does — those run through
// ir.Next and the outbox, and a second surface over them is how the
// "one caller, one implementation" rule in queue.go quietly stops being true.
// Reading and scheduling stay in the HTML app; this is for getting harvested
// passages out and light corrections back in.
//
// The writable fields are exactly those nothing else reads. Title, note,
// chapter and an override for the passage's wording are all inert: no
// anchoring pass locates against them, no outbox write pushes them upstream.
// The verbatim quote — which does both — is not writable here at all. See
// AnnotationEdit and migration 018 for why that split is what makes editing
// text from another program safe rather than merely careful.
//
// It is unversioned. There is one consumer, it lives on the same machine, and
// it is deployed by the same person who deploys this; that is the same reason
// the HTML templates can change freely. Should that ever stop being true, a
// second prefix alongside this one costs nothing to add.

// apiDocument is one document in the annotation feed.
type apiDocument struct {
	ID         int64  `json:"id"`
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
	URL        string `json:"url"`

	// Title is what the document should be called, with the reader's own
	// override already applied — Document.Heading, not the raw column, so a
	// consumer never has to know that distinction exists.
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Author   string `json:"author,omitempty"`
	Language string `json:"language,omitempty"`

	Tags []string `json:"tags"`

	PublishedAt string `json:"published_at,omitempty"`
	ImportedAt  string `json:"imported_at,omitempty"`

	IsArchived      bool `json:"is_archived"`
	IsStarred       bool `json:"is_starred"`
	MissingUpstream bool `json:"missing_upstream"`

	// AnnotationCount and AnnotationsUpdatedAt are what a consumer polls on:
	// the second is the newest change among this document's passages, so
	// comparing it against whatever was stored last time is enough to decide
	// whether the mirrored copy needs rewriting. Feed it back as ?since= to
	// get only what has moved.
	AnnotationCount      int    `json:"annotation_count"`
	AnnotationsUpdatedAt string `json:"annotations_updated_at,omitempty"`

	// Annotations is populated only on the single-document endpoint. The feed
	// omits it so that listing a whole library stays one query and one small
	// response.
	Annotations []apiAnnotation `json:"annotations,omitempty"`
}

// apiAnnotation is one harvested passage.
type apiAnnotation struct {
	ID         int64  `json:"id"`
	DocumentID int64  `json:"document_id"`
	Source     string `json:"source"`

	// ExternalID is the provider's own identifier for this annotation, empty
	// for a passage extracted here rather than imported. It is the stable key
	// to mirror against: increader's own id is stable too, but a consumer
	// that also saw these annotations through some earlier importer will
	// already have keyed its notes on the provider's.
	ExternalID string `json:"external_id,omitempty"`

	// Title is the heading for this passage. Derived from the text when the
	// annotation was created and free to be replaced with something
	// meaningful — nothing here depends on its value.
	Title string `json:"title"`

	// Text is the passage as it should be displayed: the reader's correction
	// when there is one, otherwise the text as it arrived. This is the field
	// to render.
	Text string `json:"text"`

	// OriginalText is the verbatim text as imported, always present even when
	// unedited. It is what increader anchors and writes back with, so a
	// consumer showing a correction can still show what it corrected — and a
	// diff between the two is the only way to see that an edit happened at
	// all besides the Edited flag.
	OriginalText string `json:"original_text"`

	// Edited reports whether Text differs from OriginalText by the reader's
	// choice rather than by coincidence.
	Edited bool `json:"edited"`

	// HTML is the passage's markup, already sanitised. Useful where the plain
	// text loses something — inline emphasis, a formula's markup — and safe
	// to ignore entirely for a consumer that wants prose.
	HTML string `json:"html,omitempty"`

	Note    string `json:"note,omitempty"`
	Chapter string `json:"chapter,omitempty"`
	Page    string `json:"page,omitempty"`
	Color   string `json:"color,omitempty"`

	// Ordinal is reading order within the work, counting from one, for
	// imported annotations; zero for a passage extracted while reading, which
	// has no place in the original's own sequence.
	Ordinal int `json:"ordinal,omitempty"`

	// Origin is "import" for a provider's own annotation or an uploaded
	// file's, "manual" for a passage extracted in the reader.
	Origin string `json:"origin"`

	// Kind is "topic" for a passage and "item" once cloze deletions have been
	// marked on it.
	Kind       string `json:"kind"`
	ClozeCount int    `json:"cloze_count,omitempty"`

	// Anchored reports whether this passage has a known position in its
	// parent article. A consumer has no use for the position itself, but the
	// fact is worth having: an unanchored passage is one whose quote could
	// not be found in the article, which usually means the article changed
	// underneath it.
	Anchored        bool `json:"anchored"`
	MissingUpstream bool `json:"missing_upstream"`

	// Priority, State, DueOn and Reps are increader's scheduling of this
	// passage. Read-only here — grading goes through the reader — but exposed
	// because "what am I about to re-read" is a reasonable thing to ask from
	// outside, and because a mirrored note is more useful when it can say so.
	Priority float64 `json:"priority"`
	State    string  `json:"state"`
	DueOn    string  `json:"due_on,omitempty"`
	Reps     int     `json:"reps"`

	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// annotationPatch is the request body of a PATCH.
//
// Every field is a pointer so an absent key leaves that field alone, which is
// what lets a consumer that only knows about titles send only titles without
// blanking everything else. A JSON null is treated the same as absent: there
// is no field here where "explicitly null" would mean something different
// from "not mentioned", and clearing one is done by sending "" rather than by
// distinguishing the two.
type annotationPatch struct {
	Title   *string `json:"title"`
	Note    *string `json:"note"`
	Chapter *string `json:"chapter"`

	// Text sets the display override, not the verbatim quote — writing the
	// quote is not offered. Sending "" removes an override and falls back to
	// the original text.
	Text *string `json:"text"`
}

// registerAPI adds the JSON routes to a mux.
//
// Kept apart from Handler's own registrations so that the API surface can be
// read in one place, rather than being picked out of the page routes by their
// prefix.
func (s *Server) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/documents", s.handleAPIDocuments)
	mux.HandleFunc("GET /api/documents/{id}", s.handleAPIDocument)
	mux.HandleFunc("GET /api/annotations/{id}", s.handleAPIAnnotation)
	mux.HandleFunc("PATCH /api/annotations/{id}", s.handleAPIEditAnnotation)
}

// handleAPIDocuments lists documents that have annotations, newest change
// first.
//
// Query parameters: since (RFC3339, only documents whose annotations changed
// after it), source (a provider name), limit.
func (s *Server) handleAPIDocuments(w http.ResponseWriter, r *http.Request) {
	filter := store.AnnotatedFilter{Source: r.FormValue("source")}

	if raw := r.FormValue("since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			s.apiError(w, http.StatusBadRequest,
				fmt.Sprintf("since must be an RFC3339 timestamp: %v", err))
			return
		}
		filter.Since = since
	}

	if raw := r.FormValue("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 {
			s.apiError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		filter.Limit = limit
	}

	documents, err := s.store.AnnotatedDocuments(filter)
	if err != nil {
		s.apiFail(w, err)
		return
	}

	payload := make([]apiDocument, 0, len(documents))
	for _, document := range documents {
		tags, err := s.store.TagsOf(document.ID)
		if err != nil {
			s.apiFail(w, err)
			return
		}
		payload = append(payload, newAPIDocument(document, tags))
	}

	// Wrapped in an object rather than returned as a bare array so that a
	// later addition — a paging cursor, a server-side clock — does not have
	// to change the response's type.
	s.writeJSON(w, http.StatusOK, map[string]any{"documents": payload})
}

// handleAPIDocument returns one document together with all of its
// annotations, in the work's own reading order.
//
// This is the endpoint an exporter builds a file from: one request produces
// everything one document's worth of notes needs.
func (s *Server) handleAPIDocument(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.apiError(w, http.StatusBadRequest, "invalid document id")
		return
	}

	document, err := s.store.DocumentByID(id)
	if err != nil {
		s.apiNotFoundOrFail(w, err)
		return
	}

	annotations, err := s.store.DocumentAnnotations(id)
	if err != nil {
		s.apiFail(w, err)
		return
	}

	tags, err := s.store.TagsOf(id)
	if err != nil {
		s.apiFail(w, err)
		return
	}

	payload := newAPIDocument(store.AnnotatedDocument{
		Document:             document,
		Annotations:          len(annotations),
		AnnotationsUpdatedAt: newestUpdate(annotations),
	}, tags)

	payload.Annotations = make([]apiAnnotation, 0, len(annotations))
	for _, row := range annotations {
		payload.Annotations = append(payload.Annotations,
			s.newAPIAnnotation(row.Element, document, row.ClozeCount))
	}

	s.writeJSON(w, http.StatusOK, payload)
}

// handleAPIAnnotation returns one annotation.
func (s *Server) handleAPIAnnotation(w http.ResponseWriter, r *http.Request) {
	element, document, ok := s.apiAnnotationTarget(w, r)
	if !ok {
		return
	}

	clozes, err := s.store.ClozesOf(element.ID)
	if err != nil {
		s.apiFail(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, s.newAPIAnnotation(element, document, len(clozes)))
}

// handleAPIEditAnnotation applies a partial edit and returns the annotation as
// it stands afterwards.
//
// Answering with the updated row rather than a bare 204 is what lets a
// consumer treat one PATCH as a complete round trip: it sends a corrected
// title, and gets back the record to write into its own copy, including the
// new updated_at that its next ?since= poll will be measured against.
func (s *Server) handleAPIEditAnnotation(w http.ResponseWriter, r *http.Request) {
	element, document, ok := s.apiAnnotationTarget(w, r)
	if !ok {
		return
	}

	var patch annotationPatch
	decoder := json.NewDecoder(r.Body)
	// An unknown field is a mistake worth reporting rather than ignoring: the
	// most likely one is a consumer sending "quote", expecting to rewrite the
	// verbatim text, and silently accepting that would leave it believing an
	// edit landed when nothing happened.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		s.apiError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}

	edit := store.AnnotationEdit{
		Title:       patch.Title,
		Note:        patch.Note,
		Chapter:     patch.Chapter,
		EditedQuote: patch.Text,
	}

	// An annotation is a passage, a note, or both; one with neither left is
	// not an annotation any more. The same guard handleEditAnnotation applies
	// to the HTML editor, checked here against what the row would hold after
	// the edit rather than against what the request happened to mention.
	after := element
	if edit.Note != nil {
		after.Note = *edit.Note
	}
	if edit.EditedQuote != nil {
		after.EditedQuote = *edit.EditedQuote
	}
	if after.DisplayQuote() == "" && after.Note == "" {
		s.apiError(w, http.StatusBadRequest,
			"an annotation needs either a passage or a note")
		return
	}

	if err := s.store.EditAnnotation(element.ID, edit, time.Now()); err != nil {
		s.apiNotFoundOrFail(w, err)
		return
	}

	updated, err := s.store.ElementByID(element.ID)
	if err != nil {
		s.apiFail(w, err)
		return
	}

	clozes, err := s.store.ClozesOf(element.ID)
	if err != nil {
		s.apiFail(w, err)
		return
	}

	s.writeJSON(w, http.StatusOK, s.newAPIAnnotation(updated, document, len(clozes)))
}

// apiAnnotationTarget resolves the {id} of an annotation route to its element
// and the document it belongs to, writing the response itself and reporting
// false when it cannot.
//
// A root element is rejected as not found rather than as a bad request: from
// outside, /api/annotations/{id} addresses a space of annotations, and an id
// that names a whole document simply is not in it.
func (s *Server) apiAnnotationTarget(w http.ResponseWriter, r *http.Request) (store.Element, store.Document, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.apiError(w, http.StatusBadRequest, "invalid annotation id")
		return store.Element{}, store.Document{}, false
	}

	element, err := s.store.ElementByID(id)
	if err != nil {
		s.apiNotFoundOrFail(w, err)
		return store.Element{}, store.Document{}, false
	}
	if element.IsRoot() {
		s.apiError(w, http.StatusNotFound, "no such annotation")
		return store.Element{}, store.Document{}, false
	}

	document, err := s.store.DocumentByID(element.DocumentID)
	if err != nil {
		s.apiNotFoundOrFail(w, err)
		return store.Element{}, store.Document{}, false
	}
	return element, document, true
}

// newAPIDocument renders a document for the API.
func newAPIDocument(document store.AnnotatedDocument, tags []string) apiDocument {
	if tags == nil {
		tags = []string{}
	}
	return apiDocument{
		ID:                   document.ID,
		Source:               document.Source,
		ExternalID:           document.ExternalID,
		URL:                  document.URL,
		Title:                document.Heading(),
		Subtitle:             document.Subtitle,
		Author:               document.Author,
		Language:             document.Language,
		Tags:                 tags,
		PublishedAt:          apiTime(document.PublishedAt),
		ImportedAt:           apiTime(document.ImportedAt),
		IsArchived:           document.IsArchived,
		IsStarred:            document.IsStarred,
		MissingUpstream:      document.MissingUpstream,
		AnnotationCount:      document.Annotations,
		AnnotationsUpdatedAt: apiTime(document.AnnotationsUpdatedAt),
	}
}

// newAPIAnnotation renders one element for the API.
//
// The markup goes out through the same sanitiser the pages use. It should
// already be safe — every path that writes content_html on an extract either
// escapes the text outright or takes its markup from an already-sanitised
// article — but sanitising on the way out costs nothing and means the API
// cannot become the one exit from this program that a future import path
// forgets about.
//
// A corrected passage's markup is built from the correction, exactly as
// articleHTML builds it for the reader. Serving the stored content_html
// instead would hand a consumer a Text and an HTML that disagree, and leave
// whichever one it happened to render showing text the reader had already
// fixed.
func (s *Server) newAPIAnnotation(element store.Element, document store.Document, clozes int) apiAnnotation {
	markup := element.ContentHTML
	if element.Edited() {
		markup = paragraphHTML(element.DisplayQuote(), element.Note)
	}
	return apiAnnotation{
		ID:              element.ID,
		DocumentID:      element.DocumentID,
		Source:          document.Source,
		ExternalID:      element.ExternalRef,
		Title:           element.Title,
		Text:            element.DisplayQuote(),
		OriginalText:    element.Quote,
		Edited:          element.Edited(),
		HTML:            bodyHTML(s.sanitize(markup, document.URL)),
		Note:            element.Note,
		Chapter:         element.Chapter,
		Page:            element.Page,
		Color:           element.Color,
		Ordinal:         element.Ordinal,
		Origin:          element.Origin,
		Kind:            element.Kind,
		ClozeCount:      clozes,
		Anchored:        element.HasRange,
		MissingUpstream: element.MissingUpstream,
		Priority:        element.Schedule.Priority,
		State:           string(element.Schedule.State),
		DueOn:           apiDate(element.Schedule.DueOn),
		Reps:            element.Schedule.Reps,
		CreatedAt:       apiTime(element.CreatedAt),
		UpdatedAt:       apiTime(element.UpdatedAt),
	}
}

// bodyHTML unwraps the document scaffolding out of a sanitised fragment.
//
// sanitize's rewriteSamePageLinks pass parses and re-renders, and x/net/html
// supplies the html/head/body a well-formed document is required to have — so
// what comes back out of a fragment going in is a whole document. Nothing
// inside this program notices, because every consumer of it re-parses and
// works from the body down. An API consumer would notice: it gets handed a
// passage and finds a document.
//
// Unwrapping here rather than in sanitize itself is deliberate. That function
// is on the path every offset in the reader is measured against, and its
// output is load-bearing in a way this is not; changing what it returns to
// tidy up an unrelated consumer is how offsets quietly shift.
func bodyHTML(fragment string) string {
	document, err := html.Parse(strings.NewReader(fragment))
	if err != nil {
		return fragment
	}

	var body *html.Node
	var find func(*html.Node)
	find = func(node *html.Node) {
		if body != nil {
			return
		}
		if node.Type == html.ElementNode && node.DataAtom == atom.Body {
			body = node
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			find(child)
		}
	}
	find(document)
	if body == nil {
		return fragment
	}

	var out strings.Builder
	for child := body.FirstChild; child != nil; child = child.NextSibling {
		if err := html.Render(&out, child); err != nil {
			return fragment
		}
	}
	return out.String()
}

// newestUpdate is the latest updated_at across a document's annotations —
// the same value AnnotatedDocuments computes in SQL, recomputed here for the
// single-document endpoint, which reads the annotations anyway and would
// otherwise need a second query to say the same thing.
func newestUpdate(annotations []store.ExtractRow) time.Time {
	var newest time.Time
	for _, row := range annotations {
		if row.UpdatedAt.After(newest) {
			newest = row.UpdatedAt
		}
	}
	return newest
}

// apiTime renders a timestamp, mapping the zero time to an empty string so
// that omitempty drops the field entirely rather than emitting year one.
func apiTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// apiDate renders a due date in the same YYYY-MM-DD form the store keeps it
// in — a date, not a timestamp, because scheduling works in whole days.
func apiDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

// writeJSON sends a value as the response body.
//
// Encoded into a buffer via json.Marshal rather than streamed into the
// ResponseWriter for the same reason render buffers a template: a failure
// halfway through would otherwise have already committed a 200 and half a
// document, leaving the consumer to parse a truncated object.
func (s *Server) writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		s.apiFail(w, fmt.Errorf("web: encode response: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

// apiError sends a JSON error body, so a consumer can parse every response the
// same way rather than special-casing failures as plain text.
func (s *Server) apiError(w http.ResponseWriter, status int, message string) {
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

// apiFail is the JSON counterpart of fail: log the detail, return a generic
// 500. Error text can carry SQL and file paths, which do not belong in a
// response whatever its content type.
func (s *Server) apiFail(w http.ResponseWriter, err error) {
	s.logger.Error("api request failed", "error", err)
	s.apiError(w, http.StatusInternalServerError, "internal error")
}

// apiNotFoundOrFail maps a missing row to 404 and anything else to 500 — the
// JSON counterpart of notFoundOrFail.
func (s *Server) apiNotFoundOrFail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	s.apiFail(w, err)
}
