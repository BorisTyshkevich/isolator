package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsPathAllowed exercises the table from §15.2. Note that IsPathAllowed
// expects already-normalized input (caller does Clean+EvalSymlinks), so for
// these table cases we feed cleaned paths directly. The "../" and "./" rows
// are normalized via filepath.Clean before the check, mirroring what
// validateAndRewriteBind does in production.
func TestIsPathAllowed(t *testing.T) {
	const user = "acm"

	// Override the workspace and home prefixes to fixed values so tests run
	// the same on any host. (Defaults already match these values, but
	// asserting them keeps the test self-explanatory.)
	prevWS, prevHome := WorkspacesDir, UsersHomePrefix
	WorkspacesDir = "/Users/Workspaces"
	UsersHomePrefix = "/Users"
	t.Cleanup(func() {
		WorkspacesDir = prevWS
		UsersHomePrefix = prevHome
	})

	cases := []struct {
		path string
		want bool
		note string
	}{
		{"/Users/Workspaces/acm", true, "ws root"},
		{"/Users/Workspaces/acm/project", true, "ws subdir"},
		{"/Users/Workspaces/acm/project/deep/dir", true, "deep subdir"},
		{"/Users/acm/tmp", true, "user tmp"},
		{"/Users/acm/tmp/subdir", true, "user tmp subdir"},
		{"/tmp", false, "shared tmp"},
		{"/private/tmp", false, "macos shared tmp"},
		{"/Users/Workspaces/other", false, "other ws"},
		{"/Users/Workspaces/other/project", false, "other ws subdir"},
		{"/Users/Workspaces/acm-evil", false, "prefix attack"},
		{"/Users/Workspaces/acm-evil/project", false, "prefix attack subdir"},
		// "/Users/Workspaces/acm/../other/secret" must be rejected after Clean
		{filepath.Clean("/Users/Workspaces/acm/../other/secret"), false, ".. traversal"},
		// "/Users/Workspaces/acm/./project" should normalize to /Users/Workspaces/acm/project
		{filepath.Clean("/Users/Workspaces/acm/./project"), true, ". segment"},
		{"/Users/admin", false, "admin home"},
		{"/etc", false, "system dir"},
		{"/etc/passwd", false, "system file"},
		{"/var/run", false, "var dir"},
		{"/var/run/docker.sock", false, "docker socket"},
		{"/", false, "root"},
		{"/Users/Workspaces", false, "parent of all workspaces"},
		// Proxy socket exception — own socket allowed, others rejected.
		{"/var/run/isolator-docker/acm.sock", true, "own proxy sock"},
		{"/var/run/isolator-docker/other.sock", false, "other user's proxy sock"},
		{"/var/run/isolator-docker", false, "proxy sock dir itself"},
	}
	for _, tc := range cases {
		t.Run(tc.note+":"+tc.path, func(t *testing.T) {
			got := IsPathAllowed(tc.path, user)
			if got != tc.want {
				t.Errorf("IsPathAllowed(%q,%q) = %v, want %v", tc.path, user, got, tc.want)
			}
		})
	}
}

// TestIsPathAllowedProxySockResolved verifies that the proxy socket exception
// also matches the EvalSymlinks-resolved form. We simulate this by pointing
// ProxySocketDir at a temp dir, creating a symlink that resolves it to a
// different path, and verifying both the symlink path and the resolved target
// are accepted.
func TestIsPathAllowedProxySockResolved(t *testing.T) {
	tmp := t.TempDir()
	user := "acm"

	// real_dir is where the actual sock lives; sym_dir is a symlink to it.
	realDir := filepath.Join(tmp, "real")
	symDir := filepath.Join(tmp, "sym")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, symDir); err != nil {
		t.Fatal(err)
	}

	// Touch the sock file so EvalSymlinks succeeds.
	sockFile := filepath.Join(realDir, user+".sock")
	if f, err := os.Create(sockFile); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}

	prevDir := ProxySocketDir
	ProxySocketDir = symDir // points at the symlink
	t.Cleanup(func() { ProxySocketDir = prevDir })

	// Literal form — uses the symlink path.
	literal := symDir + "/" + user + ".sock"
	if !IsPathAllowed(literal, user) {
		t.Errorf("literal proxy sock path rejected: %q", literal)
	}
	// Resolved form — EvalSymlinks of the literal, what ResolvePath returns.
	resolved, err := filepath.EvalSymlinks(literal)
	if err != nil {
		t.Fatal(err)
	}
	if resolved == literal {
		t.Skip("EvalSymlinks did not change the path; macOS /var symlink behavior not exercised here")
	}
	if !IsPathAllowed(resolved, user) {
		t.Errorf("resolved proxy sock path rejected: %q", resolved)
	}
	// Other user's socket — must remain blocked even when own is allowed.
	if IsPathAllowed(symDir+"/other.sock", user) {
		t.Error("other user's proxy sock incorrectly allowed")
	}
}

// TestResolvePathSymlink creates a temp workspace, places a symlink inside it
// pointing outside (to /etc), and verifies that ResolvePath returns the
// outside target so IsPathAllowed rejects it.
func TestResolvePathSymlink(t *testing.T) {
	tmp := t.TempDir()
	user := "acm"

	// Build a fake workspace under tmp: <tmp>/Workspaces/acm
	ws := filepath.Join(tmp, "Workspaces", user)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}

	// Override package-level prefix so IsPathAllowed checks against tmp.
	prevWS, prevHome := WorkspacesDir, UsersHomePrefix
	WorkspacesDir = filepath.Join(tmp, "Workspaces")
	UsersHomePrefix = filepath.Join(tmp, "users")
	t.Cleanup(func() {
		WorkspacesDir = prevWS
		UsersHomePrefix = prevHome
	})

	// Create a symlink inside ws that points outside (we use /etc which exists
	// on macOS and Linux).
	link := filepath.Join(ws, "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}

	resolved := ResolvePath(link)
	if resolved == link {
		t.Fatalf("ResolvePath did not follow symlink: %s", resolved)
	}
	if IsPathAllowed(resolved, user) {
		t.Errorf("symlink target %q passed IsPathAllowed (should be rejected)", resolved)
	}
}

// TestResolvePathMissing exercises the EvalSymlinks-fallback branch: a path
// that does not exist must still be returned cleanly (Clean only).
func TestResolvePathMissing(t *testing.T) {
	got := ResolvePath("/nonexistent/foo/../bar")
	want := "/nonexistent/bar"
	if got != want {
		t.Errorf("ResolvePath fallback = %q, want %q", got, want)
	}
}
