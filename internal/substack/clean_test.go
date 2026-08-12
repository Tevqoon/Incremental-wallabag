package substack

import (
	"strings"
	"testing"
)

// TestCleanBody is a table over what cleanBody must strip and, just as
// important, what it must leave completely untouched — the negative cases
// are the ones a naive implementation gets wrong, by over-matching into
// content that merely resembles chrome.
func TestCleanBody(t *testing.T) {
	tests := []struct {
		name      string
		html      string
		wantIn    []string // substrings that must survive in the output
		wantNotIn []string // substrings that must not appear in the output
	}{
		{
			name:      "strips a subscribe widget",
			html:      `<p>Real article text.</p><div class="subscribe-widget"><a href="#">Subscribe now</a></div>`,
			wantIn:    []string{"Real article text."},
			wantNotIn: []string{"Subscribe now", "subscribe-widget"},
		},
		{
			name:      "strips the like/comment/share row",
			html:      `<p>Real article text.</p><div class="post-ufi"><button>Like</button><button>Comment</button><button>Share</button></div>`,
			wantIn:    []string{"Real article text."},
			wantNotIn: []string{"post-ufi", "Comment</button>"},
		},
		{
			name:      "strips the paywall block",
			html:      `<p>Teaser paragraph.</p><div class="paywall"><p>Subscribe to keep reading</p></div>`,
			wantIn:    []string{"Teaser paragraph."},
			wantNotIn: []string{"paywall", "Subscribe to keep reading"},
		},
		{
			name: "a CDN image survives with its URL untouched",
			html: `<p>Before.</p><img src="https://substackcdn.com/image/fetch/w_1456/example.jpeg" alt="a photo"><p>After.</p>`,
			wantIn: []string{
				`src="https://substackcdn.com/image/fetch/w_1456/example.jpeg"`,
				"Before.", "After.",
			},
		},
		{
			name: "a tweet embed survives untouched",
			html: `<p>Before.</p><a data-component-name="Twitter2ToDOM" href="https://twitter.com/someone/status/12345"><p>Someone</p><p>A tweet.</p></a><p>After.</p>`,
			wantIn: []string{
				`data-component-name="Twitter2ToDOM"`,
				`href="https://twitter.com/someone/status/12345"`,
				"Someone", "A tweet.",
			},
		},
		{
			name:   "ordinary prose is unchanged",
			html:   `<p>Ordinary prose paragraph with nothing special about it at all.</p>`,
			wantIn: []string{"Ordinary prose paragraph with nothing special about it at all."},
		},
		{
			// Spelled with \u escapes rather than typed as literal
			// characters, matching invisibleFormatting's own convention in
			// clean.go: these characters are invisible by definition, so a
			// literal in a fixture would be exactly as unverifiable by eye
			// as the bug this test exists to catch.
			name:      "invisible formatting characters are stripped from text",
			html:      "<p>Soft\u00adhyphen and zero\u200bwidth space.</p>",
			wantIn:    []string{"Softhyphen and zerowidth space."},
			wantNotIn: []string{"\u00ad", "\u200b"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cleaned, warnings := cleanBody(test.html)
			if len(warnings) != 0 {
				t.Errorf("warnings = %v, want none", warnings)
			}
			for _, want := range test.wantIn {
				if !strings.Contains(cleaned, want) {
					t.Errorf("cleaned output missing %q\ngot: %s", want, cleaned)
				}
			}
			for _, notWant := range test.wantNotIn {
				if strings.Contains(cleaned, notWant) {
					t.Errorf("cleaned output still contains %q, want it stripped\ngot: %s", notWant, cleaned)
				}
			}
		})
	}
}

// TestCleanBodyEmptyInput checks the degenerate case does not panic and
// returns cleanly, since a post could plausibly have an empty body_html
// (e.g. an image-only post with no text).
func TestCleanBodyEmptyInput(t *testing.T) {
	cleaned, warnings := cleanBody("")
	if cleaned != "" {
		t.Errorf("cleaned = %q, want empty", cleaned)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}
