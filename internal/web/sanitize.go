package web

import "github.com/microcosm-cc/bluemonday"

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
