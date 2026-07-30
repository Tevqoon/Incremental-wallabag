package ir

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Cloze is one deletion within an item's text.
type Cloze struct {
	// Ordinal is the deletion's number. Anki turns each distinct ordinal into
	// its own card, so two deletions sharing an ordinal are tested together.
	Ordinal int

	// Start and End are character offsets into the item's text.
	Start int
	End   int

	// Hint is shown in place of the deletion. Optional.
	Hint string
}

// RenderCloze rewrites text with Anki's cloze syntax: {{c1::deleted}}, or
// {{c1::deleted::hint}} when a hint is set.
//
// This is the one place increader emits a foreign system's format, and it is
// deliberately a pure string transformation with no Anki dependency: the app
// stores offsets, and the syntax is generated at export time.
func RenderCloze(text string, clozes []Cloze) (string, error) {
	if len(clozes) == 0 {
		return text, nil
	}

	ordered := make([]Cloze, len(clozes))
	copy(ordered, clozes)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Start < ordered[j].Start })

	var out strings.Builder
	position := 0

	for _, cloze := range ordered {
		switch {
		case cloze.Start < 0 || cloze.End > len(text):
			return "", fmt.Errorf("ir: cloze c%d spans [%d,%d), outside a %d-character text",
				cloze.Ordinal, cloze.Start, cloze.End, len(text))
		case cloze.End <= cloze.Start:
			return "", fmt.Errorf("ir: cloze c%d is empty", cloze.Ordinal)
		case cloze.Start < position:
			// Anki cannot represent nested deletions, so overlap has to be
			// rejected here rather than producing quietly broken cards.
			return "", fmt.Errorf("ir: cloze c%d overlaps the previous deletion", cloze.Ordinal)
		case cloze.Ordinal < 1:
			return "", fmt.Errorf("ir: cloze ordinal must be 1 or greater, got %d", cloze.Ordinal)
		}

		out.WriteString(text[position:cloze.Start])
		out.WriteString("{{c" + strconv.Itoa(cloze.Ordinal) + "::")
		out.WriteString(text[cloze.Start:cloze.End])
		if cloze.Hint != "" {
			out.WriteString("::" + cloze.Hint)
		}
		out.WriteString("}}")
		position = cloze.End
	}

	out.WriteString(text[position:])
	return out.String(), nil
}

// NextOrdinal returns the ordinal a new deletion should take, so each cloze
// added to an item becomes its own card.
func NextOrdinal(existing []Cloze) int {
	highest := 0
	for _, cloze := range existing {
		if cloze.Ordinal > highest {
			highest = cloze.Ordinal
		}
	}
	return highest + 1
}
