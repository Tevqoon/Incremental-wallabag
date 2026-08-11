package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestWatchQueueListsTheArticle(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	response := get(t, server, "/w")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	body := response.Body.String()
	if !strings.Contains(body, "A test article") {
		t.Error("watch queue does not list the article")
	}
	// The watch layout must not pull in anything that depends on
	// JavaScript running: no htmx, no MathJax, no site nav.
	if strings.Contains(body, "htmx") || strings.Contains(body, "mathjax") {
		t.Error("watch queue pulled in a script the watch web viewer might choke on")
	}
}

func TestWatchReadShowsArticleAndGradeForms(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	// The root topic for the seeded document is element 1, same as every
	// other reader test in this package assumes.
	response := get(t, server, "/w/read/1")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	body := response.Body.String()
	if !strings.Contains(body, "quick brown fox") {
		t.Error("watch reader does not show the article text")
	}
	if !strings.Contains(body, `action="/elements/1/grade"`) {
		t.Error("watch reader does not post grades through a plain form")
	}
	if !strings.Contains(body, `name="redirect" value="/w/next"`) {
		t.Error("watch reader's grade forms should redirect back into the watch flow")
	}
}

func TestWatchGradeRedirectsWithinWatchFlow(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	form := url.Values{"grade": {"next"}, "redirect": {"/w/next"}}
	response := post(t, server, "/elements/1/grade", form)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", response.Code)
	}
	if got := response.Header().Get("Location"); got != "/w/next" {
		t.Errorf("Location = %q, want /w/next", got)
	}
}

func TestWatchNextRedirectsToNextDueOrQueue(t *testing.T) {
	server, _, _ := newTestServer(t, true)

	response := get(t, server, "/w/next")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", response.Code)
	}
	if got := response.Header().Get("Location"); got != "/w/read/1" {
		t.Errorf("Location = %q, want /w/read/1", got)
	}
}
