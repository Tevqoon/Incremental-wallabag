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

// missingHandleSpace finds a name run directly into its @handle with no
// separating space. Substack's markup concatenates them across two
// separately-styled elements with nothing between, which reads fine with
// its own CSS and like a typo without it.
var missingHandleSpace = regexp.MustCompile(`([^\s])@`)

// rewriteEmbeds finds Substack's own tweet-embed widgets in rawHTML and
// replaces each with a single blockquote linking to the original tweet.
//
// The widget itself never survives rendering as-is: it's a link wrapping
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
// A blockquote wrapping one link is what survives that flattening intact:
// unlike a bare wrapping element, blockquote is itself one of the elements
// ir.ParseArticle recognises as an addressable block on its own, so the
// whole thing becomes one block whose only content is the link.
func rewriteEmbeds(rawHTML string) string {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return rawHTML
	}

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		for child := node.FirstChild; child != nil; {
			next := child.NextSibling
			if replacement := tweetEmbedReplacement(child); replacement != nil {
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
