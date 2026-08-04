// Package web serves the reading interface.
//
// Pages are rendered server-side with html/template; htmx handles partial
// updates so that extracting a passage re-renders one element rather than the
// page. The only hand-written JavaScript translates a text selection into the
// block/offset coordinates the ir package addresses passages by.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"

	"github.com/Tevqoon/increader/internal/ir"
	"github.com/Tevqoon/increader/internal/source"
	"github.com/Tevqoon/increader/internal/store"
	"github.com/Tevqoon/increader/internal/version"
)

//go:embed templates/*.html static/*
var assets embed.FS

// pageNames are the full-page templates. Each is parsed together with the
// layout into its own template set, because they all define a "content"
// block and parsing them into one set would make those definitions collide.
var pageNames = []string{"dashboard.html", "queue.html", "reader.html", "library.html", "extracts.html"}

// Server holds everything the handlers need. Dependencies arrive through this
// struct rather than package-level variables, so a test can build a Server with
// its own store and no global setup.
type Server struct {
	store        *store.Store
	sources      map[string]source.Source
	dailyLimit   int
	extractDelay int
	logger       *slog.Logger
	policy       *bluemonday.Policy
	pages        map[string]*template.Template

	// publish asks the syncer to drain the outbox now. Optional: without it
	// queued writes still go out on the next sync, just later.
	publish func()

	// syncNow runs a full sync of every source immediately, blocking until it
	// finishes. Optional: without it, new documents at a provider only arrive
	// on the next scheduled tick.
	syncNow func(context.Context) error
}

// Options configures a Server.
type Options struct {
	Store      *store.Store
	Sources    map[string]source.Source
	DailyLimit int

	// ExtractDelay is how many days ahead a newly made extract becomes due.
	ExtractDelay int

	Logger *slog.Logger

	// Publish is called after a change that needs sending to a provider, so it
	// leaves promptly instead of waiting for the sync interval. It must not
	// block: reading should never wait on the network.
	Publish func()

	// SyncNow runs a full sync immediately, for the "sync now" button. Unlike
	// Publish it is expected to block for the duration of the request: the
	// point is for the page that follows to already show what it fetched.
	SyncNow func(context.Context) error
}

// New builds a Server and parses its templates.
//
// Template parsing happens once at startup rather than per request: a syntax
// error should stop the process immediately, not surface as a 500 the first
// time someone opens that page.
func New(options Options) (*Server, error) {
	server := &Server{
		store:        options.Store,
		sources:      options.Sources,
		dailyLimit:   options.DailyLimit,
		extractDelay: options.ExtractDelay,
		logger:       options.Logger,
		policy:       newPolicy(),
		pages:        make(map[string]*template.Template),
		publish:      options.Publish,
		syncNow:      options.SyncNow,
	}

	for _, name := range pageNames {
		page, err := template.New(name).
			Funcs(templateFuncs).
			ParseFS(assets, "templates/layout.html", "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("web: parse template %s: %w", name, err)
		}
		server.pages[name] = page
	}

	return server, nil
}

// Handler returns the router.
//
// Patterns use Go 1.22's method-and-path routing ("POST /elements/{id}/grade"),
// which is why there is no third-party router in this project.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /static/", http.FileServerFS(assets))

	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /queue", s.handleQueue)
	mux.HandleFunc("POST /sync", s.handleSyncNow)
	mux.HandleFunc("GET /next", s.handleNext)
	mux.HandleFunc("GET /library", s.handleLibrary)
	mux.HandleFunc("POST /library/bulk", s.handleLibraryBulk)
	mux.HandleFunc("DELETE /documents/{id}", s.handleDeleteDocument)
	mux.HandleFunc("GET /documents/{id}/images/{imageID}", s.handleDocumentImage)
	mux.HandleFunc("GET /extracts", s.handleExtracts)
	mux.HandleFunc("GET /read/{id}", s.handleRead)

	mux.HandleFunc("POST /elements/{id}/extract", s.handleExtract)
	mux.HandleFunc("POST /elements/{id}/cloze", s.handleCloze)
	mux.HandleFunc("DELETE /elements/{id}/cloze/{ordinal}", s.handleDeleteCloze)
	mux.HandleFunc("POST /elements/{id}/grade", s.handleGrade)
	mux.HandleFunc("POST /elements/{id}/backlog", s.handleBacklog)
	mux.HandleFunc("POST /elements/{id}/progress", s.handleProgress)
	mux.HandleFunc("POST /elements/{id}/unsuspend", s.handleUnsuspend)
	mux.HandleFunc("POST /elements/{id}/star", s.handleStar)
	mux.HandleFunc("POST /elements/{id}/tags", s.handleAddTag)
	mux.HandleFunc("POST /elements/{id}/tags/remove", s.handleRemoveTag)
	mux.HandleFunc("DELETE /elements/{id}", s.handleDeleteExtract)

	return mux
}

// today is the reader's current day.
//
// It reads time.Local rather than carrying its own *time.Location, and that is
// deliberate. Due dates are stored as bare dates, so writing them and comparing
// them must use the same zone; two components each holding their own idea of
// "today" would disagree by a day whenever they were configured differently,
// and the symptom — material appearing a day early or late — gives no hint of
// the cause. main pins time.Local to the configured timezone at startup, which
// makes the process's local zone the single answer to the question.
func (s *Server) today() time.Time {
	return ir.Day(time.Now())
}

// publishSoon nudges the syncer to drain the outbox.
//
// Best-effort by design: the write is already committed and durable, so failing
// to nudge only delays it to the next sync. That is exactly why the outbox
// exists rather than the handler calling the API itself.
func (s *Server) publishSoon() {
	if s.publish != nil {
		s.publish()
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DB().PingContext(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "ok %s\n", version.Current().Short())
}

// render writes a full page.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	page, ok := s.pages[name]
	if !ok {
		s.fail(w, fmt.Errorf("web: no such page %q", name))
		return
	}

	// Rendered into a buffer first. Executing straight into the ResponseWriter
	// would commit a 200 and a half-written page before a template error could
	// be reported, leaving the reader with a truncated article and no clue why.
	var buffer bytes.Buffer
	if err := page.ExecuteTemplate(&buffer, "layout", data); err != nil {
		s.fail(w, fmt.Errorf("web: render %s: %w", name, err))
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buffer.WriteTo(w)
}

// fail logs an error and returns a generic 500. The detail stays in the log:
// error text can carry SQL and file paths, which do not belong in a response.
func (s *Server) fail(w http.ResponseWriter, err error) {
	s.logger.Error("request failed", "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// elementID reads the {id} path parameter.
func elementID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("web: invalid element id %q: %w", r.PathValue("id"), err)
	}
	return id, nil
}

// articleHTML returns the sanitised HTML for an element, fetching the article
// body from its source the first time a document is opened.
//
// Lazy fetching is what makes syncing a large library cheap: the sync stores
// metadata only, and the body arrives when someone actually reads it.
func (s *Server) articleHTML(ctx context.Context, element store.Element) (string, error) {
	if !element.IsRoot() {
		return s.sanitize(element.ContentHTML), nil
	}

	document, err := s.store.DocumentByID(element.DocumentID)
	if err != nil {
		return "", err
	}

	if !document.HasContent {
		body, highlights, err := s.fetchBody(ctx, document)
		if err != nil {
			return "", err
		}
		if err := s.store.SetDocumentContent(document.ID, body); err != nil {
			return "", err
		}
		document.ContentHTML = body

		// Highlights that only arrive with a full fetch are imported here, at
		// the moment the article is first opened.
		sanitized := s.sanitize(body)
		if err := s.importHighlights(element, sanitized, highlights); err != nil {
			// A failed import must not stop the article from being read; the
			// highlights can be imported again on the next open.
			s.logger.Error("could not import highlights",
				"document", document.ID, "error", err)
		}
	}

	sanitized := s.sanitize(document.ContentHTML)

	// Highlights imported during sync have no position, because the listing
	// that carries them omits the article text. Now that the text is here they
	// can be anchored, which is what makes them render as marks.
	if err := s.anchorHighlights(document, element, sanitized); err != nil {
		s.logger.Error("could not anchor highlights",
			"document", document.ID, "error", err)
	}

	return sanitized, nil
}

// fetchBody retrieves an article body, and its highlights when the provider
// can supply them.
func (s *Server) fetchBody(ctx context.Context, document store.Document) (string, []source.Highlight, error) {
	provider, ok := s.sources[document.Source]
	if !ok {
		return "", nil, fmt.Errorf("web: document %d came from source %q, which is not configured",
			document.ID, document.Source)
	}

	// Ask for the richer form when the provider offers it, and fall back to a
	// plain body fetch when it does not.
	if enricher, ok := provider.(source.Enricher); ok {
		full, err := enricher.FullDocument(ctx, document.ExternalID)
		if err != nil {
			return "", nil, fmt.Errorf("web: fetch document %d: %w", document.ID, err)
		}
		return full.ContentHTML, full.Highlights, nil
	}

	body, err := provider.Content(ctx, document.ExternalID)
	if err != nil {
		return "", nil, fmt.Errorf("web: fetch body of document %d: %w", document.ID, err)
	}
	return body, nil, nil
}

// anchorHighlights gives imported extracts their position in the parent, now
// that the article body is available — and separately, upgrades one already
// anchored if its provider's own position record can now recover more of it
// than was available the last time this ran.
//
// Highlights arrive during sync from a metadata listing, which carries the
// annotation text but no article HTML — so they are stored without a position.
// The first time the article is actually opened its text exists, and each quote
// can be located and anchored. Anchoring is what makes an imported highlight
// render as a mark in the article rather than sitting there as a detached
// passage.
//
// Located primarily by text, not by the position wallabag itself recorded:
// that was measured against wallabag's own copy of the article, and does
// not survive increader's sanitising. But a highlight's own quote is not
// always reliable either — wallabag's database silently truncates a long
// one — so ranges (see source.RangeResolver) gets a look any time it might
// still have something better to offer: always for a highlight that never
// anchored at all, and also for one that already did, in case it was
// anchored by an earlier version of this pass that only had the truncated
// quote and no range to recover from yet — the exact situation a highlight
// imported before that column existed is in, until its next ordinary sync
// backfills one. Once the recovered text stops growing, ranges has nothing
// left to offer either, and repeating the check finds that out cheaply
// rather than needing to remember it was already asked.
func (s *Server) anchorHighlights(document store.Document, element store.Element, sanitizedHTML string) error {
	children, err := s.store.ChildrenOf(element.ID)
	if err != nil {
		return err
	}

	var pending []store.Element
	for _, child := range children {
		if child.Origin != store.OriginImport {
			continue
		}
		if !child.HasRange || child.Ranges != "" {
			pending = append(pending, child)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	article, err := ir.ParseArticle(sanitizedHTML)
	if err != nil {
		return err
	}

	// Absent for a provider with no such capability (or none configured at
	// all) — resolver stays nil and the type assertion below always fails,
	// which is exactly "skip the fallback", not an error.
	resolver, _ := s.sources[document.Source].(source.RangeResolver)

	now := time.Now()
	for _, extract := range pending {
		position, located := article.Locate(extract.Quote)

		if resolver != nil && extract.Ranges != "" {
			// Worth resolving whenever it might change the outcome: always
			// for a fresh miss, and for an existing anchor only if the
			// range can recover something longer than what is already
			// stored — Locate already succeeded on that, so a same-length
			// or shorter recovery has nothing to add.
			if recovered, ok := resolver.ResolveRange(document.ContentHTML, json.RawMessage(extract.Ranges)); ok &&
				(!located || len(recovered) > len(extract.Quote)) {
				if recoveredPosition, recoveredLocated := article.Locate(recovered); recoveredLocated {
					position, located = recoveredPosition, true
				}
			}
		}
		if !located {
			continue
		}

		// Prefer the article's own copy of the passage: it carries the inline
		// markup, and its whitespace matches the offsets stored beside it.
		quote, err := article.Text(position)
		if err != nil {
			continue
		}
		// Nothing actually changed — the common case for an already
		// anchored highlight whose range had nothing further to offer.
		// Skip the write rather than touching updated_at for no reason.
		if extract.HasRange && quote == extract.Quote {
			continue
		}
		markup, err := article.HTML(position)
		if err != nil {
			continue
		}

		if err := s.store.AnchorExtract(extract.ID, position, quote, markup, now); err != nil {
			return err
		}
	}
	return nil
}

// importHighlights creates extracts for annotations that reached the reader
// outside a sync.
//
// Only used for providers that cannot supply annotations in a listing; wallabag
// 2.6 and later import them during sync instead. Kept because the Source
// interface does not require the cheap path, and a provider without it should
// still get its highlights.
func (s *Server) importHighlights(element store.Element, sanitizedHTML string, highlights []source.Highlight) error {
	if len(highlights) == 0 {
		return nil
	}

	article, err := ir.ParseArticle(sanitizedHTML)
	if err != nil {
		return err
	}

	now := time.Now()
	for _, highlight := range highlights {
		if strings.TrimSpace(highlight.Quote) == "" {
			continue
		}

		found, located := article.Locate(highlight.Quote)
		quote := highlight.Quote
		contentHTML := "<p>" + template.HTMLEscapeString(highlight.Quote) + "</p>"

		if located {
			if text, err := article.Text(found); err == nil {
				quote = text
			}
			if markup, err := article.HTML(found); err == nil {
				contentHTML = markup
			}
		}

		_, err := s.store.CreateExtract(store.NewExtract{
			ParentID:    element.ID,
			DocumentID:  element.DocumentID,
			Kind:        store.KindTopic,
			Title:       store.SummariseQuote(quote),
			ContentHTML: contentHTML,
			Quote:       quote,
			Range:       found,
			HasRange:    located,
			Priority:    element.Schedule.Priority,
			Origin:      store.OriginImport,
			// Keyed by the provider's annotation id, so re-importing the same
			// highlight is rejected by the unique index rather than duplicated.
			ExternalRef: highlight.ExternalID,
			Ranges:      string(highlight.Ranges),
		}, now)
		if err != nil {
			if store.IsDuplicate(err) {
				continue
			}
			return err
		}
	}

	return nil
}

// parseArticle sanitises, parses and returns an element's article together
// with the marks for extracts already taken from it, and a resolved local
// URL for each image it contains — see resolveImages.
func (s *Server) parseArticle(ctx context.Context, element store.Element) (*ir.Article, []ir.Mark, map[string]ir.ResolvedImage, error) {
	sanitized, err := s.articleHTML(ctx, element)
	if err != nil {
		return nil, nil, nil, err
	}

	article, err := ir.ParseArticle(sanitized)
	if err != nil {
		return nil, nil, nil, err
	}

	children, err := s.store.ChildrenOf(element.ID)
	if err != nil {
		return nil, nil, nil, err
	}

	marks := make([]ir.Mark, 0, len(children))
	for _, child := range children {
		if child.HasRange {
			marks = append(marks, ir.Mark{Range: child.Range, ElementID: child.ID})
		}
	}

	imageURLs := s.resolveImages(ctx, element.DocumentID, article.Images())

	return article, marks, imageURLs, nil
}

// templateFuncs are the helpers available inside templates.
var templateFuncs = template.FuncMap{
	// percent renders a 0..1 priority as a whole number for display.
	"percent": func(value float64) int { return int(value*100 + 0.5) },

	// pct is part/total as a whole-number percentage, for the dashboard's bar
	// breakdowns — 0 rather than a division panic when total is 0, since an
	// empty library is an ordinary state for this app, not an error.
	"pct": func(part, total int) int {
		if total == 0 {
			return 0
		}
		return part * 100 / total
	},

	// build is a function rather than per-page data so every template can
	// show it without each handler having to remember to pass it.
	"build": func() string { return version.Current().Short() },

	"date": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Format("2 Jan 2006")
	},

	"days": func(interval float64) string {
		if interval < 1 {
			return "new"
		}
		return strconv.Itoa(int(interval+0.5)) + "d"
	},

	// backlogOptions exposes the same fuzzed presets the reader's schedule
	// panel uses, so a list row's reschedule control offers exactly the same
	// choices — see ir.BacklogOptions.
	"backlogOptions": ir.BacklogOptions,

	// level buckets a day's activity count into a handful of discrete shades
	// for the heatmap grid — a raw count would make two different days look
	// like two different colours for no perceptible reason.
	"level": func(reviews, extracts int) int {
		switch n := reviews + extracts; {
		case n == 0:
			return 0
		case n == 1:
			return 1
		case n <= 2:
			return 2
		case n <= 4:
			return 3
		default:
			return 4
		}
	},
}
