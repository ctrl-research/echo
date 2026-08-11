// Package version exposes build metadata, stamped via -ldflags at build time
// and falling back to Go's embedded VCS info for local builds.
package version

import "runtime/debug"

var (
	// Version is the release tag, set with
	//   -ldflags "-X github.com/jonathanng/echo/internal/version.Version=v1.2.3"
	Version = "dev"
	// Commit is the git SHA, set the same way.
	Commit = ""
	// Date is the build timestamp in RFC 3339, set the same way.
	Date = ""
)

func init() {
	if Commit != "" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			Commit = s.Value
		case "vcs.time":
			if Date == "" {
				Date = s.Value
			}
		}
	}
}

// String renders the full build identity for logs and the health endpoint.
func String() string {
	s := Version
	if Commit != "" {
		short := Commit
		if len(short) > 12 {
			short = short[:12]
		}
		s += "+" + short
	}
	return s
}
