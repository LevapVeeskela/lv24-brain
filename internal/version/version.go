// Package version carries build identity shared by the CLI and server.
package version

// Version is the engine version. Override at build time with
// -ldflags "-X github.com/levapveeskela/lv24-brain/internal/version.Version=...".
var (
	Version = "0.1.0"
	Commit  = "dev"
	Date    = "unknown"
)
