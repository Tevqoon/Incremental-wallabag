package substack

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPostHonoursCache pre-seeds CacheDir with one clean cached post and one
// paywalled cached post, then runs Ingest over an archive naming both. The
// clean one must be served from cache with no network request; the
// paywalled one, despite being "cached", was fetched while the subscription
// had lapsed and must be re-fetched — see post's own doc comment on why a
// cached-but-paywalled file is not trusted.
func TestPostHonoursCache(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.archivePages[0] = []archiveFixture{
		newArchiveFixture(1, "clean-cached", "newsletter", "everyone"),
		newArchiveFixture(2, "paywalled-cached", "newsletter", "only_paid"),
	}
	// The network copy of the paywalled slug, once increader actually
	// fetches it, has real content — proving the refetch happened and was
	// used, not just requested and discarded. Padded well past
	// paywallBodyLengthThreshold: isPaywalled treats a short only_paid body
	// as a preview even with no paywall marker present, so a short fixture
	// here would trip that heuristic and make this test indistinguishable
	// from testing the length check instead of the cache-skip rule it
	// actually targets.
	fake.posts["paywalled-cached"] = postFixture{
		ID: 2, Slug: "paywalled-cached", Type: "newsletter", Audience: "only_paid",
		Title: "Post 2", CanonicalURL: "https://example.substack.com/p/paywalled-cached",
		PostDate: testPostDate.Format("2006-01-02T15:04:05Z07:00"),
		BodyHTML: "<p>Now that the subscription is active again, here is the real, full body of this post. " +
			strings.Repeat("Padding this well past the paywall body length threshold with ordinary prose. ", 40) +
			"</p>",
	}

	importer := newTestImporter(t, fake.Server, nil)

	seedCache(t, importer, "clean-cached", newFreePostFixture(1, "clean-cached"))
	seedCache(t, importer, "paywalled-cached", postFixture{
		ID: 2, Slug: "paywalled-cached", Type: "newsletter", Audience: "only_paid",
		Title: "Post 2", CanonicalURL: "https://example.substack.com/p/paywalled-cached",
		PostDate: testPostDate.Format("2006-01-02T15:04:05Z07:00"),
		BodyHTML: `<p>Teaser.</p><div class="paywall"><p>Subscribe.</p></div>`,
	})

	logger := testLogger(&strings.Builder{})
	documents, result, err := importer.Ingest(context.Background(), logger)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if result.Cached != 1 {
		t.Errorf("Result.Cached = %d, want 1", result.Cached)
	}
	if result.Fetched != 1 {
		t.Errorf("Result.Fetched = %d, want 1", result.Fetched)
	}
	if got := fake.postRequestCount("clean-cached"); got != 0 {
		t.Errorf("clean-cached was requested %d times over the network, want 0", got)
	}
	if got := fake.postRequestCount("paywalled-cached"); got != 1 {
		t.Errorf("paywalled-cached was requested %d times over the network, want exactly 1", got)
	}
	if len(documents) != 2 {
		t.Fatalf("len(documents) = %d, want 2", len(documents))
	}
}

// seedCache writes fixture directly to where cachePath says slug's cache
// file belongs, bypassing the network entirely — simulating a file left
// behind by an earlier run.
func seedCache(t *testing.T, importer *Importer, slug string, fixture postFixture) {
	t.Helper()
	path := importer.cachePath(slug)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("seed cache dir: %v", err)
	}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("seed cache file: %v", err)
	}
}

// TestIngestAbortsAfterThreeConsecutivePaywalledFetches pins the
// cookie-validity guard: Ingest must abort the entire run — not skip one
// post and continue — once three fetches in a row come back paywalled, and
// nothing after the third failing slug may be requested at all.
func TestIngestAbortsAfterThreeConsecutivePaywalledFetches(t *testing.T) {
	fake := newFakeSubstack(t)

	var archive []archiveFixture
	for id := 1; id <= 5; id++ {
		slug := "paid-" + string(rune('a'+id-1))
		archive = append(archive, newArchiveFixture(id, slug, "newsletter", "only_paid"))
		fake.posts[slug] = newPaywalledPostFixture(id, slug)
	}
	fake.archivePages[0] = archive

	importer := newTestImporter(t, fake.Server, nil)
	logger := testLogger(&strings.Builder{})

	_, result, err := importer.Ingest(context.Background(), logger)
	if err == nil {
		t.Fatal("expected an error aborting the run")
	}
	if !strings.Contains(err.Error(), "3 consecutive") {
		t.Errorf("error = %q, want it to name the 3-consecutive-paywall guard", err.Error())
	}
	if result.StillPaywalled != 3 {
		t.Errorf("Result.StillPaywalled = %d, want 3 (the abort must fire on the third, not run past it)", result.StillPaywalled)
	}

	// Slugs 4 and 5 (paid-d, paid-e) must never have been requested: the
	// abort has to stop the whole run, not merely record the failure and
	// move on to the next post.
	for _, slug := range []string{"paid-d", "paid-e"} {
		if got := fake.postRequestCount(slug); got != 0 {
			t.Errorf("slug %q was requested %d times after the abort should have fired, want 0", slug, got)
		}
	}
}

// TestFetchRetriesAfter429ThenSucceeds covers getJSON/fetchRaw's retry path:
// a 429 on the first attempt must not fail the request outright, and the
// eventual 200 must be the one that decides the result.
func TestFetchRetriesAfter429ThenSucceeds(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.archivePages[0] = []archiveFixture{
		newArchiveFixture(1, "flaky", "newsletter", "everyone"),
	}
	fake.posts["flaky"] = newFreePostFixture(1, "flaky")
	fake.postStatus["flaky"] = []int{http.StatusTooManyRequests, http.StatusOK}

	importer := newTestImporter(t, fake.Server, nil)

	body, fromCache, err := importer.post(context.Background(), "flaky")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if fromCache {
		t.Error("fromCache = true, want false (this was never cached)")
	}
	if body.ID != 1 {
		t.Errorf("body.ID = %d, want 1", body.ID)
	}
	if got := fake.postRequestCount("flaky"); got != 2 {
		t.Errorf("requests = %d, want 2 (one 429, one success)", got)
	}
}

// TestFetchGivesUpAfterMaxAttempts covers the other half: a slug that keeps
// failing with a retryable status must stop being retried once MaxAttempts
// is spent, and report an error rather than hang or retry forever.
func TestFetchGivesUpAfterMaxAttempts(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.postStatus["always-500"] = []int{http.StatusInternalServerError}

	importer := newTestImporter(t, fake.Server, func(cfg *Config) {
		cfg.MaxAttempts = 3
	})

	_, _, err := importer.post(context.Background(), "always-500")
	if err == nil {
		t.Fatal("expected an error after exhausting MaxAttempts")
	}
	if got := fake.postRequestCount("always-500"); got != 3 {
		t.Errorf("requests = %d, want exactly MaxAttempts (3)", got)
	}
}

// TestPostRejectsPathTraversalSlug pins validateSlug's job: an archive
// listing is untrusted input, and a slug built to escape CacheDir via "../"
// or an embedded "/" must be rejected outright, before any file I/O or
// network request is attempted.
func TestPostRejectsPathTraversalSlug(t *testing.T) {
	fake := newFakeSubstack(t)
	importer := newTestImporter(t, fake.Server, nil)

	for _, slug := range []string{
		"../../etc/passwd",
		"foo/../../bar",
		"nested/slug",
		"",
	} {
		t.Run(slug, func(t *testing.T) {
			_, _, err := importer.post(context.Background(), slug)
			if err == nil {
				t.Fatalf("post(%q): expected an error, got none", slug)
			}
			if got := fake.postRequestCount(slug); got != 0 {
				t.Errorf("post(%q) made %d network requests, want 0", slug, got)
			}
		})
	}

	// cachePath itself is a second, independent line of defense: even
	// called directly with something unsafe, it must never produce a path
	// outside CacheDir/Host.
	path := importer.cachePath("../../../etc/passwd")
	if strings.Contains(path, "..") {
		t.Errorf("cachePath(%q) = %q, still contains \"..\"", "../../../etc/passwd", path)
	}
	cacheRoot := filepath.Join(importer.cfg.CacheDir, importer.cfg.Host)
	if !strings.HasPrefix(path, cacheRoot) {
		t.Errorf("cachePath escaped CacheDir/Host: got %q, want a prefix of %q", path, cacheRoot)
	}
}

// TestHasPaywallMarker checks the true and false sides of detection: a real
// preview fixture (a paywall div) must be caught, and a free post that
// merely ends with its own subscribe call-to-action prose must not be —
// the false-positive hasPaywallMarker's doc comment specifically calls out.
func TestHasPaywallMarker(t *testing.T) {
	tests := []struct {
		name string
		html string
		want bool
	}{
		{
			name: "real paywall block",
			html: `<p>Here is a short teaser paragraph.</p><div class="paywall"><p>Subscribe to keep reading.</p></div>`,
			want: true,
		},
		{
			name: "subscribe widget class",
			html: `<p>Body text.</p><div class="subscribe-widget"><a href="#">Subscribe now</a></div>`,
			want: true,
		},
		{
			name: "free post ending with its own subscribe CTA prose",
			html: `<p>A full, long article body with plenty of real content in it.</p><p>Thanks for reading! Subscribe below to get future posts in your inbox.</p>`,
			want: false,
		},
		{
			name: "empty body",
			html: "",
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasPaywallMarker(test.html); got != test.want {
				t.Errorf("hasPaywallMarker(%q) = %v, want %v", test.html, got, test.want)
			}
		})
	}
}
