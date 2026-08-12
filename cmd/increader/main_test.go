package main

import (
	"reflect"
	"testing"
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
