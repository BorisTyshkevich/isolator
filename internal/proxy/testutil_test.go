package proxy

import "os"

// makeAllDirs is a thin wrapper around os.MkdirAll used by tests so test files
// don't need to import "os" individually.
func makeAllDirs(p string) error {
	return os.MkdirAll(p, 0o755)
}

// makeSymlink wraps os.Symlink for test use.
func makeSymlink(target, link string) error {
	return os.Symlink(target, link)
}

// mkShortTempDir creates a short-path temp directory under /tmp. macOS's
// default TMPDIR (/var/folders/...) blows past the ~104 char Unix-socket
// pathname limit, which makes net.Listen("unix", ...) fail. Tests that bind
// Unix sockets must use this helper.
func mkShortTempDir(prefix string) (string, error) {
	return os.MkdirTemp("/tmp", prefix)
}

// removeAll wraps os.RemoveAll.
func removeAll(p string) error {
	return os.RemoveAll(p)
}
