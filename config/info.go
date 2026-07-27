// Package config loads and validates the mirror server configuration,
// publishes it for reload-aware readers, configures logging, and carries
// the application identity and build identifiers.
package config

// Build identifiers populated at build time via -ldflags.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
	Mode    = "dev"
)

// Application identifiers used by the CLI and the upstream user agent.
const (
	Name        = "repo-sync"
	DisplayName = "Repository Sync"
	Description = "Synchronize Linux package repositories into a local directory tree."
)
