package web

import (
	"os"
	"os/exec"
	"testing"
)

// TestReaderInBrowser runs the selection-flow checks in a real browser.
//
// Skipped unless INCREADER_BROWSER_TEST names a running instance, because it
// needs node, playwright and a downloaded browser — none of which belong in the
// dependency set of an application whose whole point is shipping as one static
// binary.
//
// It is wired into `go test` regardless, so the skip line is a standing
// reminder that this path exists and is not covered by anything else. The tests
// in this package drive the handlers directly: they prove a correct request
// yields a correct extract, and are silent on whether the browser ever sends
// one. Every bug in the selection toolbar so far has lived in that gap.
func TestReaderInBrowser(t *testing.T) {
	base := os.Getenv("INCREADER_BROWSER_TEST")
	if base == "" {
		t.Skip("set INCREADER_BROWSER_TEST=http://127.0.0.1:8080 to run the browser checks")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}

	command := exec.Command("node", "browser/reader_test.mjs")
	command.Env = append(os.Environ(), "BASE="+base)

	output, err := command.CombinedOutput()
	t.Logf("browser checks:\n%s", output)
	if err != nil {
		t.Fatalf("browser checks failed: %v", err)
	}
}
