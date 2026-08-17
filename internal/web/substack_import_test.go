package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tevqoon/increader/internal/store"
)

// newSubstackTestServer builds a Server with ImportSubstackURL set to fn —
// deliberately not newTestServer/newTestServerWithDelay, which seed a whole
// wallabag document and fake source neither of these tests need: the
// handler under test only ever touches s.store.UploadedDocuments() (for the
// page's own unrelated "add to" dropdown) and the closure itself.
func newSubstackTestServer(t *testing.T, fn func(ctx context.Context, url string) (string, error)) *Server {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	server, err := New(Options{
		Store:             db,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		ImportSubstackURL: fn,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

// newFeedRefreshTestServer is newSubstackTestServer's counterpart for
// RefreshSubstackFeed — same minimal setup, no seeded wallabag document or
// source, since handleRefreshSubstackFeed only ever touches the closure and
// (via renderImport) s.store.UploadedDocuments().
func newFeedRefreshTestServer(t *testing.T, fn func(ctx context.Context) (string, error)) *Server {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	server, err := New(Options{
		Store:               db,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		RefreshSubstackFeed: fn,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

// TestImportSubstackURLNotConfigured: with no ImportSubstackURL closure —
// ingest.substack has no session cookie in config.yaml — the endpoint must
// say so rather than panic on a nil call, and the import page itself must
// not offer the section at all.
func TestImportSubstackURLNotConfigured(t *testing.T) {
	server := newSubstackTestServer(t, nil)

	if response := get(t, server, "/import"); strings.Contains(response.Body.String(), "Import from a URL") {
		t.Error("the substack-import section rendered despite no closure being configured")
	}

	response := post(t, server, "/import/substack", url.Values{"substack_url": {"https://example.substack.com/p/a-post"}})
	if response.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", response.Code)
	}
}

func TestImportSubstackURLShowsSectionWhenConfigured(t *testing.T) {
	server := newSubstackTestServer(t, func(context.Context, string) (string, error) {
		return "", nil
	})

	if response := get(t, server, "/import"); !strings.Contains(response.Body.String(), "Import from a URL") {
		t.Error("the substack-import section did not render despite a closure being configured")
	}
}

func TestImportSubstackURLRequiresAURL(t *testing.T) {
	called := false
	server := newSubstackTestServer(t, func(context.Context, string) (string, error) {
		called = true
		return "", nil
	})

	response := post(t, server, "/import/substack", url.Values{"substack_url": {"  "}})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendering the form with an error)", response.Code)
	}
	if called {
		t.Error("the closure was called despite an empty URL")
	}
	if !strings.Contains(response.Body.String(), "Paste a Substack post URL") {
		t.Error("submitting with no URL gave no useful message")
	}
}

func TestImportSubstackURLSuccessShowsReport(t *testing.T) {
	var gotURL string
	server := newSubstackTestServer(t, func(_ context.Context, url string) (string, error) {
		gotURL = url
		return "created 1 entry\n", nil
	})

	response := post(t, server, "/import/substack",
		url.Values{"substack_url": {"https://example.substack.com/p/a-post"}})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if gotURL != "https://example.substack.com/p/a-post" {
		t.Errorf("closure received %q, want the posted URL", gotURL)
	}
	body := response.Body.String()
	if !strings.Contains(body, "created 1 entry") {
		t.Error("the report was not shown")
	}
	if !strings.Contains(body, "https://example.substack.com/p/a-post") {
		t.Error("the imported URL was not shown")
	}
}

func TestImportSubstackURLFailureShowsError(t *testing.T) {
	server := newSubstackTestServer(t, func(context.Context, string) (string, error) {
		return "", errors.New("the session cookie was rejected")
	})

	response := post(t, server, "/import/substack",
		url.Values{"substack_url": {"https://example.substack.com/p/a-post"}})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendering the form with an error)", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "the session cookie was rejected") {
		t.Error("the closure's error was not shown")
	}
	// The form re-fills with the URL that failed, so a retry after fixing
	// the actual problem does not mean retyping it.
	if !strings.Contains(body, `value="https://example.substack.com/p/a-post"`) {
		t.Error("the failed URL was not re-filled into the form")
	}
}

// TestRefreshSubstackFeedNotConfigured mirrors
// TestImportSubstackURLNotConfigured for the second closure — nil hides the
// button and the endpoint says so rather than panicking on a nil call.
func TestRefreshSubstackFeedNotConfigured(t *testing.T) {
	server := newFeedRefreshTestServer(t, nil)

	if response := get(t, server, "/import"); strings.Contains(response.Body.String(), "Check for new articles") {
		t.Error("the feed-refresh section rendered despite no closure being configured")
	}

	response := post(t, server, "/import/substack/refresh", url.Values{})
	if response.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", response.Code)
	}
}

func TestRefreshSubstackFeedShowsButtonWhenConfigured(t *testing.T) {
	server := newFeedRefreshTestServer(t, func(context.Context) (string, error) {
		return "", nil
	})

	if response := get(t, server, "/import"); !strings.Contains(response.Body.String(), "Check for new articles") {
		t.Error("the feed-refresh section did not render despite a closure being configured")
	}
}

func TestRefreshSubstackFeedSuccessShowsReport(t *testing.T) {
	server := newFeedRefreshTestServer(t, func(context.Context) (string, error) {
		return "created 2, updated 1, skipped 40\n", nil
	})

	response := post(t, server, "/import/substack/refresh", url.Values{})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), "created 2, updated 1, skipped 40") {
		t.Error("the report was not shown")
	}
}

func TestRefreshSubstackFeedFailureShowsError(t *testing.T) {
	server := newFeedRefreshTestServer(t, func(context.Context) (string, error) {
		return "", errors.New("subscription to example.substack.com has lapsed")
	})

	response := post(t, server, "/import/substack/refresh", url.Values{})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendering the page with an error)", response.Code)
	}
	if !strings.Contains(response.Body.String(), "subscription to example.substack.com has lapsed") {
		t.Error("the closure's error was not shown")
	}
}

// TestRefreshSubstackFeedIsIndependentOfURLImport: the two actions share a
// page but must never be confused with each other — a feed refresh's own
// report must not appear tagged onto the URL-import section's UI, and vice
// versa. Both closures configured here, only one ever called.
func TestRefreshSubstackFeedIsIndependentOfURLImport(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	urlImportCalled := false
	server, err := New(Options{
		Store:  db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ImportSubstackURL: func(context.Context, string) (string, error) {
			urlImportCalled = true
			return "", nil
		},
		RefreshSubstackFeed: func(context.Context) (string, error) {
			return "feed refresh report", nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	response := post(t, server, "/import/substack/refresh", url.Values{})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if urlImportCalled {
		t.Error("refreshing the feed called the single-URL import closure")
	}
	if !strings.Contains(response.Body.String(), "feed refresh report") {
		t.Error("the feed-refresh report was not shown")
	}
}
