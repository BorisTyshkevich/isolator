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

// ProxySocketDir is the dir holding per-user proxy sockets:
// /var/run/isolator-docker/<user>.sock. Overridable for tests.
var ProxySocketDir = "/var/run/isolator-docker"

// Constants required across the package per §2.
const (
	MaxBodySize = 16 * 1024 * 1024 // 16 MB
	OwnerLabel  = "dev.boris.isolator.user"
)

// IsPathAllowed reports whether path (already normalized via ResolvePath, see
// §9.5) is inside one of the user's allowed roots:
//   - <WorkspacesDir>/<user> (root or subtree)
//   - /Users/<user>/tmp     (root or subtree)
//   - <ProxySocketDir>/<user>.sock (exact match — the user's own proxy
//     socket, needed by Ryuk and similar docker-in-docker tools; see
//     docs/ryuk-and-the-proxy-socket.md)
func IsPathAllowed(path, user string) bool {
	wsRoot := WorkspacesDir + "/" + user
	if path == wsRoot || strings.HasPrefix(path, wsRoot+"/") {
		return true
	}
	tmpRoot := UsersHomePrefix + "/" + user + "/tmp"
	if path == tmpRoot || strings.HasPrefix(path, tmpRoot+"/") {
		return true
	}
	literal := ProxySocketDir + "/" + user + ".sock"
	if path == literal {
		return true
	}
	// On macOS /var is a symlink to /private/var; ResolvePath of the bind
	// source ends up at /private/var/run/... so accept the resolved form too.
	if resolved, err := filepath.EvalSymlinks(literal); err == nil && path == resolved {
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
