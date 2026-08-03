package web

import (
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

// invisibleFormatting is the set of Unicode characters that carry no
// meaning as plain text but frequently ride along in scraped press HTML:
// soft hyphens marking a typesetter's optional line-break point, and a
// handful of zero-width joiners and marks. Confirmed on a real article
// (soft hyphens throughout an FT piece, littering every export of the
// highlight's text) rather than assumed from a general character list.
//
// Spelled out as \u escapes rather than typed literally: these characters
// are invisible by definition, so a literal in source would be exactly as
// unverifiable by eye as the bug this exists to fix.
var invisibleFormatting = map[rune]bool{
	'\u00ad': true, // soft hyphen
	'\u200b': true, // zero-width space
	'\u200c': true, // zero-width non-joiner
	'\u200d': true, // zero-width joiner
	'\u2060': true, // word joiner
	'\ufeff': true, // byte-order mark / zero-width no-break space
}

// stripInvisibleFormatting removes invisibleFormatting's characters from
// HTML text. Safe to run over the whole markup string, not just text nodes:
// none of these characters have any legitimate role in tag or attribute
// syntax, so nothing structural can depend on one surviving.
func stripInvisibleFormatting(s string) string {
	return strings.Map(func(r rune) rune {
		if invisibleFormatting[r] {
			return -1
		}
		return r
	}, s)
}

// sanitize runs the bluemonday policy and then strips invisibleFormatting.
//
// This must be the only path from raw article HTML to anything ir.ParseArticle
// or Locate ever sees: block text, offsets and rendered markup are all derived
// from whatever string comes out of here, so stripping later — say, from an
// already-parsed Block.Text — would shorten it without shortening the DOM
// node backing it, and silently misalign every offset downstream.
func (s *Server) sanitize(rawHTML string) string {
	return stripInvisibleFormatting(s.policy.Sanitize(rawHTML))
}

// newPolicy builds the sanitiser applied to every article body before it is
// parsed or rendered.
//
// Article HTML is arbitrary content from the open web. Even though increader is
// only reachable over Tailscale, a malicious article rendered in the reader
// would run with full access to increader's own origin — it could create
// extracts, dismiss material, or read the whole library. Sanitising is not
// optional because the deployment is private.
//
// The policy is also load-bearing for correctness, not just safety: block
// indices and character offsets are computed against the *sanitised* HTML, so
// this function defines the coordinate system that the browser and the server
// agree on. It must therefore be deterministic, which bluemonday is.
func newPolicy() *bluemonday.Policy {
	policy := bluemonday.UGCPolicy()

	// Structural elements a readable article needs and UGCPolicy omits.
	policy.AllowElements("figure", "figcaption", "picture", "section", "article")

	// Images are worth keeping — many articles are unreadable without their
	// diagrams. The cost is that loading one tells its host you are reading
	// this article, which for a private reader is a modest but real leak.
	policy.AllowImages()
	policy.AllowAttrs("alt", "title").OnElements("img")

	// Links open elsewhere and must not hand the opener a window reference.
	policy.RequireNoFollowOnLinks(true)
	policy.RequireNoReferrerOnLinks(true)
	policy.AddTargetBlankToFullyQualifiedLinks(true)

	// Only absolute web URLs survive. A relative link in an article would
	// otherwise resolve against increader's own origin and point at the app.
	policy.AllowURLSchemes("http", "https", "mailto")
	policy.RequireParseableURLs(true)
	policy.AllowRelativeURLs(false)

	return policy
}
