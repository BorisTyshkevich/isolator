// Command docker-proxy is a per-user Unix-socket proxy in front of the real
// Docker daemon socket. It enforces multi-tenant isolation as specified in
// docs/docker-proxy-go-spec.md. See §1 for the threat model.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bvt/isolator/internal/proxy"
)

func main() {
	var (
		username           = flag.String("user", "", "Username of the sandboxed user (required)")
		socketPath         = flag.String("socket", "", "Path for the proxy's Unix socket (required)")
		upstream           = flag.String("upstream", "/var/run/docker.sock", "Path to the real Docker daemon socket")
		insecureSkipChecks = flag.Bool("insecure-skip-checks", false, "Skip parent dir ownership and user UID checks (testing)")
	)
	flag.Parse()

	// log to stdout per §17.
	log.SetOutput(os.Stdout)
	log.SetFlags(0)

	if *username == "" || *socketPath == "" {
		fmt.Fprintln(os.Stderr, "FATAL: --user and --socket are required")
		flag.Usage()
		os.Exit(2)
	}

	// 2. Resolve target user UID.
	targetUID, err := resolveUID(*username, *insecureSkipChecks)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	// 3. Verify parent dir ownership.
	if !*insecureSkipChecks {
		if err := verifyParentRootOwned(*socketPath); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
	}

	// 4. Remove stale socket.
	_ = os.Remove(*socketPath)

	// 5. Bind socket with restrictive permissions (umask 0077 -> mode 0600).
	oldUmask := syscall.Umask(0o077)
	listener, err := net.Listen("unix", *socketPath)
	syscall.Umask(oldUmask)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: net.Listen: %v\n", err)
		os.Exit(1)
	}

	// 6. Set socket ownership.
	if !*insecureSkipChecks {
		if err := os.Chown(*socketPath, targetUID, -1); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: chown %s -> %d: %v\n", *socketPath, targetUID, err)
			_ = listener.Close()
			os.Exit(1)
		}
	}
	// 7. Set mode explicitly.
	if err := os.Chmod(*socketPath, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: chmod %s 600: %v\n", *socketPath, err)
		_ = listener.Close()
		os.Exit(1)
	}

	// 9. Log startup.
	log.Printf("[%s] proxy: %s (uid=%d, mode 600) -> %s", *username, *socketPath, targetUID, *upstream)

	// Build handler and server.
	handler := proxy.NewHandler(*username, *upstream)

	// Wrap listener with peer-UID enforcement.
	wrapped := &peerUIDListener{
		Listener:    listener,
		expectedUID: uint32(targetUID),
		user:        *username,
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	// 10. Signal handler.
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			_ = server.Close()
			_ = listener.Close()
			_ = os.Remove(*socketPath)
		})
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(0)
	}()

	if err := server.Serve(wrapped); err != nil && err != http.ErrServerClosed {
		log.Printf("[%s] ERROR: server.Serve: %v", *username, err)
	}
	cleanup()
}

// resolveUID looks up username and returns its UID. Under
// --insecure-skip-checks a missing user is tolerated and the current process
// UID is returned (used only for logging in that case).
func resolveUID(username string, insecure bool) (int, error) {
	u, err := user.Lookup(username)
	if err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		return uid, nil
	}
	if insecure {
		return os.Getuid(), nil
	}
	return 0, fmt.Errorf("FATAL: user %q not found", username)
}

// verifyParentRootOwned checks that the parent dir of socketPath is owned by
// uid 0 (per §3 step 3). Returns a fatal-formatted error.
func verifyParentRootOwned(socketPath string) error {
	parent := filepath.Dir(socketPath)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("FATAL: stat %s: %v", parent, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("FATAL: %s: stat unavailable", parent)
	}
	if stat.Uid != 0 {
		return fmt.Errorf("FATAL: %s is not root-owned (uid=%d) -- refusing to start", parent, stat.Uid)
	}
	return nil
}

// peerUIDListener wraps a Unix listener and enforces SO_PEERCRED/LOCAL_PEERCRED
// matching expectedUID. Connections that fail the check are closed
// immediately. Lookup failures are tolerated (defense-in-depth, §4).
type peerUIDListener struct {
	net.Listener
	expectedUID uint32
	user        string
}

func (l *peerUIDListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			// Not a Unix conn — pass through (only for tests via TCP).
			return c, nil
		}
		uid, ok := proxy.GetPeerUID(uc)
		if !ok {
			// Lookup failure: allow through.
			return c, nil
		}
		if uid != l.expectedUID {
			log.Printf("[%s] BLOCKED: peer uid=%d does not match expected uid=%d", l.user, uid, l.expectedUID)
			_ = c.Close()
			continue
		}
		return c, nil
	}
}
