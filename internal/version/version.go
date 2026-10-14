// Package version holds the build version (set with -ldflags -X).
package version

// Version is overridden at build time: -X .../internal/version.Version=2.0.0
var Version = "2.0.0-dev"
