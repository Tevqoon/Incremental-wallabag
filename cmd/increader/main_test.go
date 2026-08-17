package main

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/Tevqoon/increader/internal/config"
)

// TestSplitCommand exercises both flag orderings splitCommand exists to
// unify — a leading subcommand (`increader import-substack -commit`) and a
// leading flag set (`increader -config c.yaml import-substack`) — for every
// subcommand increader accepts, import-substack included.
func TestSplitCommand(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCommand string
		wantRest    []string
	}{
		{
			name:        "command first",
			args:        []string{"sync", "-full"},
			wantCommand: "sync",
			wantRest:    []string{"-full"},
		},
		{
			name:        "flags first, no command",
			args:        []string{"-config", "c.yaml"},
			wantCommand: "",
			wantRest:    []string{"-config", "c.yaml"},
		},
		{
			name:        "import-substack, command first",
			args:        []string{"import-substack", "-commit"},
			wantCommand: "import-substack",
			wantRest:    []string{"-commit"},
		},
		{
			name:        "import-substack, flags first",
			args:        []string{"-config", "c.yaml", "-commit", "import-substack"},
			wantCommand: "",
			wantRest:    []string{"-config", "c.yaml", "-commit", "import-substack"},
		},
		{
			name:        "import-substack with host override, command first",
			args:        []string{"import-substack", "-host", "example.substack.com", "-commit"},
			wantCommand: "import-substack",
			wantRest:    []string{"-host", "example.substack.com", "-commit"},
		},
		{
			name:        "no args",
			args:        nil,
			wantCommand: "",
			wantRest:    nil,
		},
		{
			name: "a flag value that happens to be spelled like a command is not " +
				"mistaken for one",
			args:        []string{"-config", "sync"},
			wantCommand: "",
			wantRest:    []string{"-config", "sync"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, rest := splitCommand(tt.args)
			if command != tt.wantCommand {
				t.Errorf("command = %q, want %q", command, tt.wantCommand)
			}
			if !reflect.DeepEqual(rest, tt.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

// TestCommandsIncludesImportSubstack guards against the flags-first ordering
// silently breaking again if import-substack were ever removed from the
// commands map without removing its case in the switch — splitCommand can
// only recognise a leading subcommand it knows about.
func TestCommandsIncludesImportSubstack(t *testing.T) {
	if !commands["import-substack"] {
		t.Error(`commands["import-substack"] = false, want true`)
	}
}

// TestImportSubstackURLHandlerGating: the single-URL web import needs only
// the session cookie, unlike Enabled() (which the whole-archive backfill
// command uses and which also requires Host) — a pasted URL supplies its
// own publication, so requiring one already configured in config.yaml would
// make the feature unusable for exactly the case it exists for: a
// publication other than the one, if any, an archive backfill is set up
// for. db is nil in both cases; the handler must never be invoked here, only
// checked for nil-ness, so that is never dereferenced.
func TestImportSubstackURLHandlerGating(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var settings config.Config
	if got := importSubstackURLHandler(nil, settings, logger); got != nil {
		t.Error("handler is non-nil with no session cookie configured")
	}

	settings.Ingest.Substack.SessionCookie = "s%3Asecret"
	if got := importSubstackURLHandler(nil, settings, logger); got == nil {
		t.Error("handler is nil despite a session cookie being configured")
	}
}

// TestRefreshSubstackFeedHandlerGating: unlike importSubstackURLHandler,
// this one gates on the full Enabled() (host and session cookie both) — a
// whole-archive refresh has no per-request URL to take a host from the way
// the single-URL import does, so it can only ever run against whichever one
// publication ingest.substack itself names.
func TestRefreshSubstackFeedHandlerGating(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var settings config.Config
	if got := refreshSubstackFeedHandler(nil, settings, logger); got != nil {
		t.Error("handler is non-nil with nothing configured")
	}

	settings.Ingest.Substack.SessionCookie = "s%3Asecret"
	if got := refreshSubstackFeedHandler(nil, settings, logger); got != nil {
		t.Error("handler is non-nil with a session cookie but no host")
	}

	settings.Ingest.Substack.Host = "example.substack.com"
	if got := refreshSubstackFeedHandler(nil, settings, logger); got == nil {
		t.Error("handler is nil despite both host and session cookie being configured")
	}
}
