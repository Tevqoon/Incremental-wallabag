package ir

import "strings"

// Locate finds a passage in the article by its text and returns the range it
// occupies.
//
// This is how a highlight made somewhere else — in wallabag's own reader, in
// KOReader — becomes an addressable extract here. Those systems record their
// highlights against their own copy of the document, with offsets that do not
// survive increader's sanitising, so the quoted text is the only durable
// handle on the passage.
//
// Matching ignores whitespace differences, because the other system's copy of
// the article will have been wrapped and indented differently from this one.
// Only single-block matches are reported: a quote spanning paragraphs cannot
// be located this way, since the block separator is exactly the whitespace the
// comparison discards.
func (a *Article) Locate(quote string) (Range, bool) {
	needle := NormalizeSpace(quote)
	if needle == "" {
		return Range{}, false
	}

	for _, block := range a.blocks {
		normalized, offsets := normalizeWithOffsets(block.Text)

		position := strings.Index(normalized, needle)
		if position < 0 {
			continue
		}

		start := offsets[position]
		// offsets maps each normalised byte to where it came from, so the byte
		// after the match ends just past the last matched one.
		end := offsets[position+len(needle)-1] + 1

		return Range{
			StartBlock:  block.Index,
			StartOffset: start,
			EndBlock:    block.Index,
			EndOffset:   end,
		}, true
	}

	return Range{}, false
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
