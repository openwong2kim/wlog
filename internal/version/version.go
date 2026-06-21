// Package version holds the build version metadata for the wlog binary.
// Commit and Date are injected at build time via -ldflags.
package version

import "fmt"

// Version is the semantic version of the build. Overridable via ldflags.
var Version = "0.1.0-dev"

// Commit is the git commit hash, injected at build time via ldflags.
var Commit string

// Date is the build timestamp, injected at build time via ldflags.
var Date string

// String returns a human-readable one-line version description including
// the commit and build date when they have been injected.
func String() string {
	s := "wlog " + Version
	if Commit != "" {
		s += " (" + Commit
		if Date != "" {
			s += ", " + Date
		}
		s += ")"
	} else if Date != "" {
		s += " (" + Date + ")"
	}
	return s
}

// Short returns just the version token (e.g. "v0.1.0-dev") for compact
// startup banners per DESIGN §6.
func Short() string {
	return fmt.Sprintf("v%s", Version)
}
