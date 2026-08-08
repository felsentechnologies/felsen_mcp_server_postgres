// Package version contains build-time identity for the MCP server.
package version

var (
	// Version is kept in sync with the repository VERSION file and can be
	// overridden by the release/container build with -ldflags -X.
	Version = "0.3.0"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns the semantic version advertised through MCP.
func String() string {
	return Version
}

// BuildString returns the version plus optional immutable build metadata for
// CLI diagnostics and health tooling.
func BuildString() string {
	result := Version
	metadata := ""
	if Commit != "" && Commit != "unknown" {
		metadata = Commit
	}
	if Date != "" && Date != "unknown" {
		if metadata != "" {
			metadata += ", "
		}
		metadata += Date
	}
	if metadata != "" {
		result += " (" + metadata + ")"
	}
	return result
}
