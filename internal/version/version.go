// Package version holds the version of Locutus and the Locutus client
// container, which are released together.
package version

// Version is the semantic version.
var Version = "v1.2"

// Build is the short git commit id, injected at build time with
// -ldflags "-X itmon.nl/locutus/internal/version.Build=...".
var Build = "dev"
