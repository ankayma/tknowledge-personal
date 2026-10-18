// Package version says which build is running, read from what the Go
// toolchain stamps into every binary built inside the git checkout: the commit,
// its time, and whether the tree had uncommitted changes. Nothing has to be
// passed at build time, so a build cannot forget it.
package version

import (
	"runtime/debug"
	"time"
)

// String is "5ee8d5f · 2026-10-01 11:20 UTC", with " · dirty" when the binary
// was built from a tree with uncommitted changes — code that is in no commit
// and can be lost when the binary is replaced. "dev" when the build carries no
// VCS stamp (go test, go run, a build outside the checkout).
func String() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, at string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return format(rev, at, dirty)
}

func format(rev, at string, dirty bool) string {
	if rev == "" {
		return "dev"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	out := rev
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		out += " · " + t.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	if dirty {
		out += " · dirty"
	}
	return out
}
