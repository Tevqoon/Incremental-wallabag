package web

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// embedTweetComponent is the data-component-name value Substack's own
// tweet-embed widget carries on its outer <a> — see rewriteEmbeds.
const embedTweetComponent = "Twitter2ToDOM"

// framePrefix introduces the link an <iframe> is replaced by, so the reader
// can tell it apart from a link the author wrote inline. Without it a bare
// title reads as an ordinary aside rather than as "something was embedded
// here and this is where it lives".
const framePrefix = "Embedded content"

// missingHandleSpace finds a name run directly into its @handle with no
// separating space. Substack's markup concatenates them across two
// separately-styled elements with nothing between, which reads fine with
// its own CSS and like a typo without it.
var missingHandleSpace = regexp.MustCompile(`([^\s])@`)

// rewriteEmbeds replaces the embedded widgets that cannot survive rendering
// with a single blockquote linking out to whatever they were showing: a
// Substack tweet-embed widget, and any <iframe> at all.
//
// The tweet widget never survives rendering as-is: it's a link wrapping
// several paragraphs (avatar, name, tweet text, engagement stats), and the
// reader's block model (see ir.ParseArticle) only ever keeps the deepest
// recognised block-level element in a chain like that — the link, sitting
// above all of them, is discarded entirely, and the paragraphs come out as
// several unrelated blocks with nothing left to say they were ever part of
// one embed. No sanitiser policy change can fix that: the element carrying
// any such marker never reaches rendering in the first place. A tweet's own
// attached photos are lost either way — Substack only ever injects those
// client-side, from a JSON blob on the widget that increader never executes
// the script to read.
//
// An <iframe> is lost for a different reason and needs the same answer.
// newPolicy's bluemonday policy has no rule admitting one and never will:
// an iframe is a whole nested browsing context, and article HTML is
// arbitrary content from the open web. So the frame is dropped outright,
// and because sites embed charts as bare fallback-free frames — danluu's
// interactive charts are one iframe each, no caption, no <noscript>, no
// still image — nothing is left behind to say a chart was ever there. The
// frame's own src and title are the only record, and they exist only until
// the sanitiser runs, which is why this recovers them beforehand.
//
// A blockquote wrapping one link is what survives that flattening intact:
// unlike a bare wrapping element, blockquote is itself one of the elements
// ir.ParseArticle recognises as an addressable block on its own, so the
// whole thing becomes one block whose only content is the link.
//
// sourceURL is the article's own URL, needed because an embed's src is
// routinely written relative to the page hosting it, and a link is worth
// nothing unless it is absolute by the time the policy vets it. Empty when
// the markup is not a whole article read at some address, in which case a
// relative src cannot be resolved at all and its frame is left to be
// stripped as before.
func rewriteEmbeds(rawHTML, sourceURL string) string {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return rawHTML
	}
	// A parse failure here is not worth abandoning the tweet rewrite for:
	// source stays nil, which frameEmbedReplacement reads as "no base to
	// resolve against", exactly as an empty sourceURL does.
	source, err := url.Parse(sourceURL)
	if err != nil || sourceURL == "" {
		source = nil
	}

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		for child := node.FirstChild; child != nil; {
			next := child.NextSibling
			if replacement := embedReplacement(child, source); replacement != nil {
				node.InsertBefore(replacement, child)
				node.RemoveChild(child)
			} else {
				walk(child)
			}
			child = next
		}
	}
	walk(doc)

	var out strings.Builder
	if err := html.Render(&out, doc); err != nil {
		return rawHTML
	}
	return out.String()
}

// embedReplacement builds the blockquote to replace node with, or nil if
// node is not an embed this pass knows how to salvage.
func embedReplacement(node *html.Node, source *url.URL) *html.Node {
	if replacement := tweetEmbedReplacement(node); replacement != nil {
		return replacement
	}
	return frameEmbedReplacement(node, source)
}

// frameEmbedReplacement builds the blockquote to replace an <iframe> with,
// or nil if node is not a frame or carries nothing that could be linked to.
//
// Applied to every frame rather than only to those a heuristic calls a
// chart: the policy admits none of them, so a frame left alone is a frame
// deleted, and a link out is better than that whatever it framed.
func frameEmbedReplacement(node *html.Node, source *url.URL) *html.Node {
	if node.Type != html.ElementNode || node.DataAtom != atom.Iframe {
		return nil
	}
	href := absoluteEmbedURL(attrValue(node, "src"), source)
	if href == "" {
		return nil
	}

	text := framePrefix
	// The title attribute is the frame's own description of what it shows,
	// and the only human-readable text a bare frame carries. Collapsed
	// because it is authored prose and may have been wrapped across lines.
	if title := strings.Join(strings.Fields(attrValue(node, "title")), " "); title != "" {
		text += ": " + title
	}

	link := &html.Node{
		Type: html.ElementNode, Data: "a", DataAtom: atom.A,
		Attr: []html.Attribute{{Key: "href", Val: href}},
	}
	link.AppendChild(&html.Node{Type: html.TextNode, Data: text})

	quote := &html.Node{Type: html.ElementNode, Data: "blockquote", DataAtom: atom.Blockquote}
	quote.AppendChild(link)
	return quote
}

// absoluteEmbedURL resolves src against the article's own URL and returns it
// only if the result is an ordinary web address, or "" otherwise.
//
// The scheme check is what keeps this from manufacturing a link the policy
// would have refused: a frame's src is routinely something no reader can
// usefully follow — about:blank, a data: document, a javascript: URL — and
// turning one of those into an <a> the reader is invited to click would be
// strictly worse than dropping the frame, which is what happens today.
func absoluteEmbedURL(src string, source *url.URL) string {
	if src == "" {
		return ""
	}
	target, err := url.Parse(src)
	if err != nil {
		return ""
	}
	if !target.IsAbs() {
		if source == nil {
			return ""
		}
		target = source.ResolveReference(target)
	}
	switch strings.ToLower(target.Scheme) {
	case "http", "https":
		return target.String()
	default:
		return ""
	}
}

// tweetEmbedReplacement builds the blockquote to replace node with, or nil
// if node is not one of Substack's tweet embeds.
func tweetEmbedReplacement(node *html.Node) *html.Node {
	if node.Type != html.ElementNode || node.DataAtom != atom.A || attrValue(node, "data-component-name") != embedTweetComponent {
		return nil
	}
	href := redirectToXcancel(attrValue(node, "href"))
	if href == "" {
		return nil
	}

	paragraphs := collectParagraphText(node, 2)
	if len(paragraphs) == 0 {
		return nil
	}
	text := paragraphs[0]
	if len(paragraphs) > 1 {
		text = missingHandleSpace.ReplaceAllString(text, "$1 @") + `: "` + paragraphs[1] + `"`
	}

	link := &html.Node{
		Type: html.ElementNode, Data: "a", DataAtom: atom.A,
		Attr: []html.Attribute{{Key: "href", Val: href}},
	}
	link.AppendChild(&html.Node{Type: html.TextNode, Data: text})

	quote := &html.Node{Type: html.ElementNode, Data: "blockquote", DataAtom: atom.Blockquote}
	quote.AppendChild(link)
	return quote
}

// redirectToXcancel points a tweet's link at xcancel.com — a read-only
// mirror with no login wall — instead of x.com itself. Everything else
// about the URL (the path carrying the author and status id) is unchanged,
// so it still resolves to the same tweet.
func redirectToXcancel(href string) string {
	parsed, err := url.Parse(href)
	if err != nil {
		return href
	}
	switch strings.ToLower(parsed.Host) {
	case "x.com", "www.x.com", "twitter.com", "www.twitter.com":
		parsed.Host = "xcancel.com"
		return parsed.String()
	default:
		return href
	}
}

func attrValue(node *html.Node, key string) string {
	for _, a := range node.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// collectParagraphText walks node's descendants in document order and
// returns up to limit <p> elements' text, each collapsed to a single line.
// A tweet embed's name/handle line and its own text are the first two, in
// that order; anything after (timestamp, engagement counts) is not
// collected.
func collectParagraphText(node *html.Node, limit int) []string {
	var out []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(out) >= limit {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.P {
			if text := strings.Join(strings.Fields(textOf(n)), " "); text != "" {
				out = append(out, text)
			}
			return
		}
		for c := n.FirstChild; c != nil && len(out) < limit; c = c.NextSibling {
			walk(c)
		}
	}
	walk(node)
	return out
}

func textOf(node *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(node)
	return b.String()
}
