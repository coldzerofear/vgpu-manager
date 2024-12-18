package version

// Base version information.
//
// This is the fallback data used when version information from git is not
// provided via go ldflags. It provides an approximation of the Apiswitch
// version for ad-hoc builds (e.g. `go build`) that cannot get the version
// information from git.
//
// If you are looking at these fields in the git tree, they look
// strange. They are modified on the fly by the build process. The
// in-tree values are dummy values used for "git archive", which also
// works for GitHub tar downloads.
var (
	// branch of git
	gitBranch string = "Not a git repo"
	// sha1 from git, output of $(git rev-parse HEAD)
	gitCommit string = "$Format:%H$"
	// state of git tree, either "clean" or "dirty"
	gitTreeState string = "Not a git tree"
	// build date in ISO8601 format, output of $(date -u +'%Y-%m-%dT%H:%M:%SZ')
	buildDate string = "1970-01-01T00:00:00Z"
)
