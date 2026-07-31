// Package version reports which build of increader is running.
//
// It exists because "am I actually running the code I just wrote" is otherwise
// unanswerable from outside the process — and the usual way to get it wrong is
// quiet: `docker compose up` reuses an existing image rather than rebuilding,
// so a stale binary starts up and behaves like a slightly older application
// with nothing anywhere saying so.
package version

import (
	"runtime/debug"
	"strings"
	"time"
)

// Info describes the running build.
type Info struct {
	// Revision is the git commit, or "unknown" outside a repository.
	Revision string

	// Time is when that commit was made.
	Time time.Time

	// Dirty reports that the working tree had uncommitted changes at build
	// time, which means Revision alone does not identify what is running.
	Dirty bool
}

// Short renders the build for a log line or a page footer.
func (i Info) Short() string {
	if i.Revision == "" {
		// `go run` does not stamp VCS metadata, so this is the normal answer
		// while developing. A container is always built with `go build` from a
		// checkout, so an unstamped one there means .git never reached the
		// build context.
		return "development build"
	}

	short := i.Revision
	if len(short) > 7 {
		short = short[:7]
	}
	if i.Dirty {
		short += "-dirty"
	}
	if !i.Time.IsZero() {
		short += " (" + i.Time.Local().Format("2006-01-02 15:04") + ")"
	}
	return short
}

// Current reads the build metadata the Go toolchain stamps into the binary.
//
// No linker flags or build arguments are involved: the toolchain records the
// VCS state automatically whenever it builds from a repository, and it survives
// -trimpath. The one requirement is that the .git directory reaches the build —
// which is why .dockerignore must not exclude it.
func Current() Info {
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return Info{}
	}

	var info Info
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			info.Revision = setting.Value
		case "vcs.time":
			if parsed, err := time.Parse(time.RFC3339, setting.Value); err == nil {
				info.Time = parsed
			}
		case "vcs.modified":
			info.Dirty = strings.EqualFold(setting.Value, "true")
		}
	}
	return info
}
