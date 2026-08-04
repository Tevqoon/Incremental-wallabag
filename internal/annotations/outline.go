package annotations

import (
	"sort"

	"rsc.io/pdf"
)

// outline is a PDF's table of contents, flattened to what a chapter lookup
// needs: a title and the page it starts on, in document order.
//
// rsc.io/pdf exposes an Outline tree already, but it keeps only the titles —
// buildOutline reads Title and recurses, and drops the destination — so it
// cannot answer "which chapter is page 214 in", which is the only question
// asked of it here. Hence walking the outline dictionary directly.
type outline struct {
	entries []outlineEntry
}

type outlineEntry struct {
	title string
	page  int
}

// chapterFor names the section a page falls in.
//
// The last entry that starts at or before the page wins, which is the same
// rule the PyMuPDF extractor uses: entries are in document order, so the
// most recent heading passed is the one you are underneath. Nesting is
// deliberately flattened rather than preferring a particular depth — a
// subsection heading is a more useful answer than the part title three
// levels up, and a book that only has part titles still gets those.
func (o outline) chapterFor(page int) string {
	chapter := ""
	for _, entry := range o.entries {
		if entry.page > page {
			break
		}
		chapter = entry.title
	}
	return chapter
}

// readOutline flattens a PDF's outline into page-ordered entries.
//
// Returns an empty outline for a document with none, which is the common case
// for a scanned paper and for anything produced by a word processor. Callers
// then get an empty chapter, and the reader can set one by hand.
func readOutline(reader *pdf.Reader) (result outline) {
	defer func() {
		// A malformed outline is not worth failing an import over: losing
		// chapter names is a far smaller loss than losing the annotations.
		if recover() != nil {
			result = outline{}
		}
	}()

	root := reader.Trailer().Key("Root")
	node := root.Key("Outlines")
	if node.Kind() != pdf.Dict {
		return outline{}
	}

	pages := pageNumbers(reader)
	if len(pages) == 0 {
		return outline{}
	}

	var entries []outlineEntry
	var walk func(entry pdf.Value, depth int)
	walk = func(entry pdf.Value, depth int) {
		// A cycle in the sibling chain would otherwise hang the import;
		// real outlines are a handful of levels deep at most.
		if depth > 32 {
			return
		}
		seen := 0
		for child := entry.Key("First"); child.Kind() == pdf.Dict; child = child.Key("Next") {
			seen++
			if seen > 10000 {
				return
			}
			title := collapseSpace(child.Key("Title").Text())
			if page, ok := destinationPage(root, child, pages); ok && title != "" {
				entries = append(entries, outlineEntry{title: title, page: page})
			}
			walk(child, depth+1)
		}
	}
	walk(node, 0)

	// Flattening the tree interleaves a section's children after it but
	// before the next section, which is already page order for a sane
	// document. Sorting makes it page order for the rest, and a stable sort
	// keeps a section ahead of its own first subsection when they start on
	// the same page.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].page < entries[j].page })
	return outline{entries: entries}
}

// pageNumbers maps each page's dictionary to its one-based page number.
//
// A destination names its page by indirect reference, and rsc.io/pdf resolves
// references on access without exposing the underlying object number — so
// there is no id to compare. What it does expose is a printed form of the
// resolved dictionary, in which nested references print as "N G R", and a
// page dictionary always carries at least its own /Contents and /Parent
// references. That makes the printed form distinct per page in practice, and
// it is the only identity available without forking the library.
//
// Built once per document, so a book's outline costs one pass over its pages
// rather than a scan per entry.
func pageNumbers(reader *pdf.Reader) map[string]int {
	count := reader.NumPage()
	if count <= 0 {
		return nil
	}

	pages := make(map[string]int, count)
	for number := 1; number <= count; number++ {
		page := reader.Page(number)
		if page.V.IsNull() {
			continue
		}
		key := page.V.String()
		// First writer wins: two pages sharing a printed form are
		// indistinguishable here, and the earlier one is the better guess
		// for a heading that points at either.
		if _, taken := pages[key]; !taken {
			pages[key] = number
		}
	}
	return pages
}

// destinationPage resolves an outline entry to a page number.
//
// Three spellings have to be handled, all of them common: an explicit /Dest
// array whose first element is the page; a /Dest naming an entry in one of
// the document's two destination catalogues; and an /A action, which is what
// a PDF written from an authoring tool usually has instead.
func destinationPage(root, entry pdf.Value, pages map[string]int) (int, bool) {
	dest := entry.Key("Dest")
	if dest.IsNull() {
		action := entry.Key("A")
		if action.Kind() == pdf.Dict {
			dest = action.Key("D")
		}
	}

	switch dest.Kind() {
	case pdf.Array:
		if dest.Len() == 0 {
			return 0, false
		}
		number, ok := pages[dest.Index(0).String()]
		return number, ok

	case pdf.Name, pdf.String:
		named := lookupNamedDestination(root, destName(dest))
		if named.Kind() == pdf.Dict {
			// A named destination may be wrapped in a dictionary with the
			// array under /D.
			named = named.Key("D")
		}
		if named.Kind() == pdf.Array && named.Len() > 0 {
			number, ok := pages[named.Index(0).String()]
			return number, ok
		}
	}
	return 0, false
}

// destName reads a destination's name, whichever way it is spelled.
func destName(dest pdf.Value) string {
	if dest.Kind() == pdf.Name {
		return dest.Name()
	}
	return dest.RawString()
}

// lookupNamedDestination finds a destination by name.
//
// PDF has two catalogues for this and both are still in use: /Root/Dests, a
// plain dictionary from the original design, and /Root/Names/Dests, a
// balanced name tree from the revision that replaced it.
func lookupNamedDestination(root pdf.Value, name string) pdf.Value {
	if name == "" {
		return pdf.Value{}
	}

	if legacy := root.Key("Dests").Key(name); !legacy.IsNull() {
		return legacy
	}
	return searchNameTree(root.Key("Names").Key("Dests"), name, 0)
}

// searchNameTree walks a PDF name tree looking for one key.
//
// A name tree node is either a leaf carrying /Names — a flat array
// alternating key and value — or an interior node carrying /Kids. /Limits
// would allow skipping subtrees that cannot contain the key, but a document's
// destination tree is small enough that searching all of it is cheaper than
// getting the string comparison subtly wrong.
func searchNameTree(node pdf.Value, name string, depth int) pdf.Value {
	if node.Kind() != pdf.Dict || depth > 32 {
		return pdf.Value{}
	}

	if names := node.Key("Names"); names.Kind() == pdf.Array {
		for i := 0; i+1 < names.Len(); i += 2 {
			if names.Index(i).RawString() == name {
				return names.Index(i + 1)
			}
		}
	}

	if kids := node.Key("Kids"); kids.Kind() == pdf.Array {
		for i := 0; i < kids.Len(); i++ {
			if found := searchNameTree(kids.Index(i), name, depth+1); !found.IsNull() {
				return found
			}
		}
	}
	return pdf.Value{}
}
