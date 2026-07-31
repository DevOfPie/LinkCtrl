// Package build carries identifying information about the running binary.
//
// Values are injected at link time via -ldflags; see the Makefile. When the
// binary is built without them (go run, go test, a plain go build) the values
// fall back to the module's VCS stamp where Go provides one, and to "dev"
// otherwise, so a version string is never empty.
package build

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// Injected via -ldflags "-X github.com/DevOfPie/LinkCtrl/internal/build.version=...".
var (
	version = ""
	commit  = ""
	date    = ""
)

const unknown = "unknown"

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

var (
	once   sync.Once
	cached Info
)

// Get returns the build information for this binary. It is safe for
// concurrent use and computes its answer once.
func Get() Info {
	once.Do(func() {
		cached = Info{
			Version:   version,
			Commit:    commit,
			Date:      date,
			GoVersion: runtime.Version(),
			Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		}

		// Fill any gaps from the VCS stamp the toolchain embeds. This is what
		// makes `go run ./cmd/linkctrl --version` report something useful
		// during development without a Makefile in the loop.
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if cached.Commit == "" {
						cached.Commit = s.Value
					}
				case "vcs.time":
					if cached.Date == "" {
						cached.Date = s.Value
					}
				case "vcs.modified":
					if s.Value == "true" {
						cached.Version += "-dirty"
					}
				}
			}
		}

		if cached.Version == "" || cached.Version == "-dirty" {
			cached.Version = "dev" + cached.Version
		}
		if cached.Commit == "" {
			cached.Commit = unknown
		}
		if cached.Date == "" {
			cached.Date = unknown
		}
	})
	return cached
}

// ShortCommit returns the commit truncated to the customary 12 characters,
// or the full value when it is shorter (for example "unknown").
func (i Info) ShortCommit() string {
	if len(i.Commit) > 12 {
		return i.Commit[:12]
	}
	return i.Commit
}

func (i Info) String() string {
	return fmt.Sprintf("linkctrl %s (commit %s, built %s, %s, %s)",
		i.Version, i.ShortCommit(), i.Date, i.GoVersion, i.Platform)
}
