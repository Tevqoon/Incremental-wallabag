package pagescan

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestParseResultPlain(t *testing.T) {
	raw := `{"pages":[{"page_label":"12","running_head":"HEAD","headings":[],"lines":["a line"],"marks":[]}]}`
	result, err := ParseResult(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Pages) != 1 || result.Pages[0].Label != "12" {
		t.Fatalf("got %+v", result)
	}
}

func TestParseResultFenced(t *testing.T) {
	raw := "```json\n{\"pages\":[]}\n```"
	result, err := ParseResult(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Pages) != 0 {
		t.Fatalf("got %+v, want empty pages", result)
	}
}

func TestParseResultFencedNoLanguageTag(t *testing.T) {
	raw := "```\n{\"pages\":[]}\n```"
	result, err := ParseResult(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Pages) != 0 {
		t.Fatalf("got %+v, want empty pages", result)
	}
}

func TestParseResultNullLabel(t *testing.T) {
	raw := `{"pages":[{"page_label":null,"running_head":null,"headings":[],"lines":[],"marks":[]}]}`
	result, err := ParseResult(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Pages[0].Label != "" {
		t.Errorf("Label: got %q, want \"\" for a null page_label", result.Pages[0].Label)
	}
	if result.Pages[0].RunningHead != "" {
		t.Errorf("RunningHead: got %q, want \"\" for a null running_head", result.Pages[0].RunningHead)
	}
}

func TestParseResultGarbage(t *testing.T) {
	if _, err := ParseResult("not json at all"); err == nil {
		t.Fatal("expected an error for non-JSON input")
	}
}

func TestParseResultMissingPagesKey(t *testing.T) {
	if _, err := ParseResult(`{"foo":"bar"}`); err == nil {
		t.Fatal("expected an error for JSON with no \"pages\" key")
	}
}

func TestParseResultEmptyPagesIsValid(t *testing.T) {
	result, err := ParseResult(`{"pages":[]}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Pages) != 0 {
		t.Fatalf("got %d pages, want 0", len(result.Pages))
	}
}

func TestSetPageLabelRoundTrip(t *testing.T) {
	raw := `{"pages":[{"page_label":"1","running_head":"HEAD","headings":["H"],"lines":["a"],"marks":[]},{"page_label":"2","running_head":"","headings":[],"lines":[],"marks":[]}]}`
	updated, err := SetPageLabel(raw, 1, "2a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	result, err := ParseResult(updated)
	if err != nil {
		t.Fatalf("re-parse failed: %v", err)
	}
	if result.Pages[1].Label != "2a" {
		t.Errorf("Label: got %q, want 2a", result.Pages[1].Label)
	}
	// The untouched page must survive the round trip unchanged.
	if result.Pages[0].Label != "1" || result.Pages[0].RunningHead != "HEAD" || len(result.Pages[0].Headings) != 1 {
		t.Errorf("page 0 was disturbed by the round trip: %+v", result.Pages[0])
	}
}

func TestSetPageLabelOutOfRange(t *testing.T) {
	raw := `{"pages":[{"page_label":"1"}]}`
	if _, err := SetPageLabel(raw, 5, "x"); err == nil {
		t.Fatal("expected an error for an out-of-range page index")
	}
	if _, err := SetPageLabel(raw, -1, "x"); err == nil {
		t.Fatal("expected an error for a negative page index")
	}
}

func TestRefPrefix(t *testing.T) {
	if got := RefPrefix(5, 2, "140"); got != "scan:p140" {
		t.Errorf("got %q, want scan:p140", got)
	}
	if got := RefPrefix(5, 2, ""); got != "scan:s5.2" {
		t.Errorf("got %q, want scan:s5.2", got)
	}
}

// --- SniffImageType ---

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 4, G: 5, B: 6, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSniffImageTypePNG(t *testing.T) {
	mime, ok := SniffImageType(tinyPNG(t), "")
	if !ok || mime != "image/png" {
		t.Errorf("got (%q, %v), want (image/png, true)", mime, ok)
	}
}

func TestSniffImageTypeJPEG(t *testing.T) {
	mime, ok := SniffImageType(tinyJPEG(t), "")
	if !ok || mime != "image/jpeg" {
		t.Errorf("got (%q, %v), want (image/jpeg, true)", mime, ok)
	}
}

func TestSniffImageTypeTrustsBytesOverDeclared(t *testing.T) {
	// A PNG declared (wrongly) as a JPEG should still sniff as PNG.
	mime, ok := SniffImageType(tinyPNG(t), "image/jpeg")
	if !ok || mime != "image/png" {
		t.Errorf("got (%q, %v), want (image/png, true) — bytes should win over declared", mime, ok)
	}
}

func TestSniffImageTypeHEIC(t *testing.T) {
	// Minimal ISO-BMFF ftyp box: 4-byte size, "ftyp", 4-byte major brand.
	data := []byte("\x00\x00\x00\x18ftypheic")
	mime, ok := SniffImageType(data, "")
	if !ok || mime != "image/heic" {
		t.Errorf("got (%q, %v), want (image/heic, true)", mime, ok)
	}
}

func TestSniffImageTypeHEIF(t *testing.T) {
	data := []byte("\x00\x00\x00\x18ftypmif1")
	mime, ok := SniffImageType(data, "")
	if !ok || mime != "image/heif" {
		t.Errorf("got (%q, %v), want (image/heif, true)", mime, ok)
	}
}

func TestSniffImageTypeUnrecognisedFallsBackToDeclared(t *testing.T) {
	garbage := []byte("this is not an image at all, just some bytes")
	mime, ok := SniffImageType(garbage, "image/heic")
	if !ok || mime != "image/heic" {
		t.Errorf("got (%q, %v), want (image/heic, true) — declared type as a fallback", mime, ok)
	}
}

func TestSniffImageTypeUnrecognisedAndUndeclaredRejected(t *testing.T) {
	garbage := []byte("this is not an image at all, just some bytes")
	_, ok := SniffImageType(garbage, "")
	if ok {
		t.Error("expected ok=false for unrecognised bytes with no usable declared type")
	}
}

func TestPagesCarriesTheFirstPrintedLine(t *testing.T) {
	pages := Pages([]Scan{{ID: 3, Result: Result{Pages: []Page{{
		Lines: []string{"  ", "¶ current anticorruption   campaign seems", "inspired by"},
	}}}}})
	if len(pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(pages))
	}
	if want := "current anticorruption campaign seems"; pages[0].FirstLine != want {
		t.Errorf("FirstLine: got %q, want %q", pages[0].FirstLine, want)
	}
}
