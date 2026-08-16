package substack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostFromURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantHost string
		wantSlug string
		wantErr  bool
	}{
		{
			name:     "ordinary post URL",
			url:      "https://example.substack.com/p/some-post-title",
			wantHost: "example.substack.com",
			wantSlug: "some-post-title",
		},
		{
			name:     "custom domain",
			url:      "https://news.example.com/p/some-post-title",
			wantHost: "news.example.com",
			wantSlug: "some-post-title",
		},
		{
			name:     "trailing /comments",
			url:      "https://example.substack.com/p/some-post-title/comments",
			wantHost: "example.substack.com",
			wantSlug: "some-post-title",
		},
		{
			name:     "trailing slash",
			url:      "https://example.substack.com/p/some-post-title/",
			wantHost: "example.substack.com",
			wantSlug: "some-post-title",
		},
		{
			name:     "tracking query string",
			url:      "https://example.substack.com/p/some-post-title?utm_source=share&utm_medium=web",
			wantHost: "example.substack.com",
			wantSlug: "some-post-title",
		},
		{
			name:    "not a post URL at all",
			url:     "https://example.substack.com/archive",
			wantErr: true,
		},
		{
			name:    "no host",
			url:     "/p/some-post-title",
			wantErr: true,
		},
		{
			name:    "empty slug",
			url:     "https://example.substack.com/p/",
			wantErr: true,
		},
		{
			name:    "not a URL at all",
			url:     "not a url \x7f",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host, slug, err := PostFromURL(test.url)
			if test.wantErr {
				if err == nil {
					t.Fatalf("PostFromURL(%q) = (%q, %q, nil), want an error", test.url, host, slug)
				}
				return
			}
			if err != nil {
				t.Fatalf("PostFromURL(%q): %v", test.url, err)
			}
			if host != test.wantHost || slug != test.wantSlug {
				t.Errorf("PostFromURL(%q) = (%q, %q), want (%q, %q)",
					test.url, host, slug, test.wantHost, test.wantSlug)
			}
		})
	}
}

// TestFetchPostFreePost: a free post needs no differential check at all —
// asserted here not just by the returned Document but by counting requests,
// so this cannot silently pass by accident if FetchPost started always
// double-fetching.
func TestFetchPostFreePost(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.posts["a-free-post"] = newFreePostFixture(1, "a-free-post")
	importer := newTestImporter(t, fake.Server, nil)

	doc, warnings, err := importer.FetchPost(context.Background(), "a-free-post")
	if err != nil {
		t.Fatalf("FetchPost: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if doc.Title != "Post 1" || doc.ExternalID != "1" {
		t.Errorf("doc = %+v, want the fixture's own id and title", doc)
	}
	if !strings.Contains(doc.ContentHTML, "full body of a free post") {
		t.Errorf("ContentHTML = %q, want the fixture's body", doc.ContentHTML)
	}

	if got := fake.postRequestCount("a-free-post"); got != 1 {
		t.Errorf("post requested %d times, want exactly 1 — a free post needs no differential check", got)
	}
}

// TestFetchPostWorkingPaidSession covers a paid post fetched under a
// session that genuinely unlocks it — the differential check must pass and
// the full body must come back.
func TestFetchPostWorkingPaidSession(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.posts["a-paid-post"] = newWorkingPaidPostFixture(2, "a-paid-post")
	importer := newTestImporter(t, fake.Server, nil)

	doc, _, err := importer.FetchPost(context.Background(), "a-paid-post")
	if err != nil {
		t.Fatalf("FetchPost: %v", err)
	}
	if !strings.Contains(doc.ContentHTML, "Real paid content") {
		t.Errorf("ContentHTML = %q, want the fixture's full body", doc.ContentHTML)
	}

	// Once authenticated, once anonymous — the differential the canary
	// depends on.
	if got := fake.postRequestCount("a-paid-post"); got != 2 {
		t.Errorf("post requested %d times, want exactly 2 (authenticated + anonymous canary)", got)
	}
}

// TestFetchPostLapsedPaidSession is the failure this whole differential
// check exists for: a dead session gets the same short preview whether or
// not the cookie is sent, and FetchPost must refuse to hand that back as if
// it were the real article.
func TestFetchPostLapsedPaidSession(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.posts["a-paid-post"] = newLapsedPaidPostFixture(3, "a-paid-post")
	importer := newTestImporter(t, fake.Server, nil)

	_, _, err := importer.FetchPost(context.Background(), "a-paid-post")
	if err == nil {
		t.Fatal("FetchPost succeeded against a lapsed session on a paid post; want an error")
	}
}

// TestFetchPostDoesNotWriteCache: unlike post (Ingest's own per-post
// fetch), FetchPost must never write a cache entry — see its own doc
// comment for why trusting one here would be unsound.
func TestFetchPostDoesNotWriteCache(t *testing.T) {
	fake := newFakeSubstack(t)
	fake.posts["a-free-post"] = newFreePostFixture(1, "a-free-post")

	cacheDir := t.TempDir()
	importer := newTestImporter(t, fake.Server, func(cfg *Config) { cfg.CacheDir = cacheDir })

	if _, _, err := importer.FetchPost(context.Background(), "a-free-post"); err != nil {
		t.Fatalf("FetchPost: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cacheDir, importer.cfg.Host, "a-free-post.json")); err == nil {
		t.Error("FetchPost wrote a cache entry; want none")
	}
}

func TestFetchPostRejectsUnsafeSlug(t *testing.T) {
	fake := newFakeSubstack(t)
	importer := newTestImporter(t, fake.Server, nil)

	if _, _, err := importer.FetchPost(context.Background(), "../../etc/passwd"); err == nil {
		t.Fatal("FetchPost accepted a path-traversal slug; want an error")
	}
}

func TestFetchPostMissing(t *testing.T) {
	fake := newFakeSubstack(t)
	importer := newTestImporter(t, fake.Server, nil)

	if _, _, err := importer.FetchPost(context.Background(), "no-such-post"); err == nil {
		t.Fatal("FetchPost succeeded for a slug the fake server has nothing for; want an error")
	}
}
