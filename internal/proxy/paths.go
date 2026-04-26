package proxy

import (
	"path/filepath"
	"strings"
)

// WorkspacesDir is the per-user workspace root prefix. Hardcoded as a package-
// level variable (not a const) so tests can override it via tempdirs. See §3
// constants and §15.2 (the symlink test requires temp-dir setup).
var WorkspacesDir = "/Users/Workspaces"

// UsersHomePrefix is the prefix under which the per-user tmp directory lives:
// /Users/<user>/tmp. Made overridable for tests as well.
var UsersHomePrefix = "/Users"

// Constants required across the package per §2.
const (
	MaxBodySize = 16 * 1024 * 1024 // 16 MB
	OwnerLabel  = "dev.boris.isolator.user"
)

// IsPathAllowed reports whether path (already normalized via ResolvePath, see
// §9.5) is inside one of the user's allowed roots:
//   - <WorkspacesDir>/<user> (root or subtree)
//   - /Users/<user>/tmp     (root or subtree)
func IsPathAllowed(path, user string) bool {
	wsRoot := WorkspacesDir + "/" + user
	if path == wsRoot || strings.HasPrefix(path, wsRoot+"/") {
		return true
	}
	tmpRoot := UsersHomePrefix + "/" + user + "/tmp"
	if path == tmpRoot || strings.HasPrefix(path, tmpRoot+"/") {
		return true
	}
	return false
}

// ResolvePath normalizes source per §9.5 step 3:
//   - filepath.Clean (always, regardless of existence; collapses ".." and ".")
//   - filepath.EvalSymlinks (fall back to the cleaned path if EvalSymlinks errors;
//     typical cause is that the path does not exist yet)
func ResolvePath(source string) string {
	cleaned := filepath.Clean(source)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return cleaned
	}
	return resolved
}
