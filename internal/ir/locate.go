package ir

import "strings"

// Locate finds a passage in the article by its text and returns the range it
// occupies — including a passage that crosses a paragraph break, which a
// highlight of any substantial length very often does.
//
// This is how a highlight made somewhere else — in wallabag's own reader, in
// KOReader — becomes an addressable extract here. Those systems record their
// highlights against their own copy of the document, with offsets that do not
// survive increader's sanitising, so the quoted text is the only durable
// handle on the passage.
//
// Matching ignores whitespace differences, because the other system's copy of
// the article will have been wrapped and indented differently from this one —
// and a paragraph break is exactly that kind of difference: the other system
// already collapsed it to plain whitespace when it recorded a multi-paragraph
// quote, the same as it did with a wrapped line's newline, so every block is
// joined into one search space with the same normalisation before matching
// rather than searched one at a time. Searching block-by-block would make a
// spanning quote structurally unlocatable no matter how exactly it was
// recorded, since the whitespace the comparison discards is the only thing
// that ever stood between two blocks in the first place.
func (a *Article) Locate(quote string) (Range, bool) {
	if position, ok := a.locateExact(quote); ok {
		return position, true
	}

	// wallabag's own annotation storage silently truncates a long quote and
	// marks the cut with a trailing "…" — confirmed against a real imported
	// highlight, not assumed: 900-ish characters in, no space before it,
	// U+2026 rather than three periods, the same convention (and the same
	// practical limit — see wallabag.maxHighlightQuoteLength) increader's
	// own outbound side already works around when it creates a highlight
	// upstream. Real article prose is never going to end on that exact
	// character at that exact position, so the untruncated quote can only
	// ever fail here. What is left after cutting the "…" back off is still
	// real, unmodified article text — just less of it than was actually
	// highlighted — and anchoring that much is a real improvement over an
	// extract that never gets a position at all.
	//
	// Only tried as a fallback, after the exact match above: a quote that
	// genuinely ends in an ellipsis in the article itself already succeeds
	// on the first attempt, so this can never turn a real, different
	// passage into a false match — it only ever recovers a passage that was
	// otherwise unlocatable.
	if trimmed, cut := strings.CutSuffix(strings.TrimSpace(quote), "…"); cut {
		return a.locateExact(trimmed)
	}
	return Range{}, false
}

// locateExact is Locate's actual search — see Locate for the ellipsis
// fallback wrapped around it.
func (a *Article) locateExact(quote string) (Range, bool) {
	needle := NormalizeSpace(quote)
	if needle == "" {
		return Range{}, false
	}

	normalized, positions := a.flattenBlocks()

	index := strings.Index(normalized, needle)
	if index < 0 {
		return Range{}, false
	}

	start := positions[index]
	// positions maps each normalised byte to where it came from, so the byte
	// after the match ends just past the last matched one.
	end := positions[index+len(needle)-1]

	return Range{
		StartBlock:  start.block,
		StartOffset: start.offset,
		EndBlock:    end.block,
		EndOffset:   end.offset + 1,
	}, true
}

// blockOffset is one position in the search space flattenBlocks builds: which
// block a byte of it came from, and that byte's offset within that block's
// own (unnormalised) text — the coordinates every stored Range uses.
type blockOffset struct {
	block, offset int
}

// flattenBlocks joins every block's normalised text into one search space for
// Locate, each pair separated by a single space so a quote spanning a
// paragraph break lines up with it exactly the way NormalizeSpace already
// collapsed that break within the quote itself.
func (a *Article) flattenBlocks() (string, []blockOffset) {
	var (
		builder   strings.Builder
		positions []blockOffset
	)

	for index, block := range a.blocks {
		if index > 0 {
			builder.WriteByte(' ')
			// A position is still needed for the separator byte, purely to
			// keep `positions` index-aligned with `builder`'s bytes — it is
			// never actually read as a match's start or end, since
			// NormalizeSpace has already trimmed the needle's own leading
			// and trailing whitespace, so neither can land on a space.
			positions = append(positions, blockOffset{block: block.Index, offset: -1})
		}

		blockNormalized, offsets := normalizeWithOffsets(block.Text)
		for _, offset := range offsets {
			positions = append(positions, blockOffset{block: block.Index, offset: offset})
		}
		builder.WriteString(blockNormalized)
	}

	return builder.String(), positions
}

// normalizeWithOffsets collapses whitespace like NormalizeSpace, and also
// returns, for each byte of the result, the index it came from in the input.
//
// That index map is the whole point: a match is found in the normalised text
// but has to be reported in the original's coordinates, because those are what
// every stored range and rendered highlight uses.
func normalizeWithOffsets(raw string) (string, []int) {
	var (
		builder strings.Builder
		offsets []int
		inSpace = true // leading whitespace is dropped, as NormalizeSpace does
	)

	for index := 0; index < len(raw); index++ {
		character := raw[index]

		if character == ' ' || character == '\t' || character == '\n' || character == '\r' {
			if !inSpace {
				builder.WriteByte(' ')
				offsets = append(offsets, index)
				inSpace = true
			}
			continue
		}

		builder.WriteByte(character)
		offsets = append(offsets, index)
		inSpace = false
	}

	normalized := builder.String()

	// A run of trailing whitespace collapsed to one space that NormalizeSpace
	// would have trimmed; drop it from both the text and the map together.
	if strings.HasSuffix(normalized, " ") {
		normalized = normalized[:len(normalized)-1]
		offsets = offsets[:len(normalized)]
	}

	return normalized, offsets
}
