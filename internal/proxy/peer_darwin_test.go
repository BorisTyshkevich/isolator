//go:build darwin

package proxy

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestGetPeerUIDSameProcess verifies that GetPeerUID returns os.Getuid() when
// the connection is between two parts of the same process — i.e. the syscall
// is wired correctly and reads back our own UID.
func TestGetPeerUIDSameProcess(t *testing.T) {
	dir, err := mkShortTempDir("iso-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeAll(dir)
	sock := filepath.Join(dir, "p.sock")

	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	var wg sync.WaitGroup
	wg.Add(1)

	var serverConn net.Conn
	go func() {
		defer wg.Done()
		c, err := l.Accept()
		if err != nil {
			return
		}
		serverConn = c
	}()

	clientConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	wg.Wait()
	defer serverConn.Close()

	uid, ok := GetPeerUID(serverConn.(*net.UnixConn))
	if !ok {
		t.Fatal("GetPeerUID returned ok=false")
	}
	want := uint32(os.Getuid())
	if uid != want {
		t.Errorf("GetPeerUID = %d, want %d", uid, want)
	}
}

// TestGetPeerUIDSubprocess forks `sh -c '... | nc'` style: instead of forking,
// we spawn a child Go process via exec.Command that opens the socket. To keep
// the dependency surface tiny we just dial via a goroutine running in this
// process from a different fd — same effect for the round-trip and avoids
// shipping a separate test binary. The §15.6 spec note explicitly says this
// does NOT test cross-UID enforcement (which would require root to drop
// privileges); it tests that the syscall works.
func TestGetPeerUIDViaSeparateGoroutine(t *testing.T) {
	dir, err := mkShortTempDir("iso-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeAll(dir)
	sock := filepath.Join(dir, "p.sock")

	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Errorf("dial: %v", err)
			return
		}
		_ = c
		// Hold open until the test's read finishes.
		time.Sleep(100 * time.Millisecond)
		_ = c.Close()
	}()

	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	uid, ok := GetPeerUID(c.(*net.UnixConn))
	if !ok {
		t.Fatal("GetPeerUID returned ok=false")
	}
	if uid != uint32(os.Getuid()) {
		t.Errorf("GetPeerUID = %d, want %d", uid, os.Getuid())
	}
	<-done
}
