package ingest

import (
	"strings"
	"testing"
)

func TestForWallabagDemotesH1(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a mid-article h1 is demoted to h2",
			in:   "<p>Intro.</p><h1>A Section Heading</h1><p>More text.</p>",
			want: "<p>Intro.</p><h2>A Section Heading</h2><p>More text.</p>",
		},
		{
			name: "several h1s all get demoted",
			in:   "<h1>First</h1><p>Body.</p><h1>Second</h1>",
			want: "<h2>First</h2><p>Body.</p><h2>Second</h2>",
		},
		{
			name: "an h1 nested inside another element is still found",
			in:   `<div class="section"><h1>Nested</h1></div>`,
			want: `<div class="section"><h2>Nested</h2></div>`,
		},
		{
			name: "h2 through h6 are left alone",
			in:   "<h2>Two</h2><h3>Three</h3><h4>Four</h4><h5>Five</h5><h6>Six</h6>",
			want: "<h2>Two</h2><h3>Three</h3><h4>Four</h4><h5>Five</h5><h6>Six</h6>",
		},
		{
			name: "content with no heading at all is unaffected",
			in:   "<p>Just a paragraph.</p><p>And another.</p>",
			want: "<p>Just a paragraph.</p><p>And another.</p>",
		},
		{
			name: "empty content stays empty",
			in:   "",
			want: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := forWallabag(test.in)
			if got != test.want {
				t.Errorf("forWallabag(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

// TestForWallabagPreservesInlineMarkupAndText: the fix must not become a
// second, cruder sanitiser — a link, its href, and ordinary text either
// side of the demoted heading all have to survive exactly as given.
func TestForWallabagPreservesInlineMarkupAndText(t *testing.T) {
	in := `<p>Before <a href="https://example.com">a link</a>.</p><h1>Heading with <em>emphasis</em></h1><p>Après un titre.</p>`
	got := forWallabag(in)

	if !strings.Contains(got, `<a href="https://example.com">a link</a>`) {
		t.Errorf("forWallabag(%q) = %q, want the link preserved", in, got)
	}
	if !strings.Contains(got, "<h2>Heading with <em>emphasis</em></h2>") {
		t.Errorf("forWallabag(%q) = %q, want the demoted heading with its own inline markup intact", in, got)
	}
	if !strings.Contains(got, "Après un titre.") {
		t.Errorf("forWallabag(%q) = %q, want non-ASCII text preserved", in, got)
	}
}

// TestForWallabagIsByteLengthNeutral pins the claim forWallabag's own doc
// comment makes: demoting cannot itself change planOne's NewBytes/OldBytes
// comparisons, because "h1" and "h2" are the same length. If this ever stops
// holding — a different demotion target, say — planOne's byte-ratio logic
// would need a second look.
func TestForWallabagIsByteLengthNeutral(t *testing.T) {
	in := "<p>Intro.</p><h1>A Section Heading</h1><p>More text.</p>"
	got := forWallabag(in)
	if len(got) != len(in) {
		t.Errorf("forWallabag changed the byte length: %d -> %d", len(in), len(got))
	}
}
