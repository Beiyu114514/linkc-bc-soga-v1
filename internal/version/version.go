// Package version holds build metadata.
package version

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func UserAgent() string { return "linkc-node/" + Version }
