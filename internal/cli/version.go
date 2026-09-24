package cli

import "strings"

// version is populated by cmd/homer from the GoReleaser ldflag. Keeping the
// default here makes package-level CLI tests and local development builds
// print the explicit development marker.
var version = "dev"

// SetVersion is called once by the thin binary entry point. It is additive so
// callers embedding the cli package can still use the default dev version.
func SetVersion(value string) {
	if strings.TrimSpace(value) != "" {
		version = value
	}
}

func currentVersion() string {
	if strings.TrimSpace(version) == "" {
		return "dev"
	}
	return version
}

func renderVersion() string {
	return "homer version: " + currentVersion()
}
