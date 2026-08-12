package substack

import (
	"strings"
	"testing"
)

// TestCleanBody is a table over what cleanBody must strip and, just as
// important, what it must leave completely untouched — the negative cases
// are the ones a naive implementation gets wrong, by over-matching into
// content that merely resembles chrome.
//
// The markup shapes here — subscribeComponentName,
// subscribeWidgetClassPrefix, subscribeWidgetExactClass, and the
// Image2ToDOM / PreformattedTextBlockToDOM / captioned-image-container /
// image2 / image2-inset / restack-image / is-viewable-img names in the
// negative cases — were confirmed against a live Substack API response on
// 2026-08-12, not invented. See isSubscribeChrome's own doc comment in
// clean.go for the fuller story of why an earlier, guessed version of these
// fixtures ("subscribe-widget", "paywall", "post-ufi") was wrong.
func TestCleanBody(t *testing.T) {
	tests := []struct {
		name      string
		html      string
		wantIn    []string // substrings that must survive in the output
		wantNotIn []string // substrings that must not appear in the output
	}{
		{
			name: "strips a subscribe widget identified by data-component-name",
			html: `<p>Real article text.</p>` +
				`<div data-component-name="SubscribeWidgetToDOM">` +
				`<p class="preamble">Get more from this publication</p>` +
				`<input class="fake-input"><button class="fake-button">Subscribe</button>` +
				`</div>`,
			wantIn:    []string{"Real article text."},
			wantNotIn: []string{"SubscribeWidgetToDOM", "fake-button", "Get more from this publication"},
		},
		{
			name: "strips subscribe chrome by the subscription-widget class family when the component attribute is absent",
			html: `<p>Real article text.</p>` +
				`<div class="subscription-widget-wrap-editor">` +
				`<div class="subscription-widget">` +
				`<p class="cta-caption">Subscribe</p>` +
				`<input class="email-input">` +
				`<button class="button primary">Subscribe now</button>` +
				`</div></div>`,
			wantIn:    []string{"Real article text."},
			wantNotIn: []string{"subscription-widget", "Subscribe now", "email-input"},
		},
		{
			name:      "strips a show-subscribe element",
			html:      `<p>Real article text.</p><div class="show-subscribe"><p>Subscribe to continue reading.</p></div>`,
			wantIn:    []string{"Real article text."},
			wantNotIn: []string{"show-subscribe", "Subscribe to continue reading."},
		},
		{
			name: "an Image2ToDOM component and its image classes survive untouched",
			html: `<p>Before.</p>` +
				`<div data-component-name="Image2ToDOM" class="captioned-image-container">` +
				`<figure class="image2 image2-inset">` +
				`<img class="is-viewable-img restack-image" src="https://substackcdn.com/image/fetch/w_1456/example.jpeg" alt="a photo">` +
				`<figcaption>A caption.</figcaption>` +
				`</figure></div><p>After.</p>`,
			wantIn: []string{
				`data-component-name="Image2ToDOM"`,
				`class="captioned-image-container"`,
				`class="image2 image2-inset"`,
				`class="is-viewable-img restack-image"`,
				`src="https://substackcdn.com/image/fetch/w_1456/example.jpeg"`,
				"A caption.", "Before.", "After.",
			},
		},
		{
			name: "a PreformattedTextBlockToDOM code block survives untouched",
			html: `<p>Before.</p><pre data-component-name="PreformattedTextBlockToDOM" class="preformatted-block"><code>func main() {}</code></pre><p>After.</p>`,
			wantIn: []string{
				`data-component-name="PreformattedTextBlockToDOM"`,
				`class="preformatted-block"`,
				"func main() {}",
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
